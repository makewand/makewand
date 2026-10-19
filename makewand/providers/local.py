"""
Local Self-Hosted Free Model Provider (Ollama / vLLM / LocalAI / llama.cpp).
Completely free, 0 Token cost, 100% offline privacy.
"""

from makewand.proxy import urlopen as proxy_urlopen
import os
import sys
import json
import time
import urllib.error
import urllib.request
from typing import Tuple, List, Optional, Dict, Any
from makewand.config import c, COLOR_GREEN, COLOR_BOLD, COLOR_CYAN, get_api_config
# Message sizes are measured as dispatched, like every review prompt (one definition).
from makewand.review_contract import dispatched_size

def get_default_local_model() -> str:
    """Detects first available local model or returns configured name."""
    cfg = get_api_config("local")
    if cfg.get("model"):
        return cfg["model"]
    avail, active, _ = is_local_model_available()
    if avail and active:
        return active
    return "qwen2.5-coder:7b"

def is_local_model_available(timeout: float = 2.0) -> Tuple[bool, str, List[str]]:
    """
    Probes local endpoint (default: http://localhost:11434/v1/models or /api/tags).
    Returns (is_available, default_model_name, list_of_all_model_names).
    """
    cfg = get_api_config("local")
    base_url = (cfg.get("base_url") or "http://localhost:11434/v1").rstrip("/")
    if base_url.endswith("/v1"):
        models_url = f"{base_url}/models"
    else:
        models_url = f"{base_url}/api/tags"

    try:
        req = urllib.request.Request(models_url, headers={"User-Agent": "makewand/3.0"})
        with proxy_urlopen(req, timeout=timeout) as resp:
            if resp.status == 200:
                data = json.loads(resp.read().decode("utf-8"))
                models = []
                # OpenAI format: {"data": [{"id": "model_name"}, ...]}
                if "data" in data and isinstance(data["data"], list):
                    for item in data["data"]:
                        m_id = item.get("id")
                        if m_id and not m_id.startswith("bge-"):  # filter out embedding-only models
                            models.append(m_id)
                # Ollama native format: {"models": [{"name": "..."}, ...]}
                elif "models" in data and isinstance(data["models"], list):
                    for item in data["models"]:
                        m_name = item.get("name")
                        if m_name and not m_name.startswith("bge-"):
                            models.append(m_name)

                if models:
                    active = cfg.get("model") or models[0]
                    if active not in models:
                        active = models[0]
                    return True, active, models
                return True, "local-model", []
    except Exception:
        pass
    return False, "", []

def list_local_models(timeout: float = 2.0) -> List[str]:
    """Returns list of all available model names on local endpoint."""
    avail, _, models = is_local_model_available(timeout=timeout)
    return models if avail else []

def get_free_gpu_vram_mb() -> Optional[int]:
    """Queries free GPU VRAM in MB to prevent OOM on host training tasks."""
    import subprocess
    try:
        out = subprocess.check_output(
            ["nvidia-smi", "--query-gpu=memory.free", "--format=csv,noheader,nounits"],
            text=True, timeout=1.5
        )
        return int(out.strip().split("\n")[0])
    except Exception:
        return None

def has_active_gpu_training() -> Tuple[bool, str]:
    """Detects whether training or heavy compute processes are actively running on GPU."""
    import subprocess
    try:
        out = subprocess.check_output(
            ["nvidia-smi", "--query-compute-apps=pid,process_name,used_gpu_memory", "--format=csv,noheader"],
            text=True, timeout=1.5
        )
        lines = [line.strip() for line in out.strip().split("\n") if line.strip()]
        for line in lines:
            parts = [p.strip() for p in line.split(",")]
            if len(parts) >= 2:
                proc = parts[1]
                if any(k in proc for k in ["python", "train", "torch", "openlimno", "tensorflow"]):
                    vram_str = parts[2] if len(parts) >= 3 else "high"
                    return True, f"PID {parts[0]} ({proc.split('/')[-1]}, {vram_str})"
        return False, ""
    except Exception:
        return False, ""

def unload_local_model(model_name: str) -> None:
    """Sends explicit unload request to Ollama daemon to immediately release GPU/system RAM."""
    cfg = get_api_config("local")
    base_url = (cfg.get("base_url") or "http://localhost:11434").rstrip("/")
    if base_url.endswith("/v1"):
        base_url = base_url[:-3]
    try:
        req = urllib.request.Request(
            f"{base_url}/api/generate",
            headers={"Content-Type": "application/json"},
            data=json.dumps({"model": model_name, "keep_alive": 0}).encode("utf-8")
        )
        proxy_urlopen(req, timeout=4)
    except Exception:
        pass

def parse_local_quota(output: str) -> Tuple[bool, str, Optional[str]]:
    """Local models are 100% free and have no commercial rate limits."""
    return False, "", None


# Ollama silently drops the start of a prompt that does not fit the model's context
# window (num_ctx, by default far below what the model supports), and its
# OpenAI-compatible endpoint can neither set num_ctx nor report what it dropped: a
# verdict could rest on part of a diff. A read-only dispatch (review, judging,
# question) therefore runs on the native /api/chat endpoint with a window sized for
# the whole prompt and counts only when the token counts show that nothing was cut
# (ollama_whole_prompt_chat). An endpoint that only offers the OpenAI-compatible API
# (vLLM, llama.cpp, LM Studio) answers one when its reported prompt token count is
# plausible for the whole prompt.
OLLAMA_TEMPLATE_TOKENS = 512   # chat template and special tokens around the messages
OLLAMA_ANSWER_TOKENS = 8192    # answer budget (num_predict) inside the window
# No tokenizer averages more than about six bytes a token over a whole prompt of code
# and prose: a prompt processed into fewer tokens than its UTF-8 size divided by this
# lost part of itself. A prompt that really does (long runs of one character) fails closed.
OLLAMA_MAX_BYTES_PER_TOKEN = 6
# The smallest context window of a chat model. When the server reports no context
# length, a silent cap at that unknown length is ruled out only for a prompt and an
# answer that fit this window.
OLLAMA_MIN_CONTEXT_TOKENS = 2048
OLLAMA_SHOW_MAX_BYTES = 16 * 1024 * 1024
OLLAMA_SHOW_ERROR_MAX_BYTES = 64 * 1024


def estimate_prompt_tokens(text: str) -> int:
    """A generous token count of text, for sizing a context window: half its UTF-8
    size as dispatched (dispatched_size), each ASCII digit counted as a whole token.

    Code and prose average three to four bytes a token, a CJK character (three
    bytes) takes about one token, and tokenizers that split numbers spend a token
    on each digit. A prompt whose tokenizer needs more tokens than this (base64 or
    hex blobs at about 1.5 bytes a token) overflows that window; Ollama then reports
    a full window or refuses the prompt, and ollama_whole_prompt_chat asks once more
    with room for one token per byte (ceiling_prompt_tokens). It never accepts a cut
    prompt (whole_prompt_error).
    """
    digits = sum(text.count(digit) for digit in "0123456789")
    return (dispatched_size(text) + digits + 1) // 2


def ceiling_prompt_tokens(text: str) -> int:
    """The most tokens text can take: its UTF-8 size as dispatched (dispatched_size,
    which never counts less than the server receives). No tokenizer spends more than a
    token on a byte - byte-level BPE merges bytes, SentencePiece falls back to one
    token per byte - so a window of this size plus the template always holds it."""
    return dispatched_size(text)


def _fills_window(response: Dict[str, Any], window: int, system_bytes: int) -> bool:
    """Whether a response with valid token counts shows the prompt filling the window
    (whole_prompt_error's window conditions): a larger window may hold all of it."""
    prompt_tokens = response.get("prompt_eval_count")
    answer_tokens = response.get("eval_count")
    if type(prompt_tokens) is not int or prompt_tokens <= 0 or type(answer_tokens) is not int or answer_tokens < 0:
        return False
    return (prompt_tokens + system_bytes + OLLAMA_TEMPLATE_TOKENS > window
            or prompt_tokens + answer_tokens >= window)


def ollama_native_root(base_url: Optional[str]) -> str:
    """Root URL of Ollama's native API for the configured OpenAI-compatible base URL."""
    base = (base_url or "http://localhost:11434/v1").rstrip("/")
    for suffix in ("/v1", "/api"):
        if base.endswith(suffix):
            return base[:-len(suffix)]
    return base


def ollama_context_length(root: str, model: Optional[str], headers: Optional[Dict[str, str]] = None,
                          timeout: float = 4.0) -> Tuple[Optional[bool], Optional[int], Optional[str]]:
    """(the endpoint is Ollama, the model's trained context length or None, error) from /api/show.

    The first value is True for Ollama, False for an endpoint that answered without
    Ollama's native API (HTTP 404/405/501, or a reply that is no Ollama model
    information: vLLM, llama.cpp, LM Studio), and None when the probe itself failed
    (no connection, timeout, server error), which proves neither.
    """
    request_headers = dict(headers or {})
    request_headers["content-type"] = "application/json"
    try:
        # "name" is the field older servers read.
        req = urllib.request.Request(f"{root}/api/show", data=json.dumps({"model": model, "name": model}).encode("utf-8"),
                                     headers=request_headers, method="POST")
        with proxy_urlopen(req, timeout=timeout) as resp:
            body = resp.read(OLLAMA_SHOW_MAX_BYTES + 1)
        if len(body) > OLLAMA_SHOW_MAX_BYTES:
            return None, None, f"响应超过 {OLLAMA_SHOW_MAX_BYTES} 字节"
        info = json.loads(body.decode("utf-8"))
    except urllib.error.HTTPError as exc:
        try:
            detail = exc.read(OLLAMA_SHOW_ERROR_MAX_BYTES).decode("utf-8", "replace").strip()
        except Exception:
            detail = ""
        finally:
            exc.close()
        error = f"HTTP {exc.code}" + (f": {detail[:200]}" if detail else "")
        return (False if exc.code in (404, 405, 501) else None), None, error
    except ValueError:
        return False, None, "响应不是 Ollama 模型信息"
    except Exception as exc:
        return None, None, f"{type(exc).__name__}: {exc}"
    if not isinstance(info, dict) or not {"model_info", "details", "modelfile"} & set(info):
        return False, None, "响应不是 Ollama 模型信息"
    model_info = info.get("model_info") if isinstance(info.get("model_info"), dict) else {}
    lengths = [value for key, value in model_info.items()
               if str(key).endswith(".context_length") and type(value) is int and value > 0]
    # The language model's own entry; other components (a vision encoder) may list one too.
    trained = model_info.get(f"{model_info.get('general.architecture')}.context_length")
    if type(trained) is int and trained > 0:
        return True, trained, None
    return True, (min(lengths) if lengths else None), None


def prompt_token_floor_error(prompt_tokens: int, prompt_bytes: int) -> Optional[str]:
    """Why prompt_tokens is too few for a whole prompt of prompt_bytes, or None."""
    if prompt_tokens * OLLAMA_MAX_BYTES_PER_TOKEN < prompt_bytes:
        return f"处理的提示只有 {prompt_tokens} tokens，远少于 {prompt_bytes} 字节的提示应有的数量，可能已被截断"
    return None


def whole_prompt_error(response: Dict[str, Any], window: int, system_bytes: int,
                       prompt_bytes: int = 0) -> Optional[str]:
    """Why an Ollama /api/chat answer may rest on part of its prompt, or None.

    A prompt over the window loses its system message or, when the last message
    alone is too long, all but a few tokens of its start, and then fills the
    window: the processed prompt (prompt_eval_count) takes more than the window
    minus the system message's share, which the message's UTF-8 bytes plus
    OLLAMA_TEMPLATE_TOKENS bound. A smaller count proves nothing was cut, and
    prompt plus answer below the window proves the context was never shifted
    (prompt tokens discarded) while answering; without both counts neither is
    proven. A count too small for the prompt's prompt_bytes
    (prompt_token_floor_error) means it was cut some other way, e.g. by a server
    that truncates to half the window or caps it below what was requested.
    """
    prompt_tokens = response.get("prompt_eval_count")
    answer_tokens = response.get("eval_count")
    if type(prompt_tokens) is not int or prompt_tokens <= 0 or type(answer_tokens) is not int or answer_tokens < 0:
        return "未报告有效的提示/回答 token 数 (prompt_eval_count/eval_count)"
    floor_error = prompt_token_floor_error(prompt_tokens, prompt_bytes)
    if floor_error:
        return floor_error
    if prompt_tokens + system_bytes + OLLAMA_TEMPLATE_TOKENS > window:
        return f"处理的提示 ({prompt_tokens} tokens) 没有为系统消息留出余量，可能已被 {window} tokens 的上下文窗口截断"
    if prompt_tokens + answer_tokens >= window:
        return (f"提示与回答 ({prompt_tokens}+{answer_tokens} tokens) 占满 {window} tokens 的上下文窗口，"
                "回答期间可能已丢弃部分提示")
    return None


def openai_compatible_whole_prompt_chat(base_url: Optional[str], headers: Dict[str, str], request: Dict[str, Any],
                                        timeout: float, max_retries: int = 2,
                                        backoff_factor: float = 0.5) -> Tuple[bool, str, Optional[str]]:
    """Read-only chat on an endpoint that only offers the OpenAI-compatible API.

    Such a server (vLLM, llama.cpp, LM Studio) cannot be given a context window,
    and refuses a prompt over its own one (vLLM, recent llama.cpp) or shortens it
    (older llama.cpp, LM Studio's truncating overflow policies). The answer counts
    only when the response reports how many prompt tokens were processed
    (usage.prompt_tokens) and that count is plausible for the whole prompt
    (prompt_token_floor_error); a refusal or a missing or too small count is
    UNVERIFIED. A cut that leaves at least one token per OLLAMA_MAX_BYTES_PER_TOKEN
    bytes cannot be told from a whole prompt by its count.
    """
    from makewand.providers.api_client import CONTEXT_LIMIT_ERROR_RE, _make_http_request
    from makewand.providers.base import ProcessExecutionError
    model = request.get("model")
    endpoint = (base_url or "http://localhost:11434/v1").rstrip("/")
    if not endpoint.endswith("/chat/completions"):
        endpoint = f"{endpoint}/chat/completions"
    prompt_bytes = sum(dispatched_size(str(m.get("content") or "")) for m in request.get("messages") or [])
    code, raw, error = _make_http_request(endpoint, headers, dict(request, stream=False), timeout=timeout,
                                          stream=False, max_retries=max_retries, backoff_factor=backoff_factor)
    if code != 200:
        if code in (400, 413) and CONTEXT_LIMIT_ERROR_RE.search(f"{raw or ''} {error or ''}"):
            return False, raw, ProcessExecutionError(
                f"本地模型 {model} 拒绝了超过其上下文长度的提示，结论未验证 (UNVERIFIED): {str(raw or error)[:200]}",
                "UNVERIFIED")
        return False, raw, error or f"LOCAL API returned status {code}"
    try:
        response = json.loads(raw)
    except ValueError as exc:
        return False, raw, f"JSON parse error: {exc}"
    choices = response.get("choices") if isinstance(response, dict) else None
    message = choices[0].get("message") if isinstance(choices, list) and choices and isinstance(choices[0], dict) else None
    answer = message.get("content") if isinstance(message, dict) else None
    if not isinstance(answer, str):
        return False, raw, "JSON parse error: OpenAI 兼容接口响应缺少回答 (choices[0].message.content)"
    usage = response.get("usage")
    prompt_tokens = usage.get("prompt_tokens") if isinstance(usage, dict) else None
    if type(prompt_tokens) is not int or prompt_tokens <= 0:
        cut = "未报告有效的提示 token 数 (usage.prompt_tokens)"
    else:
        cut = prompt_token_floor_error(prompt_tokens, prompt_bytes)
    if cut:
        return False, answer, ProcessExecutionError(
            f"本地模型 {model} {cut}，不能确认模型读到了完整提示，结论未验证 (UNVERIFIED)", "UNVERIFIED")
    return True, answer, None


def ollama_whole_prompt_chat(base_url: Optional[str], headers: Dict[str, str], request: Dict[str, Any],
                             timeout: float, max_retries: int = 2,
                             backoff_factor: float = 0.5) -> Tuple[bool, str, Optional[str]]:
    """Sends an OpenAI-style chat request to Ollama's native /api/chat for the whole prompt.

    The context window is a generous token estimate of the messages
    (estimate_prompt_tokens) plus OLLAMA_TEMPLATE_TOKENS and OLLAMA_ANSWER_TOKENS,
    capped at the model's trained context length, the cap Ollama would otherwise
    apply silently. A prompt denser than the estimate (base64, hex) fills that window
    - the reported prompt_eval_count reaches it - or a newer Ollama refuses it: then,
    once, the request is sent again with a window of one token per byte of the
    messages (ceiling_prompt_tokens), still capped at the trained length, and that
    answer is judged the same way. When /api/show reports no context length (a server without
    model_info, or none of its keys), that cap is unknown, nothing is retried and the
    answer counts only when prompt and answer fit OLLAMA_MIN_CONTEXT_TOKENS. An endpoint without Ollama's native API
    gets the request on its OpenAI-compatible API instead
    (openai_compatible_whole_prompt_chat). Returns (ok, answer, error); the error
    is UNVERIFIED when the native API cannot be probed, when the server refuses a
    prompt over the window, or when whole_prompt_error() finds the prompt may be cut.
    """
    from makewand.providers.api_client import CONTEXT_LIMIT_ERROR_RE, _make_http_request
    from makewand.providers.base import ProcessExecutionError
    started = time.monotonic()
    root = ollama_native_root(base_url)
    model = request.get("model")
    native, trained, probe_error = ollama_context_length(root, model, headers, timeout=min(4.0, max(0.1, timeout)))
    if native is False:
        return openai_compatible_whole_prompt_chat(base_url, headers, request,
                                                   timeout=max(0.001, timeout - (time.monotonic() - started)),
                                                   max_retries=max_retries, backoff_factor=backoff_factor)
    if not native:
        return False, "", ProcessExecutionError(
            f"无法确认本地模型端点 {root} 是否提供 Ollama 原生接口 (/api/show: {probe_error})，无法为提示设置上下文窗口并核对 token 数，"
            "不能确认模型读到了完整提示，只读结论不予采纳 (UNVERIFIED)", "UNVERIFIED")
    messages = request.get("messages") or []
    contents = [(m.get("role"), str(m.get("content") or "")) for m in messages]
    system_bytes = sum(dispatched_size(text) for role, text in contents if role == "system")
    prompt_bytes = sum(dispatched_size(text) for _, text in contents)
    window = sum(estimate_prompt_tokens(text) for _, text in contents) + OLLAMA_TEMPLATE_TOKENS + OLLAMA_ANSWER_TOKENS
    # The retry's window: room for one token per byte, which no tokenizer exceeds.
    ceiling = sum(ceiling_prompt_tokens(text) for _, text in contents) + OLLAMA_TEMPLATE_TOKENS + OLLAMA_ANSWER_TOKENS
    if trained:
        window = min(window, trained)
        ceiling = min(ceiling, trained)
    while True:
        # Only with a known trained length can a larger window be asked for without Ollama capping it silently.
        can_grow = bool(trained) and ceiling > window
        options = dict(request.get("options") or {})  # num_gpu / num_thread of the GPU guard
        options.update(num_ctx=window, num_predict=OLLAMA_ANSWER_TOKENS, temperature=request.get("temperature", 0.2))
        body = {"model": model, "messages": messages, "stream": False, "keep_alive": request.get("keep_alive", "0"),
                # Newer Ollama then refuses a prompt over the window instead of shortening
                # it, and stops instead of shifting the context when the window is full.
                "truncate": False, "shift": False, "options": options}
        code, raw, error = _make_http_request(f"{root}/api/chat", headers, body,
                                              timeout=max(0.001, timeout - (time.monotonic() - started)),
                                              stream=False, max_retries=max_retries, backoff_factor=backoff_factor)
        if code != 200:
            if code in (400, 413) and CONTEXT_LIMIT_ERROR_RE.search(f"{raw or ''} {error or ''}"):
                if can_grow:
                    window = ceiling
                    continue
                return False, raw, ProcessExecutionError(
                    f"本地模型 {model} 拒绝了超过 {window} tokens 上下文窗口的提示，结论未验证 (UNVERIFIED): {str(raw or error)[:200]}",
                    "UNVERIFIED")
            if code in (404, 405, 501):
                # /api/show without /api/chat (a server imitating part of Ollama's API).
                return openai_compatible_whole_prompt_chat(base_url, headers, request,
                                                           timeout=max(0.001, timeout - (time.monotonic() - started)),
                                                           max_retries=max_retries, backoff_factor=backoff_factor)
            return False, raw, error or f"LOCAL API returned status {code}"
        try:
            response = json.loads(raw)
        except ValueError as exc:
            return False, raw, f"JSON parse error: {exc}"
        message = response.get("message") if isinstance(response, dict) else None
        answer = message.get("content") if isinstance(message, dict) else None
        if not isinstance(answer, str) or response.get("done") is not True:
            return False, raw, "JSON parse error: Ollama /api/chat 响应缺少已完成的回答"
        cut = whole_prompt_error(response, window, system_bytes, prompt_bytes)
        if (cut and can_grow and _fills_window(response, window, system_bytes)
                and not prompt_token_floor_error(response["prompt_eval_count"], prompt_bytes)):
            window = ceiling  # the prompt filled the estimated window: ask once more with room for all of it
            continue
        break
    if not cut and not trained and whole_prompt_error(response, min(window, OLLAMA_MIN_CONTEXT_TOKENS),
                                                      system_bytes, prompt_bytes):
        # Both counts are valid here: whole_prompt_error() above checked them.
        cut = (f"的上下文长度未由服务端报告 (/api/show 中没有 context_length)，无法排除提示被静默截断到该长度："
               f"处理的提示与回答 ({response['prompt_eval_count']}+{response['eval_count']} tokens) "
               f"超出任何模型都有的 {OLLAMA_MIN_CONTEXT_TOKENS} tokens 窗口；请升级 Ollama")
    if cut:
        return False, answer, ProcessExecutionError(f"本地模型 {model} {cut}，结论未验证 (UNVERIFIED)", "UNVERIFIED")
    return True, answer, None


def execute_local_task(
    prompt: str,
    cwd: Optional[str] = None,
    timeout: int = 300,
    tier: str = "standard",
    model: Optional[str] = None,
    stream: bool = False,
    readonly: bool = False,
    repo_root: Optional[str] = None,
    repo_trust: str = "trusted",
    allow_network: bool = True,
    role: str = "coder",
    **kwargs
) -> Tuple[bool, Optional[str], Optional[str]]:
    """
    Dispatches task to locally deployed open-source model via OpenAI-compatible endpoint.
    Zero token cost; data goes wherever the configured endpoint lives.

    Inference runs inside the model server (Ollama / vLLM), not in a child of
    makewand, so makewand must not renice itself: os.nice() on the caller is
    irreversible without privileges and would slow every later dispatch, test
    run and sandbox of this process. CPU/GPU pressure is controlled through the
    server request options (num_gpu / num_thread) below instead. (If makewand
    ever spawns its own inference child, lower *its* priority via preexec_fn.)

    The endpoint is a chat completion API: it returns text and never edits files.
    Callers that need file changes must verify the diff (run_pipeline/run_race do).
    A read-only dispatch (review, judging, question) must rest on the whole prompt:
    it runs on Ollama's native API with a sufficient context window, or on an
    OpenAI-compatible-only endpoint with its reported prompt token count checked,
    and fails closed (UNVERIFIED) otherwise (ollama_whole_prompt_chat).
    """
    from makewand.providers.api_client import call_api_chat
    from makewand.workflow import provider_outcome
    from makewand.config import COLOR_YELLOW, is_provider_enabled

    # Untrusted repo enforcement
    if repo_trust == "untrusted":
        allow_network = False
        if not readonly:
            return False, None, "不可信仓库 (--repo-trust=untrusted) 仅允许只读审计与分析，禁止执行写入或修改任务"

    if not is_provider_enabled("local"):
        return False, None, "本地大模型 (Local AI) 当前已被用户在配置中手动禁用。运行 'makewand enable local' 重新开启"

    if readonly and role == "coder":
        # Read-only dispatches (reviews, Q&A) must use the reviewer system prompt.
        role = "reviewer"
    if not readonly:
        from makewand.config import COLOR_CYAN
        print(c("ℹ️ 本地模型已激活 Agentic 文件解析落盘；返回的代码块将由系统自动写入工作区并参与质量门禁验收。", COLOR_CYAN), file=sys.stderr)

    avail, def_model, all_models = is_local_model_available()
    if not avail:
        return False, None, "本地大模型服务 (Ollama / vLLM) 未在 http://localhost:11434 启动或不可访问"

    active_model = model or def_model

    # Dynamic VRAM Safety Guard & Priority Invariant:
    # If host GPU is busy with training tasks (PID 657840) or free VRAM < 12GB,
    # strictly offload to CPU (num_gpu=0, 16 threads) so main training NEVER faces CUDA OOM.
    is_training, train_info = has_active_gpu_training()
    free_vram = get_free_gpu_vram_mb()
    extra_params = {
        "keep_alive": "0"
    }

    if is_training or free_vram is None or free_vram < 12288:
        if is_training:
            reason = f"检测到重要计算任务在运行: {train_info}"
        elif free_vram is None:
            reason = "GPU 显存状态探测不可用 (fail-closed)"
        else:
            reason = f"显存仅余 {free_vram}MB"
        print(c(f"🛡️ [GPU 保护与低优先级调度] {reason}。为绝对保证训练任务零 OOM 风险，编程模型 ({active_model}) 已降权并强制卸载至 CPU+RAM 伴随运行 (0 显存争抢)...", COLOR_YELLOW), file=sys.stderr)
        threads = min(16, max(1, os.cpu_count() or 4))
        extra_params["options"] = {"num_gpu": 0, "num_thread": threads}
    else:
        print(c(f"⚡ [GPU 加速运行] 当前显存充裕 ({free_vram}MB 可用，无正在进行的重型训练任务)，启用本地 GPU 加速运行...", COLOR_GREEN), file=sys.stderr)

    print(c(f"[Makewand -> Local AI] 派发免费本地任务 (模型: {active_model}, 零Token成本)...", COLOR_GREEN), file=sys.stderr)

    try:
        result = call_api_chat(
            provider="local",
            prompt=prompt,
            model=active_model,
            tier=tier,
            stream=stream,
            timeout=timeout,
            cwd=cwd,
            role=role,
            print_prefix=c(f"[Local AI ({active_model}) Live]", COLOR_GREEN + COLOR_BOLD) if stream else "",
            extra_params=extra_params,
            repo_trust=repo_trust,
            readonly=readonly,
        )

        return provider_outcome(result)
    finally:
        unload_local_model(active_model)
