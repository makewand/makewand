"""
Makewand configuration and environment constants.
"""

import sys
from pathlib import Path

# Cache directories
CONFIG_DIR = Path.home() / ".config" / "makewand"
STATUS_CACHE_FILE = CONFIG_DIR / "status.json"
CANDIDATES_DIR = CONFIG_DIR / "candidates"
BACKUPS_DIR = CONFIG_DIR / "backups"

# Compatibility cache path with Gemini / Antigravity config
LEGACY_TRIO_CACHE = Path.home() / ".gemini" / "config" / "trio_status.json"

# Terminal Color Codes
COLOR_GREEN = "\033[92m"
COLOR_YELLOW = "\033[93m"
COLOR_RED = "\033[91m"
COLOR_BLUE = "\033[94m"
COLOR_CYAN = "\033[96m"
COLOR_PURPLE = "\033[95m"
COLOR_BOLD = "\033[1m"
COLOR_RESET = "\033[0m"

def supports_color() -> bool:
    return sys.stdout.isatty()

def c(text: str, color: str) -> str:
    if supports_color():
        return f"{color}{text}{COLOR_RESET}"
    return text

def ensure_config_dir():
    CONFIG_DIR.mkdir(parents=True, exist_ok=True)
    CANDIDATES_DIR.mkdir(parents=True, exist_ok=True)
    BACKUPS_DIR.mkdir(parents=True, exist_ok=True)
    if LEGACY_TRIO_CACHE.parent.exists():
        LEGACY_TRIO_CACHE.parent.mkdir(parents=True, exist_ok=True)

API_KEYS_FILE = CONFIG_DIR / "api_keys.json"
CONFIG_FILE = CONFIG_DIR / "config.json"

def load_user_config() -> dict:
    """Loads ~/.config/makewand/config.json."""
    if CONFIG_FILE.exists():
        try:
            import json
            with open(CONFIG_FILE, "r", encoding="utf-8") as f:
                return json.load(f)
        except Exception:
            pass
    return {}

def load_api_keys() -> dict:
    """Loads ~/.config/makewand/api_keys.json."""
    if API_KEYS_FILE.exists():
        try:
            import json
            with open(API_KEYS_FILE, "r", encoding="utf-8") as f:
                return json.load(f)
        except Exception:
            pass
    return {}

def save_api_key(provider: str, api_key: str, base_url: str = None, model: str = None) -> bool:
    """Saves API configuration for a provider into api_keys.json."""
    ensure_config_dir()
    keys = load_api_keys()
    p = provider.lower().strip()
    if p not in keys:
        keys[p] = {}
    keys[p]["api_key"] = api_key
    if base_url:
        keys[p]["base_url"] = base_url
    if model:
        keys[p]["model"] = model
    try:
        import json
        with open(API_KEYS_FILE, "w", encoding="utf-8") as f:
            json.dump(keys, f, ensure_ascii=False, indent=2)
        return True
    except Exception:
        return False

def get_api_config(provider: str) -> dict:
    """
    Returns API configuration for provider.
    Priority: Environment variables -> api_keys.json -> config.json.
    """
    import os
    p = provider.lower().strip()
    file_keys = load_api_keys().get(p, {})
    cfg = load_user_config()

    res = {
        "api_key": None,
        "base_url": None,
        "model": None
    }

    # 1. Environment Variable Checks
    if p in ("codex", "openai"):
        res["api_key"] = os.environ.get("OPENAI_API_KEY")
        res["base_url"] = os.environ.get("OPENAI_BASE_URL", "https://api.openai.com/v1")
        res["model"] = os.environ.get("OPENAI_MODEL", "gpt-4o")
    elif p in ("claude", "anthropic"):
        res["api_key"] = os.environ.get("ANTHROPIC_API_KEY")
        res["base_url"] = os.environ.get("ANTHROPIC_BASE_URL", "https://api.anthropic.com")
        res["model"] = os.environ.get("ANTHROPIC_MODEL", "claude-3-7-sonnet-20250219")
    elif p in ("agy", "gemini", "google"):
        res["api_key"] = os.environ.get("GEMINI_API_KEY") or os.environ.get("GOOGLE_API_KEY")
        res["base_url"] = os.environ.get("GEMINI_BASE_URL", "https://generativelanguage.googleapis.com")
        res["model"] = os.environ.get("GEMINI_MODEL", "gemini-2.0-flash")
    elif p in ("grok", "xai"):
        res["api_key"] = os.environ.get("XAI_API_KEY") or os.environ.get("GROK_API_KEY")
        res["base_url"] = os.environ.get("XAI_BASE_URL", "https://api.x.ai/v1")
        res["model"] = os.environ.get("GROK_MODEL", "grok-2-latest")
    elif p in ("muse", "meta"):
        res["api_key"] = os.environ.get("META_API_KEY") or os.environ.get("MUSE_API_KEY")
        res["base_url"] = os.environ.get("META_BASE_URL")
        res["model"] = os.environ.get("META_MODEL", "llama-3.3-70b-instruct")
    elif p in ("deepseek", "deepseek-coder", "deepseek-chat"):
        res["api_key"] = os.environ.get("DEEPSEEK_API_KEY")
        res["base_url"] = os.environ.get("DEEPSEEK_BASE_URL", "https://api.deepseek.com/v1")
        res["model"] = os.environ.get("DEEPSEEK_MODEL", "deepseek-chat")
    elif p in ("qwen", "dashscope", "aliyun"):
        res["api_key"] = os.environ.get("DASHSCOPE_API_KEY") or os.environ.get("QWEN_API_KEY")
        res["base_url"] = os.environ.get("DASHSCOPE_BASE_URL", "https://dashscope.aliyuncs.com/compatible-mode/v1")
        res["model"] = os.environ.get("QWEN_MODEL", "qwen2.5-coder-32b-instruct")
    elif p in ("openrouter",):
        res["api_key"] = os.environ.get("OPENROUTER_API_KEY")
        res["base_url"] = os.environ.get("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1")
        res["model"] = os.environ.get("OPENROUTER_MODEL", "auto")
    elif p in ("siliconflow", "silicon"):
        res["api_key"] = os.environ.get("SILICONFLOW_API_KEY")
        res["base_url"] = os.environ.get("SILICONFLOW_BASE_URL", "https://api.siliconflow.cn/v1")
        res["model"] = os.environ.get("SILICONFLOW_MODEL", "deepseek-ai/DeepSeek-V3")
    elif p in ("kimi", "moonshot"):
        res["api_key"] = os.environ.get("MOONSHOT_API_KEY") or os.environ.get("KIMI_API_KEY")
        res["base_url"] = os.environ.get("MOONSHOT_BASE_URL", "https://api.moonshot.cn/v1")
        res["model"] = os.environ.get("MOONSHOT_MODEL", "kimi-latest")
    elif p in ("glm", "zhipu"):
        res["api_key"] = os.environ.get("ZHIPU_API_KEY") or os.environ.get("GLM_API_KEY")
        res["base_url"] = os.environ.get("ZHIPU_BASE_URL", "https://open.bigmodel.cn/api/paas/v4")
        res["model"] = os.environ.get("GLM_MODEL", "glm-4-plus")
    elif p in ("aider",):
        res["api_key"] = os.environ.get("AIDER_API_KEY") or os.environ.get("ANTHROPIC_API_KEY") or os.environ.get("OPENAI_API_KEY") or os.environ.get("DEEPSEEK_API_KEY")
        res["model"] = os.environ.get("AIDER_MODEL")
    elif p in ("local", "ollama"):
        res["api_key"] = os.environ.get("LOCAL_MODEL_API_KEY", "ollama")
        res["base_url"] = os.environ.get("LOCAL_MODEL_ENDPOINT") or os.environ.get("OLLAMA_ENDPOINT") or os.environ.get("OLLAMA_HOST") or cfg.get("ollama_url", "http://localhost:11434/v1")
        res["model"] = os.environ.get("LOCAL_MODEL_NAME") or os.environ.get("OLLAMA_MODEL") or cfg.get("local_model_name") or cfg.get("local_model")



    # 2. File keys fallback / override
    if file_keys:
        if file_keys.get("api_key") and not res["api_key"]:
            res["api_key"] = file_keys["api_key"]
        if file_keys.get("base_url"):
            res["base_url"] = file_keys["base_url"]
        if file_keys.get("model"):
            res["model"] = file_keys["model"]

    # Normalize local base_url to ensure it has /v1 if using OpenAI compatibility
    if p in ("local", "ollama") and res["base_url"]:
        b = res["base_url"].rstrip("/")
        if not b.endswith("/v1") and not b.endswith("/api"):
            res["base_url"] = f"{b}/v1"

    return res

def has_api_configured(provider: str) -> bool:
    """Returns True if provider has valid API key or endpoint configured."""
    p = provider.lower().strip()
    if p in ("local", "ollama"):
        return True  # Local free model doesn't require cloud API key
    cfg = get_api_config(p)
    return bool(cfg.get("api_key"))

def save_user_config(cfg: dict) -> bool:
    """Saves dictionary to ~/.config/makewand/config.json."""
    ensure_config_dir()
    try:
        import json
        with open(CONFIG_FILE, "w", encoding="utf-8") as f:
            json.dump(cfg, f, ensure_ascii=False, indent=2)
        return True
    except Exception:
        return False

def get_enabled_providers() -> dict:
    """
    Returns dict of provider -> bool.
    Default: all providers enabled (True).
    Can be overridden in config.json under 'enabled_providers'
    or via environment variables (e.g. MAKEWAND_DISABLE_<PROVIDER>=1 or MAKEWAND_ENABLE_PROVIDERS=...).
    """
    import os
    cfg = load_user_config()
    enabled = {p: True for p in ALL_SUPPORTED_PROVIDERS}
    user_settings = cfg.get("enabled_providers", {})
    if isinstance(user_settings, dict):
        for k, v in user_settings.items():
            k_clean = normalize_provider_name(k)
            if k_clean in enabled:
                enabled[k_clean] = bool(v)

    # Check env overrides
    # 1. MAKEWAND_DISABLE_<PROVIDER>=1
    for k in list(enabled.keys()):
        env_dis = os.environ.get(f"MAKEWAND_DISABLE_{k.upper()}")
        if env_dis in ("1", "true", "yes"):
            enabled[k] = False
        env_en = os.environ.get(f"MAKEWAND_ENABLE_{k.upper()}")
        if env_en in ("1", "true", "yes"):
            enabled[k] = True

    # 2. MAKEWAND_ENABLE_PROVIDERS=agy,claude,...
    whitelist = os.environ.get("MAKEWAND_ENABLE_PROVIDERS")
    if whitelist:
        wl_set = set(normalize_provider_name(x.strip()) for x in whitelist.split(","))
        for k in list(enabled.keys()):
            enabled[k] = k in wl_set

    return enabled

ALL_SUPPORTED_PROVIDERS = [
    "claude", "codex", "agy", "grok", "muse", "aider", "cursor", "copilot",
    "deepseek", "qwen", "glm", "kimi", "openrouter", "siliconflow", "local"
]

def get_all_supported_providers() -> list:
    """Returns copy of all supported provider names."""
    return list(ALL_SUPPORTED_PROVIDERS)

def normalize_provider_name(provider: str) -> str:
    """Normalizes aliases to standard canonical provider name."""
    p = provider.lower().strip()
    if p in ("ollama",):
        return "local"
    elif p in ("gemini", "google"):
        return "agy"
    elif p in ("anthropic",):
        return "claude"
    elif p in ("openai",):
        return "codex"
    elif p in ("xai",):
        return "grok"
    elif p in ("meta",):
        return "muse"
    elif p in ("dashscope", "aliyun"):
        return "qwen"
    elif p in ("zhipu",):
        return "glm"
    elif p in ("moonshot",):
        return "kimi"
    elif p in ("silicon",):
        return "siliconflow"
    return p

def is_provider_enabled(provider: str) -> bool:
    """Returns True if provider is enabled."""
    p = normalize_provider_name(provider)
    enabled_map = get_enabled_providers()
    return enabled_map.get(p, True)

def set_provider_enabled(provider: str, enabled: bool) -> bool:
    """Toggles provider enabled/disabled state in ~/.config/makewand/config.json."""
    p = normalize_provider_name(provider)
    if p not in ALL_SUPPORTED_PROVIDERS:
        return False

    cfg = load_user_config()
    if "enabled_providers" not in cfg or not isinstance(cfg["enabled_providers"], dict):
        cfg["enabled_providers"] = {}
    cfg["enabled_providers"][p] = bool(enabled)
    return save_user_config(cfg)

def has_subscription_configured(provider: str) -> bool:
    """Returns True if local subscription/agent CLI for provider is installed and functional."""
    import shutil
    import subprocess
    p = normalize_provider_name(provider)
    if p in ("local", "aider", "deepseek", "qwen", "glm", "kimi", "openrouter", "siliconflow"):
        return False
    if p == "copilot":
        if shutil.which("copilot"):
            return True
        if shutil.which("gh"):
            try:
                r = subprocess.run(["gh", "copilot", "--version"], capture_output=True, timeout=1.5)
                return r.returncode == 0
            except Exception:
                return False
        return False
    if p == "cursor":
        if not shutil.which("cursor"):
            return False
        try:
            r = subprocess.run(["cursor", "--version"], capture_output=True, timeout=1.5)
            return r.returncode == 0
        except Exception:
            return False
    return shutil.which(p) is not None


def get_provider_execution_mode(provider: str) -> str:
    """
    Returns the execution mode for a provider:
    - 'disabled': Explicitly disabled by user
    - 'hybrid': Both subscription and API are configured (Priority: Subscription -> Fallback: API)
    - 'subscription': Only subscription is configured
    - 'api': Only API is configured
    - 'local': Local self-hosted free model
    - 'none': Neither configured
    """
    p = normalize_provider_name(provider)
    if not is_provider_enabled(p):
        return "disabled"
    if p in ("local", "ollama"):
        from makewand.providers.local import is_local_model_available
        avail, _, _ = is_local_model_available(timeout=0.5)
        return "local" if avail else "none"
    sub_ok = has_subscription_configured(p)
    api_ok = has_api_configured(p)
    if sub_ok and api_ok:
        return "hybrid"
    elif sub_ok:
        return "subscription"
    elif api_ok:
        return "api"
    return "none"

def get_active_providers() -> list:
    """
    Dynamically returns the list of all providers that the user has ACTUALLY
    logged into, configured an API key for, or enabled locally.
    Providers that are disabled or unconfigured are completely excluded.
    """
    active = []
    for p in ALL_SUPPORTED_PROVIDERS:
        mode = get_provider_execution_mode(p)
        if mode in ("subscription", "api", "hybrid", "local"):
            active.append(p)
    return active

