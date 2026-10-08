"""
Model discovery across AI subscription CLIs.

Each provider entry reports where its data came from:
- "source": "detected" when the list was read from the local CLI cache/config,
  otherwise "builtin" (makewand's hardcoded reference list, which may be stale);
- "default_source": the same for "current_default".
Callers must not present builtin values as detected versions.
"""

import logging
import os
import re
import json
from pathlib import Path
from typing import Dict, Any, List, Tuple, Optional

logger = logging.getLogger(__name__)

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


class DiscoveredAgyModels(tuple):
    """Container for discovered Antigravity / Gemini model catalog."""

    def __new__(cls, models: List[Tuple[str, str]], configured_default: Optional[str], configured_effort: Optional[str]):
        return super().__new__(cls, (models, configured_default, configured_effort))

    @property
    def models(self) -> List[Tuple[str, str]]:
        return self[0]

    @property
    def configured_default(self) -> Optional[str]:
        return self[1]

    @property
    def configured_effort(self) -> Optional[str]:
        return self[2]

    def __getitem__(self, item):
        if item == "models":
            return self[0]
        if item == "configured_default":
            return self[1]
        if item == "configured_effort":
            return self[2]
        if isinstance(item, str):
            raise KeyError(item)
        return super().__getitem__(item)

    def get(self, key, default=None):
        if key == "models":
            return self[0]
        if key == "configured_default":
            return self[1]
        if key == "configured_effort":
            return self[2]
        return default


def _discover_local_agy_models() -> DiscoveredAgyModels:
    """
    Scans local Gemini / Antigravity config and cache directories.
    Returns DiscoveredAgyModels(models, configured_default, configured_effort).
    """
    discovered_agy: List[Tuple[str, str]] = []
    seen = set()
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
                                if norm_slug and (norm_slug, desc) not in seen:
                                    seen.add((norm_slug, desc))
                                    discovered_agy.append((norm_slug, desc))
                            elif isinstance(m, str):
                                norm_slug, _ = _normalize_agy_model(m)
                                if norm_slug and (norm_slug, "") not in seen:
                                    seen.add((norm_slug, ""))
                                    discovered_agy.append((norm_slug, ""))
                    elif isinstance(items, dict):
                        for k, v in items.items():
                            norm_slug, _ = _normalize_agy_model(k)
                            if norm_slug:
                                desc = v.get("description", "") if isinstance(v, dict) else ""
                                if (norm_slug, desc) not in seen:
                                    seen.add((norm_slug, desc))
                                    discovered_agy.append((norm_slug, desc))
                except Exception:
                    pass

        for s_path in [
            base / "antigravity-cli" / "settings.json",
            base / "settings.json",
            base / "config" / "config.json",
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
                        if eff_val in ("low", "medium", "high", "max") and not configured_effort:
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
                                    if norm_val and (norm_val, desc) not in seen:
                                        seen.add((norm_val, desc))
                                        discovered_agy.append((norm_val, desc))
                except Exception:
                    pass

    # Check makewand agy models cache if no static models found yet
    if not discovered_agy:
        agy_cache_file = Path.home() / ".cache" / "makewand" / "agy_models_cache.json"
        if not agy_cache_file.exists():
            _sync_agy_models_cache(cache_file=agy_cache_file)
        if agy_cache_file.exists():
            try:
                cdata = json.loads(agy_cache_file.read_text(encoding="utf-8"))
                for m in cdata.get("models", []):
                    if isinstance(m, dict):
                        slug = m.get("id") or m.get("slug")
                        desc = m.get("description") or m.get("name") or ""
                        norm_slug, _ = _normalize_agy_model(slug or "")
                        if norm_slug and (norm_slug, desc) not in seen:
                            seen.add((norm_slug, desc))
                            discovered_agy.append((norm_slug, desc))
            except Exception:
                pass

    return DiscoveredAgyModels(discovered_agy, configured_default, configured_effort)


def _sync_agy_models_cache(cache_file: Optional[Path] = None, timeout: float = 4.0, max_age: float = 43200) -> Optional[Path]:
    """
    Syncs available models from `agy models` CLI if cache is absent or older than max_age (12h).
    Never executes in test isolation mode (MAKEWAND_TEST_ISOLATION_ROOT).
    """
    from makewand.config import is_provider_enabled
    if "MAKEWAND_TEST_ISOLATION_ROOT" in os.environ or not is_provider_enabled("agy"):
        return None
    import shutil
    import subprocess
    import tempfile
    import time
    if not shutil.which("agy"):
        return None

    if cache_file is None:
        cache_file = Path.home() / ".cache" / "makewand" / "agy_models_cache.json"

    try:
        if cache_file.exists():
            age = time.time() - cache_file.stat().st_mtime
            if age < max_age and cache_file.stat().st_size > 30:
                return cache_file
    except Exception:
        pass

    try:
        res = subprocess.run(
            ["agy", "models"],
            capture_output=True,
            text=True,
            timeout=timeout,
            cwd=tempfile.gettempdir(),
        )
        if res.returncode == 0 and res.stdout:
            models_data = []
            for line in res.stdout.splitlines():
                line = re.sub(r'^[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏\s]+Fetching available models\.\.\.', '', line).strip()
                line = re.sub(r'\x1b\[[0-9;]*m', '', line).strip()
                if not line:
                    continue
                parts = line.split(None, 1)
                if parts:
                    slug = parts[0]
                    desc = parts[1] if len(parts) > 1 else ""
                    if _valid_model_id(slug):
                        models_data.append({"id": slug, "description": desc})
            if models_data:
                cache_file.parent.mkdir(parents=True, exist_ok=True)
                tmp_file = cache_file.with_suffix(".tmp")
                tmp_file.write_text(json.dumps({"updated_at": time.time(), "models": models_data}, indent=2), encoding="utf-8")
                tmp_file.replace(cache_file)
                return cache_file
    except Exception:
        pass
    return None


_refresh_agy_models_cache = _sync_agy_models_cache


class DiscoveredMuseModels(tuple):
    """Container for discovered Muse Code model catalog."""

    def __new__(cls, models: List[Tuple[str, str]], configured_default: Optional[str], configured_effort: Optional[str]):
        return super().__new__(cls, (models, configured_default, configured_effort))

    @property
    def models(self) -> List[Tuple[str, str]]:
        return self[0]

    @property
    def configured_default(self) -> Optional[str]:
        return self[1]

    @property
    def configured_effort(self) -> Optional[str]:
        return self[2]

    def __getitem__(self, item):
        if item == "models":
            return self[0]
        if item == "configured_default":
            return self[1]
        if item == "configured_effort":
            return self[2]
        if isinstance(item, str):
            raise KeyError(item)
        return super().__getitem__(item)

    def get(self, key, default=None):
        if key == "models":
            return self[0]
        if key == "configured_default":
            return self[1]
        if key == "configured_effort":
            return self[2]
        return default


def _discover_local_muse_models() -> DiscoveredMuseModels:
    """
    Scans local Muse model-catalog and config directories.
    Returns DiscoveredMuseModels(models, configured_default, configured_effort).
    """
    discovered_muse: List[Tuple[str, str]] = []
    seen = set()
    configured_default = None
    configured_effort = None
    catalog_default = None

    # 1. Check official model catalog cache
    muse_cat_bases = []
    if os.environ.get("MUSE_DATA_DIR"):
        muse_cat_bases.append(Path(os.environ["MUSE_DATA_DIR"]).expanduser() / "model-catalog")
    if os.environ.get("XDG_DATA_HOME"):
        muse_cat_bases.append(Path(os.environ["XDG_DATA_HOME"]).expanduser() / "muse" / "model-catalog")
    muse_cat_bases.extend([
        Path.home() / ".local" / "share" / "muse" / "model-catalog",
        Path.home() / ".config" / "muse" / "model-catalog",
        Path.home() / ".muse" / "model-catalog",
    ])

    for cat_dir in muse_cat_bases:
        if not cat_dir.exists():
            continue
        try:
            for json_file in sorted(cat_dir.glob("*.json"), key=lambda f: f.stat().st_mtime, reverse=True):
                try:
                    cdata = json.loads(json_file.read_text(encoding="utf-8"))
                    rows = cdata.get("rows", [])
                    if isinstance(rows, list):
                        for r in rows:
                            if not isinstance(r, dict):
                                continue
                            if r.get("visibility") == "hidden":
                                continue
                            mid = r.get("model_id") or r.get("id") or r.get("slug")
                            if not _valid_model_id(mid):
                                continue
                            desc = r.get("description") or r.get("display_label") or ""
                            if (mid, desc) not in seen:
                                seen.add((mid, desc))
                                discovered_muse.append((mid, desc))
                            if (r.get("is_default") or r.get("is_current")) and not catalog_default:
                                catalog_default = mid
                except Exception:
                    pass
        except Exception:
            pass

    # 2. Check settings.json for user-configured model and effort
    for s_path in [
        Path.home() / ".config" / "muse" / "settings.json",
        Path.home() / ".muse" / "settings.json",
    ]:
        if s_path.exists():
            try:
                sdata = json.loads(s_path.read_text(encoding="utf-8"))
                raw_model = sdata.get("model") or sdata.get("default_model")
                if _valid_model_id(raw_model):
                    configured_default = raw_model.strip()
                raw_eff = sdata.get("reasoning_effort") or sdata.get("effort")
                if raw_eff and isinstance(raw_eff, str) and raw_eff.strip():
                    eff_val = raw_eff.strip().lower()
                    if eff_val in ("none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"):
                        configured_effort = eff_val
            except Exception:
                pass

    if not configured_default and catalog_default:
        configured_default = catalog_default

    return DiscoveredMuseModels(discovered_muse, configured_default, configured_effort)


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
    clean_no_params = re.sub(r"[:\-]\d+[bB]\b", "", clean)
    matches = re.findall(r"(?:(?<=[a-zA-Z])|(?<![a-zA-Z0-9]))(\d+)(?:[.\-_](\d+))?(?:[.\-_](\d+))?(?![a-zA-Z0-9])", clean_no_params)
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
        "muse": {"current_default": "muse-spark-1.3-contributor", "available": ["muse-spark-1.3-contributor", "muse-spark-1.3", "muse-spark-1.2-contributor", "muse-spark-1.2"]},
        "grok": {"current_default": "grok-4.7", "available": ["grok-4.7", "grok-4.7-build-fast", "grok-4.6", "grok-4.5"]},
        "local": {"current_default": "qwen2.5-coder:7b", "available": ["qwen2.5-coder:7b", "gemma4:31b", "gemma4:26b", "gemma4:e4b"]}
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
        if models["claude"]["source"] == "detected":
            cfg_default = None
            try:
                if claude_json.exists():
                    raw_cfg = json.loads(claude_json.read_text(encoding="utf-8"))
                    m_val = raw_cfg.get("model")
                    if _valid_model_id(m_val) and m_val.startswith("claude-"):
                        cfg_default = m_val
            except Exception:
                pass
            if cfg_default:
                models["claude"]["current_default"] = cfg_default
                models["claude"]["default_source"] = "detected"
            elif models["claude"]["available"]:
                ranked_std = rank_models_for_tier(models["claude"]["available"], "standard")
                if ranked_std:
                    models["claude"]["current_default"] = ranked_std[0][1]
                    models["claude"]["default_source"] = "detected"
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

    # Discover Muse models from official model catalog (~/.local/share/muse/model-catalog/*.json) and ~/.config/muse/settings.json
    try:
        muse_res = _discover_local_muse_models()
        if muse_res.models:
            muse_available = []
            for mid, desc in muse_res.models:
                lbl = f"{mid} ({desc})" if desc else mid
                muse_available.append(lbl)
            models["muse"]["available"] = sorted(
                muse_available,
                key=lambda x: (parse_semver(x.split()[0]), x),
                reverse=True
            )
            models["muse"]["source"] = "detected"

        if muse_res.configured_default:
            models["muse"]["current_default"] = muse_res.configured_default
            models["muse"]["default_source"] = "detected"
        elif muse_res.models:
            ranked_std = rank_models_for_tier(muse_res.models, "standard")
            if ranked_std:
                models["muse"]["current_default"] = ranked_std[0][1]
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
        agy_res = _discover_local_agy_models()
        found_agy = set()
        for norm_slug, desc in agy_res.models:
            found_agy.add(f"{norm_slug} ({desc})" if desc else norm_slug)

        if found_agy:
            models["agy"]["available"] = sorted(
                list(found_agy),
                key=lambda x: (parse_semver(x.split()[0]), x),
                reverse=True
            )
            models["agy"]["source"] = "detected"
        if agy_res.configured_default:
            models["agy"]["current_default"] = agy_res.configured_default
            models["agy"]["default_source"] = "detected"
        elif found_agy:
            ranked_std = rank_models_for_tier(list(found_agy), "standard")
            if ranked_std:
                models["agy"]["current_default"] = ranked_std[0][1]
                models["agy"]["default_source"] = "detected"
    except Exception:
        pass

    # Discover Local Self-Hosted (Ollama / vLLM) models
    try:
        from makewand.providers.local import is_local_model_available
        from makewand.config import is_provider_enabled
        avail, active, local_models = is_local_model_available(timeout=0.8) if is_provider_enabled("local") else (False, None, [])
        if avail and local_models:
            models["local"]["available"] = local_models
            models["local"]["source"] = "detected"
            if active:
                models["local"]["current_default"] = active
                models["local"]["default_source"] = "detected"
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
            return _tier_resolution("opus", "max", is_dynamic=False, full_id="claude-opus-4-20250514")
        elif tier == "fast":
            return _tier_resolution("haiku", "low", is_dynamic=False, full_id="claude-haiku-4-5-20251001")
        else:
            return _tier_resolution("sonnet", "medium", is_dynamic=False, full_id="claude-sonnet-4-20250514")

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
        muse_res = _discover_local_muse_models()
        discovered_muse = muse_res.models
        configured_default = muse_res.configured_default
        configured_effort = muse_res.configured_effort

        effort = configured_effort if (tier == "standard" and configured_effort) else ("max" if tier == "deep" else ("low" if tier == "fast" else "high"))

        # If user explicitly configured a model in settings.json and requested standard tier, honor it
        if tier == "standard" and configured_default:
            return _tier_resolution(configured_default, effort, is_dynamic=True,
                                    effort_source="detected" if configured_effort else "builtin")

        if discovered_muse:
            ranked = rank_models_for_tier(discovered_muse, tier)
            if ranked:
                return _tier_resolution(ranked[0][1], effort, is_dynamic=True,
                                        effort_source="detected" if configured_effort else "builtin")

        if configured_default:
            return _tier_resolution(configured_default, effort, is_dynamic=True,
                                    effort_source="detected" if configured_effort else "builtin")

        # Static fallback when no dynamic catalog is detected
        fallback_models = {
            "fast": "muse-spark-1.2",
            "deep": "muse-spark-1.3",
            "standard": "muse-spark-1.3-contributor",
        }
        return _tier_resolution(fallback_models.get(tier, "muse-spark-1.3-contributor"), effort, is_dynamic=False)

    elif provider == "agy":
        agy_res = _discover_local_agy_models()
        discovered_agy = agy_res.models
        configured_default = agy_res.configured_default
        configured_effort = agy_res.configured_effort

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

    elif provider == "gemini":
        gemini_models = {
            "fast": "gemini-2.5-flash",
            "standard": "gemini-2.5-flash",
            "deep": "gemini-2.5-pro",
        }
        effort = "high" if tier == "deep" else ("low" if tier == "fast" else "medium")
        return _tier_resolution(gemini_models.get(tier, "gemini-2.5-flash"), effort, is_dynamic=False, full_id=gemini_models.get(tier, "gemini-2.5-flash"))

    elif provider in ("local", "ollama"):
        try:
            from makewand.providers.local import is_local_model_available
            from makewand.config import is_provider_enabled
            avail, active, models_list = is_local_model_available(timeout=0.8) if is_provider_enabled("local") else (False, None, [])
            if avail:
                if tier == "standard" and active:
                    return _tier_resolution(active, "medium", is_dynamic=True, effort_source="builtin")
                if models_list:
                    ranked = rank_models_for_tier(models_list, tier)
                    target = ranked[0][1] if ranked else (active or models_list[0])
                    return _tier_resolution(target, "medium", is_dynamic=True, effort_source="builtin")
                if active:
                    return _tier_resolution(active, "medium", is_dynamic=True, effort_source="builtin")
        except Exception:
            pass
        return _tier_resolution("qwen2.5-coder:7b", "medium", is_dynamic=False, effort_source="builtin")

    return _tier_resolution("default", "medium", is_dynamic=False)


# Backward-compatible and convenience alias
resolve_model_and_effort = get_provider_model_tier


def export_routing_overrides(config_dir: Optional[Path] = None, target_filename: str = "discovered.json") -> Optional[Path]:
    """
    Exports dynamically detected models to Go router's <config_dir>/discovered.json.
    Ensures single source of truth across Python CLI, Go server, and TUI.
    Never overwrites user-managed routing.json.
    Strictly conforms to Go router's rawDefaults JSON schema with valid cost entries.
    Uses atomic write via temporary file replacement.
    """
    if config_dir is None:
        env_dir = os.environ.get("MAKEWAND_CONFIG_DIR")
        config_dir = Path(env_dir).expanduser() if env_dir else Path.home() / ".config" / "makewand"
    else:
        config_dir = Path(config_dir).expanduser()

    # If in test isolation mode and config_dir was not explicitly given, use test root
    if "MAKEWAND_TEST_ISOLATION_ROOT" in os.environ and not os.environ.get("MAKEWAND_CONFIG_DIR") and config_dir == Path.home() / ".config" / "makewand":
        iso_root = Path(os.environ["MAKEWAND_TEST_ISOLATION_ROOT"]).expanduser()
        config_dir = iso_root / ".config" / "makewand"

    target_file = config_dir / target_filename

    # 1. Read existing target_file if present to preserve custom strategies/costs.
    # Refuse to overwrite if target_file is corrupt/malformed to prevent data loss.
    existing_data: Dict[str, Any] = {}
    if target_file.exists():
        try:
            existing_data = json.loads(target_file.read_text(encoding="utf-8"))
            if not isinstance(existing_data, dict):
                logger.warning("Existing %s is not a dictionary; aborting export to prevent data loss", target_file)
                return None
        except Exception as e:
            logger.warning("Failed to parse existing %s (%s); aborting export to prevent data loss", target_file, e)
            return None

    models_map = existing_data.get("models") if isinstance(existing_data.get("models"), dict) else {}
    costs_map = existing_data.get("costs") if isinstance(existing_data.get("costs"), dict) else {}

    # 2. Map provider tier resolutions using canonical model IDs rather than CLI shortcuts
    providers_to_sync = ["claude", "codex", "gemini", "agy", "muse", "grok", "local"]
    for prov in providers_to_sync:
        prov_key = prov
        try:
            res_cheap = get_provider_model_tier(prov, "fast")
            res_mid = get_provider_model_tier(prov, "standard")
            res_prem = get_provider_model_tier(prov, "deep")

            cheap_m = res_cheap.get("full_id") or res_cheap.get("model")
            mid_m = res_mid.get("full_id") or res_mid.get("model")
            prem_m = res_prem.get("full_id") or res_prem.get("model")

            p_models = models_map.get(prov_key, {})
            if not isinstance(p_models, dict):
                p_models = {}
            if cheap_m:
                p_models["cheap"] = cheap_m
            if mid_m:
                p_models["mid"] = mid_m
            if prem_m:
                p_models["premium"] = prem_m
            models_map[prov_key] = p_models
        except Exception:
            pass

    # 3. Pricing table protection: preserve explicit valid costs (including explicit 0.0),
    # but do NOT inject unconfigured models at 0.0 which would wipe out Go router's price table.
    filtered_costs: Dict[str, Any] = {}
    for mid, c_val in costs_map.items():
        if isinstance(c_val, dict):
            inp = c_val.get("input")
            out = c_val.get("output")
            if isinstance(inp, (int, float)) and isinstance(out, (int, float)) and inp >= 0 and out >= 0:
                filtered_costs[mid] = c_val
    costs_map = filtered_costs

    # 4. Construct payload strictly conforming to rawDefaults
    payload: Dict[str, Any] = {
        "models": models_map,
        "costs": costs_map,
    }
    # Preserve optional tables if present in existing_data
    for opt_key in ("strategies", "build_strategies", "context_budgets", "power_ensemble"):
        if opt_key in existing_data and isinstance(existing_data[opt_key], dict):
            payload[opt_key] = existing_data[opt_key]

    # 5. Atomic write
    try:
        config_dir.mkdir(parents=True, exist_ok=True)
        tmp_file = config_dir / f".{target_filename}.tmp.{os.getpid()}"
        tmp_file.write_text(json.dumps(payload, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
        tmp_file.replace(target_file)
        return target_file
    except Exception as e:
        logger.warning("Failed to atomically write %s: %s", target_file, e)
        return None

