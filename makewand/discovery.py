"""
Model discovery across AI subscription CLIs.

Each provider entry reports where its data came from:
- "source": "detected" when the list was read from the local CLI cache/config,
  otherwise "builtin" (makewand's hardcoded reference list, which may be stale);
- "default_source": the same for "current_default".
Callers must not present builtin values as detected versions.
"""

import os
import re
import json
from pathlib import Path
from typing import Dict, Any

def discover_available_models() -> Dict[str, Any]:
    models = {
        "claude": {"current_default": "claude-sonnet-5 (Sonnet 5)", "available": ["claude-fable-5-1[1m] (Fable 5.1 - 官方最高旗舰 · Mythos级最强模型)", "claude-opus-5-5 (Opus 5.5 - 深度复杂推理)", "claude-sonnet-5 (Sonnet 5 - 默认主力高敏捷)", "claude-haiku-4-5-20251001 (Haiku 4.5 - 轻量极速)"]},
        "codex": {"current_default": "gpt-6-astra", "available": []},
        "agy": {"current_default": "Gemini 3.8 Flash / Pro", "available": ["gemini-3.8-flash", "gemini-3.8-pro", "gemini-pro", "gemini-ultra"]},
        "muse": {"current_default": "Meta Provider (Default Llama / Code Preset)", "available": ["native-basic", "miniswe"]},
        "grok": {"current_default": "grok-4.7", "available": ["grok-4.7", "grok-4.7-build-fast", "grok-4.6", "grok-4.5"]}
    }
    for entry in models.values():
        entry["source"] = "builtin"
        entry["default_source"] = "builtin"

    # Discover Claude models from official catalog cache (~/.claude/cache/model-catalog/*.json) and ~/.claude.json
    try:
        cat_dir = Path.home() / ".claude" / "cache" / "model-catalog"
        if cat_dir.exists():
            json_files = sorted(cat_dir.glob("*.json"), key=lambda f: f.stat().st_mtime, reverse=True)
            if json_files:
                cat_data = json.loads(json_files[0].read_text(encoding="utf-8"))
                cfg_models = cat_data.get("catalog", {}).get("config", {}).get("models", [])
                catalog_found = []
                for m in cfg_models:
                    mid = m.get("id")
                    mname = m.get("name")
                    mdesc = m.get("description", "")
                    notice = m.get("notice", {}).get("text", "") if m.get("notice") else ""
                    if "most capable" in notice.lower() or "toughest" in mdesc.lower():
                        lbl = f"{mid} ({mname} - 官方最高旗舰 · Mythos级最强)"
                    elif "opus" in mname.lower():
                        lbl = f"{mid} ({mname} - 深度复杂推理)"
                    elif "sonnet" in mname.lower():
                        lbl = f"{mid} ({mname} - 默认主力高敏捷)"
                    elif "haiku" in mname.lower():
                        lbl = f"{mid} ({mname} - 轻量极速)"
                    else:
                        lbl = f"{mid} ({mname})"
                    catalog_found.append(lbl)
                if catalog_found:
                    models["claude"]["available"] = catalog_found
                    models["claude"]["source"] = "detected"
    except Exception:
        pass

    try:
        claude_json = Path.home() / ".claude.json"
        if claude_json.exists():
            raw = claude_json.read_text(encoding="utf-8")
            data = json.loads(raw)
            # Only merge with the current list when that list itself was detected;
            # never mix the builtin reference list into "detected" results.
            found = set(models["claude"]["available"]) if models["claude"]["source"] == "detected" else set()
            for m in re.findall(r'"model":\s*"([^"]+)"', raw):
                found.add(m)
            for m in data.get("additionalModelOptionsCache", []):
                val = m.get("value")
                lbl = m.get("label")
                if val and str(val).startswith("claude-"):
                    found.add(f"{val} ({lbl})" if lbl else str(val))
            detected = sorted([m for m in found if m.startswith("claude-")], reverse=True)
            if detected:
                models["claude"]["available"] = detected
                models["claude"]["source"] = "detected"
    except Exception:
        pass

    # Discover Codex models from models_cache.json, config.toml, and CODEX_HOME
    try:
        codex_bases = []
        if os.environ.get("CODEX_HOME"):
            codex_bases.append(Path(os.environ["CODEX_HOME"]))
        codex_bases.extend([Path.home() / ".codex", Path.home() / ".codex-2"])

        found = set()
        configured_model = None

        for base in codex_bases:
            if not base.exists():
                continue

            # 1. Read models_cache.json (official model catalog from OpenAI)
            cache_file = base / "models_cache.json"
            if cache_file.exists():
                try:
                    cdata = json.loads(cache_file.read_text(encoding="utf-8"))
                    for m in cdata.get("models", []):
                        slug = m.get("slug") or m.get("id")
                        desc = m.get("description", "")
                        if slug:
                            if desc:
                                found.add(f"{slug} ({desc})")
                            else:
                                found.add(slug)
                except Exception:
                    pass

            # 2. Read config.toml
            codex_toml = base / "config.toml"
            if codex_toml.exists():
                try:
                    raw = codex_toml.read_text(encoding="utf-8")
                    for m in re.findall(r'\[tui\.model_availability_nux\]\s*([\s\S]*?)(?=\n\[|$)', raw):
                        for line in m.splitlines():
                            if "=" in line:
                                name = line.split("=")[0].strip().strip('"')
                                found.add(name)
                    for m in re.findall(r'model\s*=\s*"([^"]+)"', raw):
                        found.add(m)
                    if not configured_model:
                        cfg_m = re.search(r'^\s*model\s*=\s*"([^"]+)"', raw, re.MULTILINE)
                        if cfg_m:
                            configured_model = cfg_m.group(1).strip()
                except Exception:
                    pass

        if found:
            models["codex"]["available"] = sorted(list(found), reverse=True)
            models["codex"]["source"] = "detected"
        if configured_model:
            models["codex"]["current_default"] = configured_model
            models["codex"]["default_source"] = "detected"
        elif found:
            slugs = [f.split()[0] for f in found]
            if "gpt-6.1-sol" in slugs:
                models["codex"]["current_default"] = "gpt-6.1-sol"
                models["codex"]["default_source"] = "detected"
            elif "gpt-6-astra" in slugs:
                models["codex"]["current_default"] = "gpt-6-astra"
                models["codex"]["default_source"] = "detected"
    except Exception:
        pass

    # Discover Muse settings from ~/.config/muse/settings.json
    try:
        muse_settings = Path.home() / ".config" / "muse" / "settings.json"
        if muse_settings.exists():
            data = json.loads(muse_settings.read_text(encoding="utf-8"))
            if "model" in data:
                models["muse"]["current_default"] = data["model"]
                models["muse"]["default_source"] = "detected"
    except Exception:
        pass

    # Discover Grok models from ~/.grok/models_cache.json
    try:
        grok_cache = Path.home() / ".grok" / "models_cache.json"
        if grok_cache.exists():
            g_data = json.loads(grok_cache.read_text(encoding="utf-8"))
            if "models" in g_data and isinstance(g_data["models"], dict):
                discovered = list(g_data["models"].keys())
                if discovered:
                    models["grok"]["available"] = discovered
                    models["grok"]["source"] = "detected"
                    if "grok-4.7" in discovered:
                        models["grok"]["current_default"] = "grok-4.7"
                        models["grok"]["default_source"] = "detected"
    except Exception:
        pass

    return models

def get_provider_model_tier(provider: str, tier: str = "standard") -> Dict[str, Any]:
    """
    Dynamically resolve the most appropriate model ID/alias and effort setting
    for a given provider and tier based on official local cache and config files.
    Returns: {"model": str, "effort": str, "is_dynamic": bool}
    """
    from makewand.config import normalize_tier
    tier = normalize_tier(tier)
    
    if provider == "claude":
        # Check Anthropic official model catalog
        try:
            cat_dir = Path.home() / ".claude" / "cache" / "model-catalog"
            if cat_dir.exists():
                files = sorted(cat_dir.glob("*.json"), key=lambda f: f.stat().st_mtime, reverse=True)
                if files:
                    cat_data = json.loads(files[0].read_text(encoding="utf-8"))
                    cfg_models = cat_data.get("catalog", {}).get("config", {}).get("models", [])
                    top_flagship = None
                    sonnet_model = None
                    haiku_model = None
                    for m in cfg_models:
                        notice = (m.get("notice", {}).get("text") or "").lower()
                        desc = (m.get("description") or "").lower()
                        mid = m.get("id", "")
                        efforts = [o.get("id") for o in m.get("thinking", {}).get("effort_options", [])]
                        max_effort = efforts[-1] if efforts else "max"
                        
                        if not top_flagship and ("most capable" in notice or "toughest" in desc or "fable" in mid or "mythos" in mid):
                            top_flagship = (mid, max_effort)
                        elif not sonnet_model and ("sonnet" in mid or "efficient" in desc):
                            sonnet_model = (mid, "medium")
                        elif not haiku_model and ("haiku" in mid or "fastest" in desc):
                            haiku_model = (mid, "low")
                    
                    if tier == "deep" and top_flagship:
                        # Use alias "fable" if ID contains fable for maximum CLI compatibility
                        alias = "fable" if "fable" in top_flagship[0] else top_flagship[0]
                        return {"model": alias, "effort": top_flagship[1], "is_dynamic": True, "full_id": top_flagship[0]}
                    elif tier == "standard" and sonnet_model:
                        alias = "sonnet" if "sonnet" in sonnet_model[0] else sonnet_model[0]
                        return {"model": alias, "effort": sonnet_model[1], "is_dynamic": True, "full_id": sonnet_model[0]}
                    elif tier == "fast" and haiku_model:
                        alias = "haiku" if "haiku" in haiku_model[0] else haiku_model[0]
                        return {"model": alias, "effort": haiku_model[1], "is_dynamic": True, "full_id": haiku_model[0]}
        except Exception:
            pass
            
        # Default fallback if catalog unavailable
        if tier == "deep":
            return {"model": "fable", "effort": "max", "is_dynamic": False, "full_id": "claude-fable-5-1"}
        elif tier == "fast":
            return {"model": "haiku", "effort": "low", "is_dynamic": False, "full_id": "claude-haiku-4-5-20251001"}
        else:
            return {"model": "sonnet", "effort": "medium", "is_dynamic": False, "full_id": "claude-sonnet-5"}

    elif provider == "codex":
        discovered_models = []
        configured_default = None

        codex_bases = []
        if os.environ.get("CODEX_HOME"):
            codex_bases.append(Path(os.environ["CODEX_HOME"]))
        codex_bases.extend([Path.home() / ".codex", Path.home() / ".codex-2"])

        for base in codex_bases:
            if not base.exists():
                continue
            cache_file = base / "models_cache.json"
            if cache_file.exists() and not discovered_models:
                try:
                    cdata = json.loads(cache_file.read_text(encoding="utf-8"))
                    for m in cdata.get("models", []):
                        slug = m.get("slug") or m.get("id")
                        if slug:
                            discovered_models.append((slug, m.get("description", "")))
                except Exception:
                    pass
            cfg_file = base / "config.toml"
            if cfg_file.exists() and not configured_default:
                try:
                    raw = cfg_file.read_text(encoding="utf-8")
                    m = re.search(r'^\s*model\s*=\s*"([^"]+)"', raw, re.MULTILINE)
                    if m:
                        configured_default = m.group(1).strip()
                except Exception:
                    pass

        slugs = [s[0] for s in discovered_models]

        if tier == "fast":
            fast_candidates = [s for s in slugs if "luna" in s or "reserve" in s]
            chosen = fast_candidates[0] if fast_candidates else (configured_default or "gpt-6-luna")
            return {"model": chosen, "effort": "low", "is_dynamic": True}

        if tier == "deep":
            deep_candidates = [s for s in slugs if "6.1" in s or "astra" in s]
            chosen = deep_candidates[0] if deep_candidates else (configured_default or "gpt-6-astra")
            return {"model": chosen, "effort": "max", "is_dynamic": True}

        # standard tier
        if configured_default and (not slugs or configured_default in slugs):
            chosen = configured_default
        elif "gpt-6.1-sol" in slugs:
            chosen = "gpt-6.1-sol"
        elif "gpt-6-astra" in slugs:
            chosen = "gpt-6-astra"
        else:
            chosen = slugs[0] if slugs else "gpt-6-astra"

        return {"model": chosen, "effort": "high", "is_dynamic": True}

    elif provider == "grok":
        model_name = "grok-4.7"
        try:
            grok_cache = Path.home() / ".grok" / "models_cache.json"
            if grok_cache.exists():
                g_data = json.loads(grok_cache.read_text(encoding="utf-8"))
                models_dict = g_data.get("models", {})
                if "grok-4.7" in models_dict:
                    model_name = "grok-4.7"
                elif models_dict:
                    model_name = list(models_dict.keys())[0]
        except Exception:
            pass
        if tier == "fast":
            effort = "low"
            # Use fast variant if requested
            model_target = "grok-4.7-build-fast" if "grok-4.7" in model_name else model_name
        elif tier == "deep":
            effort = "high"
            model_target = model_name
        else:
            effort = "medium"
            model_target = model_name
        return {"model": model_target, "effort": effort, "is_dynamic": True}

    elif provider == "muse":
        model_name = "muse-spark-1.3-contributor"
        try:
            muse_settings = Path.home() / ".config" / "muse" / "settings.json"
            if muse_settings.exists():
                data = json.loads(muse_settings.read_text(encoding="utf-8"))
                if "model" in data:
                    model_name = data["model"]
        except Exception:
            pass
        effort = "xhigh" if tier == "deep" else ("high" if tier == "standard" else "low")
        return {"model": model_name, "effort": effort, "is_dynamic": True}

    elif provider == "agy":
        model_name = "gemini-3.1-pro-high" if tier == "deep" else "gemini-3.8-flash-high"
        effort = "high" if tier in ("deep", "standard") else "low"
        return {"model": model_name, "effort": effort, "is_dynamic": True}

    return {"model": "default", "effort": "medium", "is_dynamic": False}
