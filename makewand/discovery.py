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
from typing import Dict, Any, List, Tuple

# Generic capability & family keywords for zero-hardcoding model ranking
FAST_FAMILIES = ["haiku", "luna", "flash", "mini", "nano", "reserve"]
FAST_KEYWORDS = ["fast", "fastest", "speed", "rapid", "lightweight", "affordable", "easier", "quick", "turbo", "efficient"]

DEEP_FAMILIES = ["opus", "fable", "mythos", "astra", "ultra", "frontier"]
DEEP_KEYWORDS = ["demanding", "toughest", "most capable", "complex", "flagship", "pro-high", "deep"]

STANDARD_FAMILIES = ["sonnet", "sol", "standard"]
STANDARD_KEYWORDS = ["workhorse", "balanced", "everyday", "general"]


def parse_semver(slug: str) -> tuple:
    """
    Extract (major, minor, patch) version tuple from model slugs or labels.
    Handles formats like:
      - 'gpt-6.1-sol' -> (6, 1, 0)
      - 'gpt-6.5-sol' -> (6, 5, 0)
      - 'gpt-7-astra' -> (7, 0, 0)
      - 'claude-opus-5-5' -> (5, 5, 0)
      - 'claude-fable-5-1[1m]' -> (5, 1, 0)
      - 'claude-haiku-4-5-20251001' -> (4, 5, 0)
      - 'grok-4.7' -> (4, 7, 0)
      - 'gemini-3.8-pro' -> (3, 8, 0)
    """
    clean = re.sub(r"\b20\d{6}\b", "", slug)
    clean = re.sub(r"\[\w+\]", "", clean)
    matches = re.findall(r"(?<![a-zA-Z0-9])(\d+)(?:[.\-_](\d+))?(?:[.\-_](\d+))?(?![a-zA-Z0-9])", clean)
    best_ver = (0, 0, 0)
    for m in matches:
        v = tuple(int(x) if x else 0 for x in m)
        if v[0] < 100 and v > best_ver:
            best_ver = v
    return best_ver


def score_model_for_tier(slug: str, desc: str = "", tier: str = "standard") -> int:
    """
    Generic capability ranking function for assigning models to tiers:
    - Combines semantic version priority with capability family and descriptive keywords.
    - Eliminates any hardcoded model version checks.
    """
    text = f"{slug} {desc}".lower()
    ver = parse_semver(slug)
    ver_score = ver[0] * 100000 + ver[1] * 1000 + ver[2]

    fast_fam = sum(1 for k in FAST_FAMILIES if k in text)
    fast_kw = sum(1 for k in FAST_KEYWORDS if k in text)

    deep_fam = sum(1 for k in DEEP_FAMILIES if k in text)
    deep_kw = sum(1 for k in DEEP_KEYWORDS if k in text)

    std_fam = sum(1 for k in STANDARD_FAMILIES if k in text)
    std_kw = sum(1 for k in STANDARD_KEYWORDS if k in text)

    tier = tier.lower()
    score = ver_score
    if tier == "fast":
        score += fast_fam * 200000 + fast_kw * 30000
        score -= (deep_fam * 250000 + deep_kw * 40000)
        score -= (std_fam * 100000)
    elif tier == "deep":
        score += deep_fam * 200000 + deep_kw * 30000
        score -= (fast_fam * 250000 + fast_kw * 40000)
    else:  # standard
        score += std_fam * 200000 + std_kw * 30000
        score -= (fast_fam * 150000)
        score -= (deep_fam * 50000)
        if not fast_fam and not deep_fam and not std_fam:
            score += 15000
    return score


def rank_models_for_tier(models, tier: str = "standard") -> List[Tuple[int, str, str]]:
    """
    Rank any list of models (tuples, dicts, or strings) for a specific tier.
    Returns sorted list of (score, slug, desc).
    """
    scored = []
    for item in models:
        if isinstance(item, tuple):
            slug = item[0]
            desc = item[1] if len(item) > 1 else ""
        elif isinstance(item, dict):
            slug = item.get("slug") or item.get("id") or item.get("model") or ""
            desc = item.get("description") or item.get("name") or ""
        else:
            parts = str(item).split(None, 1)
            slug = parts[0]
            desc = parts[1] if len(parts) > 1 else ""
        if not slug:
            continue
        s = score_model_for_tier(slug, desc, tier)
        scored.append((s, slug, desc))
    scored.sort(key=lambda x: x[0], reverse=True)
    return scored


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
            models["codex"]["available"] = sorted(
                list(found),
                key=lambda x: (parse_semver(x.split()[0]), x),
                reverse=True
            )
            models["codex"]["source"] = "detected"
        if configured_model:
            models["codex"]["current_default"] = configured_model
            models["codex"]["default_source"] = "detected"
        elif found:
            ranked_std = rank_models_for_tier(list(found), "standard")
            if ranked_std:
                models["codex"]["current_default"] = ranked_std[0][1]
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
                    models["grok"]["available"] = sorted(
                        discovered,
                        key=lambda x: (parse_semver(x.split()[0]), x),
                        reverse=True
                    )
                    models["grok"]["source"] = "detected"
                    ranked_std = rank_models_for_tier(discovered, "standard")
                    if ranked_std:
                        models["grok"]["current_default"] = ranked_std[0][1]
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
                    claude_catalog = []
                    efforts_map = {}
                    for m in cfg_models:
                        mid = m.get("id", "")
                        if not mid:
                            continue
                        mname = m.get("name", "")
                        mdesc = m.get("description", "")
                        notice = (m.get("notice", {}).get("text") or "") if m.get("notice") else ""
                        efforts = [o.get("id") for o in m.get("thinking", {}).get("effort_options", [])]
                        efforts_map[mid] = efforts
                        claude_catalog.append((mid, f"{mname} {mdesc} {notice}"))

                    if claude_catalog:
                        ranked = rank_models_for_tier(claude_catalog, tier)
                        if ranked:
                            top_id = ranked[0][1]
                            alias = "fable" if "fable" in top_id else ("haiku" if "haiku" in top_id else ("sonnet" if "sonnet" in top_id else top_id))
                            efforts = efforts_map.get(top_id, [])
                            if tier == "deep":
                                effort = efforts[-1] if efforts else "max"
                            elif tier == "fast":
                                effort = "low"
                            else:
                                effort = "medium" if "medium" in efforts else ("high" if "high" in efforts else "medium")
                            return {"model": alias, "effort": effort, "is_dynamic": True, "full_id": top_id}
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

        effort = "max" if tier == "deep" else ("low" if tier == "fast" else "high")

        # If user explicitly configured a model in config.toml and requested standard tier, honor it
        if tier == "standard" and configured_default:
            return {"model": configured_default, "effort": effort, "is_dynamic": True}

        if discovered_models:
            ranked = rank_models_for_tier(discovered_models, tier)
            if ranked:
                return {"model": ranked[0][1], "effort": effort, "is_dynamic": True}

        fallback_models = {
            "fast": "gpt-6-luna",
            "deep": "gpt-6-astra",
            "standard": configured_default or "gpt-6.1-sol"
        }
        return {"model": fallback_models.get(tier, "gpt-6.1-sol"), "effort": effort, "is_dynamic": False}

    elif provider == "grok":
        discovered_grok = []
        try:
            grok_cache = Path.home() / ".grok" / "models_cache.json"
            if grok_cache.exists():
                g_data = json.loads(grok_cache.read_text(encoding="utf-8"))
                models_dict = g_data.get("models", {})
                for k, v in models_dict.items():
                    desc = v.get("description", "") if isinstance(v, dict) else ""
                    discovered_grok.append((k, desc))
        except Exception:
            pass

        effort = "high" if tier == "deep" else ("low" if tier == "fast" else "medium")
        if discovered_grok:
            ranked = rank_models_for_tier(discovered_grok, tier)
            if ranked:
                return {"model": ranked[0][1], "effort": effort, "is_dynamic": True}

        fallback = "grok-4.7-build-fast" if tier == "fast" else "grok-4.7"
        return {"model": fallback, "effort": effort, "is_dynamic": False}

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
