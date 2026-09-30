"""
Makewand Multi-Model 3-Way AST & Patch Semantic Merger.

Enables combining the complementary strengths of concurrent race candidates
(e.g. Candidate A fixed function foo(), Candidate B improved function bar())
into a verified hybrid candidate (Candidate M), tested and applied atomically.
"""

import os
import sys
import ast
import shutil
import tempfile
import subprocess
from pathlib import Path
from typing import Dict, List, Optional, Tuple, Any, Set

from makewand.config import (
    c,
    COLOR_BOLD,
    COLOR_CYAN,
    COLOR_GREEN,
    COLOR_YELLOW,
    COLOR_RED,
    COLOR_RESET,
)
from makewand.git_helper import run_git_cmd, get_git_diff


def line_level_3way_merge(
    base_text: str,
    text_a: str,
    text_b: str,
    label_a: str = "Candidate_A",
    label_base: str = "Baseline",
    label_b: str = "Candidate_B"
) -> Tuple[bool, str, bool]:
    """
    Performs a 3-way line merge of text_a and text_b against base_text.
    Returns (success, merged_text, had_conflicts).
    """
    if text_a == text_b:
        return True, text_a, False
    if text_a == base_text:
        return True, text_b, False
    if text_b == base_text:
        return True, text_a, False

    with tempfile.TemporaryDirectory(prefix="makewand_merge_") as tmpdir:
        tmp_path = Path(tmpdir)
        f_base = tmp_path / "base.txt"
        f_a = tmp_path / "a.txt"
        f_b = tmp_path / "b.txt"

        f_base.write_text(base_text, encoding="utf-8", errors="replace")
        f_a.write_text(text_a, encoding="utf-8", errors="replace")
        f_b.write_text(text_b, encoding="utf-8", errors="replace")

        cmd = [
            "git", "merge-file", "-p",
            "-L", label_a,
            "-L", label_base,
            "-L", label_b,
            str(f_a), str(f_base), str(f_b)
        ]
        try:
            res = subprocess.run(cmd, capture_output=True, text=True, timeout=10)
            merged_out = res.stdout
            has_conflict_markers = "<<<<<<<" in merged_out and "=======" in merged_out
            if res.returncode == 0 and not has_conflict_markers:
                return True, merged_out, False
            return False, merged_out, True
        except Exception:
            pass

    # Pure Python fallback line merge
    import difflib
    diff_a = list(difflib.ndiff(base_text.splitlines(keepends=True), text_a.splitlines(keepends=True)))
    diff_b = list(difflib.ndiff(base_text.splitlines(keepends=True), text_b.splitlines(keepends=True)))
    # If fallback fails to merge cleanly
    return False, text_a, True


def extract_python_symbols(source_code: str) -> Dict[str, Tuple[int, int, str, ast.AST]]:
    """
    Extracts top-level functions, async functions, and classes from Python source.
    Returns a dict mapping symbol name to (start_line, end_line, source_slice, ast_node).
    Lines are 1-indexed.
    """
    symbols = {}
    try:
        tree = ast.parse(source_code)
    except SyntaxError:
        return symbols

    lines = source_code.splitlines(keepends=True)
    for node in tree.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
            start_line = getattr(node, "lineno", 1)
            end_line = getattr(node, "end_lineno", start_line)
            # Slice source lines
            slice_lines = "".join(lines[start_line - 1 : end_line])
            symbols[node.name] = (start_line, end_line, slice_lines, node)
    return symbols


def extract_python_imports(source_code: str) -> Tuple[List[str], int]:
    """
    Extracts import statements from Python source and the line number where imports end.
    Returns (import_lines, last_import_line).
    """
    import_lines = []
    last_line = 0
    try:
        tree = ast.parse(source_code)
    except SyntaxError:
        return [], 0

    lines = source_code.splitlines(keepends=True)
    for node in tree.body:
        if isinstance(node, (ast.Import, ast.ImportFrom)):
            start = getattr(node, "lineno", 1)
            end = getattr(node, "end_lineno", start)
            import_lines.append("".join(lines[start - 1 : end]).strip())
            if end > last_line:
                last_line = end
    return import_lines, last_line


def ast_merge_python_file(base_code: str, code_a: str, code_b: str) -> Tuple[bool, str, str]:
    """
    Performs an AST-aware semantic merge for Python files:
    1. Compares symbol definitions (functions, classes) in A and B against Base.
    2. If A modified a symbol and B left it unchanged, takes A's version.
    3. If B modified a symbol and A left it unchanged, takes B's version.
    4. Merges newly added imports and top-level additions.
    5. Falls back to line-level 3-way merge if both modified the same symbol or syntax errors exist.
    Returns (success, merged_code, merge_strategy).
    """
    if code_a == code_b:
        return True, code_a, "identical"
    if code_a == base_code:
        return True, code_b, "take_b"
    if code_b == base_code:
        return True, code_a, "take_a"

    syms_base = extract_python_symbols(base_code)
    syms_a = extract_python_symbols(code_a)
    syms_b = extract_python_symbols(code_b)

    # If any file couldn't be parsed, fallback immediately to line-level merge
    if not syms_base and not syms_a and not syms_b:
        ok, res, conflict = line_level_3way_merge(base_code, code_a, code_b)
        return ok, res, "line_3way" if ok else "conflict"

    # Analyze changes per symbol
    all_sym_names = sorted(set(syms_base.keys()) | set(syms_a.keys()) | set(syms_b.keys()))
    replacements_a: Dict[str, Tuple[int, int, str]] = {}
    replacements_b: Dict[str, Tuple[int, int, str]] = {}
    conflicting_symbols = []

    for name in all_sym_names:
        in_base = name in syms_base
        in_a = name in syms_a
        in_b = name in syms_b

        # Check modifications
        mod_a = False
        if in_base and in_a:
            mod_a = (syms_a[name][2].strip() != syms_base[name][2].strip())
        elif in_base != in_a:
            mod_a = True

        mod_b = False
        if in_base and in_b:
            mod_b = (syms_b[name][2].strip() != syms_base[name][2].strip())
        elif in_base != in_b:
            mod_b = True

        if mod_a and mod_b:
            # Both modified the same symbol
            if in_a and in_b and syms_a[name][2].strip() == syms_b[name][2].strip():
                # Both made the identical change to this symbol
                replacements_a[name] = syms_a[name][:3]  # type: ignore
            else:
                conflicting_symbols.append(name)
        elif mod_a:
            if in_a and in_base:
                replacements_a[name] = syms_a[name][:3]  # type: ignore
        elif mod_b:
            if in_b and in_base:
                replacements_b[name] = syms_b[name][:3]  # type: ignore

    # If both modified the same symbol differently, AST symbol-level splicing cannot resolve it
    if conflicting_symbols:
        ok, res, conflict = line_level_3way_merge(base_code, code_a, code_b)
        return ok, res, "line_3way" if ok else "conflict"

    # Splice symbol changes into base code lines
    base_lines = base_code.splitlines(keepends=True)
    # Sort replacements by line number descending so line indices don't shift earlier edits
    actions = []
    for name, (start, end, src) in replacements_a.items():
        base_start, base_end, _, _ = syms_base[name]
        actions.append((base_start, base_end, src))
    for name, (start, end, src) in replacements_b.items():
        base_start, base_end, _, _ = syms_base[name]
        actions.append((base_start, base_end, src))

    actions.sort(key=lambda x: x[0], reverse=True)

    # Apply symbol replacements
    for b_start, b_end, replacement_src in actions:
        rep_lines = replacement_src.splitlines(keepends=True)
        if not rep_lines or not rep_lines[-1].endswith("\n"):
            rep_lines.append("\n")
        base_lines[b_start - 1 : b_end] = rep_lines

    # Check for newly added symbols in A or B
    added_in_a = [name for name in syms_a if name not in syms_base and name not in syms_b]
    added_in_b = [name for name in syms_b if name not in syms_base and name not in syms_a]
    for name in added_in_a:
        base_lines.append("\n\n" + syms_a[name][2].strip() + "\n")
    for name in added_in_b:
        base_lines.append("\n\n" + syms_b[name][2].strip() + "\n")

    # Check for newly added imports
    imports_base, last_imp_base = extract_python_imports(base_code)
    imports_a, _ = extract_python_imports(code_a)
    imports_b, _ = extract_python_imports(code_b)

    new_imports = []
    base_imp_set = set(imports_base)
    for imp in imports_a + imports_b:
        if imp not in base_imp_set and imp not in new_imports:
            new_imports.append(imp)

    if new_imports:
        insert_idx = min(last_imp_base, len(base_lines))
        import_block = "".join(f"{imp}\n" for imp in new_imports)
        base_lines.insert(insert_idx, import_block)

    merged_code = "".join(base_lines)

    # Validate syntax of merged code
    try:
        ast.parse(merged_code)
        return True, merged_code, "ast_symbol_splice"
    except SyntaxError:
        # Fallback to standard 3-way line merge
        ok, res, conflict = line_level_3way_merge(base_code, code_a, code_b)
        return ok, res, "line_3way_fallback" if ok else "conflict"


def merge_file_content(
    rel_path: str,
    base_file: Optional[Path],
    file_a: Optional[Path],
    file_b: Optional[Path],
) -> Tuple[bool, Optional[str], str]:
    """
    Merges content of a single file between Base, Candidate A, and Candidate B.
    Returns (success, merged_content, strategy_or_error).
    """
    content_base = base_file.read_text(encoding="utf-8", errors="replace") if (base_file and base_file.is_file()) else ""
    content_a = file_a.read_text(encoding="utf-8", errors="replace") if (file_a and file_a.is_file()) else ""
    content_b = file_b.read_text(encoding="utf-8", errors="replace") if (file_b and file_b.is_file()) else ""

    if rel_path.endswith(".py"):
        ok, res, strategy = ast_merge_python_file(content_base, content_a, content_b)
        if ok:
            return True, res, strategy
        return False, None, f"Python 语义合并冲突 ({strategy})"

    ok, res, conflict = line_level_3way_merge(content_base, content_a, content_b, label_a="Cand_A", label_b="Cand_B")
    if ok and not conflict:
        return True, res, "line_3way"
    return False, None, "文本行合并冲突 (包含重叠修改)"


def get_worktree_changes_fallback(base_dir: Path, worktree_dir: Path) -> Dict[str, str]:
    """Fallback scanner comparing worktree_dir files against base_dir when git status is empty or non-git."""
    changes = {}
    if not worktree_dir.exists():
        return changes
    for root, _, files in os.walk(str(worktree_dir)):
        for f in files:
            p = Path(root) / f
            if os.path.islink(p):
                continue
            rel = p.relative_to(worktree_dir).as_posix()
            if rel.startswith(".git"):
                continue
            base_p = base_dir / rel
            if not base_p.exists():
                changes[rel] = "A"
            else:
                try:
                    if p.read_bytes() != base_p.read_bytes():
                        changes[rel] = "M"
                except Exception:
                    changes[rel] = "M"
    if base_dir.exists():
        for root, _, files in os.walk(str(base_dir)):
            for f in files:
                p = Path(root) / f
                if os.path.islink(p):
                    continue
                rel = p.relative_to(base_dir).as_posix()
                if rel.startswith(".git"):
                    continue
                wt_p = worktree_dir / rel
                if not wt_p.exists():
                    changes[rel] = "D"
    return changes


def semantic_merge_candidate_worktrees(
    base_cwd: str,
    cand_a_dir: Path,
    cand_b_dir: Path,
    output_dir: Path,
    baseline_commit: Optional[str] = None
) -> Tuple[bool, Dict[str, str], List[str], str]:
    """
    Synthesizes Candidate A and Candidate B into output_dir using 3-way semantic merge.
    Returns (success, merged_changes, conflict_files, summary_message).
    """
    from makewand.candidate import get_candidate_files_changed

    changes_a = get_candidate_files_changed(cand_a_dir, baseline_commit=baseline_commit)
    changes_b = get_candidate_files_changed(cand_b_dir, baseline_commit=baseline_commit)

    # Fallback to direct directory diff if git returned nothing (e.g. non-git directory or mock test dir)
    if not changes_a:
        changes_a = get_worktree_changes_fallback(Path(base_cwd), cand_a_dir)
    if not changes_b:
        changes_b = get_worktree_changes_fallback(Path(base_cwd), cand_b_dir)

    all_files = sorted(set(changes_a.keys()) | set(changes_b.keys()))
    if not all_files:
        return True, {}, [], "无待合并的代码改动"

    merged_changes: Dict[str, str] = {}
    conflict_files: List[str] = []

    for rel_path in all_files:
        st_a = changes_a.get(rel_path)
        st_b = changes_b.get(rel_path)

        path_base = Path(base_cwd) / rel_path
        path_a = cand_a_dir / rel_path
        path_b = cand_b_dir / rel_path
        out_target = output_dir / rel_path

        out_target.parent.mkdir(parents=True, exist_ok=True)

        # Case 1: Modified/added only in A
        if st_a and not st_b:
            if st_a == "D":
                if out_target.exists():
                    out_target.unlink()
                merged_changes[rel_path] = "D"
            else:
                shutil.copy2(path_a, out_target)
                merged_changes[rel_path] = st_a
            continue

        # Case 2: Modified/added only in B
        if st_b and not st_a:
            if st_b == "D":
                if out_target.exists():
                    out_target.unlink()
                merged_changes[rel_path] = "D"
            else:
                shutil.copy2(path_b, out_target)
                merged_changes[rel_path] = st_b
            continue

        # Case 3: Both modified / touched the same file
        if st_a == "D" and st_b == "D":
            if out_target.exists():
                out_target.unlink()
            merged_changes[rel_path] = "D"
            continue

        if (st_a == "D" and st_b != "D") or (st_b == "D" and st_a != "D"):
            conflict_files.append(rel_path)
            continue

        # Both added or modified
        if path_a.is_file() and path_b.is_file():
            # Check if byte identical
            if path_a.read_bytes() == path_b.read_bytes():
                shutil.copy2(path_a, out_target)
                merged_changes[rel_path] = st_a
                continue

            # Merge contents
            ok, merged_content, strat = merge_file_content(rel_path, path_base, path_a, path_b)
            if ok and merged_content is not None:
                out_target.write_text(merged_content, encoding="utf-8")
                merged_changes[rel_path] = "M"
            else:
                conflict_files.append(rel_path)
        else:
            conflict_files.append(rel_path)

    if conflict_files:
        return False, {}, conflict_files, f"语义合并冲突: 共有 {len(conflict_files)} 个文件存在不可调和的冲突改动 ({', '.join(conflict_files[:5])})"

    return True, merged_changes, [], f"3-way 语义合并成功，共融合 {len(merged_changes)} 个文件"
