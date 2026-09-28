"""
Makewand Repo-Map Engine (inspired by Aider's Tree-Sitter Repo-Map)

Generates a compact, budgeted structural symbol map (classes, functions, interfaces,
structs and method signatures) across the repository. This provides AI coders with
global architecture context without overloading the context window.
"""

import ast
import os
import re
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

# Max file size to parse (skip massive generated files)
MAX_FILE_SIZE_BYTES = 256 * 1024


def _extract_python_symbols(content: str) -> List[str]:
    """Extracts top-level and class-level functions/classes using standard ast."""
    symbols = []
    try:
        tree = ast.parse(content)
    except Exception:
        return []

    for node in tree.body:
        if isinstance(node, ast.ClassDef):
            symbols.append(f"  class {node.name}:")
            for item in node.body:
                if isinstance(item, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    args = [a.arg for a in item.args.args if a.arg != "self" and a.arg != "cls"]
                    arg_str = ", ".join(args[:4]) + ("..." if len(args) > 4 else "")
                    prefix = "async def" if isinstance(item, ast.AsyncFunctionDef) else "def"
                    symbols.append(f"    {prefix} {item.name}({arg_str})")
        elif isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            args = [a.arg for a in node.args.args]
            arg_str = ", ".join(args[:4]) + ("..." if len(args) > 4 else "")
            prefix = "async def" if isinstance(node, ast.AsyncFunctionDef) else "def"
            symbols.append(f"  {prefix} {node.name}({arg_str})")
    return symbols


def _extract_go_symbols(content: str) -> List[str]:
    """Extracts Go types, interfaces, structs, and functions."""
    symbols = []
    # Match type Foo struct / interface
    type_pattern = re.compile(r"^\s*type\s+([A-Za-z0-9_]+)\s+(struct|interface)", re.MULTILINE)
    for m in type_pattern.finditer(content):
        symbols.append(f"  type {m.group(1)} {m.group(2)}")

    # Match func (r *Receiver) Method(args) ... or func Foo(args) ...
    func_pattern = re.compile(r"^\s*func\s+(?:\(([^)]+)\)\s+)?([A-Za-z0-9_]+)\s*\(([^)]*)\)", re.MULTILINE)
    for m in func_pattern.finditer(content):
        recv, name, args = m.groups()
        if name.startswith("Test") or name.startswith("Benchmark") or name.startswith("Example"):
            continue
        clean_args = re.sub(r"\s+", " ", args.strip())
        if len(clean_args) > 30:
            clean_args = clean_args[:27] + "..."
        if recv:
            clean_recv = re.sub(r"\s+", " ", recv.strip())
            symbols.append(f"  func ({clean_recv}) {name}({clean_args})")
        else:
            symbols.append(f"  func {name}({clean_args})")
    return symbols


def _extract_ts_js_symbols(content: str) -> List[str]:
    """Extracts TypeScript / JavaScript classes, interfaces, and exported functions."""
    symbols = []
    # Classes & Interfaces & Types
    class_pattern = re.compile(r"^\s*(?:export\s+)?(?:default\s+)?(class|interface|type)\s+([A-Za-z0-9_]+)", re.MULTILINE)
    for m in class_pattern.finditer(content):
        symbols.append(f"  {m.group(1)} {m.group(2)}")

    # Functions
    func_pattern = re.compile(r"^\s*(?:export\s+)?(?:async\s+)?function\s+([A-Za-z0-9_]+)\s*\(([^)]*)\)", re.MULTILINE)
    for m in func_pattern.finditer(content):
        name, args = m.groups()
        clean_args = re.sub(r"\s+", " ", args.strip())
        if len(clean_args) > 30:
            clean_args = clean_args[:27] + "..."
        symbols.append(f"  function {name}({clean_args})")

    # Arrow functions / const functions: export const foo = (...) =>
    arrow_pattern = re.compile(r"^\s*(?:export\s+)?const\s+([A-Za-z0-9_]+)\s*=\s*(?:async\s*)?\(([^)]*)\)\s*=>", re.MULTILINE)
    for m in arrow_pattern.finditer(content):
        name, args = m.groups()
        clean_args = re.sub(r"\s+", " ", args.strip())
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
        r"^\s*(?:pub(?:\([^)]+\))?\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern(?:\s+\"[^\"]+\")?\s+)?fn\s+([A-Za-z0-9_]+)\s*(?:<[^>]*>)?\s*\(([^)]*)\)",
        re.MULTILINE,
    )
    for m in fn_pattern.finditer(content):
        name, args = m.groups()
        if name.startswith("test_") or name.startswith("bench_"):
            continue
        clean_args = re.sub(r"\s+", " ", args.strip())
        if len(clean_args) > 30:
            clean_args = clean_args[:27] + "..."
        symbols.append(f"  fn {name}({clean_args})")

    # Impl blocks
    impl_pattern = re.compile(
        r"^\s*impl(?:\s*<[^>]*>)?\s+([A-Za-z0-9_]+(?:\s+for\s+[A-Za-z0-9_]+)?)",
        re.MULTILINE,
    )
    for m in impl_pattern.finditer(content):
        target = m.group(1).strip()
        symbols.append(f"  impl {target}")

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
    return []


def _dir_priority(d: str) -> int:
    dl = d.lower()
    if dl in (
        "makewand", "internal", "cmd", "serverdb", "serverauth",
        "serveradmin", "serverhttp", "serverui", "servermetrics",
        "router", "src", "pkg", "lib", "core", "app"
    ):
        return 0
    if any(k in dl for k in ("bench", "fixture", "test", "tests", "script", "scripts", "doc", "docs", "site")):
        return 2
    return 1


def _file_priority(fname: str) -> int:
    fn_lower = fname.lower()
    if "test" in fn_lower or "bench" in fn_lower or "fixture" in fn_lower or "mock" in fn_lower:
        return 1
    return 0


def generate_repo_map(cwd: str, max_lines: int = 80, max_files: int = 40) -> str:
    """
    Generates a concise repository symbol map for the given directory.
    Output is bounded to max_lines to fit within prompt token budgets.
    """
    root = Path(cwd).resolve()
    if not root.is_dir():
        return ""

    file_symbols: Dict[str, List[str]] = {}
    total_found_files = 0

    for dirpath, dirnames, filenames in os.walk(root):
        # Exclude ignored directories in-place
        dirnames[:] = [d for d in dirnames if d not in DEFAULT_IGNORE_DIRS and not d.startswith(".")]
        # Prioritize core business/source code over test suites and benchmarks
        dirnames.sort(key=lambda d: (_dir_priority(d), d))

        rel_dir = Path(dirpath).relative_to(root)
        if any(part in DEFAULT_IGNORE_DIRS for part in rel_dir.parts):
            continue

        # Prioritize non-test implementation files
        sorted_files = sorted(filenames, key=lambda f: (_file_priority(f), f))

        for fname in sorted_files:
            if fname.startswith("."):
                continue
            ext = os.path.splitext(fname)[1].lower()
            if ext not in (".py", ".go", ".ts", ".tsx", ".js", ".jsx", ".rs"):
                continue

            full_p = Path(dirpath) / fname
            rel_p = str(full_p.relative_to(root))

            syms = extract_file_symbols(full_p)
            if syms:
                file_symbols[rel_p] = syms
                total_found_files += 1
                if total_found_files >= max_files:
                    break
        if total_found_files >= max_files:
            break

    if not file_symbols:
        return ""

    output_lines: List[str] = []
    # Sort files according to priority: core dirs first, non-test first
    def _file_sort_key(rel_p: str) -> Tuple[int, int, str]:
        parts = Path(rel_p).parts
        d_prio = _dir_priority(parts[0]) if len(parts) > 1 else 0
        f_prio = _file_priority(Path(rel_p).name)
        return (d_prio, f_prio, rel_p)

    for rel_path, syms in sorted(file_symbols.items(), key=lambda item: _file_sort_key(item[0])):
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
