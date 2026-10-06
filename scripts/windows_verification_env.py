"""Private verification state, never a user's provider or Git configuration."""
import json
import os
import subprocess
from pathlib import Path

PROVIDERS = ("claude", "codex", "agy", "gemini", "grok", "muse", "aider", "cursor",
             "copilot", "deepseek", "qwen", "glm", "kimi", "openrouter", "siliconflow", "local", "remote")


def scrub_env(source):
    prefixes = ("MAKEWAND_", "GIT_", "XDG_", "CODEX_", "CLAUDE_", "ANTHROPIC_", "OPENAI_",
                "GOOGLE_", "GCLOUD_", "CLOUDSDK_", "AWS_", "AZURE_", "OLLAMA_", "AIDER_",
                "COPILOT_", "CURSOR_", "AGY_", "GEMINI_", "PYTHON")
    names = {"CLAUDECODE", "SSH_AUTH_SOCK", "SSH_ASKPASS", "GCM_INTERACTIVE", "GORACE", "GOFLAGS", "GOWORK", "GOENV", "BOTO_CONFIG"}
    suffixes = ("_API_KEY", "_AUTH_TOKEN", "_TOKEN", "_BASE_URL", "_API_URL", "_CREDENTIALS")
    return {key: value for key, value in source.items()
            if key.upper() not in names and not key.upper().startswith(prefixes) and not key.upper().endswith(suffixes)}


def isolated_env(directory, source=None, *, stub_codex=False):
    directory = Path(directory)
    for child in ("home", "home/AppData/Local", "home/AppData/Roaming", "config", "tmp", "codex"):
        (directory / child).mkdir(parents=True, exist_ok=True)
    env = scrub_env(os.environ if source is None else source)
    overrides = dict(HOME=str(directory / "home"), USERPROFILE=str(directory / "home"),
               APPDATA=str(directory / "home/AppData/Roaming"), LOCALAPPDATA=str(directory / "home/AppData/Local"),
               HOMEDRIVE="", HOMEPATH=str(directory / "home"), CODEX_HOME=str(directory / "codex"),
               MAKEWAND_CONFIG_DIR=str(directory / "config"), MAKEWAND_API_POLICY="subscription_only",
               TMP=str(directory / "tmp"), TEMP=str(directory / "tmp"), SystemTemp=str(directory / "tmp"),
               GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_SYSTEM=os.devnull,
               GIT_TERMINAL_PROMPT="0", GCM_INTERACTIVE="Never", GOENV="off", GOWORK="off")
    overridden = {key.upper() for key in overrides}
    env = {key: value for key, value in env.items() if key.upper() not in overridden}
    env.update(overrides)
    disabled = {name: False for name in PROVIDERS}
    if stub_codex:
        disabled["codex"] = True
    config = {"api_policy": "subscription_only", "allow_paid": False, "enabled_providers": disabled}
    path = directory / "config/config.json"
    path.write_text(json.dumps(config) + "\n", encoding="utf-8")
    path.chmod(0o600)
    return env


def clean_checkout(source, commit, env):
    """Require committed tracked inputs and no additional untracked source files."""
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=source, env=env).decode().strip()
    if head != commit or subprocess.run(["git", "diff", "--quiet", "HEAD", "--"], cwd=source, env=env).returncode:
        raise ValueError("source must be the exact clean verification commit")
    extra = subprocess.check_output(["git", "ls-files", "--others", "--exclude-standard", "-z"], cwd=source, env=env)
    if extra:
        raise ValueError("untracked checkout inputs are not accepted")
    return head
