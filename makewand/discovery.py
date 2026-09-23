"""
Dynamic model discovery across all AI subscription ecosystems.
"""

import re
import json
from pathlib import Path
from typing import Dict, Any

def discover_available_models() -> Dict[str, Any]:
    models = {
        "claude": {"current_default": "官方动态默认 (Sonnet-5 / Fable)", "available": []},
        "codex": {"current_default": "gpt-6-astra", "available": []},
        "agy": {"current_default": "Gemini 3.8 Flash / Pro", "available": ["gemini-3.8-flash", "gemini-3.8-pro", "gemini-pro", "gemini-ultra"]},
        "muse": {"current_default": "Meta Provider (Default Llama / Code Preset)", "available": ["native-basic", "miniswe"]}
    }

    # Discover Claude models from ~/.claude.json
    claude_json = Path.home() / ".claude.json"
    if claude_json.exists():
        try:
            raw = claude_json.read_text(encoding="utf-8")
            data = json.loads(raw)
            found = set()
            for m in re.findall(r'"model":\s*"([^"]+)"', raw):
                found.add(m)
            for m in data.get("additionalModelOptionsCache", []):
                val = m.get("value")
                lbl = m.get("label")
                found.add(f"{val} ({lbl})")
            models["claude"]["available"] = sorted(list(found), reverse=True)
        except Exception:
            pass

    # Discover Codex models from ~/.codex/config.toml
    codex_toml = Path.home() / ".codex" / "config.toml"
    if codex_toml.exists():
        try:
            raw = codex_toml.read_text(encoding="utf-8")
            found = set()
            for m in re.findall(r'\[tui\.model_availability_nux\]\s*([\s\S]*?)(?=\n\[|$)', raw):
                for line in m.splitlines():
                    if "=" in line:
                        name = line.split("=")[0].strip().strip('"')
                        found.add(name)
            for m in re.findall(r'model\s*=\s*"([^"]+)"', raw):
                found.add(m)
            models["codex"]["available"] = sorted(list(found), reverse=True)
            if "gpt-6-astra" in found:
                models["codex"]["current_default"] = "gpt-6-astra"
        except Exception:
            pass

    # Discover Muse settings from ~/.config/muse/settings.json
    muse_settings = Path.home() / ".config" / "muse" / "settings.json"
    if muse_settings.exists():
        try:
            data = json.loads(muse_settings.read_text(encoding="utf-8"))
            if "model" in data:
                models["muse"]["current_default"] = data["model"]
        except Exception:
            pass

    return models
