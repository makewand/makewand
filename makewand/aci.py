"""
Makewand Agent-Computer Interface (ACI) Engine (inspired by SWE-agent).
Provides compact, token-efficient system inspection tools and folded output
truncation designed specifically to prevent context-window blowup.
"""

import os
import re
from pathlib import Path
from typing import Optional, List, Tuple

def truncate_output_folded(output: str, max_lines: int = 80, max_bytes: int = 16384) -> str:
    """
    Truncates large terminal or command outputs with a folded middle section.
    Preserves critical initial headers and tail error summaries while protecting LLM context.
    """
    if not output:
        return ""

    # Check byte size first
    if len(output.encode("utf-8", errors="replace")) > max_bytes:
        # Approximate line truncation
        lines = output.splitlines()
    else:
        lines = output.splitlines()

    if len(lines) <= max_lines and len(output.encode("utf-8", errors="replace")) <= max_bytes:
        return output

    half = max(10, max_lines // 2)
    head = lines[:half]
    tail = lines[-half:]
    folded_count = len(lines) - (len(head) + len(tail))
    total_bytes = len(output.encode("utf-8", errors="replace"))

    folded_notice = (
        f"\n... [Makewand ACI: 已折叠 {folded_count} 行冗余输出 (总大小 {total_bytes} 字节)。"
        f"核心头尾已保留，可使用 view_window 或精确 search 查看特定片段] ...\n"
    )

    return "\n".join(head) + folded_notice + "\n".join(tail)

def _is_safe_path(rel_path: str, cwd: str) -> Optional[str]:
    clean_cwd = os.path.realpath(os.path.abspath(cwd))
    p = rel_path.strip().strip("'\"`*:#")
    if not p:
        return None
    full = os.path.realpath(os.path.abspath(os.path.join(clean_cwd, p)))
    if not full.startswith(clean_cwd + os.sep) and full != clean_cwd:
        return None
    return full

def view_window(file_path: str, line_number: int = 1, window_size: int = 20, cwd: Optional[str] = None) -> str:
    """
    Views a focused window of lines around line_number with line number prefixes.
    Prevents loading entire huge files into context.
    """
    base_dir = cwd or os.getcwd()
    full_path = _is_safe_path(file_path, base_dir)
    if not full_path or not os.path.isfile(full_path):
        return f"Error: 文件不存在或越界访问: {file_path}"

    try:
        with open(full_path, "r", encoding="utf-8", errors="replace") as f:
            lines = f.readlines()
    except Exception as e:
        return f"Error: 无法读取文件 {file_path}: {e}"

    total_lines = len(lines)
    if total_lines == 0:
        return f"=== [文件为空: {file_path}] ==="

    target = max(1, min(line_number, total_lines))
    start_line = max(1, target - window_size)
    end_line = min(total_lines, target + window_size)

    output_lines = [
        f"=== [File: {file_path} | Total: {total_lines} 行 | 窗口: L{start_line}-L{end_line}] ==="
    ]

    for idx in range(start_line, end_line + 1):
        line_content = lines[idx - 1].rstrip("\r\n")
        prefix = "▶ " if idx == target else "  "
        output_lines.append(f"{prefix}L{idx:4d}: {line_content}")

    return "\n".join(output_lines)

def search_code(term: str, target_dir: Optional[str] = None, max_results: int = 20, cwd: Optional[str] = None) -> str:
    """
    Performs compact code search across repository files, returning matching line numbers
    and snippets without excessive verbose output.
    """
    base_dir = os.path.realpath(os.path.abspath(cwd or os.getcwd()))
    search_dir = os.path.join(base_dir, target_dir) if target_dir else base_dir
    search_dir = os.path.realpath(search_dir)

    if not search_dir.startswith(base_dir):
        return "Error: 搜索目录非法"

    try:
        pattern = re.compile(term, re.IGNORECASE)
    except re.error:
        pattern = re.compile(re.escape(term), re.IGNORECASE)

    results: List[Tuple[str, int, str]] = []
    ignored_dirs = {".git", ".hg", "node_modules", "vendor", "__pycache__", ".venv", "venv", "dist", "build"}

    for root, dirs, files in os.walk(search_dir):
        dirs[:] = [d for d in dirs if d not in ignored_dirs and not d.startswith(".")]
        for file in files:
            if file.startswith(".") or file.endswith((".pyc", ".so", ".exe", ".bin", ".tar", ".gz", ".zip")):
                continue
            full_file = os.path.join(root, file)
            rel_file = os.path.relpath(full_file, base_dir)
            try:
                with open(full_file, "r", encoding="utf-8", errors="ignore") as f:
                    for line_idx, line in enumerate(f, 1):
                        if pattern.search(line):
                            results.append((rel_file, line_idx, line.strip()))
                            if len(results) >= max_results:
                                break
            except Exception:
                pass
            if len(results) >= max_results:
                break
        if len(results) >= max_results:
            break

    if not results:
        return f"未找到匹配项: '{term}'"

    lines = [f"=== [Makewand ACI 代码搜索: '{term}' (找到 {len(results)} 条匹配)] ==="]
    for rel_file, line_no, content in results:
        # truncate single line content if too long
        c_short = content[:120] + ("..." if len(content) > 120 else "")
        lines.append(f"{rel_file}:L{line_no}: {c_short}")

    return "\n".join(lines)
