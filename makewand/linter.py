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
import tempfile
from pathlib import Path
from typing import List, Sequence, Dict, Tuple, Optional
from makewand.sandbox import run_in_sandbox, is_bwrap_available

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
                        ["prettier", "--no-config", "--write", full_path],
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
                # In-process compilation check: runs in microseconds without subprocess overhead,
                # and strictly immune to cwd Python module shadowing (e.g. malicious py_compile.py).
                with open(full_path, "rb") as f:
                    source_bytes = f.read()
                compile(source_bytes, rel, "exec", dont_inherit=True)
            except SyntaxError as syn_err:
                err_msg = f"SyntaxError: {syn_err.msg} (line {syn_err.lineno}, col {syn_err.offset})"
                errors.append(f"[{rel}] {err_msg}")
            except Exception as e:
                errors.append(f"[{rel}] 静态编译检查异常: {e}")

        elif ext == ".go":
            if shutil.which("go"):
                try:
                    rel_dir = os.path.dirname(rel) or "."
                    has_mod = os.path.exists(os.path.join(clean_cwd, "go.mod"))
                    pkg_target = f"./{rel_dir}" if has_mod else rel
                    if is_bwrap_available():
                        code, out, err_out, _ = run_in_sandbox(
                            ["go", "vet", pkg_target],
                            workspace=clean_cwd,
                            timeout=8,
                            allow_network=False,
                            readonly=True,
                            audit_context="linter_syntax_check",
                        )
                        if code != 0:
                            err = (err_out or out).strip() or f"Go 代码检查失败: {rel}"
                            errors.append(f"[{rel}] {err}")
                    else:
                        with tempfile.TemporaryDirectory() as td:
                            dest = os.path.join(td, os.path.basename(rel))
                            shutil.copy2(full_path, dest)
                            if has_mod:
                                shutil.copy2(os.path.join(clean_cwd, "go.mod"), os.path.join(td, "go.mod"))
                            p = subprocess.run(
                                ["go", "vet", os.path.basename(rel)],
                                cwd=td,
                                capture_output=True,
                                text=True,
                                timeout=8,
                            )
                            if p.returncode != 0:
                                err = (p.stderr or p.stdout).strip() or f"Go 代码检查失败: {rel}"
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
                    if is_bwrap_available():
                        code, out, err_out, _ = run_in_sandbox(
                            ["node", "-c", rel],
                            workspace=clean_cwd,
                            timeout=5,
                            allow_network=False,
                            readonly=True,
                            audit_context="linter_syntax_check",
                        )
                        if code != 0:
                            err = (err_out or out).strip() or f"JavaScript 语法错误: {rel}"
                            errors.append(f"[{rel}] {err}")
                    else:
                        with tempfile.TemporaryDirectory() as td:
                            dest = os.path.join(td, os.path.basename(rel))
                            shutil.copy2(full_path, dest)
                            p = subprocess.run(
                                ["node", "-c", os.path.basename(rel)],
                                cwd=td,
                                capture_output=True,
                                text=True,
                                timeout=5,
                            )
                            if p.returncode != 0:
                                err = (p.stderr or p.stdout).strip() or f"JavaScript 语法错误: {rel}"
                                errors.append(f"[{rel}] {err}")
                except Exception as e:
                    errors.append(f"[{rel}] Node.js 语法检查异常: {e}")

        elif ext == ".rs":
            if shutil.which("rustc"):
                try:
                    if is_bwrap_available():
                        extra_ro: List[str] = []
                        rustup_dir = os.path.expanduser("~/.rustup")
                        if os.path.exists(rustup_dir):
                            real_rustup = os.path.realpath(rustup_dir)
                            extra_ro.append(real_rustup)
                            if real_rustup != rustup_dir:
                                extra_ro.append(rustup_dir)
                        code, out, err_out, _ = run_in_sandbox(
                            ["rustc", "--emit=metadata", "--out-dir", "/tmp", rel],
                            workspace=clean_cwd,
                            timeout=8,
                            allow_network=False,
                            readonly=True,
                            extra_ro_binds=extra_ro if extra_ro else None,
                            audit_context="linter_syntax_check",
                        )
                        if code != 0:
                            err = err_out.strip().splitlines()[0] if err_out.strip() else f"Rust 编译检查失败: {rel}"
                            errors.append(f"[{rel}] {err}")
                    else:
                        with tempfile.TemporaryDirectory() as td:
                            dest = os.path.join(td, os.path.basename(rel))
                            shutil.copy2(full_path, dest)
                            p = subprocess.run(
                                ["rustc", "--emit=metadata", "-o", os.devnull, os.path.basename(rel)],
                                cwd=td,
                                capture_output=True,
                                text=True,
                                timeout=8,
                            )
                            if p.returncode != 0:
                                err = p.stderr.strip().splitlines()[0] if p.stderr.strip() else f"Rust 编译检查失败: {rel}"
                                errors.append(f"[{rel}] {err}")
                except Exception as e:
                    errors.append(f"[{rel}] Rustc 语法检查异常: {e}")

    return (len(errors) == 0, errors)
