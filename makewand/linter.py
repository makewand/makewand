"""
Makewand Auto-Linter & Fast Syntax Gate (inspired by Aider).
Provides millisecond-level automated formatting and syntax pre-validation
before launching expensive end-to-end test suites.
"""

import os
import re
import shutil
import subprocess
import sys
import json
import tempfile
from pathlib import Path
from typing import List, Sequence, Dict, Tuple, Optional
from makewand.sandbox import run_in_sandbox, is_bwrap_available


def _is_pure_javascript(full_path: str) -> bool:
    """
    Determines whether a JavaScript file (.js, .mjs, .cjs) is pure standard JS
    or if it contains JSX / TypeScript syntax that requires transpilation (Babel/SWC/tsc).
    Files containing JSX or TS constructs cannot be validated with raw `node -c`.
    """
    try:
        with open(full_path, "r", encoding="utf-8", errors="ignore") as f:
            content = f.read(262144)
    except Exception:
        return True

    # 1. JSX pragmas
    if "@jsx" in content or "@jsxImportSource" in content or "@jsxRuntime" in content:
        return False

    # 2. React / Preact JSX imports with JSX tags
    if re.search(r"""(?:from\s+['"]react['"]|require\(['"]react['"]\)|from\s+['"]preact['"]|require\(['"]preact['"]\))""", content):
        if re.search(r"""<[/A-Za-z>]""", content):
            return False

    # 3. JSX Fragment shorthand: <> ... </>
    if "<>" in content or "</>" in content:
        return False

    # 4. JSX closing tags: </tag>
    if re.search(r"""</[a-zA-Z][a-zA-Z0-9_.-]*>""", content):
        return False

    # 5. JSX opening tags with uppercase component names: e.g. <App />, <CustomComponent>
    if re.search(r"""<[A-Z][a-zA-Z0-9_]*(\s+[^<>]*)?(?:/?>)""", content):
        return False

    # 6. Common HTML/SVG elements in JSX: <div ..., <span ..., <p ..., etc.
    common_jsx_tags = r"""<(?:div|span|p|a|button|input|form|h[1-6]|ul|ol|li|table|tr|td|th|header|footer|nav|section|main|aside|img|svg|label|select|option)\b[^<>]*>"""
    if re.search(common_jsx_tags, content, re.IGNORECASE):
        return False

    return True


def _find_cargo_toml(clean_cwd: str, file_path: str) -> Optional[str]:
    """
    Finds enclosing Cargo.toml for file_path within clean_cwd.
    Returns Cargo.toml path if found, or None.
    """
    try:
        curr = os.path.dirname(os.path.realpath(file_path))
        clean_cwd_real = os.path.realpath(clean_cwd)
        while True:
            candidate = os.path.join(curr, "Cargo.toml")
            if os.path.isfile(candidate):
                return candidate
            if curr == clean_cwd_real or os.path.dirname(curr) == curr:
                break
            curr = os.path.dirname(curr)
        root_candidate = os.path.join(clean_cwd_real, "Cargo.toml")
        if os.path.isfile(root_candidate):
            return root_candidate
    except Exception:
        pass
    return None

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
            go_bin = shutil.which("go")
            gofmt_bin = shutil.which("gofmt")
            if go_bin or gofmt_bin:
                try:
                    rel_dir = os.path.dirname(rel) or "."
                    has_mod = os.path.exists(os.path.join(clean_cwd, "go.mod"))
                    pkg_target = f"./{rel_dir}" if has_mod else rel

                    extra_ro: List[str] = []
                    for b in (go_bin, gofmt_bin):
                        if b and os.path.exists(b):
                            real_b = os.path.realpath(b)
                            extra_ro.append(real_b)
                            b_parent = os.path.dirname(os.path.dirname(real_b))
                            if os.path.isdir(b_parent) and b_parent not in ("/", "/usr", "/usr/local"):
                                extra_ro.append(b_parent)

                    code = 0
                    raw_err = ""
                    err_cat = None
                    if go_bin:
                        code, out, err_out, err_cat = run_in_sandbox(
                            ["go", "vet", pkg_target],
                            workspace=clean_cwd,
                            timeout=8,
                            allow_network=False,
                            readonly=True,
                            extra_ro_binds=extra_ro if extra_ro else None,
                            extra_env={"GOTOOLCHAIN": "local", "GOPROXY": "off"},
                            audit_context="linter_syntax_check",
                        )
                        raw_err = (err_out or out).strip()

                    # Infrastructure or sandbox failure is not a code syntax defect
                    if (not go_bin or code != 0) and (
                        raw_err.startswith("bwrap:")
                        or "SandboxConfigError" in raw_err
                        or "SandboxUnavailable" in raw_err
                        or err_cat in ("SandboxUnavailable", "SandboxConfigError")
                        or code == -1
                    ):
                        raw_err = ""
                    else:
                        if not go_bin or code != 0:
                            # If go vet failed due to missing module/packages in offline sandbox or go was absent,
                            # verify pure syntax directly with gofmt -e.
                            if gofmt_bin:
                                g_code, g_out, g_err, g_cat = run_in_sandbox(
                                    [gofmt_bin, "-e", rel],
                                    workspace=clean_cwd,
                                    timeout=5,
                                    allow_network=False,
                                    readonly=True,
                                    extra_ro_binds=extra_ro if extra_ro else None,
                                    audit_context="linter_syntax_check",
                                )
                                g_err_strip = (g_err or g_out).strip()
                                if g_code == 0:
                                    raw_err = ""
                                elif (
                                    g_err_strip.startswith("bwrap:")
                                    or "SandboxConfigError" in g_err_strip
                                    or "SandboxUnavailable" in g_err_strip
                                    or g_cat in ("SandboxUnavailable", "SandboxConfigError")
                                    or g_code == -1
                                ):
                                    raw_err = ""
                                elif g_err.strip():
                                    raw_err = g_err.strip()
                        if raw_err:
                            errors.append(f"[{rel}] {raw_err}")
                except Exception as e:
                    errors.append(f"[{rel}] Go 语法检查异常: {e}")

        elif ext == ".json":
            try:
                with open(full_path, "r", encoding="utf-8") as f:
                    json.load(f)
            except Exception as e:
                errors.append(f"[{rel}] JSON 格式错误: {e}")

        elif ext in (".js", ".mjs", ".cjs"):
            # If the file contains JSX tags, fragments, React imports, or TS annotations,
            # vanilla `node -c` will fail with "Unexpected token '<'" or "Unexpected token ':'".
            # JSX and TypeScript require a transpiler/bundler (Babel/SWC/tsc) and must not be
            # flagged by the raw Node syntax gate.
            if not _is_pure_javascript(full_path):
                continue

            node_bin = shutil.which("node")
            if node_bin:
                try:
                    extra_ro: List[str] = []
                    if os.path.exists(node_bin):
                        extra_ro.append(os.path.realpath(node_bin))
                    code, out, err_out, err_cat = run_in_sandbox(
                        ["node", "-c", rel],
                        workspace=clean_cwd,
                        timeout=5,
                        allow_network=False,
                        readonly=True,
                        extra_ro_binds=extra_ro if extra_ro else None,
                        audit_context="linter_syntax_check",
                    )
                    raw_err = (err_out or out).strip()
                    if code != 0:
                        # Infrastructure or sandbox failure is not a syntax error
                        if (
                            raw_err.startswith("bwrap:")
                            or "SandboxConfigError" in raw_err
                            or "SandboxUnavailable" in raw_err
                            or err_cat in ("SandboxUnavailable", "SandboxConfigError")
                            or code == -1
                        ):
                            pass
                        # Skip JSX or TS tokens that slipped past static heuristic
                        elif "Unexpected token '<'" in raw_err or "Unexpected token ':'" in raw_err:
                            pass
                        else:
                            err = raw_err or f"JavaScript 语法错误: {rel}"
                            errors.append(f"[{rel}] {err}")
                except Exception as e:
                    errors.append(f"[{rel}] Node.js 语法检查异常: {e}")

        elif ext == ".rs":
            # If the file belongs to a Cargo crate (Cargo.toml exists), standalone `rustc` treats
            # non-root files as isolated crates, causing false-positive E0432 (unresolved import)
            # and E0601 (no main function). Crate compilation is validated by `cargo test` in orchestrator.
            if _find_cargo_toml(clean_cwd, full_path):
                continue

            rustc_bin = shutil.which("rustc")
            if rustc_bin:
                try:
                    rustup_dir = os.environ.get("RUSTUP_HOME") or os.path.expanduser("~/.rustup")
                    if not os.path.exists(rustup_dir):
                        user = os.environ.get("USER", "user")
                        alt = f"/home/{user}/.rustup"
                        if os.path.exists(alt):
                            rustup_dir = alt

                    extra_ro: List[str] = []
                    if os.path.exists(rustup_dir):
                        real_rustup = os.path.realpath(rustup_dir)
                        extra_ro.append(real_rustup)
                        if real_rustup != rustup_dir:
                            extra_ro.append(rustup_dir)
                    if os.path.exists(rustc_bin):
                        extra_ro.append(os.path.realpath(rustc_bin))

                    # Standalone Rust files: validate syntax using --crate-type lib so missing main() is not an error
                    code, out, err_out, err_cat = run_in_sandbox(
                        ["rustc", "--crate-type", "lib", "--emit=metadata", "--out-dir", "/tmp", rel],
                        workspace=clean_cwd,
                        timeout=8,
                        allow_network=False,
                        readonly=True,
                        extra_ro_binds=extra_ro if extra_ro else None,
                        extra_env={"RUSTUP_HOME": rustup_dir} if os.path.exists(rustup_dir) else None,
                        audit_context="linter_syntax_check",
                    )
                    raw_err = (err_out or out).strip()
                    if code != 0:
                        # Infrastructure or sandbox failure is not a syntax error
                        if (
                            raw_err.startswith("bwrap:")
                            or "SandboxConfigError" in raw_err
                            or "SandboxUnavailable" in raw_err
                            or err_cat in ("SandboxUnavailable", "SandboxConfigError")
                            or code == -1
                        ):
                            pass
                        else:
                            err = err_out.strip().splitlines()[0] if err_out.strip() else f"Rust 编译检查失败: {rel}"
                            errors.append(f"[{rel}] {err}")
                except Exception as e:
                    errors.append(f"[{rel}] Rustc 语法检查异常: {e}")


    return (len(errors) == 0, errors)
