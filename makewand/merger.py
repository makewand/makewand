"""
Makewand Multi-Model 3-Way AST & Patch Semantic Merger.

Enables combining the complementary strengths of concurrent race candidates
(e.g. Candidate A fixed function foo(), Candidate B improved function bar())
into a verified hybrid candidate (Candidate M), tested and applied atomically.
"""

import os
import ast
import difflib
import tempfile
import subprocess
from pathlib import Path
from typing import Dict, List, Optional, Tuple


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

    # The Python-aware caller may retry with conservative whole-text edits.
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
            start_line = min([getattr(node, "lineno", 1)] +
                             [decorator.lineno for decorator in node.decorator_list])
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


def _merge_disjoint_text(base_code: str, code_a: str, code_b: str):
    """Merge whole-text edits; AST is used only to recognize import additions.

    Rebuilding a module from selected symbols loses constants, deletions,
    decorators and other top-level statements. Every byte participates here.
    """
    base = base_code.splitlines(keepends=True)
    edits = []
    for side, source in enumerate((code_a, code_b)):
        lines = source.splitlines(keepends=True)
        for tag, start, end, other_start, other_end in difflib.SequenceMatcher(
                None, base, lines, autojunk=False).get_opcodes():
            if tag != "equal":
                edits.append((start, end, lines[other_start:other_end], side))
    merged_edits = []
    for start, end, replacement, side in sorted(edits, key=lambda item: (item[0], item[1], item[3])):
        if merged_edits:
            prior_start, prior_end, prior_replacement, prior_side = merged_edits[-1]
            if start == prior_start and end == prior_end and replacement == prior_replacement:
                continue
            if start == end == prior_start == prior_end:
                def imports_only(lines):
                    try:
                        nodes = ast.parse("".join(lines)).body
                        return bool(nodes) and all(isinstance(node, (ast.Import, ast.ImportFrom)) for node in nodes)
                    except SyntaxError:
                        return False
                if imports_only(prior_replacement) and imports_only(replacement):
                    merged_edits[-1] = (start, end, prior_replacement + replacement, prior_side)
                    continue
                return False, ""
            # Insertion at a replacement boundary is ambiguous. Adjacent
            # nonempty edits are safe and keep their complete source text.
            overlap = start < prior_end or (start == prior_start and (start == end or prior_start == prior_end))
            if overlap:
                return False, ""
        merged_edits.append((start, end, replacement, side))
    result = list(base)
    for start, end, replacement, _ in reversed(merged_edits):
        result[start:end] = replacement
    return True, "".join(result)


def ast_merge_python_file(base_code: str, code_a: str, code_b: str) -> Tuple[bool, str, str]:
    """Use a complete text three-way merge, with conservative AST assistance."""
    if code_a == code_b:
        ok, merged, strategy = True, code_a, "identical"
    elif code_a == base_code:
        ok, merged, strategy = True, code_b, "take_b"
    elif code_b == base_code:
        ok, merged, strategy = True, code_a, "take_a"
    else:
        # An identical newly added definition belongs in the result once. Git
        # can duplicate it when one side also changes the preceding last line.
        # Remove only that exact shared AST span from A; all remaining text,
        # including constants, comments and module statements, still participates.
        symbols_base = extract_python_symbols(base_code)
        symbols_a = extract_python_symbols(code_a)
        symbols_b = extract_python_symbols(code_b)
        shared = [symbols_a[name] for name in symbols_a.keys() & symbols_b.keys()
                  if name not in symbols_base and symbols_a[name][2] == symbols_b[name][2]]
        merge_a = code_a
        if shared:
            lines_a = code_a.splitlines(keepends=True)
            for start, end, _, _ in sorted(shared, reverse=True, key=lambda symbol: symbol[0]):
                del lines_a[start - 1:end]
            merge_a = "".join(lines_a)
        ok, merged, _ = line_level_3way_merge(base_code, merge_a, code_b)
        if not ok:
            ok, merged = _merge_disjoint_text(base_code, merge_a, code_b)
        # Keep the existing public strategy label for callers; the implementation
        # now preserves full text rather than splicing a subset of AST symbols.
        strategy = "ast_symbol_splice" if ok else "conflict"
    if ok:
        try:
            ast.parse(merged)
        except SyntaxError:
            return False, merged, "conflict"
    return ok, merged, strategy


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
    try:
        content_base = base_file.read_text(encoding="utf-8") if (base_file and base_file.is_file()) else ""
        content_a = file_a.read_text(encoding="utf-8") if (file_a and file_a.is_file()) else ""
        content_b = file_b.read_text(encoding="utf-8") if (file_b and file_b.is_file()) else ""
    except (UnicodeError, OSError) as exc:
        return False, None, f"文本合并输入不可读: {exc}"

    if rel_path.endswith(".py"):
        ok, res, strategy = ast_merge_python_file(content_base, content_a, content_b)
        if ok:
            return True, res, strategy
        return False, None, f"Python 语义合并冲突 ({strategy})"

    ok, res, conflict = line_level_3way_merge(content_base, content_a, content_b, label_a="Cand_A", label_b="Cand_B")
    if ok and not conflict:
        return True, res, "line_3way"
    return False, None, "文本行合并冲突 (包含重叠修改)"


def _changes_between(baseline, candidate):
    return {path: ("D" if path not in candidate else "A" if path not in baseline else "M")
            for path in sorted(baseline.keys() | candidate.keys())
            if baseline.get(path) != candidate.get(path)}


def get_worktree_changes_fallback(base_dir: Path, worktree_dir: Path) -> Dict[str, str]:
    """Compare complete regular-file records, including permission changes."""
    from makewand.candidate import build_manifest
    return _changes_between(build_manifest(base_dir), build_manifest(worktree_dir))


def _merge_path(root: Path, relative: str) -> Path:
    from makewand.candidate import _verify_safe_target_path
    parts = Path(relative).parts
    if not parts or Path(relative).is_absolute() or any(part in (".", "..", ".git") for part in parts):
        raise ValueError("invalid merge path")
    return _verify_safe_target_path(root, relative)


def _sealed_bytes(root: Path, relative: str, expected) -> bytes:
    import hashlib
    import stat
    target = _merge_path(root, relative)
    with os.fdopen(os.open(target, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)), "rb") as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode):
            raise ValueError("merge input must be a regular file")
        content = handle.read()
        if expected != {"sha256": hashlib.sha256(content).hexdigest(), "mode": stat.S_IMODE(info.st_mode)}:
            raise ValueError("merge input changed after sealing")
        return content


def semantic_merge_candidate_worktrees(
    base_cwd: str,
    cand_a_dir: Path,
    cand_b_dir: Path,
    output_dir: Path,
    baseline_commit: Optional[str] = None,
    changes_a=None,
    changes_b=None,
    manifest_a=None,
    manifest_b=None,
    baseline_manifest=None,
) -> Tuple[bool, Dict[str, str], List[str], str]:
    """Merge frozen whole-file inputs. Git metadata cannot choose deliverables."""
    from makewand.candidate import build_manifest, _atomic_copy
    base_dir = Path(base_cwd)
    try:
        baseline = baseline_manifest if baseline_manifest is not None else build_manifest(base_dir)
        sealed_a = manifest_a if manifest_a is not None else build_manifest(cand_a_dir)
        sealed_b = manifest_b if manifest_b is not None else build_manifest(cand_b_dir)
        actual_a, actual_b = _changes_between(baseline, sealed_a), _changes_between(baseline, sealed_b)
        if changes_a is not None and changes_a != actual_a or changes_b is not None and changes_b != actual_b:
            return False, {}, [], "候选变更计划与冻结基线不一致"
        changes_a, changes_b = actual_a, actual_b
        merged_changes, conflicts = {}, []
        for relative in sorted(changes_a.keys() | changes_b.keys()):
            st_a, st_b = changes_a.get(relative), changes_b.get(relative)
            target = _merge_path(output_dir, relative)
            _merge_path(base_dir, relative)
            _merge_path(cand_a_dir, relative)
            _merge_path(cand_b_dir, relative)
            if st_a == "D" and st_b == "D" or st_a == "D" and not st_b or st_b == "D" and not st_a:
                if target.exists():
                    target.unlink()
                merged_changes[relative] = "D"
                continue
            if st_a == "D" or st_b == "D":
                conflicts.append(relative)
                continue
            if not st_b:
                _atomic_copy(str(output_dir), relative, cand_a_dir / relative, sealed_a[relative])
                merged_changes[relative] = st_a
                continue
            if not st_a:
                _atomic_copy(str(output_dir), relative, cand_b_dir / relative, sealed_b[relative])
                merged_changes[relative] = st_b
                continue
            content_a = _sealed_bytes(cand_a_dir, relative, sealed_a[relative])
            content_b = _sealed_bytes(cand_b_dir, relative, sealed_b[relative])
            content_base = _sealed_bytes(base_dir, relative, baseline[relative]) if relative in baseline else b""
            mode_base = baseline.get(relative, {}).get("mode")
            mode_a, mode_b = sealed_a[relative]["mode"], sealed_b[relative]["mode"]
            if mode_a == mode_b:
                mode = mode_a
            elif mode_a == mode_base:
                mode = mode_b
            elif mode_b == mode_base:
                mode = mode_a
            else:
                conflicts.append(relative)
                continue
            if mode & 0o7000:
                conflicts.append(relative)
                continue
            if content_a == content_b:
                merged = content_a
            else:
                try:
                    text_base, text_a, text_b = [content.decode("utf-8") for content in (content_base, content_a, content_b)]
                    if relative.endswith(".py"):
                        ok, text, _ = ast_merge_python_file(text_base, text_a, text_b)
                    else:
                        ok, text, _ = line_level_3way_merge(text_base, text_a, text_b)
                except UnicodeError:
                    ok = False
                if not ok:
                    conflicts.append(relative)
                    continue
                merged = text.encode("utf-8")
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(merged)
            target.chmod(mode)
            merged_changes[relative] = "M" if relative in baseline else "A"
        if conflicts:
            return False, {}, conflicts, "语义合并冲突: " + ", ".join(conflicts[:5])
        return True, merged_changes, [], f"3-way 语义合并成功，共融合 {len(merged_changes)} 个文件"
    except (OSError, ValueError, KeyError) as exc:
        return False, {}, [], f"合并输入完整性校验失败: {exc}"
