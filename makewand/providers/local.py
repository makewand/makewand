"""
Local Self-Hosted Free Model Provider (Ollama / vLLM / LocalAI / llama.cpp).
Completely free, 0 Token cost, 100% offline privacy.
"""

import os
import sys
import json
import urllib.request
from typing import Tuple, List, Optional, Dict, Any
from makewand.config import c, COLOR_GREEN, COLOR_BOLD, COLOR_CYAN, get_api_config

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
        with urllib.request.urlopen(req, timeout=timeout) as resp:
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
        urllib.request.urlopen(req, timeout=4)
    except Exception:
        pass

def parse_local_quota(output: str) -> Tuple[bool, str, Optional[str]]:
    """Local models are 100% free and have no commercial rate limits."""
    return False, "", None

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
    Zero token cost, fully private.
    Enforces GPU VRAM protection and lower scheduling priority to prevent impacting training tasks.
    """
    from makewand.providers.api_client import call_api_chat
    from makewand.config import COLOR_YELLOW, is_provider_enabled

    if not is_provider_enabled("local"):
        return False, None, "本地大模型 (Local AI) 当前已被用户在配置中手动禁用。运行 'makewand enable local' 重新开启"

    # Lower CPU scheduling priority so background training tasks get full CPU
    try:
        os.nice(15)
    except Exception:
        pass

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

    if is_training or (free_vram is not None and free_vram < 12288):
        reason = f"检测到重要计算任务在运行: {train_info}" if is_training else f"显存仅余 {free_vram}MB"
        print(c(f"🛡️ [GPU 保护与低优先级调度] {reason}。为绝对保证训练任务零 OOM 风险，编程模型 ({active_model}) 已降权并强制卸载至 CPU+RAM 伴随运行 (0 显存争抢)...", COLOR_YELLOW), file=sys.stderr)
        extra_params["options"] = {"num_gpu": 0, "num_thread": 16}
    else:
        print(c(f"⚡ [GPU 加速运行] 当前显存充裕 ({free_vram}MB 可用，无正在进行的重型训练任务)，启用本地 GPU 加速运行...", COLOR_GREEN), file=sys.stderr)

    print(c(f"[Makewand -> Local AI] 派发免费本地任务 (模型: {active_model}, 零Token成本)...", COLOR_GREEN), file=sys.stderr)

    try:
        ok, out, err = call_api_chat(
            provider="local",
            prompt=prompt,
            model=active_model,
            tier=tier,
            stream=stream,
            timeout=timeout,
            cwd=cwd,
            role=role,
            print_prefix=c(f"[Local AI ({active_model}) Live]", COLOR_GREEN + COLOR_BOLD) if stream else "",
            extra_params=extra_params
        )

        if ok:
            return True, out, None
        return False, out, err
    finally:
        unload_local_model(active_model)

