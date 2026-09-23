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
