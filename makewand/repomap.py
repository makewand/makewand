"""
Makewand Repo-Map Engine (inspired by Aider's Tree-Sitter Repo-Map)

Generates a compact, budgeted structural symbol map (classes, functions, interfaces,
structs and method signatures) across the repository. This provides AI coders with
global architecture context without overloading the context window.
"""

import ast
import os
import re
import subprocess
from pathlib import Path
from typing import Dict, List, Optional, Set, Tuple

# Directories to skip when building the repository symbol map
DEFAULT_IGNORE_DIRS: Set[str] = {
    ".git",
    "__pycache__",
    "node_modules",
    "vendor",
    "target",
    "dist",
    "build",
    ".pytest_cache",
    ".mypy_cache",
    ".venv",
    "venv",
    "env",
    ".coverage",
    ".tox",
    ".idea",
    ".vscode",
    "site-packages",
}

# Supported file extensions for symbol extraction
SUPPORTED_EXTENSIONS: Set[str] = {
    ".py", ".go", ".ts", ".tsx", ".js", ".jsx", ".rs",
    ".c", ".cpp", ".cc", ".cxx", ".h", ".hpp",
}

# Max file size to parse (skip massive generated files)
MAX_FILE_SIZE_BYTES = 256 * 1024


def _extract_balanced_parens(s: str, start_pos: int) -> Tuple[str, int]:
    """Extracts content inside balanced parentheses starting at start_pos."""
    depth = 1
    i = start_pos
    s_len = len(s)
    while i < s_len and depth > 0:
        ch = s[i]
        if ch == '(':
            depth += 1
        elif ch == ')':
            depth -= 1
        i += 1
    if depth == 0:
        return s[start_pos:i - 1], i
    return "", -1


def _extract_python_symbols(content: str) -> List[str]:
    """Extracts top-level and class-level functions/classes using standard ast."""
    symbols = []
    try:
        tree = ast.parse(content)
    except Exception:
        return []

    def _node_priority(node: ast.AST) -> int:
        if isinstance(node, ast.ClassDef):
            return 0 if not node.name.startswith("_") else 2
        elif isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            return 1 if not node.name.startswith("_") else 3
        return 4

    for node in sorted(tree.body, key=_node_priority):
        if isinstance(node, ast.ClassDef):
            symbols.append(f"  class {node.name}:")
            def _method_priority(m: ast.AST) -> int:
                if isinstance(m, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    if m.name == "__init__":
                        return 0
                    if not m.name.startswith("_"):
                        return 1
                    return 2
                return 3

            for item in sorted(node.body, key=_method_priority):
                if isinstance(item, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    args = []
                    if hasattr(item.args, "posonlyargs"):
                        for a in item.args.posonlyargs:
                            if a.arg not in ("self", "cls"):
                                args.append(a.arg)
                    for a in item.args.args:
                        if a.arg not in ("self", "cls"):
                            args.append(a.arg)
                    for a in item.args.kwonlyargs:
                        args.append(a.arg)
                    arg_str = ", ".join(args[:4]) + ("..." if len(args) > 4 else "")
                    prefix = "async def" if isinstance(item, ast.AsyncFunctionDef) else "def"
                    symbols.append(f"    {prefix} {item.name}({arg_str})")
        elif isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            args = []
            if hasattr(node.args, "posonlyargs"):
                for a in node.args.posonlyargs:
                    args.append(a.arg)
            for a in node.args.args:
                args.append(a.arg)
            for a in node.args.kwonlyargs:
                args.append(a.arg)
            arg_str = ", ".join(args[:4]) + ("..." if len(args) > 4 else "")
            prefix = "async def" if isinstance(node, ast.AsyncFunctionDef) else "def"
            symbols.append(f"  {prefix} {node.name}({arg_str})")
    return symbols


def _extract_go_symbols(content: str) -> List[str]:
    """Extracts Go types, interfaces, structs, and functions."""
    symbols = []

    # Match grouped type (...) blocks
    type_block_pattern = re.compile(r"^\s*type\s*\(([^)]+)\)", re.MULTILINE)
    for tb in type_block_pattern.finditer(content):
        block_text = tb.group(1)
        for line in block_text.splitlines():
            m = re.match(r"^\s*([A-Za-z0-9_]+)\s*(=?\s*[A-Za-z0-9_*\[\]]+(?:\.[A-Za-z0-9_]+)?)", line)
            if m:
                symbols.append(f"  type {m.group(1)} {m.group(2)}")

    # Match single type Foo struct / interface / alias / etc
    type_pattern = re.compile(r"^\s*type\s+([A-Za-z0-9_]+)\s+([A-Za-z0-9_*\[\]]+(?:\.[A-Za-z0-9_]+)?)", re.MULTILINE)
    for m in type_pattern.finditer(content):
        if m.group(1) == "(":
            continue
        symbols.append(f"  type {m.group(1)} {m.group(2)}")

    # Match func (r *Receiver) Method(args) ... or func Foo[T any](args) ...
    func_pattern = re.compile(r"^\s*func\s+(?:\(([^)]+)\)\s+)?([A-Za-z0-9_]+)\s*(?:\[[^\]]*\])?\s*\(", re.MULTILINE)
    for m in func_pattern.finditer(content):
        recv, name = m.groups()
        if name.startswith("Test") or name.startswith("Benchmark") or name.startswith("Example"):
            continue
        raw_args, _ = _extract_balanced_parens(content, m.end())
        clean_args = re.sub(r"\s+", " ", raw_args.strip())
        if len(clean_args) > 30:
            clean_args = clean_args[:27] + "..."
        if recv:
            clean_recv = re.sub(r"\s+", " ", recv.strip())
            symbols.append(f"  func ({clean_recv}) {name}({clean_args})")
        else:
            symbols.append(f"  func {name}({clean_args})")

    return symbols


def _extract_ts_js_symbols(content: str) -> List[str]:
    """Extracts TypeScript / JavaScript classes, interfaces, types, enums, and functions."""
    symbols = []
    # Classes & Interfaces & Types & Enums
    class_pattern = re.compile(r"^\s*(?:export\s+)?(?:default\s+)?(class|interface|type|enum)\s+([A-Za-z0-9_]+)", re.MULTILINE)
    for m in class_pattern.finditer(content):
        symbols.append(f"  {m.group(1)} {m.group(2)}")

    # Functions: export function foo(args) or async function foo(args)
    func_pattern = re.compile(r"^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s+([A-Za-z0-9_]+)\s*(?:<[^>]*>)?\s*\(", re.MULTILINE)
    for m in func_pattern.finditer(content):
        name = m.group(1)
        raw_args, _ = _extract_balanced_parens(content, m.end())
        clean_args = re.sub(r"\s+", " ", raw_args.strip())
        if len(clean_args) > 30:
            clean_args = clean_args[:27] + "..."
        symbols.append(f"  function {name}({clean_args})")

    # Arrow functions / const functions: export const foo = (...) => or const foo = async (...) =>
    arrow_pattern = re.compile(r"^\s*(?:export\s+)?const\s+([A-Za-z0-9_]+)\s*(?::\s*[^=]+)?=\s*(?:async\s*)?(?:<[^>]*>)?\s*\(", re.MULTILINE)
    for m in arrow_pattern.finditer(content):
        name = m.group(1)
        raw_args, end_pos = _extract_balanced_parens(content, m.end())
        rest = content[end_pos:end_pos + 40]
        if "=>" in rest:
            clean_args = re.sub(r"\s+", " ", raw_args.strip())
            if len(clean_args) > 30:
                clean_args = clean_args[:27] + "..."
            symbols.append(f"  const {name}({clean_args})")

    return symbols


def _extract_rust_symbols(content: str) -> List[str]:
    """Extracts Rust structs, enums, traits, functions, and impl blocks."""
    symbols = []
    # Structs, Enums, Traits, Type aliases
    type_pattern = re.compile(
        r"^\s*(?:pub(?:\([^)]+\))?\s+)?(struct|enum|trait|type)\s+([A-Za-z0-9_]+)",
        re.MULTILINE,
    )
    for m in type_pattern.finditer(content):
        symbols.append(f"  {m.group(1)} {m.group(2)}")

    # Functions
    fn_pattern = re.compile(
        r"^\s*(?:pub(?:\([^)]+\))?\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern(?:\s+\"[^\"]+\")?\s+)?fn\s+([A-Za-z0-9_]+)\s*(?:<[^>]*>)?\s*\(",
        re.MULTILINE,
    )
    for m in fn_pattern.finditer(content):
        name = m.group(1)
        if name.startswith("test_") or name.startswith("bench_"):
            continue
        raw_args, _ = _extract_balanced_parens(content, m.end())
        clean_args = re.sub(r"\s+", " ", raw_args.strip())
        if len(clean_args) > 30:
            clean_args = clean_args[:27] + "..."
        symbols.append(f"  fn {name}({clean_args})")

    # Impl blocks with trait bounds, lifetimes, and path qualifiers
    impl_pattern = re.compile(
        r"^\s*impl(?:\s*<[^>]*>)?\s+([A-Za-z0-9_:<>\s&'*+(),]+?)(?:\s+where\b|\s*\{)",
        re.MULTILINE,
    )
    for m in impl_pattern.finditer(content):
        target = re.sub(r"\s+", " ", m.group(1).strip())
        if target:
            symbols.append(f"  impl {target}")

    return symbols


def _extract_c_cpp_symbols(content: str) -> List[str]:
    """Extracts C and C++ classes, structs, enums, and top-level functions."""
    symbols = []
    # Classes, Structs, Enums
    type_pattern = re.compile(r"^\s*(?:typedef\s+)?(class|struct|enum(?:\s+class)?)\s+([A-Za-z0-9_]+)", re.MULTILINE)
    for m in type_pattern.finditer(content):
        kind = m.group(1)
        name = m.group(2)
        symbols.append(f"  {kind} {name}")

    # Functions
    func_pattern = re.compile(r"^\s*(?:[A-Za-z0-9_:*&<>]+\s+)+([A-Za-z0-9_]+)\s*\(", re.MULTILINE)
    for m in func_pattern.finditer(content):
        name = m.group(1)
        if name in ("if", "while", "for", "switch", "catch", "return", "sizeof"):
            continue
        raw_args, _ = _extract_balanced_parens(content, m.end())
        clean_args = re.sub(r"\s+", " ", raw_args.strip())
        if len(clean_args) > 30:
            clean_args = clean_args[:27] + "..."
        symbols.append(f"  func {name}({clean_args})")

    return symbols


def extract_file_symbols(file_path: Path) -> List[str]:
    """Extracts symbols according to the file extension."""
    suffix = file_path.suffix.lower()
    try:
        if file_path.stat().st_size > MAX_FILE_SIZE_BYTES:
            return []
        content = file_path.read_text(encoding="utf-8", errors="ignore")
    except Exception:
        return []

    if suffix == ".py":
        return _extract_python_symbols(content)
    elif suffix == ".go":
        return _extract_go_symbols(content)
    elif suffix in (".ts", ".tsx", ".js", ".jsx"):
        return _extract_ts_js_symbols(content)
    elif suffix == ".rs":
        return _extract_rust_symbols(content)
    elif suffix in (".c", ".cpp", ".cc", ".cxx", ".h", ".hpp"):
        return _extract_c_cpp_symbols(content)
    return []


def _collect_candidate_files(root: Path) -> List[str]:
    """
    Collects code file paths relative to root, prioritizing git-tracked files
    if available, otherwise falling back to filesystem walk.
    """
    # 1. Fast path: git ls-files if inside a git repo
    try:
        res = subprocess.run(
            ["git", "ls-files", "-z"],
            cwd=str(root),
            capture_output=True,
            text=False,
            timeout=5,
        )
        if res.returncode == 0 and res.stdout:
            raw_entries = res.stdout.split(b"\x00")
            git_files = []
            for entry in raw_entries:
                if not entry:
                    continue
                try:
                    rel_p = entry.decode("utf-8")
                except UnicodeDecodeError:
                    continue
                if Path(rel_p).suffix.lower() in SUPPORTED_EXTENSIONS:
                    git_files.append(rel_p)
            if git_files:
                return git_files
    except Exception:
        pass

    # 2. Fallback: os.walk
    collected = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in DEFAULT_IGNORE_DIRS and not d.startswith(".")]
        rel_dir = Path(dirpath).relative_to(root)
        if any(part in DEFAULT_IGNORE_DIRS for part in rel_dir.parts):
            continue
        for fname in filenames:
            if fname.startswith("."):
                continue
            ext = os.path.splitext(fname)[1].lower()
            if ext in SUPPORTED_EXTENSIONS:
                rel_p = str((Path(dirpath) / fname).relative_to(root))
                collected.append(rel_p)
    return collected


def _score_candidate_file(rel_p: str, root_name: str) -> Tuple[int, int, int, str]:
    """
    Hierarchical file prioritization score (lower = higher priority):
    - Tier 0: Direct root files & core project packages (makewand, src, internal, pkg, core, router, app, lib)
    - Tier 1: CLI entrypoint matching project name (cmd/makewand/main.go)
    - Tier 2: General implementation files
    - Tier 3: Test runner binaries, helper scripts (cmd/buildtest, cmd/casefix, scripts)
    - Tier 4: Test files & benchmark suites
    """
    parts = Path(rel_p).parts
    if not parts:
        return (99, 1, 99, rel_p)

    fname = parts[-1].lower()

    # Test / bench file check
    is_test_file = (
        fname.startswith("test_")
        or fname.endswith("_test.py")
        or fname.endswith("_test.go")
        or fname.endswith(".test.ts")
        or fname.endswith(".spec.ts")
        or fname.endswith(".test.js")
        or fname.endswith(".spec.js")
        or "bench" in fname
        or "mock" in fname
        or "fixture" in fname
    )

    # Test / bench directory check
    is_test_dir = any(
        any(k in part.lower() for k in ("test", "bench", "fixture", "mock", "doc", "script", "site", "example"))
        for part in parts[:-1]
    )

    first = parts[0].lower()

    if is_test_file or is_test_dir:
        tier = 4
    elif len(parts) == 1:
        tier = 0
    elif first == root_name or first in ("src", "internal", "core", "app", "pkg", "router", "lib"):
        tier = 0
    elif first == "cmd":
        if len(parts) > 1 and parts[1].lower() == root_name:
            tier = 1
        elif any(k in parts[1].lower() for k in ("test", "fix", "versus")):
            tier = 3
        else:
            tier = 2
    else:
        tier = 2

    return (tier, 1 if is_test_file else 0, len(parts), rel_p)


def generate_repo_map(cwd: str, max_lines: int = 80, max_files: int = 40) -> str:
    """
    Generates a concise repository symbol map for the given directory.
    Output is bounded to max_lines to fit within prompt token budgets.
    """
    root = Path(cwd).resolve()
    if not root.is_dir():
        return ""

    root_name = root.name.lower()
    candidate_files = _collect_candidate_files(root)
    if not candidate_files:
        return ""

    # Sort candidates by architectural priority
    sorted_files = sorted(candidate_files, key=lambda f: _score_candidate_file(f, root_name))

    file_symbols: Dict[str, List[str]] = {}
    total_processed_files = 0

    for rel_p in sorted_files:
        full_p = root / rel_p
        syms = extract_file_symbols(full_p)
        if syms:
            file_symbols[rel_p] = syms
            total_processed_files += 1
            if total_processed_files >= max_files:
                break

    if not file_symbols:
        return ""

    output_lines: List[str] = []
    for rel_path in sorted(file_symbols.keys(), key=lambda f: _score_candidate_file(f, root_name)):
        syms = file_symbols[rel_path]
        output_lines.append(f"{rel_path}:")
        for sym in syms[:8]:  # Limit top 8 symbols per file
            output_lines.append(sym)
            if len(output_lines) >= max_lines:
                output_lines.append("  ... (more symbols truncated)")
                return "\n".join(output_lines)

    return "\n".join(output_lines)


def format_repo_map_for_prompt(cwd: str, max_lines: int = 80) -> str:
    """
    Formats the repository symbol map as a prompt injection block.
    Returns empty string if no symbols are found.
    """
    repo_map = generate_repo_map(cwd, max_lines=max_lines)
    if not repo_map:
        return ""
    return f"\n【代码库全局架构拓扑感知 (Repo-Map)】\n{repo_map}\n"
