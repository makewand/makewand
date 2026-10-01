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
from typing import Dict, Any, List, Tuple, Optional

# Generic capability & family keywords for zero-hardcoding model ranking
FAST_FAMILIES = ["haiku", "luna", "flash", "mini", "nano", "reserve"]
FAST_KEYWORDS = ["fast", "fastest", "speed", "rapid", "lightweight", "affordable", "easier", "quick", "turbo", "efficient"]

DEEP_FAMILIES = ["opus", "fable", "mythos", "astra", "ultra", "frontier"]
DEEP_KEYWORDS = ["demanding", "toughest", "most capable", "complex", "flagship", "pro-high", "deep"]

STANDARD_FAMILIES = ["sonnet", "sol", "standard"]
STANDARD_KEYWORDS = ["workhorse", "balanced", "everyday", "general"]


def _valid_model_id(value):
    return isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:/\[\]-]{0,199}", value.strip()) is not None


def _normalize_agy_model(raw: str) -> Tuple[str, Optional[str]]:
    """
    Extracts canonical model ID and effort from Antigravity/Gemini model string.
    E.g.:
      'Gemini 3.8 Flash (High)' -> ('gemini-3.8-flash', 'high')
      'gemini-3.8-pro' -> ('gemini-3.8-pro', None)
      'gemini-3.1-pro-high' -> ('gemini-3.1-pro', 'high')
    """
    if not isinstance(raw, str):
        return "", None
    val = raw.strip()
    if not val or val.startswith("-") or re.search(r"[\r\n\t\x00-\x1f]", val):
        return "", None

    effort = None
    m_eff = re.search(r"\((\w+)\)", val)
    if m_eff:
        eff_candidate = m_eff.group(1).lower()
        if eff_candidate in ("low", "medium", "high", "max"):
            effort = eff_candidate
        val = re.sub(r"\s*\(\w+\)\s*", "", val).strip()

    if effort is None:
        for eff in ("high", "medium", "low", "max"):
            if val.lower().endswith(f"-{eff}"):
                effort = eff
                val = val[:-len(eff) - 1]
                break

    if " " in val:
        slug = re.sub(r"\s+", "-", val).lower()
    else:
        slug = val.lower()

    if not _valid_model_id(slug):
        return "", None

    return slug, effort




def _tier_resolution(model, effort, *, is_dynamic, full_id=None, effort_source="builtin"):
    """Model provenance and effort provenance are independent facts.

    Builtin effort settings preserve historical defaults; they do not prove
    that a newly discovered commercial model supports those capabilities.
    """
    result = {"model": model.strip(), "effort": effort, "is_dynamic": is_dynamic,
              "source": "detected" if is_dynamic else "builtin", "effort_source": effort_source}
    if full_id is not None:
        result["full_id"] = full_id.strip()
    return result


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
        "claude": {"current_default": "claude-sonnet-5 (Sonnet 5)", "available": ["claude-fable-5-1[1m] (Fable 5.1)", "claude-opus-5-5 (Opus 5.5)", "claude-sonnet-5 (Sonnet 5)", "claude-haiku-4-5-20251001 (Haiku 4.5)"]},
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
        cat_dir = Path(os.environ.get("CLAUDE_CONFIG_DIR") or Path.home() / ".claude").expanduser() / "cache" / "model-catalog"
        if cat_dir.exists():
            json_files = sorted(cat_dir.glob("*.json"), key=lambda f: f.stat().st_mtime, reverse=True)
            if json_files:
                cat_data = json.loads(json_files[0].read_text(encoding="utf-8"))
                cfg_models = cat_data.get("catalog", {}).get("config", {}).get("models", [])
                catalog_found = []
                for m in cfg_models:
                    if not isinstance(m, dict):
                        continue
                    mid = m.get("id")
                    mname = m.get("name")
                    if not _valid_model_id(mid):
                        continue
                    lbl = f"{mid} ({mname})" if isinstance(mname, str) and mname.strip() else mid
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
                if _valid_model_id(m):
                    found.add(m.strip())
            for m in data.get("additionalModelOptionsCache", []):
                val = m.get("value")
                lbl = m.get("label")
                if _valid_model_id(val) and val.startswith("claude-"):
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
            codex_bases.append(Path(os.environ["CODEX_HOME"]).expanduser())
        else:
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
                        if not isinstance(m, dict):
                            continue
                        slug = m.get("slug") or m.get("id")
                        desc = m.get("description", "")
                        if _valid_model_id(slug):
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
                                if _valid_model_id(name):
                                    found.add(name)
                    for m in re.findall(r'model\s*=\s*"([^"]+)"', raw):
                        if _valid_model_id(m):
                            found.add(m.strip())
                    if not configured_model:
                        cfg_m = re.search(r'^\s*model\s*=\s*"([^"]+)"', raw, re.MULTILINE)
                        if cfg_m and _valid_model_id(cfg_m.group(1)):
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
            if _valid_model_id(data.get("model")):
                models["muse"]["current_default"] = data["model"].strip()
                models["muse"]["default_source"] = "detected"
    except Exception:
        pass

    # Discover Grok models from ~/.grok/models_cache.json
    try:
        grok_cache = Path.home() / ".grok" / "models_cache.json"
        if grok_cache.exists():
            g_data = json.loads(grok_cache.read_text(encoding="utf-8"))
            if "models" in g_data and isinstance(g_data["models"], dict):
                discovered = [model for model in g_data["models"] if _valid_model_id(model)]
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

    # Discover Antigravity / Gemini models from ~/.gemini
    try:
        gemini_bases = []
        if os.environ.get("GEMINI_CONFIG_DIR"):
            gemini_bases.append(Path(os.environ["GEMINI_CONFIG_DIR"]).expanduser())
        elif os.environ.get("AGY_CONFIG_DIR"):
            gemini_bases.append(Path(os.environ["AGY_CONFIG_DIR"]).expanduser())
        else:
            gemini_bases.extend([
                Path.home() / ".gemini",
                Path.home() / ".config" / "gemini",
                Path.home() / ".config" / "antigravity",
            ])

        found_agy = set()
        configured_agy_default = None

        for base in gemini_bases:
            if not base.exists():
                continue

            # 1. Official / catalog caches
            for cache_name in ("models_cache.json", "model_catalog.json"):
                cache_file = base / cache_name
                if not cache_file.exists():
                    cache_file = base / "antigravity-cli" / cache_name
                if cache_file.exists():
                    try:
                        cdata = json.loads(cache_file.read_text(encoding="utf-8"))
                        items = cdata.get("models") or cdata.get("catalog", {}).get("models", [])
                        if isinstance(items, list):
                            for m in items:
                                if isinstance(m, dict):
                                    slug = m.get("id") or m.get("slug")
                                    desc = m.get("description") or m.get("name") or ""
                                    norm_slug, _ = _normalize_agy_model(slug or "")
                                    if norm_slug:
                                        found_agy.add(f"{norm_slug} ({desc})" if desc else norm_slug)
                                elif isinstance(m, str):
                                    norm_slug, _ = _normalize_agy_model(m)
                                    if norm_slug:
                                        found_agy.add(norm_slug)
                        elif isinstance(items, dict):
                            for k, v in items.items():
                                norm_slug, _ = _normalize_agy_model(k)
                                if norm_slug:
                                    desc = v.get("description", "") if isinstance(v, dict) else ""
                                    found_agy.add(f"{norm_slug} ({desc})" if desc else norm_slug)
                    except Exception:
                        pass

            # 2. Config files
            for s_path in [
                base / "antigravity-cli" / "settings.json",
                base / "settings.json",
                base / "config" / "config.json"
            ]:
                if s_path.exists():
                    try:
                        sdata = json.loads(s_path.read_text(encoding="utf-8"))
                        raw_m = sdata.get("model") or sdata.get("default_model")
                        if raw_m and isinstance(raw_m, str):
                            norm_m, _ = _normalize_agy_model(raw_m)
                            if norm_m:
                                if not configured_agy_default:
                                    configured_agy_default = norm_m
                                found_agy.add(norm_m)
                        # Also check model lists if present
                        for key in ("available_models", "models", "additionalModelOptionsCache"):
                            m_list = sdata.get(key)
                            if isinstance(m_list, list):
                                for item in m_list:
                                    val = item.get("value") or item.get("id") if isinstance(item, dict) else item
                                    if isinstance(val, str):
                                        norm_val, _ = _normalize_agy_model(val)
                                        if norm_val:
                                            found_agy.add(norm_val)
                    except Exception:
                        pass

        if found_agy:
            models["agy"]["available"] = sorted(
                list(found_agy),
                key=lambda x: (parse_semver(x.split()[0]), x),
                reverse=True
            )
            models["agy"]["source"] = "detected"
        if configured_agy_default:
            models["agy"]["current_default"] = configured_agy_default
            models["agy"]["default_source"] = "detected"
        elif found_agy:
            ranked_std = rank_models_for_tier(list(found_agy), "standard")
            if ranked_std:
                models["agy"]["current_default"] = ranked_std[0][1]
                models["agy"]["default_source"] = "detected"
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
            cat_dir = Path(os.environ.get("CLAUDE_CONFIG_DIR") or Path.home() / ".claude").expanduser() / "cache" / "model-catalog"
            if cat_dir.exists():
                files = sorted(cat_dir.glob("*.json"), key=lambda f: f.stat().st_mtime, reverse=True)
                if files:
                    cat_data = json.loads(files[0].read_text(encoding="utf-8"))
                    cfg_models = cat_data.get("catalog", {}).get("config", {}).get("models", [])
                    claude_catalog = []
                    efforts_map = {}
                    for m in cfg_models:
                        if not isinstance(m, dict):
                            continue
                        mid = m.get("id", "")
                        if not _valid_model_id(mid):
                            continue
                        mname = m.get("name", "")
                        mdesc = m.get("description", "")
                        notice = (m.get("notice", {}).get("text") or "") if m.get("notice") else ""
                        thinking = m.get("thinking") if isinstance(m.get("thinking"), dict) else {}
                        efforts = [o.get("id") for o in thinking.get("effort_options", [])
                                   if isinstance(o, dict) and isinstance(o.get("id"), str)
                                   and re.fullmatch(r"[a-z][a-z0-9_-]{0,31}", o["id"])]
                        efforts_map[mid] = efforts
                        claude_catalog.append((mid, f"{mname} {mdesc} {notice}"))

                    if claude_catalog:
                        ranked = rank_models_for_tier(claude_catalog, tier)
                        if ranked:
                            top_id = ranked[0][1]
                            alias = top_id  # Do not invent an alias absent from the selected catalog.
                            efforts = efforts_map.get(top_id, [])
                            if tier == "deep":
                                effort = efforts[-1] if efforts else "max"
                            elif tier == "fast":
                                effort = "low"
                            else:
                                effort = "medium" if "medium" in efforts else ("high" if "high" in efforts else "medium")
                            # Only a listed effort is detected. A static tier
                            # preference never implies a model capability.
                            if efforts and effort not in efforts:
                                effort = efforts[0] if tier == "fast" else efforts[-1]
                            return _tier_resolution(alias, effort, is_dynamic=True, full_id=top_id,
                                                    effort_source="detected" if efforts else "builtin")
        except Exception:
            pass

        # Default fallback if catalog unavailable
        if tier == "deep":
            return _tier_resolution("fable", "max", is_dynamic=False, full_id="claude-fable-5-1")
        elif tier == "fast":
            return _tier_resolution("haiku", "low", is_dynamic=False, full_id="claude-haiku-4-5-20251001")
        else:
            return _tier_resolution("sonnet", "medium", is_dynamic=False, full_id="claude-sonnet-5")

    elif provider == "codex":
        discovered_models = []
        configured_default = None
        configured_effort = None

        codex_bases = []
        if os.environ.get("CODEX_HOME"):
            codex_bases.append(Path(os.environ["CODEX_HOME"]).expanduser())
        else:
            codex_bases.extend([Path.home() / ".codex", Path.home() / ".codex-2"])

        for base in codex_bases:
            if not base.exists():
                continue
            cache_file = base / "models_cache.json"
            if cache_file.exists() and not discovered_models:
                try:
                    cdata = json.loads(cache_file.read_text(encoding="utf-8"))
                    for m in cdata.get("models", []):
                        if not isinstance(m, dict):
                            continue
                        slug = m.get("slug") or m.get("id")
                        if _valid_model_id(slug):
                            discovered_models.append((slug, m.get("description", "")))
                except Exception:
                    pass
            cfg_file = base / "config.toml"
            if cfg_file.exists():
                try:
                    raw = cfg_file.read_text(encoding="utf-8")
                    if not configured_default:
                        m = re.search(r'^\s*model\s*=\s*"([^"]+)"', raw, re.MULTILINE)
                        if m and _valid_model_id(m.group(1)):
                            configured_default = m.group(1).strip()
                    m_eff = re.search(r'^\s*model_reasoning_effort\s*=\s*"([^"]+)"', raw, re.MULTILINE)
                    if m_eff:
                        configured_effort = m_eff.group(1).strip()
                except Exception:
                    pass

        effort = configured_effort if (tier == "standard" and configured_effort) else ("max" if tier == "deep" else ("low" if tier == "fast" else "high"))

        # If user explicitly configured a model in config.toml and requested standard tier, honor it
        if tier == "standard" and configured_default:
            return _tier_resolution(configured_default, effort, is_dynamic=True)

        if discovered_models:
            ranked = rank_models_for_tier(discovered_models, tier)
            if ranked:
                return _tier_resolution(ranked[0][1], effort, is_dynamic=True)

        fallback_models = {
            "fast": "gpt-6-luna",
            "deep": "gpt-6-astra",
            "standard": configured_default or "gpt-6.1-sol"
        }
        return _tier_resolution(fallback_models.get(tier, "gpt-6.1-sol"), effort, is_dynamic=False)

    elif provider == "grok":
        discovered_grok = []
        try:
            grok_cache = Path.home() / ".grok" / "models_cache.json"
            if grok_cache.exists():
                g_data = json.loads(grok_cache.read_text(encoding="utf-8"))
                models_dict = g_data.get("models", {})
                for k, v in models_dict.items():
                    if not _valid_model_id(k):
                        continue
                    desc = v.get("description", "") if isinstance(v, dict) else ""
                    discovered_grok.append((k, desc))
        except Exception:
            pass

        effort = "high" if tier == "deep" else ("low" if tier == "fast" else "medium")
        if discovered_grok:
            ranked = rank_models_for_tier(discovered_grok, tier)
            if ranked:
                return _tier_resolution(ranked[0][1], effort, is_dynamic=True)

        fallback = "grok-4.7-build-fast" if tier == "fast" else "grok-4.7"
        return _tier_resolution(fallback, effort, is_dynamic=False)

    elif provider == "muse":
        model_name = "muse-spark-1.3-contributor"
        detected = False
        configured_effort = None
        try:
            muse_settings = Path.home() / ".config" / "muse" / "settings.json"
            if muse_settings.exists():
                data = json.loads(muse_settings.read_text(encoding="utf-8"))
                if _valid_model_id(data.get("model")):
                    model_name = data["model"].strip()
                    detected = True
                cfg_effort = data.get("reasoning_effort") or data.get("effort")
                if cfg_effort and isinstance(cfg_effort, str):
                    configured_effort = cfg_effort.strip()
        except Exception:
            pass
        effort = configured_effort if (tier == "standard" and configured_effort) else ("xhigh" if tier == "deep" else ("high" if tier == "standard" else "low"))
        return _tier_resolution(model_name, effort, is_dynamic=detected)

    elif provider == "agy":
        discovered_agy = []
        configured_default = None
        configured_effort = None

        gemini_bases = []
        if os.environ.get("GEMINI_CONFIG_DIR"):
            gemini_bases.append(Path(os.environ["GEMINI_CONFIG_DIR"]).expanduser())
        elif os.environ.get("AGY_CONFIG_DIR"):
            gemini_bases.append(Path(os.environ["AGY_CONFIG_DIR"]).expanduser())
        else:
            gemini_bases.extend([
                Path.home() / ".gemini",
                Path.home() / ".config" / "gemini",
                Path.home() / ".config" / "antigravity",
            ])

        for base in gemini_bases:
            if not base.exists():
                continue

            for cache_name in ("models_cache.json", "model_catalog.json"):
                cache_file = base / cache_name
                if not cache_file.exists():
                    cache_file = base / "antigravity-cli" / cache_name
                if cache_file.exists() and not discovered_agy:
                    try:
                        cdata = json.loads(cache_file.read_text(encoding="utf-8"))
                        items = cdata.get("models") or cdata.get("catalog", {}).get("models", [])
                        if isinstance(items, list):
                            for m in items:
                                if isinstance(m, dict):
                                    slug = m.get("id") or m.get("slug")
                                    desc = m.get("description") or m.get("name") or ""
                                    norm_slug, _ = _normalize_agy_model(slug or "")
                                    if norm_slug:
                                        discovered_agy.append((norm_slug, desc))
                                elif isinstance(m, str):
                                    norm_slug, _ = _normalize_agy_model(m)
                                    if norm_slug:
                                        discovered_agy.append((norm_slug, ""))
                        elif isinstance(items, dict):
                            for k, v in items.items():
                                norm_slug, _ = _normalize_agy_model(k)
                                if norm_slug:
                                    desc = v.get("description", "") if isinstance(v, dict) else ""
                                    discovered_agy.append((norm_slug, desc))
                    except Exception:
                        pass

            for s_path in [
                base / "antigravity-cli" / "settings.json",
                base / "settings.json",
                base / "config" / "config.json"
            ]:
                if s_path.exists():
                    try:
                        sdata = json.loads(s_path.read_text(encoding="utf-8"))
                        raw_model = sdata.get("model") or sdata.get("default_model")
                        if raw_model and isinstance(raw_model, str):
                            norm_model, eff_extracted = _normalize_agy_model(raw_model)
                            if norm_model:
                                if not configured_default:
                                    configured_default = norm_model
                                if eff_extracted and not configured_effort:
                                    configured_effort = eff_extracted
                        raw_eff = sdata.get("reasoning_effort") or sdata.get("effort")
                        if raw_eff and isinstance(raw_eff, str) and raw_eff.strip():
                            eff_val = raw_eff.strip().lower()
                            if eff_val in ("low", "medium", "high", "max"):
                                configured_effort = eff_val
                        # Also check model lists if present in settings.json
                        for key in ("available_models", "models", "additionalModelOptionsCache"):
                            m_list = sdata.get(key)
                            if isinstance(m_list, list):
                                for item in m_list:
                                    val = item.get("value") or item.get("id") or item.get("slug") if isinstance(item, dict) else item
                                    desc = item.get("description") or item.get("name") or "" if isinstance(item, dict) else ""
                                    if isinstance(val, str):
                                        norm_val, _ = _normalize_agy_model(val)
                                        if norm_val:
                                            discovered_agy.append((norm_val, desc))
                    except Exception:
                        pass

        effort = configured_effort if (tier == "standard" and configured_effort) else ("max" if tier == "deep" else ("low" if tier == "fast" else "high"))

        if tier == "standard" and configured_default:
            return _tier_resolution(configured_default, effort, is_dynamic=True,
                                    effort_source="detected" if configured_effort else "builtin")

        if discovered_agy:
            ranked = rank_models_for_tier(discovered_agy, tier)
            if ranked:
                return _tier_resolution(ranked[0][1], effort, is_dynamic=True,
                                        effort_source="detected" if configured_effort else "builtin")

        # Static fallback when no dynamic catalog is detected
        fallback_models = {
            "fast": "gemini-3.8-flash-high",
            "deep": "gemini-3.1-pro-high",
            "standard": configured_default or "gemini-3.8-flash-high",
        }
        fallback_effort = "high" if tier in ("deep", "standard") else "low"
        return _tier_resolution(fallback_models.get(tier, "gemini-3.8-flash-high"), fallback_effort, is_dynamic=False)

    return _tier_resolution("default", "medium", is_dynamic=False)


# Backward-compatible and convenience alias
resolve_model_and_effort = get_provider_model_tier
