"""
Makewand Auto-Linter & Fast Syntax Gate (inspired by Aider).
Provides millisecond-level automated formatting and syntax pre-validation
before launching expensive end-to-end test suites.
"""

import os
import shutil
import subprocess
import sys
import json
from pathlib import Path
from typing import List, Sequence, Dict, Tuple, Optional

def auto_format_files(cwd: str, file_paths: Sequence[str]) -> Dict[str, bool]:
    """
    Applies language-idiomatic auto-formatting to modified files in-place.
    Gracefully skips if relevant formatters (ruff, black, gofmt, etc.) are unavailable.
    Returns mapping of {rel_path: was_formatted}.
    """
    clean_cwd = os.path.realpath(os.path.abspath(cwd))
    results: Dict[str, bool] = {}

    for rel in file_paths:
        full_path = os.path.realpath(os.path.abspath(os.path.join(clean_cwd, rel)))
        if not full_path.startswith(clean_cwd + os.sep) and full_path != clean_cwd:
            continue
        if not os.path.isfile(full_path):
            continue

        ext = Path(full_path).suffix.lower()
        success = False

        if ext == ".py":
            # 1. Try ruff format
            if shutil.which("ruff"):
                try:
                    p = subprocess.run(
                        ["ruff", "format", full_path],
                        cwd=clean_cwd,
                        capture_output=True,
                        timeout=5
                    )
                    success = (p.returncode == 0)
                except Exception:
                    pass
            # 2. Fallback to black
            if not success and shutil.which("black"):
                try:
                    p = subprocess.run(
                        ["black", "-q", full_path],
                        cwd=clean_cwd,
                        capture_output=True,
                        timeout=5
                    )
                    success = (p.returncode == 0)
                except Exception:
                    pass

        elif ext == ".go":
            if shutil.which("gofmt"):
                try:
                    p = subprocess.run(
                        ["gofmt", "-w", full_path],
                        cwd=clean_cwd,
                        capture_output=True,
                        timeout=5
                    )
                    success = (p.returncode == 0)
                except Exception:
                    pass

        elif ext == ".rs":
            if shutil.which("rustfmt"):
                try:
                    p = subprocess.run(
                        ["rustfmt", full_path],
                        cwd=clean_cwd,
                        capture_output=True,
                        timeout=5
                    )
                    success = (p.returncode == 0)
                except Exception:
                    pass

        elif ext in (".js", ".jsx", ".ts", ".tsx", ".json"):
            if shutil.which("biome"):
                try:
                    p = subprocess.run(
                        ["biome", "format", "--write", full_path],
                        cwd=clean_cwd,
                        capture_output=True,
                        timeout=5
                    )
                    success = (p.returncode == 0)
                except Exception:
                    pass
            elif shutil.which("prettier"):
                try:
                    p = subprocess.run(
                        ["prettier", "--write", full_path],
                        cwd=clean_cwd,
                        capture_output=True,
                        timeout=5
                    )
                    success = (p.returncode == 0)
                except Exception:
                    pass

        results[rel] = success

    return results

def fast_syntax_check(cwd: str, file_paths: Sequence[str]) -> Tuple[bool, List[str]]:
    """
    Performs fast static syntax and compilation validation on modified files.
    Returns (all_valid, error_messages).
    Detects syntax errors in milliseconds without needing to run full integration tests.
    """
    clean_cwd = os.path.realpath(os.path.abspath(cwd))
    errors: List[str] = []

    for rel in file_paths:
        full_path = os.path.realpath(os.path.abspath(os.path.join(clean_cwd, rel)))
        if not full_path.startswith(clean_cwd + os.sep) and full_path != clean_cwd:
            continue
        if not os.path.isfile(full_path):
            continue

        ext = Path(full_path).suffix.lower()

        if ext == ".py":
            try:
                # python3 -m py_compile checks syntax without executing module
                p = subprocess.run(
                    [sys.executable, "-m", "py_compile", full_path],
                    cwd=clean_cwd,
                    capture_output=True,
                    text=True,
                    timeout=5
                )
                if p.returncode != 0:
                    err = p.stderr.strip() or f"Python 语法错误: {rel}"
                    errors.append(f"[{rel}] {err}")
            except Exception as e:
                errors.append(f"[{rel}] 静态编译检查异常: {e}")

        elif ext == ".go":
            if shutil.which("go"):
                try:
                    # go vet checks syntax and common mistakes on specific package/file
                    p = subprocess.run(
                        ["go", "vet", f"./{os.path.dirname(rel) or '.'}"],
                        cwd=clean_cwd,
                        capture_output=True,
                        text=True,
                        timeout=8
                    )
                    if p.returncode != 0:
                        err = p.stderr.strip() or f"Go 代码检查失败: {rel}"
                        errors.append(f"[{rel}] {err}")
                except Exception as e:
                    errors.append(f"[{rel}] Go vet 检查异常: {e}")

        elif ext == ".json":
            try:
                with open(full_path, "r", encoding="utf-8") as f:
                    json.load(f)
            except Exception as e:
                errors.append(f"[{rel}] JSON 格式错误: {e}")

        elif ext in (".js", ".mjs", ".cjs"):
            if shutil.which("node"):
                try:
                    p = subprocess.run(
                        ["node", "-c", full_path],
                        cwd=clean_cwd,
                        capture_output=True,
                        text=True,
                        timeout=5
                    )
                    if p.returncode != 0:
                        err = p.stderr.strip() or f"JavaScript 语法错误: {rel}"
                        errors.append(f"[{rel}] {err}")
                except Exception as e:
                    errors.append(f"[{rel}] Node.js 语法检查异常: {e}")

        elif ext == ".rs":
            if shutil.which("rustc"):
                try:
                    p = subprocess.run(
                        ["rustc", "--emit=metadata", "-o", "/dev/null", full_path],
                        cwd=clean_cwd,
                        capture_output=True,
                        text=True,
                        timeout=8
                    )
                    if p.returncode != 0:
                        err = p.stderr.strip().splitlines()[0] if p.stderr.strip() else f"Rust 编译检查失败: {rel}"
                        errors.append(f"[{rel}] {err}")
                except Exception as e:
                    errors.append(f"[{rel}] Rustc 语法检查异常: {e}")

    return (len(errors) == 0, errors)
