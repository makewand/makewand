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
import urllib.request
import urllib.error
from typing import Tuple, Optional, Dict, Any, List
from makewand.config import get_api_config, c, COLOR_CYAN, COLOR_YELLOW, COLOR_RED, COLOR_RESET

DEFAULT_SYSTEM_PROMPTS = {
    "coder": (
        "You are an expert software engineer and code implementation specialist. "
        "Analyze the project requirements carefully and output precise, idiomatic, and robust code. "
        "When modifying existing files or creating new ones, provide complete working code blocks."
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

class _DeadlineExceeded(Exception):
    """Raised inside the request worker when the total deadline has passed."""


def _response_socket(resp: Any):
    """Best-effort access to the socket under an http.client response."""
    import socket as _socket
    fp = getattr(resp, "fp", None)
    raw = getattr(fp, "raw", None)
    sock = getattr(raw, "_sock", None)
    return sock if isinstance(sock, _socket.socket) else None


def _abort_response(resp: Any) -> None:
    """Wake a reader blocked in recv() on this response (shutdown, not close)."""
    import socket as _socket
    sock = _response_socket(resp)
    if sock is not None:
        try:
            sock.shutdown(_socket.SHUT_RDWR)
        except OSError:
            pass


def _perform_request(req, per_read_timeout: float, stream: bool, print_prefix: str,
                     deadline: float, abort: "threading.Event", holder: List[Any],
                     partial: List[str]) -> Tuple[int, str, Optional[str]]:
    with urllib.request.urlopen(req, timeout=per_read_timeout) as resp:
        holder.append(resp)
        code = resp.status
        if stream:
            # Simple SSE / chunk streaming
            for line in resp:
                if abort.is_set() or time.monotonic() >= deadline:
                    raise _DeadlineExceeded()
                line_str = line.decode("utf-8", errors="replace")
                if line_str.startswith("data: "):
                    data_part = line_str[6:].strip()
                    if data_part == "[DONE]":
                        break
                    try:
                        delta_json = json.loads(data_part)
                        delta_content = ""
                        choices = delta_json.get("choices")
                        if choices and isinstance(choices, list) and len(choices) > 0:
                            delta_content = choices[0].get("delta", {}).get("content", "")
                        elif "delta" in delta_json:
                            delta_content = delta_json["delta"].get("text", "")
                        if delta_content:
                            partial.append(delta_content)
                            if print_prefix:
                                sys.stdout.write(delta_content)
                                sys.stdout.flush()
                    except Exception:
                        pass
            if abort.is_set():
                raise _DeadlineExceeded()
            if print_prefix:
                sys.stdout.write("\n")
                sys.stdout.flush()
            return code, "".join(partial), None
        try:
            raw_response = resp.read().decode("utf-8", errors="replace")
        except Exception:
            if abort.is_set():
                raise _DeadlineExceeded()
            raise
        if abort.is_set():
            raise _DeadlineExceeded()
        return code, raw_response, None


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
    body_bytes = json.dumps(data).encode("utf-8")
    total = max(0.001, float(timeout))
    deadline = time.monotonic() + total

    def _timeout_result(partial_text: str = "") -> Tuple[int, str, Optional[str]]:
        return -1, partial_text, f"Total timeout exceeded: API call did not finish within {timeout}s (monotonic deadline)"

    attempt = 0
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return _timeout_result()
        req = urllib.request.Request(url, data=body_bytes, headers=headers, method="POST")
        abort = threading.Event()
        holder: List[Any] = []
        partial: List[str] = []
        outcome: Dict[str, Any] = {}

        def _runner():
            try:
                outcome["value"] = _perform_request(req, remaining, stream, print_prefix,
                                                    deadline, abort, holder, partial)
            except BaseException as exc:  # re-raised in the calling thread
                outcome["error"] = exc

        worker = threading.Thread(target=_runner, name="makewand-api-request", daemon=True)
        worker.start()
        worker.join(max(0.0, deadline - time.monotonic()))
        if worker.is_alive():
            abort.set()
            for resp in holder:
                _abort_response(resp)
            worker.join(0.5)
            return _timeout_result("".join(partial))

        try:
            if "error" in outcome:
                raise outcome["error"]
            return outcome["value"]
        except _DeadlineExceeded:
            return _timeout_result("".join(partial))
        except urllib.error.HTTPError as e:
            err_body = e.read().decode("utf-8", errors="replace") if e.fp else ""
            if e.code in (429, 500, 502, 503, 504) and attempt < max_retries:
                attempt += 1
                sleep_sec = backoff_factor * (2 ** (attempt - 1))
                if deadline - time.monotonic() <= sleep_sec:
                    return e.code, err_body, f"HTTP Error {e.code}: {e.reason} - {err_body[:200]} (retry skipped: total timeout would be exceeded)"
                time.sleep(sleep_sec)
                continue
            return e.code, err_body, f"HTTP Error {e.code}: {e.reason} - {err_body[:200]}"
        except (urllib.error.URLError, TimeoutError) as e:
            if attempt < max_retries:
                attempt += 1
                sleep_sec = backoff_factor * (2 ** (attempt - 1))
                if deadline - time.monotonic() <= sleep_sec:
                    return _timeout_result()
                time.sleep(sleep_sec)
                continue
            reason = getattr(e, "reason", str(e))
            return -1, "", f"Network/URL Error: {reason}"
        except Exception as e:
            return -1, "", f"Execution Exception: {str(e)}"

def call_api_chat(
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
) -> Tuple[bool, str, Optional[str]]:
    """
    Dispatches a task via API to OpenAI, Anthropic, Gemini, xAI, or Local (Ollama/vLLM).
    Returns (success: bool, response_content: str, error_msg: Optional[str]).
    """
    from makewand.config import is_api_allowed, api_policy_error
    if not is_api_allowed(provider):
        return False, "", api_policy_error()
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
            return True, raw, None
        try:
            resp_json = json.loads(raw)
            content_blocks = resp_json.get("content", [])
            text_chunks = [b.get("text", "") for b in content_blocks if b.get("type") == "text"]
            return True, "".join(text_chunks), None
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
            return True, raw, None
        try:
            resp_json = json.loads(raw)
            choices = resp_json.get("choices", [])
            if choices:
                msg_content = choices[0].get("message", {}).get("content", "")
                return True, msg_content, None
            return False, raw, "No choices returned by API"
        except Exception as e:
            return False, raw, f"JSON parse error: {e}"
