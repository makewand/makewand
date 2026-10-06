"""
Makewand configuration and environment constants.
"""

import os
import sys
from pathlib import Path
from typing import Optional, List, Dict, Any

# Cache directories
CONFIG_DIR = Path(os.environ.get("MAKEWAND_CONFIG_DIR", Path.home() / ".config" / "makewand")).expanduser().resolve()
STATUS_CACHE_FILE = CONFIG_DIR / "status.json"
CANDIDATES_DIR = CONFIG_DIR / "candidates"
BACKUPS_DIR = CONFIG_DIR / "backups"

# Private state for artifacts (delivery/rejected patches, rollback backups,
# workspace locks) and shadow worktrees. Never a shared fixed /tmp path.
def _default_state_dir() -> Path:
    xdg_state = os.environ.get("XDG_STATE_HOME", "")
    # XDG: relative values are invalid and must be ignored.
    base = Path(xdg_state).expanduser() if xdg_state and Path(xdg_state).expanduser().is_absolute() else Path.home() / ".local" / "state"
    return base / "makewand"

ARTIFACTS_DIR = Path(os.path.abspath(Path(os.environ.get("MAKEWAND_ARTIFACTS_DIR") or (_default_state_dir() / "artifacts")).expanduser()))
SHADOW_WORKTREES_DIR = Path(os.path.abspath(Path(os.environ.get("MAKEWAND_SHADOW_DIR") or (_default_state_dir() / "shadow-worktrees")).expanduser()))


def ensure_private_dir(path) -> Path:
    """Create ``path`` (and missing parents) and return it as a private 0700 directory.

    Refuses a final component that is a symlink, is not a directory, or is not
    owned by the current user, so a pre-planted directory in a shared location
    can never receive Makewand artifacts.
    """
    target = Path(os.path.abspath(Path(path).expanduser()))
    if os.name == "nt":
        from makewand.native_windows import ensure_private_directory
        return ensure_private_directory(target)
    try:
        target.parent.mkdir(parents=True, exist_ok=True)
        os.mkdir(target, 0o700)
    except FileExistsError:
        pass
    info = os.lstat(target)
    import stat as _stat
    if _stat.S_ISLNK(info.st_mode):
        raise PermissionError(f"refusing symlinked private directory: {target}")
    if not _stat.S_ISDIR(info.st_mode):
        raise PermissionError(f"private directory path is not a directory: {target}")
    if hasattr(os, "getuid") and info.st_uid != os.getuid():
        raise PermissionError(f"private directory is owned by another user (uid {info.st_uid}): {target}")
    if _stat.S_IMODE(info.st_mode) != 0o700:
        nofollow = getattr(os, "O_NOFOLLOW", 0)
        if nofollow and hasattr(os, "fchmod"):
            fd = os.open(target, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | nofollow)
            try:
                os.fchmod(fd, 0o700)
            finally:
                os.close(fd)
        else:  # pragma: no cover - platforms without O_NOFOLLOW
            os.chmod(target, 0o700)
    return target

# Compatibility cache path with Gemini / Antigravity config
LEGACY_TRIO_CACHE = (CONFIG_DIR / "legacy_status.json" if os.environ.get("MAKEWAND_CONFIG_DIR")
                     else Path.home() / ".gemini" / "config" / "trio_status.json")

# Terminal Color Codes
COLOR_GREEN = "\033[92m"
COLOR_YELLOW = "\033[93m"
COLOR_RED = "\033[91m"
COLOR_BLUE = "\033[94m"
COLOR_CYAN = "\033[96m"
COLOR_PURPLE = "\033[95m"
COLOR_MAGENTA = "\033[95m"
COLOR_BOLD = "\033[1m"
COLOR_DIM = "\033[2m"
COLOR_GRAY = "\033[90m"
COLOR_RESET = "\033[0m"

def supports_color() -> bool:
    return sys.stdout.isatty()

def c(text: str, color: str) -> str:
    if supports_color():
        return f"{color}{text}{COLOR_RESET}"
    return text

def ensure_config_dir():
    if os.name == "nt":
        for directory in (CONFIG_DIR, CANDIDATES_DIR, BACKUPS_DIR):
            ensure_private_dir(directory)
        return
    CONFIG_DIR.mkdir(parents=True, exist_ok=True)
    CANDIDATES_DIR.mkdir(parents=True, exist_ok=True)
    BACKUPS_DIR.mkdir(parents=True, exist_ok=True)
    if LEGACY_TRIO_CACHE.parent.exists():
        LEGACY_TRIO_CACHE.parent.mkdir(parents=True, exist_ok=True)

API_KEYS_FILE = CONFIG_DIR / "api_keys.json"
CONFIG_FILE = CONFIG_DIR / "config.json"

class ConfigError(ValueError):
    """An existing execution-policy or credentials file cannot be decoded."""


def _load_json_object(path: Path) -> dict:
    import json
    try:
        with open(path, "r", encoding="utf-8") as stream:
            value = json.load(stream)
    except FileNotFoundError:
        return {}
    except (OSError, UnicodeError, ValueError) as error:
        raise ConfigError(f"could not load {path.name}: {error}") from error
    if not isinstance(value, dict):
        raise ConfigError(f"{path.name} must contain a JSON object")
    return value


def _validate_provider_controls(value: dict) -> None:
    """Authorization fields accept only their declared types or whole-field null."""
    enabled = value.get("enabled_providers")
    if enabled is not None and (not isinstance(enabled, dict)
                                or any(not isinstance(name, str) or not isinstance(flag, bool)
                                       for name, flag in enabled.items())):
        raise ConfigError("enabled_providers must be an object of booleans")
    active = value.get("active_providers")
    if active is not None and (not isinstance(active, list)
                               or any(not isinstance(name, str) for name in active)):
        raise ConfigError("active_providers must be an array of strings")
    local = value.get("local_model_enabled")
    if local is not None and not isinstance(local, bool):
        raise ConfigError("local_model_enabled must be a boolean")


def load_user_config() -> dict:
    """Load policy; only a missing optional file permits normal defaults."""
    value = _load_json_object(CONFIG_FILE)
    _validate_provider_controls(value)
    return value


_credential_warnings = set()

def load_api_keys() -> dict:
    """An invalid optional source warns, while environment fields still resolve."""
    try:
        try:
            os.chmod(API_KEYS_FILE, 0o600)
        except OSError:
            pass
        return _load_json_object(API_KEYS_FILE)
    except ConfigError as error:
        warning = (str(API_KEYS_FILE), str(error))
        if warning not in _credential_warnings:
            print(f"Warning: {error}; ignoring this optional credentials source", file=sys.stderr)
            _credential_warnings.add(warning)
        return {}


def _atomic_write_json(path: Path, value: dict) -> None:
    """Replace a complete private file; every pre-replace failure preserves it."""
    import json
    import tempfile
    data = json.dumps(value, ensure_ascii=False, indent=2, allow_nan=False)
    descriptor, temporary = tempfile.mkstemp(prefix=f".{path.stem}-", suffix=".json", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            if os.name != "nt":
                os.fchmod(stream.fileno(), 0o600)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass

def save_api_key(provider: str, api_key: str, base_url: str = None, model: str = None) -> bool:
    """Saves API configuration for a provider into api_keys.json."""
    try:
        # Saving is stricter than optional-source reads: never replace an
        # unreadable old credentials document with an empty fallback.
        keys = _load_json_object(API_KEYS_FILE)
        p = provider.lower().strip()
        fields = keys.get(p)
        keys[p] = dict(fields) if isinstance(fields, dict) else {}
        keys[p]["api_key"] = api_key
        if base_url:
            keys[p]["base_url"] = base_url
        if model:
            keys[p]["model"] = model
        ensure_config_dir()
        _atomic_write_json(API_KEYS_FILE, keys)
        return True
    except (OSError, ValueError, TypeError):
        return False

def _provider_aliases(provider: str) -> list:
    """Alias order is stable; a canonical entry wins conflicting aliases."""
    canonical = normalize_provider_name(provider)
    aliases = sorted(alias for alias, target in PROVIDER_ALIASES.items() if target == canonical)
    return aliases + [canonical]


def _api_entry(document, provider: str) -> dict:
    result = {}
    if isinstance(document, dict):
        for name in _provider_aliases(provider):
            fields = document.get(name)
            if isinstance(fields, dict):
                result.update(fields)
    return result


def get_api_config(provider: str) -> dict:
    """Resolve each field: nonempty environment > api_keys.json > config.json.

    Go flat fields take precedence over config.json's legacy nested ``api``
    entries. Aliases share one API configuration (notably openai/codex).
    Reading credentials never authorizes paid API use; is_api_allowed remains
    the separate admission gate.
    """
    p = normalize_provider_name(provider)
    cfg = load_user_config()
    file_keys = _api_entry(load_api_keys(), p)
    nested = _api_entry(cfg.get("api", {}), p)
    specs = {
        "codex": ("openai", ["OPENAI_API_KEY"], ["OPENAI_BASE_URL"], ["OPENAI_MODEL"], "https://api.openai.com/v1", "gpt-4o"),
        "claude": ("claude", ["ANTHROPIC_API_KEY"], ["ANTHROPIC_BASE_URL"], ["ANTHROPIC_MODEL"], "https://api.anthropic.com", "claude-sonnet-4-20250514"),
        "agy": ("gemini", ["GEMINI_API_KEY", "GOOGLE_API_KEY"], ["GEMINI_BASE_URL"], ["GEMINI_MODEL"], "https://generativelanguage.googleapis.com", "gemini-2.5-flash"),
        "grok": ("grok", ["XAI_API_KEY", "GROK_API_KEY"], ["XAI_BASE_URL"], ["GROK_MODEL"], "https://api.x.ai/v1", "grok-2-latest"),
        "muse": ("muse", ["META_API_KEY", "MUSE_API_KEY"], ["META_BASE_URL"], ["META_MODEL"], None, "llama-3.3-70b-instruct"),
        "deepseek": ("deepseek", ["DEEPSEEK_API_KEY"], ["DEEPSEEK_BASE_URL"], ["DEEPSEEK_MODEL"], "https://api.deepseek.com/v1", "deepseek-chat"),
        "qwen": ("qwen", ["DASHSCOPE_API_KEY", "QWEN_API_KEY"], ["DASHSCOPE_BASE_URL"], ["QWEN_MODEL"], "https://dashscope.aliyuncs.com/compatible-mode/v1", "qwen2.5-coder-32b-instruct"),
        "openrouter": ("openrouter", ["OPENROUTER_API_KEY"], ["OPENROUTER_BASE_URL"], ["OPENROUTER_MODEL"], "https://openrouter.ai/api/v1", "auto"),
        "siliconflow": ("siliconflow", ["SILICONFLOW_API_KEY"], ["SILICONFLOW_BASE_URL"], ["SILICONFLOW_MODEL"], "https://api.siliconflow.cn/v1", "deepseek-ai/DeepSeek-V3"),
        "kimi": ("kimi", ["MOONSHOT_API_KEY", "KIMI_API_KEY"], ["MOONSHOT_BASE_URL"], ["MOONSHOT_MODEL"], "https://api.moonshot.cn/v1", "kimi-latest"),
        "glm": ("glm", ["ZHIPU_API_KEY", "GLM_API_KEY", "ZHIPUAI_API_KEY"], ["ZHIPU_BASE_URL", "GLM_BASE_URL"], ["GLM_MODEL", "ZHIPU_MODEL"], "https://open.bigmodel.cn/api/paas/v4", "glm-4-plus"),
        "aider": ("aider", ["AIDER_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "DEEPSEEK_API_KEY"], [], ["AIDER_MODEL"], None, None),
        "local": ("ollama", ["LOCAL_MODEL_API_KEY"], ["LOCAL_MODEL_ENDPOINT", "OLLAMA_ENDPOINT", "OLLAMA_HOST"], ["LOCAL_MODEL_NAME", "OLLAMA_MODEL"], "http://localhost:11434/v1", None),
    }
    prefix, key_envs, url_envs, model_envs, default_url, default_model = specs.get(p, (p, [], [], [], None, None))

    def nonempty(value):
        return value.strip() if isinstance(value, str) and value.strip() else None

    result = {}
    for field, envs, default in (("api_key", key_envs, "ollama" if p == "local" else None),
                                 ("base_url", url_envs, default_url), ("model", model_envs, default_model)):
        value = nonempty(cfg.get(f"{prefix}_{field}")) or nonempty(nested.get(field)) or default
        if p == "local":
            value = (nonempty(cfg.get("ollama_url")) or value) if field == "base_url" else value
            if field == "model":
                value = nonempty(cfg.get("ollama_model")) or nonempty(cfg.get("local_model_name")) or nonempty(cfg.get("local_model")) or value
        value = nonempty(file_keys.get(field)) or value
        for env in envs:
            override = nonempty(os.environ.get(env))
            if override:
                value = override
                break
        result[field] = value
    if p == "local" and result["base_url"]:
        base = result["base_url"].rstrip("/")
        result["base_url"] = base if base.endswith(("/v1", "/api")) else f"{base}/v1"
    return result

def has_api_configured(provider: str) -> bool:
    """Returns True if provider has valid API key or endpoint configured."""
    p = provider.lower().strip()
    if p in ("local", "ollama"):
        return True  # Local free model doesn't require cloud API key
    cfg = get_api_config(p)
    return bool(cfg.get("api_key"))

def get_api_policy() -> str:
    """Cloud API billing requires an explicit opt-in, even when keys exist."""
    value = os.environ.get("MAKEWAND_API_POLICY", load_user_config().get("api_policy", "subscription_only"))
    return "allow_paid" if isinstance(value, str) and value.strip().lower() == "allow_paid" else "subscription_only"

def is_api_allowed(provider: str) -> bool:
    return normalize_provider_name(provider) == "local" or get_api_policy() == "allow_paid"

def api_policy_error() -> str:
    return ("当前 API 策略为 subscription_only，已阻止可能产生费用的云 API 调用；"
            "如需允许按量计费，请设置 MAKEWAND_API_POLICY=allow_paid "
            "或配置 api_policy=allow_paid。")

def save_user_config(cfg: dict) -> bool:
    """Saves dictionary to ~/.config/makewand/config.json."""
    try:
        if not isinstance(cfg, dict):
            return False
        # Keep other frontends' fields when this caller supplies only its own
        # updates, and refuse to destroy an invalid existing policy document.
        merged = load_user_config()
        merged.update(cfg)
        _validate_provider_controls(merged)
        ensure_config_dir()
        _atomic_write_json(CONFIG_FILE, merged)
        return True
    except (OSError, ValueError, TypeError):
        return False

def get_enabled_providers(extra_names=()) -> dict:
    """
    Returns dict of provider -> bool.
    Default: cloud/CLI providers enabled; local models require opt-in.
    Can be overridden in config.json under 'enabled_providers'
    or via environment variables (e.g. MAKEWAND_DISABLE_<PROVIDER>=1 or MAKEWAND_ENABLE_PROVIDERS=...).
    """
    import os
    cfg = load_user_config()
    enabled = {p: p != "local" for p in ALL_SUPPORTED_PROVIDERS}
    for name in extra_names:
        provider = normalize_provider_name(name)
        enabled[provider] = provider != "local"
    # Read the previous website installer schema during upgrades. Canonical
    # enabled_providers and explicit environment settings still take precedence.
    legacy_active = cfg.get("active_providers")
    if isinstance(legacy_active, list):
        active = {normalize_provider_name(p) for p in legacy_active if isinstance(p, str)}
        enabled = {p: p in active for p in enabled}
    if isinstance(cfg.get("local_model_enabled"), bool):
        enabled["local"] = cfg["local_model_enabled"]
    user_settings = cfg.get("enabled_providers", {})
    if isinstance(user_settings, dict):
        names = sorted(user_settings, key=lambda name: (normalize_provider_name(name) == name.lower().strip(), name))
        for name in names:
            if isinstance(user_settings[name], bool):
                enabled[normalize_provider_name(name)] = user_settings[name]

    # Aliases resolve before canonical names, then ENABLE wins DISABLE for the
    # same name. The final allowlist remains the highest-priority override.
    for provider in list(enabled):
        for name in _provider_aliases(provider):
            if os.environ.get(f"MAKEWAND_DISABLE_{name.upper()}") in ("1", "true", "yes"):
                enabled[provider] = False
            if os.environ.get(f"MAKEWAND_ENABLE_{name.upper()}") in ("1", "true", "yes"):
                enabled[provider] = True
    whitelist = os.environ.get("MAKEWAND_ENABLE_PROVIDERS")
    if whitelist:
        allowed = {normalize_provider_name(name) for name in whitelist.split(",")}
        for provider in list(enabled):
            enabled[provider] = provider in allowed

    return enabled

ALL_SUPPORTED_PROVIDERS = [
    "claude", "codex", "agy", "grok", "muse", "aider", "cursor", "copilot",
    "deepseek", "qwen", "glm", "kimi", "openrouter", "siliconflow", "local"
]

def get_all_supported_providers() -> list:
    """Returns copy of all supported provider names."""
    return list(ALL_SUPPORTED_PROVIDERS)

PROVIDER_ALIASES = {
    "ollama": "local", "gemini": "agy", "google": "agy", "anthropic": "claude",
    "openai": "codex", "xai": "grok", "meta": "muse", "dashscope": "qwen", "aliyun": "qwen",
    "zhipu": "glm", "moonshot": "kimi", "silicon": "siliconflow",
    "deepseek-coder": "deepseek", "deepseek-chat": "deepseek",
}


def normalize_provider_name(provider: str) -> str:
    """Normalize provider aliases and API siblings to one enablement identity."""
    name = provider.lower().strip()
    if name.endswith("-api"):
        name = name[:-4]
    return PROVIDER_ALIASES.get(name, name)


def normalize_tier(tier: Optional[str]) -> str:
    """
    Normalizes tier/mode string between Python (fast, standard, deep)
    and Go (fast, balanced, power).
    """
    if not tier:
        return "standard"
    t = str(tier).lower().strip()
    if t in ("fast",):
        return "fast"
    if t in ("standard", "balanced"):
        return "standard"
    if t in ("deep", "power"):
        return "deep"
    if t in ("auto",):
        return "auto"
    return "standard"

def tier_to_go_mode(tier: Optional[str]) -> str:
    """Translates Python tier to Go canonical usage mode (fast, balanced, power)."""
    norm = normalize_tier(tier)
    if norm == "fast":
        return "fast"
    elif norm == "deep":
        return "power"
    return "balanced"

def is_provider_enabled(provider: str) -> bool:
    """Returns True if provider is enabled."""
    p = normalize_provider_name(provider)
    enabled_map = get_enabled_providers((p,))
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
    if not is_provider_enabled(p):
        return False
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
    api_ok = is_api_allowed(p) and has_api_configured(p)
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
