"""
Unified API Client for Makewand.
Supports OpenAI-compatible APIs (OpenAI, xAI, DeepSeek, Ollama, vLLM, LocalAI),
Anthropic Messages API, and Google Gemini API.
"""

import os
import sys
import json
import time
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

def _make_http_request(url: str, headers: Dict[str, str], data: Dict[str, Any], timeout: int = 180, stream: bool = False, print_prefix: str = "") -> Tuple[int, str, Optional[str]]:
    """Executes HTTP POST request using urllib.request."""
    body_bytes = json.dumps(data).encode("utf-8")
    req = urllib.request.Request(url, data=body_bytes, headers=headers, method="POST")

    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            code = resp.status
            if stream:
                full_text = []
                # Simple SSE / chunk streaming
                for line in resp:
                    line_str = line.decode("utf-8", errors="replace")
                    if line_str.startswith("data: "):
                        data_part = line_str[6:].strip()
                        if data_part == "[DONE]":
                            break
                        try:
                            delta_json = json.loads(data_part)
                            # OpenAI style
                            delta_content = delta_json.get("choices", [{}])[0].get("delta", {}).get("content", "")
                            # Anthropic style
                            if not delta_content and "delta" in delta_json:
                                delta_content = delta_json["delta"].get("text", "")
                            if delta_content:
                                full_text.append(delta_content)
                                if print_prefix:
                                    sys.stdout.write(delta_content)
                                    sys.stdout.flush()
                        except Exception:
                            pass
                if print_prefix:
                    sys.stdout.write("\n")
                    sys.stdout.flush()
                return code, "".join(full_text), None
            else:
                raw_response = resp.read().decode("utf-8", errors="replace")
                return code, raw_response, None
    except urllib.error.HTTPError as e:
        err_body = e.read().decode("utf-8", errors="replace") if e.fp else ""
        return e.code, err_body, f"HTTP Error {e.code}: {e.reason} - {err_body[:200]}"
    except urllib.error.URLError as e:
        return -1, "", f"Network/URL Error: {e.reason}"
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
    extra_params: Optional[Dict[str, Any]] = None
) -> Tuple[bool, str, Optional[str]]:
    """
    Dispatches a task via API to OpenAI, Anthropic, Gemini, xAI, or Local (Ollama/vLLM).
    Returns (success: bool, response_content: str, error_msg: Optional[str]).
    """
    p = provider.lower().strip()
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
        code, raw, err = _make_http_request(endpoint, headers, data, timeout=timeout, stream=stream, print_prefix=print_prefix)
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
        code, raw, err = _make_http_request(endpoint, headers, data, timeout=timeout, stream=False)
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

        code, raw, err = _make_http_request(endpoint, headers, data, timeout=timeout, stream=stream, print_prefix=print_prefix)
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
