"""
Unified API Client for Makewand.
Supports OpenAI-compatible APIs (OpenAI, xAI, DeepSeek, Ollama, vLLM, LocalAI),
Anthropic Messages API, and Google Gemini API.
"""

import os
import sys
import json
import time
import threading
import functools
import inspect
import math
import urllib.request
import urllib.error
import re
from pathlib import Path
from typing import Tuple, Optional, Dict, Any, List
from makewand.config import get_api_config, c, COLOR_CYAN, COLOR_YELLOW, COLOR_RED, COLOR_RESET, COLOR_GREEN

DEFAULT_SYSTEM_PROMPTS = {
    "coder": (
        "You are an expert software engineer and autonomous code implementation agent. "
        "Analyze the project requirements carefully and implement the required changes by outputting "
        "file code blocks with target relative filepaths, formatted exactly as:\n"
        "```filepath: relative/path/to/file.ext\n"
        "<complete file content>\n"
        "```\n"
        "Or provide a standard unified diff block (```diff ... ```). "
        "Always ensure your code is complete, syntactically valid, and includes all necessary imports."
    ),
    "reviewer": (
        "You are an elite, independent Red-Team Code Reviewer and Security Auditor. "
        "Scrutinize the provided code and diff for logic flaws, edge case regressions, resource leaks, "
        "concurrency deadlocks, and security vulnerabilities. "
        "At the end of your analysis, emit a verdict in this exact format:\n"
        "MAKEWAND_VERDICT:\n"
        '{"pass": true|false, "defects": ["description of defect 1", ...]}\n'
    )
}

def apply_agentic_code_output(output: str, cwd: str, repo_trust: str = "trusted") -> List[str]:
    """
    Parses LLM code generation output and applies file modifications to cwd.
    Supports:
      1. Explicit filepath code blocks: ```filepath: path/to/file.ext\n<content>\n```
      2. File marker directives: File: `path/to/file.ext` or ### `path/to/file.ext`
      3. Unified diff blocks: ```diff\n--- a/file\n+++ b/file\n...```
    Returns list of modified relative file paths.
    Enforces strict security containment: no path traversal, no .git tampering, no symlink escape.
    """
    if not output or not cwd or repo_trust == "untrusted":
        return []

    clean_cwd = os.path.realpath(os.path.abspath(cwd))
    modified: List[str] = []

    def _is_safe_rel_path(p: str) -> Optional[str]:
        p = p.strip().strip("'\"`*:#")
        if not p or os.path.isabs(p):
            return None
        norm = os.path.normpath(p)
        parts = Path(norm).parts
        if ".." in parts or any(part.lower() == ".git" for part in parts):
            return None
        full = os.path.realpath(os.path.abspath(os.path.join(clean_cwd, norm)))
        if not full.startswith(clean_cwd + os.sep) and full != clean_cwd:
            return None
        return norm

    def _safe_write_file(rel: str, content: str) -> bool:
        dest = os.path.join(clean_cwd, rel)
        if os.path.isdir(dest):
            return False
        parent = os.path.dirname(dest)
        os.makedirs(parent, exist_ok=True)
        # Write to a temporary file in the same directory and atomically replace (os.replace).
        # This prevents partial writes on interruption and safely replaces inodes without
        # in-place truncating external hardlinked files (defense against F04).
        import tempfile
        tmp_path = None
        try:
            with tempfile.NamedTemporaryFile("w", dir=parent, delete=False, encoding="utf-8") as tmp:
                tmp.write(content)
                tmp.flush()
                tmp_path = tmp.name
            os.replace(tmp_path, dest)
            return True
        except Exception:
            if tmp_path and os.path.exists(tmp_path):
                try:
                    os.remove(tmp_path)
                except OSError:
                    pass
            return False

    # 1. First, check for unified diff blocks and apply via git apply if possible
    diff_blocks = re.findall(r"```(?:diff|patch)?\s*\n(--- [^\n]+\n\+\+\+ [^\n]+\n[\s\S]*?)```", output)
    if diff_blocks:
        for diff_text in diff_blocks:
            try:
                import subprocess
                p = subprocess.run(
                    ["git", "apply", "--whitespace=nowarn", "-"],
                    input=diff_text.encode("utf-8"),
                    cwd=clean_cwd,
                    capture_output=True,
                    timeout=5
                )
                if p.returncode == 0:
                    for line in diff_text.splitlines():
                        if line.startswith("+++ b/"):
                            rel = _is_safe_rel_path(line[6:].strip())
                            if rel and rel not in modified:
                                modified.append(rel)
            except Exception:
                pass
        if modified:
            try:
                from makewand.linter import auto_format_files
                auto_format_files(clean_cwd, modified)
            except Exception:
                pass
            return modified

    # 2. Pattern 1: ```[lang] (filepath|file|path)[=:\s]+path/to/file.ext
    p1 = re.compile(r"```[a-zA-Z0-9_-]*\s+(?:filepath|file|path)[=:\s]+[\"']?([^\s\"'\n`]+)[\"']?\s*\n([\s\S]*?)```")
    for match in p1.finditer(output):
        rel = _is_safe_rel_path(match.group(1))
        if rel and rel not in modified:
            if _safe_write_file(rel, match.group(2)):
                modified.append(rel)

    # 3. Pattern 2: ```(filepath|path|file)[:\s]+path/to/file.ext
    p2 = re.compile(r"```(?:filepath|path|file)[:\s]+[\"']?([^\s\"'\n`]+)[\"']?\s*\n([\s\S]*?)```")
    for match in p2.finditer(output):
        rel = _is_safe_rel_path(match.group(1))
        if rel and rel not in modified:
            if _safe_write_file(rel, match.group(2)):
                modified.append(rel)

    # 4. Pattern 3: Header preceding code block:
    # e.g.: ### `path/to/file.ext`\n```python\n...```
    # or File: `path/to/file.ext`\n```python\n...```
    p3 = re.compile(r"(?:###|##|#|\*\*File:\*\*|File:)\s+[`\"']?([a-zA-Z0-9_./\\-]+\.[a-zA-Z0-9]+)[`\"']?\s*\n+```[a-zA-Z0-9_-]*\s*\n([\s\S]*?)```")
    for match in p3.finditer(output):
        rel = _is_safe_rel_path(match.group(1))
        if rel and rel not in modified:
            if _safe_write_file(rel, match.group(2)):
                modified.append(rel)

    # 5. Pattern 4: Single-Program Tool Batch JSON blocks (CodeMode pattern)
    # e.g.: ```json:makewand-tools\n[{"action": "write_file", "path": "...", "content": "..."}]\n```
    tool_blocks = re.findall(r"```(?:json)?[:\s]*(?:makewand-tools|tools|actions|batch)?\s*\n(\[\s*\{[\s\S]*?\}\s*\])\s*```", output)
    if tool_blocks:
        for block in tool_blocks:
            try:
                actions = json.loads(block)
                if isinstance(actions, list):
                    for act in actions:
                        if isinstance(act, dict) and act.get("action") in ("write_file", "write", "create_file"):
                            p = act.get("path") or act.get("filepath") or act.get("file")
                            content = act.get("content", "")
                            if p and isinstance(content, str):
                                rel = _is_safe_rel_path(str(p))
                                if rel and rel not in modified:
                                    if _safe_write_file(rel, content):
                                        modified.append(rel)
            except Exception:
                pass

    if modified:
        try:
            from makewand.linter import auto_format_files
            auto_format_files(clean_cwd, modified)
        except Exception:
            pass

    return modified


def execute_agentic_tool_batch(
    tools: List[Dict[str, Any]],
    cwd: str,
    allow_command: bool = True
) -> List[Dict[str, Any]]:
    """
    Executes a batch of agentic tool actions inside cwd with safety confinement.
    Supported actions:
      - write_file: {"action": "write_file", "path": "...", "content": "..."}
      - read_file: {"action": "read_file", "path": "..."}
      - run_command: {"action": "run_command", "cmd": "..."} (executes in sandbox)
    """
    clean_cwd = os.path.realpath(os.path.abspath(cwd))
    results: List[Dict[str, Any]] = []

    def _is_safe_rel_path(p: str) -> Optional[str]:
        p = str(p).strip().strip("'\"`*:#")
        if not p or os.path.isabs(p):
            return None
        norm = os.path.normpath(p)
        parts = Path(norm).parts
        if ".." in parts or any(part.lower() == ".git" for part in parts):
            return None
        full = os.path.realpath(os.path.abspath(os.path.join(clean_cwd, norm)))
        if not full.startswith(clean_cwd + os.sep) and full != clean_cwd:
            return None
        return norm

    for tool in tools:
        if not isinstance(tool, dict):
            results.append({"status": "error", "error": "Tool call must be an object"})
            continue

        action = str(tool.get("action", "")).strip().lower()
        if action in ("write_file", "write", "create_file"):
            p = tool.get("path") or tool.get("filepath") or tool.get("file")
            content = tool.get("content", "")
            rel = _is_safe_rel_path(str(p)) if p else None
            if not rel:
                results.append({"action": action, "path": p, "status": "error", "error": "Invalid or unsafe filepath"})
                continue
            dest = os.path.join(clean_cwd, rel)
            parent = os.path.dirname(dest)
            os.makedirs(parent, exist_ok=True)
            import tempfile
            tmp_path = None
            try:
                with tempfile.NamedTemporaryFile("w", dir=parent, delete=False, encoding="utf-8") as tmp:
                    tmp.write(str(content))
                    tmp.flush()
                    tmp_path = tmp.name
                os.replace(tmp_path, dest)
                results.append({"action": action, "path": rel, "status": "ok", "bytes_written": len(str(content).encode("utf-8"))})
            except Exception as e:
                if tmp_path and os.path.exists(tmp_path):
                    try:
                        os.remove(tmp_path)
                    except OSError:
                        pass
                results.append({"action": action, "path": rel, "status": "error", "error": str(e)})

        elif action in ("read_file", "read", "view_file"):
            p = tool.get("path") or tool.get("filepath") or tool.get("file")
            rel = _is_safe_rel_path(str(p)) if p else None
            if not rel:
                results.append({"action": action, "path": p, "status": "error", "error": "Invalid or unsafe filepath"})
                continue
            dest = os.path.join(clean_cwd, rel)
            if not os.path.isfile(dest):
                results.append({"action": action, "path": rel, "status": "error", "error": "File does not exist"})
                continue
            try:
                if os.path.getsize(dest) > 512 * 1024:
                    results.append({"action": action, "path": rel, "status": "error", "error": "File exceeds maximum 512KB limit"})
                    continue
                with open(dest, "r", encoding="utf-8", errors="ignore") as f:
                    content = f.read()
                results.append({"action": action, "path": rel, "status": "ok", "content": content})
            except Exception as e:
                results.append({"action": action, "path": rel, "status": "error", "error": str(e)})

        elif action in ("run_command", "cmd", "exec"):
            if not allow_command:
                results.append({"action": action, "status": "error", "error": "Command execution disabled"})
                continue
            cmd = tool.get("cmd") or tool.get("command")
            if not cmd:
                results.append({"action": action, "status": "error", "error": "Missing cmd argument"})
                continue
            from makewand.sandbox import run_in_sandbox
            cmd_args = ["bash", "-c", str(cmd)] if isinstance(cmd, str) else list(cmd)
            ret, out, err, ex = run_in_sandbox(cmd_args, workspace=clean_cwd, timeout=60)
            results.append({
                "action": action,
                "cmd": str(cmd),
                "status": "ok" if ret == 0 else "failed",
                "returncode": ret,
                "stdout": out,
                "stderr": err,
                "error": ex
            })
        else:
            results.append({"action": action, "status": "error", "error": f"Unknown tool action: {action}"})

    return results


class _DeadlineExceeded(Exception):
    """Raised inside the request worker when the total deadline has passed."""


class _IncompleteStream(Exception):
    """A received stream does not establish a completed remote response."""


def _response_socket(resp: Any):
    """Best-effort access to the socket under an http.client response."""
    import socket as _socket
    fp = resp
    for _ in range(4):
        raw = getattr(fp, "raw", None)
        sock = getattr(raw, "_sock", None)
        if isinstance(sock, _socket.socket):
            return sock
        fp = getattr(fp, "fp", None)
        if fp is None:
            break
    return None


def _abort_response(resp: Any) -> None:
    """Wake a reader blocked in recv() on this response (shutdown, not close)."""
    import socket as _socket
    sock = _response_socket(resp)
    if sock is not None:
        try:
            sock.shutdown(_socket.SHUT_RDWR)
        except OSError:
            pass


MAX_API_RESPONSE_BYTES = 10 * 1024 * 1024  # 10MB limit aligned with Go router maxOutputBytes


def _stream_post_sse(
    resp: Any,
    deadline: Optional[float] = None,
    abort: Optional["threading.Event"] = None,
    partial: Optional[List[str]] = None,
    print_prefix: str = "",
) -> str:
    """Streams SSE responses from resp, enforcing MAX_API_RESPONSE_BYTES."""
    if partial is None:
        partial = []
    if abort is None:
        abort = threading.Event()
    if deadline is None:
        deadline = float("inf")

    total_bytes = 0
    terminal = False
    for line in resp:
        if abort.is_set() or time.monotonic() >= deadline:
            raise _DeadlineExceeded()
        line_bytes = len(line) if isinstance(line, (bytes, bytearray)) else len(line.encode("utf-8", errors="replace"))
        total_bytes += line_bytes
        if total_bytes > MAX_API_RESPONSE_BYTES:
            raise RuntimeError("API stream exceeded maximum 10MB limit")
        line_str = line.decode("utf-8", errors="replace") if isinstance(line, (bytes, bytearray)) else str(line)
        if line_str.startswith("data:"):
            data_part = line_str[5:].strip()
            if data_part == "[DONE]":
                terminal = True
                break
            try:
                delta_json = json.loads(data_part)
                if not isinstance(delta_json, dict):
                    raise ValueError("stream frame must be an object")
                delta_content = ""
                choices = delta_json.get("choices")
                if delta_json.get("type") == "message_stop":
                    terminal = True
                if isinstance(choices, list) and any(isinstance(item, dict) and item.get("finish_reason") is not None for item in choices):
                    terminal = True
                if choices and isinstance(choices, list) and len(choices) > 0:
                    delta_content = choices[0].get("delta", {}).get("content", "")
                elif "delta" in delta_json:
                    delta_content = delta_json["delta"].get("text", "")
                if delta_content:
                    partial.append(delta_content)
                    if print_prefix:
                        sys.stdout.write(delta_content)
                        sys.stdout.flush()
            except (ValueError, TypeError, AttributeError) as exc:
                raise _IncompleteStream("Malformed API stream frame") from exc
            if terminal:
                break
    if abort.is_set():
        raise _DeadlineExceeded()
    if not terminal:
        raise _IncompleteStream("API stream ended without a terminal response")
    if print_prefix:
        sys.stdout.write("\n")
        sys.stdout.flush()
    return "".join(partial)


def _post_json(
    resp: Any,
    abort: Optional["threading.Event"] = None,
) -> str:
    """Reads non-streaming response body, enforcing MAX_API_RESPONSE_BYTES."""
    if abort is None:
        abort = threading.Event()
    try:
        raw_bytes = resp.read(MAX_API_RESPONSE_BYTES + 1)
    except Exception:
        if abort.is_set():
            raise _DeadlineExceeded()
        raise
    if abort.is_set():
        raise _DeadlineExceeded()
    if len(raw_bytes) > MAX_API_RESPONSE_BYTES:
        raise RuntimeError("API response exceeded maximum 10MB limit")
    return raw_bytes.decode("utf-8", errors="replace") if isinstance(raw_bytes, (bytes, bytearray)) else str(raw_bytes)


def _perform_request(req, per_read_timeout: float, stream: bool, print_prefix: str,
                     deadline: float, abort: "threading.Event", holder: List[Any],
                     partial: List[str]) -> Tuple[int, str, Optional[str]]:
    with urllib.request.urlopen(req, timeout=per_read_timeout) as resp:
        holder.append(resp)
        code = resp.status
        if stream:
            content = _stream_post_sse(resp, deadline=deadline, abort=abort, partial=partial, print_prefix=print_prefix)
            return code, content, None
        content = _post_json(resp, abort=abort)
        return code, content, None


def _make_http_request(
    url: str,
    headers: Dict[str, str],
    data: Dict[str, Any],
    timeout: int = 180,
    stream: bool = False,
    print_prefix: str = "",
    max_retries: int = 2,
    backoff_factor: float = 0.5,
) -> Tuple[int, str, Optional[str]]:
    """
    Executes an HTTP POST with exponential backoff retry under a *total* deadline.

    `timeout` bounds the whole call (all attempts, backoff sleeps, headers and the
    streamed or buffered body), measured with time.monotonic(). urllib's own
    timeout is only a per-recv limit, so a server that trickles a byte at a time
    could otherwise extend a call indefinitely. Each attempt runs in a worker
    thread that the caller stops waiting for at the deadline; the response
    socket is shut down so the worker unwinds promptly.
    """
    from makewand.providers.base import ProcessExecutionError
    from makewand.execution_contract import ExecutionRequest
    from makewand.execution_runtime import current_context, execute, execution_context, mark_provider_invocation, task_id
    from makewand.workflow import provider_outcome

    body_bytes = json.dumps(data).encode("utf-8")
    total = max(0.001, float(timeout))
    parent_deadline = current_context().get("_deadline_monotonic")
    deadline = min(time.monotonic() + total, parent_deadline) if parent_deadline is not None else time.monotonic() + total

    def timeout_result(partial_text=""):
        return -1, partial_text, ProcessExecutionError(f"Total timeout exceeded: API call did not finish within {timeout}s (monotonic deadline)", "TIMEOUT")

    def perform(remaining):
        req = urllib.request.Request(url, data=body_bytes, headers=headers, method="POST")
        abort = threading.Event()
        holder: List[Any] = []
        partial: List[str] = []
        outcome: Dict[str, Any] = {}

        def runner():
            try:
                outcome["value"] = _perform_request(req, remaining, stream, print_prefix, deadline, abort, holder, partial)
            except urllib.error.HTTPError as exc:
                # Error bodies are untrusted network reads too. Keep their
                # bounded read in this deadline-controlled worker.
                holder.append(exc)
                try:
                    body = exc.read(65536).decode("utf-8", errors="replace") if exc.fp else ""
                    outcome["value"] = (exc.code, body, ProcessExecutionError(f"HTTP Error {exc.code}: {exc.reason} - {body[:200]}", "FAILED"))
                except Exception as error:
                    outcome["error"] = error
                finally:
                    exc.close()
            except BaseException as exc:
                outcome["error"] = exc

        worker = threading.Thread(target=runner, name="makewand-api-request", daemon=True)
        worker.start()
        worker.join(max(0.0, min(remaining, deadline - time.monotonic())))
        if worker.is_alive():
            abort.set()
            for resp in holder:
                _abort_response(resp)
            # No replay and no synchronous wait beyond the caller's deadline.
            return timeout_result("".join(partial))
        error = outcome.get("error")
        if error is not None:
            if isinstance(error, (_DeadlineExceeded, TimeoutError)) or isinstance(getattr(error, "reason", None), TimeoutError):
                return timeout_result("".join(partial))
            if isinstance(error, _IncompleteStream):
                return -1, "".join(partial), ProcessExecutionError(str(error), "UNKNOWN")
            if isinstance(error, urllib.error.URLError):
                return -1, "".join(partial), ProcessExecutionError(f"Network/URL Error: {error.reason}", "UNKNOWN")
            if isinstance(error, (KeyboardInterrupt, SystemExit)):
                raise error
            return -1, "".join(partial), ProcessExecutionError(f"Execution Exception: {error}", "UNKNOWN")
        return outcome["value"]

    attempt = 0
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return timeout_result()
        if attempt == 0:
            value = perform(remaining)
        else:
            context = current_context()
            parent = context.get("_request")
            request = ExecutionRequest(task_id=task_id(), stage=context.get("stage") or "api_retry",
                engine=parent.engine if parent else "api", tier=parent.tier if parent else "standard",
                model=parent.model if parent else None, readonly=parent.readonly if parent else False,
                api_policy=parent.api_policy if parent else "allow_paid",
                timeout_ms=max(1, int(remaining * 1000)))
            captured = []

            def invoke(effective):
                mark_provider_invocation()
                raw = perform(min(remaining, effective) if effective is not None else remaining)
                captured.append(raw)
                return provider_outcome((raw[0] == 200, raw[1], raw[2]))

            with execution_context(lease_id=None):
                result = execute(request, invoke)
            if not captured or result.status not in ("PASSED", "FAILED"):
                return -1, result.output or "", ProcessExecutionError(result.error or "API retry could not complete", result.status)
            value = captured[0]
        code, raw, error = value
        # Only an explicit HTTP refusal permits a new dispatch. Connection
        # loss, deadline and incomplete response have unknown remote outcomes.
        if code not in (429, 500, 502, 503, 504) or getattr(error, "execution_status", None) != "FAILED" or attempt >= max_retries:
            return value
        attempt += 1
        sleep_sec = backoff_factor * (2 ** (attempt - 1))
        if deadline - time.monotonic() <= sleep_sec:
            return code, raw, error
        time.sleep(sleep_sec)

def _call_api_chat(
    provider: str,
    prompt: str,
    system_prompt: Optional[str] = None,
    model: Optional[str] = None,
    tier: str = "standard",
    stream: bool = False,
    timeout: int = 180,
    cwd: Optional[str] = None,
    role: str = "coder",
    print_prefix: str = "",
    extra_params: Optional[Dict[str, Any]] = None,
    max_retries: int = 2,
    backoff_factor: float = 0.5,
    repo_trust: str = "trusted",
    readonly: bool = False,
) -> Tuple[bool, str, Optional[str]]:
    """
    Dispatches a task via API to OpenAI, Anthropic, Gemini, xAI, or Local (Ollama/vLLM).
    Returns (success: bool, response_content: str, error_msg: Optional[str]).
    """
    from makewand.config import is_api_allowed, api_policy_error
    if not is_api_allowed(provider):
        return False, "", api_policy_error()
    if repo_trust == "untrusted" and not readonly:
        return False, "", "不可信仓库 (--repo-trust=untrusted) 仅允许只读审计与分析，禁止执行写入或修改任务"
    if extra_params:
        extra_params = dict(extra_params)
        if "max_retries" in extra_params:
            max_retries = extra_params.pop("max_retries")
        if "backoff_factor" in extra_params:
            backoff_factor = extra_params.pop("backoff_factor")

    p = provider.lower().strip()
    from makewand.config import normalize_tier
    tier = normalize_tier(tier)
    cfg = get_api_config(p)
    api_key = cfg.get("api_key")
    base_url = cfg.get("base_url")
    active_model = model or cfg.get("model")

    if not system_prompt:
        system_prompt = DEFAULT_SYSTEM_PROMPTS.get(role, DEFAULT_SYSTEM_PROMPTS["coder"])
        if cwd:
            system_prompt += f"\nTarget working directory: {cwd}"

    def _apply_code_if_coder(text: str) -> None:
        if repo_trust == "untrusted" or readonly:
            return
        if role == "coder" and cwd and text:
            try:
                mod_files = apply_agentic_code_output(text, cwd, repo_trust=repo_trust)
                if mod_files:
                    print(c(f"✔ [{p.upper()} Agentic] 成功提取并落地 {len(mod_files)} 个修改文件: {', '.join(mod_files[:4])}", COLOR_GREEN), file=sys.stderr)
            except Exception:
                pass

    # 1. Anthropic Claude Messages API
    if p in ("claude", "anthropic"):
        if not api_key:
            return False, "", "未配置 ANTHROPIC_API_KEY，无法使用 Claude API"
        if not base_url:
            base_url = "https://api.anthropic.com"
        endpoint = base_url.rstrip("/")
        if not endpoint.endswith("/v1/messages") and not endpoint.endswith("/messages"):
            endpoint = f"{endpoint}/v1/messages"

        active_model = active_model or ("claude-3-7-sonnet-20250219" if tier != "fast" else "claude-3-5-haiku-20241022")
        headers = {
            "x-api-key": api_key,
            "anthropic-version": "2023-06-01",
            "content-type": "application/json"
        }
        data = {
            "model": active_model,
            "max_tokens": 8192,
            "system": system_prompt,
            "messages": [{"role": "user", "content": prompt}],
            "stream": stream
        }
        code, raw, err = _make_http_request(
            endpoint, headers, data, timeout=timeout, stream=stream,
            print_prefix=print_prefix, max_retries=max_retries, backoff_factor=backoff_factor
        )
        if code != 200:
            return False, raw, err or f"Anthropic API returned status {code}"
        if stream:
            _apply_code_if_coder(raw)
            return True, raw, None
        try:
            resp_json = json.loads(raw)
            content_blocks = resp_json.get("content", [])
            text_chunks = [b.get("text", "") for b in content_blocks if b.get("type") == "text"]
            res_text = "".join(text_chunks)
            _apply_code_if_coder(res_text)
            return True, res_text, None
        except Exception as e:
            return False, raw, f"JSON parse error: {e}"

    # 2. Google Gemini API
    elif p in ("agy", "gemini", "google"):
        if not api_key:
            return False, "", "未配置 GEMINI_API_KEY / GOOGLE_API_KEY，无法使用 Gemini API"
        if not base_url:
            base_url = "https://generativelanguage.googleapis.com"
        active_model = active_model or ("gemini-2.0-flash" if tier == "fast" else "gemini-2.5-pro")
        endpoint = f"{base_url.rstrip('/')}/v1beta/models/{active_model}:generateContent?key={api_key}"
        headers = {"content-type": "application/json"}
        full_content = f"System Instructions:\n{system_prompt}\n\nTask:\n{prompt}"
        data = {
            "contents": [{"role": "user", "parts": [{"text": full_content}]}],
            "generationConfig": {"temperature": 0.2}
        }
        code, raw, err = _make_http_request(
            endpoint, headers, data, timeout=timeout, stream=False,
            max_retries=max_retries, backoff_factor=backoff_factor
        )
        if code != 200:
            return False, raw, err or f"Gemini API returned status {code}"
        try:
            resp_json = json.loads(raw)
            candidates = resp_json.get("candidates", [])
            if candidates:
                parts = candidates[0].get("content", {}).get("parts", [])
                text_out = "".join(p.get("text", "") for p in parts)
                if stream and print_prefix:
                    sys.stdout.write(text_out + "\n")
                    sys.stdout.flush()
                _apply_code_if_coder(text_out)
                return True, text_out, None
            return False, raw, "No candidates returned by Gemini API"
        except Exception as e:
            return False, raw, f"JSON parse error: {e}"

    # 3. OpenAI-Compatible Format (Codex/OpenAI, Grok/xAI, Muse/Meta, DeepSeek, Qwen, OpenRouter, SiliconFlow, Kimi, GLM, Local)
    else:
        if p in ("codex", "openai") and not api_key:
            return False, "", "未配置 OPENAI_API_KEY，无法使用 OpenAI API"
        if p in ("grok", "xai") and not api_key:
            return False, "", "未配置 XAI_API_KEY / GROK_API_KEY，无法使用 xAI API"
        if p in ("muse", "meta") and not api_key:
            return False, "", "未配置 META_API_KEY / MUSE_API_KEY，无法使用 Meta/Muse API"
        if p in ("deepseek", "deepseek-coder", "deepseek-chat") and not api_key:
            return False, "", "未配置 DEEPSEEK_API_KEY，无法使用 DeepSeek API"
        if p in ("qwen", "dashscope", "aliyun") and not api_key:
            return False, "", "未配置 DASHSCOPE_API_KEY / QWEN_API_KEY，无法使用通义千问 API"
        if p in ("openrouter",) and not api_key:
            return False, "", "未配置 OPENROUTER_API_KEY，无法使用 OpenRouter API"
        if p in ("siliconflow", "silicon") and not api_key:
            return False, "", "未配置 SILICONFLOW_API_KEY，无法使用 SiliconFlow API"
        if p in ("kimi", "moonshot") and not api_key:
            return False, "", "未配置 MOONSHOT_API_KEY / KIMI_API_KEY，无法使用 Moonshot/Kimi API"
        if p in ("glm", "zhipu") and not api_key:
            return False, "", "未配置 ZHIPU_API_KEY / GLM_API_KEY，无法使用智谱 GLM API"

        if not base_url:
            if p in ("codex", "openai"):
                base_url = "https://api.openai.com/v1"
            elif p in ("grok", "xai"):
                base_url = "https://api.x.ai/v1"
            elif p in ("muse", "meta"):
                base_url = "https://api.meta.ai/v1"
            elif p in ("deepseek", "deepseek-coder", "deepseek-chat"):
                base_url = "https://api.deepseek.com/v1"
            elif p in ("qwen", "dashscope", "aliyun"):
                base_url = "https://dashscope.aliyuncs.com/compatible-mode/v1"
            elif p in ("openrouter",):
                base_url = "https://openrouter.ai/api/v1"
            elif p in ("siliconflow", "silicon"):
                base_url = "https://api.siliconflow.cn/v1"
            elif p in ("kimi", "moonshot"):
                base_url = "https://api.moonshot.cn/v1"
            elif p in ("glm", "zhipu"):
                base_url = "https://open.bigmodel.cn/api/paas/v4"
            elif p in ("local", "ollama"):
                base_url = "http://localhost:11434/v1"
            else:
                base_url = "https://api.openai.com/v1"

        endpoint = base_url.rstrip("/")
        if not endpoint.endswith("/chat/completions"):
            endpoint = f"{endpoint}/chat/completions"

        # Model defaults
        if not active_model:
            if p in ("codex", "openai"):
                active_model = "o3-mini" if tier == "fast" else ("o1" if tier == "deep" else "gpt-4o")
            elif p in ("grok", "xai"):
                active_model = "grok-2-latest"
            elif p in ("muse", "meta"):
                active_model = "llama-3.3-70b-instruct"
            elif p in ("deepseek", "deepseek-coder", "deepseek-chat"):
                active_model = "deepseek-reasoner" if tier == "deep" else "deepseek-chat"
            elif p in ("qwen", "dashscope", "aliyun"):
                active_model = "qwen-max" if tier == "deep" else "qwen2.5-coder-32b-instruct"
            elif p in ("openrouter",):
                active_model = "deepseek/deepseek-r1" if tier == "deep" else "auto"
            elif p in ("siliconflow", "silicon"):
                active_model = "deepseek-ai/DeepSeek-R1" if tier == "deep" else "deepseek-ai/DeepSeek-V3"
            elif p in ("kimi", "moonshot"):
                active_model = "kimi-latest"
            elif p in ("glm", "zhipu"):
                active_model = "glm-4-plus" if tier == "deep" else "codegeex-4"
            elif p in ("local", "ollama"):
                from makewand.providers.local import get_default_local_model
                active_model = get_default_local_model()


        headers = {"content-type": "application/json"}
        if api_key and api_key != "ollama":
            headers["Authorization"] = f"Bearer {api_key}"

        messages = [
            {"role": "system", "content": system_prompt},
            {"role": "user", "content": prompt}
        ]
        data = {
            "model": active_model,
            "messages": messages,
            "temperature": 0.2,
            "stream": stream
        }
        if extra_params:
            data.update(extra_params)
        if p in ("local", "ollama") and "keep_alive" not in data:
            data["keep_alive"] = "0"

        code, raw, err = _make_http_request(
            endpoint, headers, data, timeout=timeout, stream=stream,
            print_prefix=print_prefix, max_retries=max_retries, backoff_factor=backoff_factor
        )
        if code != 200:
            return False, raw, err or f"{p.upper()} API returned status {code}"
        if stream:
            _apply_code_if_coder(raw)
            return True, raw, None
        try:
            resp_json = json.loads(raw)
            choices = resp_json.get("choices", [])
            if choices:
                msg_content = choices[0].get("message", {}).get("content", "")
                _apply_code_if_coder(msg_content)
                return True, msg_content, None
            return False, raw, "No choices returned by API"
        except Exception as e:
            return False, raw, f"JSON parse error: {e}"


_API_SIGNATURE = inspect.signature(_call_api_chat)


@functools.wraps(_call_api_chat)
def call_api_chat(*args, **kwargs):
    """Preserve the API interface while sharing SDK admission and read-only scope."""
    from makewand.execution_contract import ExecutionRequest, ExecutionResult
    from makewand.execution_runtime import claim_provider_invocation, current_context, execute, execution_context, mark_provider_invocation, task_id
    from makewand.providers.base import ProcessExecutionError
    from makewand.workflow import provider_outcome
    from makewand.config import get_api_policy, is_api_allowed, api_policy_error, normalize_provider_name

    arguments = _API_SIGNATURE.bind(*args, **kwargs)
    explicit_args = dict(arguments.arguments)
    arguments.apply_defaults()
    options = dict(arguments.arguments)
    context = current_context()
    parent = context.get("_request")
    parent_trust = parent.get("repo_trust") if isinstance(parent, dict) else (getattr(parent, "repo_trust", None) if parent is not None else context.get("repo_trust"))
    parent_ro = parent.get("readonly", False) if isinstance(parent, dict) else (getattr(parent, "readonly", False) if parent is not None else context.get("readonly", False))
    if parent_trust == "untrusted":
        req_trust = "untrusted"
    elif "repo_trust" in explicit_args and explicit_args["repo_trust"] is not None:
        req_trust = explicit_args["repo_trust"]
    else:
        req_trust = parent_trust or options.get("repo_trust", "trusted")
    readonly = (
        parent_ro
        or options.get("readonly", False)
        or options["role"] == "reviewer"
        or (parent is not None and getattr(parent, "stage", None) in ("review", "judge", "audit"))
        or (context.get("stage") in ("review", "judge", "audit"))
    )
    if readonly:
        options["role"] = "reviewer"
    options["readonly"] = readonly
    options["repo_trust"] = req_trust
    if req_trust == "untrusted" and not readonly:
        from makewand.execution_contract import ExecutionResult
        from makewand.providers.base import ProcessExecutionError
        return ExecutionResult(False, "", ProcessExecutionError("不可信仓库 (--repo-trust=untrusted) 仅允许只读审计与分析，禁止执行写入或修改任务", "SANDBOX_UNAVAILABLE"))
    if (not is_api_allowed(options["provider"])
            or parent is not None and parent.api_policy == "subscription_only" and normalize_provider_name(options["provider"]) != "local"):
        return False, "", api_policy_error()
    try:
        timeout = float(options["timeout"])
        if isinstance(options["timeout"], bool) or not math.isfinite(timeout) or timeout <= 0:
            raise ValueError("invalid timeout")
    except (TypeError, ValueError, OverflowError):
        return ExecutionResult(False, "", "API timeout must be a finite positive number", status="INVALID_REQUEST")

    def invoke(remaining):
        mark_provider_invocation()
        effective = dict(options)
        if remaining is not None:
            effective["timeout"] = min(timeout, remaining)
        return provider_outcome(_call_api_chat(**effective))

    if claim_provider_invocation():
        deadline = context.get("_deadline_monotonic")
        remaining = max(0, deadline - time.monotonic()) if deadline is not None else None
        return invoke(remaining)
    request = ExecutionRequest(task_id=task_id(), stage=context.get("stage") or "api",
        engine=options["provider"], tier=options["tier"], model=options["model"], readonly=bool(readonly),
        repo_trust=req_trust,
        api_policy=parent.api_policy if parent else get_api_policy(), timeout_ms=max(1, int(timeout * 1000)),
        prompt=options["prompt"], cwd=options["cwd"])
    with execution_context(lease_id=None):
        result = execute(request, invoke)
    # Legacy CLI adapters unpack and reconstruct the tuple; keep runner-owned
    # status evidence in the string-compatible error across those boundaries.
    if not result.success and result.error is not None:
        return ExecutionResult(False, result.output, ProcessExecutionError(result.error, result.status), **result._metadata)
    return result
