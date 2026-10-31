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
from datetime import datetime, timezone
from pathlib import Path
from typing import Dict, Any, List, Tuple, Optional

logger = logging.getLogger(__name__)

# Generic capability & family keywords for zero-hardcoding model ranking
FAST_FAMILIES = ["haiku", "luna", "flash", "mini", "nano", "reserve"]
FAST_KEYWORDS = ["fast", "fastest", "speed", "rapid", "lightweight", "affordable", "easier", "quick", "turbo", "efficient"]

DEEP_FAMILIES = ["opus", "fable", "mythos", "astra", "ultra", "pro", "wonder"]
# "frontier" is marketing prose ("SpaceXAI's latest frontier model"), not a model
# family: it may favour a model for deep work but must not push it out of the
# standard tier the way a deep family name does.
DEEP_KEYWORDS = ["demanding", "toughest", "most capable", "complex", "flagship", "frontier", "pro-high", "deep", "thinking", "reasoning", "max-effort"]
# The deep wording that places a model at the top of its line (rather than saying
# it can reason). In the cheap tier it marks the model as premium, like a family.
PREMIUM_KEYWORDS = ["demanding", "toughest", "most capable", "flagship", "frontier", "max-effort"]

STANDARD_FAMILIES = ["sonnet", "sol", "standard"]
STANDARD_KEYWORDS = ["workhorse", "balanced", "everyday", "general"]

# Price statements in a description ("Fast variant. 2x the price.") describe what
# a model costs, not how capable it is. A surcharged variant is never the cheap
# (fast) or everyday (standard) choice; a discounted one counts as affordable.
# Price words carry the signal on their own. "Usage", "credits" and "rate" also
# describe allowances ("4x the usage of sol", "2x the rate limits"), so they count
# only where the model is said to consume them: "(2x usage)", "uses credits 2x
# faster", "consumes credits at 2x the rate".
_PRICE_FACTOR = r"(?:\d+(?:\.\d+)?\s*[x×](?!\w)|[x×]\s*\d+(?:\.\d+)?(?![\w.])|\d+(?:\.\d+)?\s+times\b|double|twice|triple|thrice|half)"
_PRICE_FACTOR_RES = tuple(re.compile(p, re.IGNORECASE) for p in (
    # "2x the price", "x2 price", "200% price", "+100% price", "2x per-token cost", "twice the standard price"
    r"(?<![\w.+])(?P<factor>" + _PRICE_FACTOR + r"|\+?\d+(?:\.\d+)?\s*%)\s+"
    r"(?:(?:the|per[-\s]?token|token|input|output|api|base|list|standard|normal|regular|usual)\s+){0,3}"
    r"(?:prices?|pricing|costs?|billing)\b",
    # "costs 2x", "priced at 2x", "billed at twice the rate", "costs twice as much", "price: 2x", "counts as 2x"
    r"\b(?:costs|priced\s+at|billed\s+at|charged\s+at|counts?(?:\s+as)?|(?:price|pricing|cost)\s*:)\s*"
    r"(?:about\s+|roughly\s+|around\s+|nearly\s+)?(?P<factor>" + _PRICE_FACTOR + r")(?!\s+(?:less|cheaper|lower|fewer)\b)",
    # "(2x usage)", but not "4x the usage of", "5x more usage" or "2x usage limits"
    r"(?<![\w.+])(?P<factor>" + _PRICE_FACTOR + r")\s+(?:usage|credits?)\b(?!\s+(?:limits?|caps?|allowance|quota)\b)",
    # "uses 2x the credits", "burns twice as many tokens": the model consumes them
    r"\b(?:uses?|consumes?|burns?)\s+(?:about\s+|roughly\s+|around\s+|nearly\s+)?(?P<factor>" + _PRICE_FACTOR + r")\s+"
    r"(?:the\s+|as\s+many\s+|as\s+much\s+)?(?:usage|credits?|quota|tokens)\b",
    # "consumes credits at 2x the rate", but not "2x the rate limits"
    r"\b(?:uses?|consumes?|burns?|draws?|counts?)\b[^.;:]{0,40}?\bat\s+(?P<factor>" + _PRICE_FACTOR + r")\s+"
    r"(?:the\s+)?(?:normal\s+|standard\s+|base\s+|usual\s+|regular\s+)?rate\b(?!\s+limits?\b)",
))
_PRICE_PREMIUM_RE = re.compile(
    r"(?<!\bless\s)(?<!\bleast\s)(?<!\bnot\s)\bexpensive\b|\bpricier\b|\bcostlier\b|\bmore\s+costly\b"
    r"|\bcosts\s+(?:\d+(?:\.\d+)?\s*%\s+)?more\b|\bpremium[-\s](?:price|pricing|rates?)\b"
    r"|\b(?:higher|increased)[-\s](?:per[-\s]?token\s+|token\s+)?(?:price|cost|pricing)\b"
    r"|\b(?:draws?|uses?|consumes?|burns?)\s+(?:down\s+|through\s+)?(?:your\s+)?(?:usage|quota|credits|limits?)\s+(?:\S+\s+){0,2}?faster\b"
    r"|\bincreased\s+(?:usage|consumption)\b(?!\s+(?:limits?|caps?|allowance|quota)\b)",
    re.IGNORECASE)
_PRICE_DISCOUNT_RE = re.compile(
    r"\bcheaper\b|\b(?:less|least)\s+expensive\b|\binexpensive\b|\bcosts\s+(?:\d+(?:\.\d+)?\s*%\s+)?less\b"
    r"|\blower[-\s](?:price|cost|pricing)\b|\bdiscounted\b|(?<![\w.])\d+(?:\.\d+)?\s*%\s+off\b",
    re.IGNORECASE)
_PRICE_WORD_FACTORS = {"double": 2.0, "twice": 2.0, "triple": 3.0, "thrice": 3.0, "half": 0.5}
# "Fast variant", "turbo version": a speed-tuned variant of another model, ranked
# like a fast family (it is not the everyday or the strongest model). "Supports
# fast mode" names a feature of the model, not a variant, and does not count.
_SPEED_VARIANT_RE = re.compile(r"\b(?:fast|faster|turbo|speed|quick|lite|light)\s+(?:variant|version)\b",
                               re.IGNORECASE)
# Larger than any version or keyword score: in the cheap tier a premium model
# ranks below every model that is not marked premium, however much newer it is.
_PREMIUM_IN_FAST_TIER_PENALTY = 10 ** 9


def _price_factor(expression: str) -> float:
    """Multiplier named by a price expression: '2x', 'x2', '3 times', '200%', '+100%', 'twice', 'half'."""
    expression = expression.strip().lower()
    if expression in _PRICE_WORD_FACTORS:
        return _PRICE_WORD_FACTORS[expression]
    number = float(re.search(r"\d+(?:\.\d+)?", expression).group())
    if expression.endswith("%"):
        return 1 + number / 100 if expression.startswith("+") else number / 100
    return number


def _price_signal(text: str) -> int:
    """+1 when the text says the model costs more than its peers, -1 when less, else 0."""
    factors = [_price_factor(match.group("factor")) for regex in _PRICE_FACTOR_RES for match in regex.finditer(text)]
    if any(factor > 1 for factor in factors) or _PRICE_PREMIUM_RE.search(text):
        return 1
    if any(factor < 1 for factor in factors) or _PRICE_DISCOUNT_RE.search(text):
        return -1
    return 0


def _without_price_statements(text: str) -> str:
    """The text with its price statements blanked out: the words of "2x the
    standard price" describe a price, not a model family or tier."""
    for regex in _PRICE_FACTOR_RES + (_PRICE_PREMIUM_RE, _PRICE_DISCOUNT_RE):
        text = regex.sub(lambda match: " " * len(match.group(0)), text)
    return text


def _matches_token(word: str, text: str) -> bool:
    """Matches a keyword only on token/word boundaries to avoid false positives (e.g. 'mini' inside 'gemini')."""
    return bool(re.search(r'(?<![a-z0-9])' + re.escape(word.lower()) + r'(?![a-z0-9])', text.lower()))


_EFFORT_ORDER = ("minimal", "low", "medium", "high", "xhigh", "max", "ultra")


def _codex_model_efforts(model: Optional[str]) -> Optional[List[str]]:
    """Reasoning efforts Codex lists for `model` in its own models_cache.json."""
    if not model:
        return None
    base = Path(os.environ["CODEX_HOME"]).expanduser() if os.environ.get("CODEX_HOME") else Path.home() / ".codex"
    try:
        data = json.loads((base / "models_cache.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    for m in data.get("models", []) if isinstance(data, dict) else []:
        if isinstance(m, dict) and (m.get("slug") or m.get("id")) == model:
            levels = m.get("supported_reasoning_levels") or []
            found = [str(lv.get("effort") if isinstance(lv, dict) else lv).strip().lower() for lv in levels]
            return [e for e in found if e in _EFFORT_ORDER] or None
    return None


def normalize_provider_effort(provider: str, effort: Optional[str], model: Optional[str] = None) -> Optional[str]:
    """
    Translates canonical effort ('low', 'medium', 'high', 'xhigh', 'max')
    into the specific parameter dialect accepted by the target CLI provider.
    """
    if not effort or effort in ("none", "default"):
        return None
    eff = str(effort).strip().lower()

    if provider == "codex":
        # Codex efforts are model-specific (models_cache.json: gpt-6-astra
        # low..ultra, gpt-5.5 up to xhigh). Clamp to the highest level the model
        # lists that does not exceed the request; pass through when unknown.
        if eff not in _EFFORT_ORDER:
            return "medium"
        supported = _codex_model_efforts(model)
        if not supported:
            return eff
        rank = _EFFORT_ORDER.index(eff)
        allowed = [e for e in supported if _EFFORT_ORDER.index(e) <= rank]
        if allowed:
            return max(allowed, key=_EFFORT_ORDER.index)
        return min(supported, key=_EFFORT_ORDER.index)

    elif provider == "grok":
        # Grok CLI accepts: low, medium, high, xhigh
        if eff in ("max", "ultra", "xhigh"):
            return "xhigh"
        elif eff == "high":
            return "high"
        elif eff in ("minimal", "low"):
            return "low"
        else:
            return "medium"

    elif provider == "agy":
        # Antigravity CLI accepts --effort low|medium|high|xhigh|max; the
        # reasoning depth is also encoded in model-id suffixes (see
        # resolve_agy_model). Pass the requested level through, clamping
        # only ultra -> max and minimal -> low.
        if eff == "minimal":
            return "low"
        if eff in ("low", "medium", "high", "xhigh", "max"):
            return eff
        if eff == "ultra":
            return "max"
        return "medium"

    elif provider == "claude":
        # Claude accepts: low, medium, high, xhigh, max
        if eff in ("ultra", "max"):
            return "max"
        elif eff in ("xhigh", "high", "medium", "low"):
            return eff
        elif eff == "minimal":
            return "low"
        return "medium"

    elif provider == "muse":
        # Muse CLI accepts: none, minimal, low, medium, high, xhigh, max, ultra
        return eff

    return eff


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
        if eff_candidate in ("low", "medium", "high", "xhigh", "max", "ultra"):
            effort = eff_candidate
        val = re.sub(r"\s*\(\w+\)\s*", "", val).strip()

    if effort is None:
        for eff in ("xhigh", "ultra", "high", "medium", "low", "max"):
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



_AGY_SUFFIX_EFFORTS = ("low", "medium", "high", "xhigh", "max", "ultra")


def _split_agy_suffix(model_id: str) -> Tuple[str, Optional[str]]:
    """Split an agy model id into (base, suffix-effort or None).

    The suffix is a trailing -low/-medium/-high (also -xhigh/-max/-ultra)
    segment: "gemini-4-argon-high" -> ("gemini-4-argon", "high"),
    "gpt-oss-120b-medium" -> ("gpt-oss-120b", "medium"),
    "gemini-3.8-flash" -> ("gemini-3.8-flash", None).
    """
    if not isinstance(model_id, str) or not model_id.strip():
        return (model_id or ""), None
    val = model_id.strip()
    lower = val.lower()
    for eff in ("xhigh", "ultra", "medium", "high", "low", "max", "minimal"):
        if lower.endswith(f"-{eff}"):
            return val[: -len(eff) - 1], eff
    return val, None


def _agy_full_model_id(raw: str) -> str:
    """Full agy model id preserving a reasoning suffix.

    Display strings ("Gemini 3.8 Flash (High)") become the suffixed id
    ("gemini-3.8-flash-high"); already-suffixed ids stay as they are.
    Returns "" for invalid input.
    """
    if not isinstance(raw, str):
        return ""
    val = raw.strip()
    if not val or val.startswith("-") or re.search(r"[\r\n\t\x00-\x1f]", val):
        return ""
    if " " in val or "(" in val:
        base, eff = _normalize_agy_model(val)
        if not base:
            return ""
        return f"{base}-{eff}" if eff else base
    slug = val.lower()
    return slug if _valid_model_id(slug) else ""


def parse_agy_models_output(text: str) -> List[Tuple[str, str]]:
    """Parse `agy models` output into [(model_id, display_name)].

    Each line is "model_id<TAB>display_name" (whitespace also accepted).
    The parser is pure: callers inject fixture text in tests instead of
    running a real CLI. Spinner/ANSI noise is stripped, invalid ids are
    skipped, duplicates keep their first occurrence.
    """
    if not text:
        return []
    models: List[Tuple[str, str]] = []
    seen = set()
    for raw_line in str(text).splitlines():
        line = re.sub(r"\x1b\[[0-9;]*m", "", raw_line)
        line = re.sub(r"^[\u280b\u2809\u2839\u2838\u283c\u2834\u2826\u2827\u2807\u280f\s]+Fetching available models\.\.\.", "", line).strip()
        line = line.strip()
        if not line:
            continue
        if "\t" in line:
            slug, desc = line.split("\t", 1)
            slug, desc = slug.strip(), desc.strip()
        else:
            parts = line.split(None, 1)
            slug = parts[0].strip()
            desc = parts[1].strip() if len(parts) > 1 else ""
        slug = slug.lower()
        if not _valid_model_id(slug):
            continue
        # English/error lines ("No models available ...", "Please sign in ...")
        # start with a bare word; real model ids are hyphenated/dotted/digit-bearing.
        if slug != "gemini" and not any(c in slug for c in "-.") and not any(c.isdigit() for c in slug):
            continue
        if slug.endswith(":"):
            continue
        key = (slug, desc)
        if key in seen:
            continue
        seen.add(key)
        models.append((slug, desc))
    return models


def resolve_agy_model(
    model: Optional[str],
    effort: Optional[str],
    available_models=None,
) -> Tuple[Optional[str], Optional[str]]:
    """Resolve an agy (model, effort) pair to what the CLI should receive.

    Rules (task E1.2):
    1. A model id that already carries a -low/-medium/-high suffix is
       used verbatim and no --effort is added.
    2. A bare base name (e.g. "gemini-4-argon") plus a requested effort
       selects the highest available suffix variant that does not exceed
       the request (using _EFFORT_ORDER); if none is <= the request the
       lowest available variant is used. No --effort is added.
    3. A model with no suffix variants falls back to --effort, translated
       by normalize_provider_effort("agy", ...).

    `available_models` is injectable for tests: an iterable of model ids
    or (id, description) pairs. When omitted, the locally discovered
    catalog (_discover_local_agy_models) is consulted.
    Returns (final_model, effort_for_--effort_or_None).
    """
    if not model or not isinstance(model, str) or not model.strip():
        # No model to resolve: keep the effort translation for the caller.
        return model, normalize_provider_effort("agy", effort, model=model) if effort else None
    model = model.strip()
    _base, suffix = _split_agy_suffix(model)
    if suffix is not None:
        return model, None
    if not effort or str(effort).strip().lower() in ("none", "default"):
        return model, None
    eff = str(effort).strip().lower()
    if available_models is None:
        try:
            available_models = _discover_local_agy_models().models
        except Exception:
            available_models = []
    variants: Dict[str, str] = {}
    for item in available_models or []:
        if isinstance(item, (tuple, list)):
            slug = item[0] if item else ""
        elif isinstance(item, dict):
            slug = item.get("id") or item.get("slug") or ""
        else:
            slug = str(item).split(None, 1)[0] if str(item).strip() else ""
        if not slug:
            continue
        base2, suff2 = _split_agy_suffix(str(slug).strip())
        if suff2 is not None and base2.lower() == model.lower():
            variants.setdefault(suff2, str(slug).strip())
    if variants:
        if eff in _EFFORT_ORDER:
            rank = _EFFORT_ORDER.index(eff)
        else:
            rank = _EFFORT_ORDER.index("medium")
        allowed = [s for s in variants if s in _EFFORT_ORDER and _EFFORT_ORDER.index(s) <= rank]
        if allowed:
            chosen = max(allowed, key=lambda s: _EFFORT_ORDER.index(s))
        else:
            chosen = min(variants, key=lambda s: _EFFORT_ORDER.index(s) if s in _EFFORT_ORDER else 99)
        return variants[chosen], None
    return model, normalize_provider_effort("agy", eff, model=model)


def _catalog_origin(path):
    try:
        cached_at = datetime.fromtimestamp(path.stat().st_mtime, timezone.utc).isoformat()
    except OSError:
        cached_at = None
    return {"source": str(path), "cached_at": cached_at}


def _verified_model_ids(provider):
    from makewand.execution_evidence import read_recent
    return {row["actual_model"] for row in read_recent(engine=provider)
            if row.get("engine") == provider and row.get("result_status") == "PASSED"
            and row.get("outcome_known") is True and type(row.get("process_exit_code")) is int
            and row["process_exit_code"] == 0
            and row.get("source") in ("cli_json", "private_home+cli_json", "private_home")
            and _valid_model_id(row.get("actual_model"))}


def model_family(model):
    """Underlying model lineage, independent of the CLI carrying it."""
    value = (model or "").lower().split(" (", 1)[0]
    for family, prefixes in (
        ("claude", ("claude", "anthropic/", "opus", "sonnet", "haiku", "fable", "mythos")),
        ("gpt", ("gpt", "openai/", "o1", "o3", "o4", "codex")),
        ("gemini", ("gemini", "google/", "google-")),
        ("grok", ("grok", "xai/")), ("muse", ("muse",)),
        ("qwen", ("qwen",)), ("llama", ("llama",)), ("gemma", ("gemma",)),
        ("deepseek", ("deepseek",)), ("glm", ("glm",)), ("kimi", ("kimi",)),
    ):
        if value.startswith(prefixes):
            return family
    return None


def model_candidate(tool, tier="standard", model=None):
    from makewand.account_identity import account_identity
    resolution = get_provider_model_tier(tool, tier)
    mid = model or resolution.get("full_id") or resolution["model"]
    identity = account_identity(tool) if tool in ("claude", "codex") else None
    return dict(tool=tool, model=mid, model_family=model_family(mid), actual_model=None,
                account_ref=identity["account_ref"] if identity and identity["identity_known"] else None,
                identity_known=bool(identity and identity["identity_known"]),
                pool_ref=identity["pool_ref"] if identity else None,
                source="explicit" if model else resolution["source"],
                selection_reason="用户显式指定模型，覆盖档位排名" if model else resolution.get("selection_reason", f"tier={tier}: {mid}"))


def _candidate_families(tool, tier, explicit_model=None):
    if explicit_model:
        return {model_family(explicit_model)}
    # Auto pacing may change the tier before dispatch. Require independence
    # from every possible tier rather than guessing one from the tool name.
    tiers = ("fast", "standard", "deep") if not tier or tier == "auto" else (tier,)
    resolutions = [get_provider_model_tier(tool, option) for option in tiers]
    return {model_family(row.get("full_id") or row["model"]) for row in resolutions}


def independent_models(reviewer, author, *, reviewer_tier="deep", author_tier="standard", author_model=None):
    """Unknown lineage cannot establish cross-model independence."""
    if reviewer == author:
        return False
    left = _candidate_families(reviewer, reviewer_tier)
    right = _candidate_families(author, author_tier, author_model)
    return None not in left and None not in right and left.isdisjoint(right)


class DiscoveredAgyModels(tuple):
    """Container for discovered Antigravity / Gemini model catalog."""

    def __new__(cls, models: List[Tuple[str, str]], configured_default: Optional[str], configured_effort: Optional[str], metadata=None):
        result = super().__new__(cls, (models, configured_default, configured_effort))
        result.metadata = metadata or {}
        return result

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


def _discover_local_agy_models(sync: bool = True) -> DiscoveredAgyModels:
    """
    Scans local Gemini / Antigravity config and cache directories.
    Returns DiscoveredAgyModels(models, configured_default, configured_effort).
    """
    discovered_agy: List[Tuple[str, str]] = []
    seen = set()
    metadata = {}
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
                                norm_slug = _agy_full_model_id(slug or "")
                                if norm_slug and (norm_slug, desc) not in seen:
                                    seen.add((norm_slug, desc))
                                    discovered_agy.append((norm_slug, desc))
                                    metadata[norm_slug] = _catalog_origin(cache_file)
                            elif isinstance(m, str):
                                norm_slug = _agy_full_model_id(m)
                                if norm_slug and (norm_slug, "") not in seen:
                                    seen.add((norm_slug, ""))
                                    discovered_agy.append((norm_slug, ""))
                                    metadata[norm_slug] = _catalog_origin(cache_file)
                    elif isinstance(items, dict):
                        for k, v in items.items():
                            norm_slug = _agy_full_model_id(k)
                            if norm_slug:
                                desc = v.get("description", "") if isinstance(v, dict) else ""
                                if (norm_slug, desc) not in seen:
                                    seen.add((norm_slug, desc))
                                    discovered_agy.append((norm_slug, desc))
                                    metadata[norm_slug] = _catalog_origin(cache_file)
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
                        if eff_val in ("low", "medium", "high", "xhigh", "max", "ultra") and not configured_effort:
                            configured_effort = eff_val
                    # Also check model lists if present in settings.json
                    for key in ("available_models", "models", "additionalModelOptionsCache"):
                        m_list = sdata.get(key)
                        if isinstance(m_list, list):
                            for item in m_list:
                                val = item.get("value") or item.get("id") or item.get("slug") if isinstance(item, dict) else item
                                desc = item.get("description") or item.get("name") or "" if isinstance(item, dict) else ""
                                if isinstance(val, str):
                                    norm_val = _agy_full_model_id(val)
                                    if norm_val and (norm_val, desc) not in seen:
                                        seen.add((norm_val, desc))
                                        discovered_agy.append((norm_val, desc))
                                        metadata[norm_val] = _catalog_origin(s_path)
                except Exception:
                    pass

    # Merge the makewand `agy models` cache (refreshed at most once a day;
    # a failed refresh keeps serving the previous catalog). This source
    # carries the full suffixed ids (gemini-4-argon-high, claude-opus-5-5-low,
    # ...) and is merged with any .gemini caches above, not only used when
    # those are empty, so `makewand models` can show every agy model.
    agy_cache_file = Path.home() / ".cache" / "makewand" / "agy_models_cache.json"
    # sync=False reads a cache that is already on disk and does not run `agy models`.
    if sync:
        _sync_agy_models_cache(cache_file=agy_cache_file)
    if agy_cache_file.exists():
        try:
            cdata = json.loads(agy_cache_file.read_text(encoding="utf-8"))
            for m in cdata.get("models", []):
                if isinstance(m, dict):
                    slug = m.get("id") or m.get("slug")
                    desc = m.get("description") or m.get("name") or ""
                    norm_slug = _agy_full_model_id(slug or "")
                    if norm_slug and (norm_slug, desc) not in seen:
                        seen.add((norm_slug, desc))
                        discovered_agy.append((norm_slug, desc))
                        metadata[norm_slug] = _catalog_origin(agy_cache_file)
                elif isinstance(m, str):
                    norm_slug = _agy_full_model_id(m)
                    if norm_slug and (norm_slug, "") not in seen:
                        seen.add((norm_slug, ""))
                        discovered_agy.append((norm_slug, ""))
                        metadata[norm_slug] = _catalog_origin(agy_cache_file)
        except Exception:
            pass

    # A catalog entry is not proof that a display name is an invocable ID.
    # Keep the entire catalog; only execution observations verify an ID.
    verified = _verified_model_ids("agy")
    for slug, _ in discovered_agy:
        metadata.setdefault(slug, {}).update(
            call_id_verified=slug in verified,
            verification_source="execution_evidence" if slug in verified else "unknown",
            # Successful execution verifies an ID, not whether it costs credits.
            auto_select=_is_native_agy_model(slug))
    return DiscoveredAgyModels(discovered_agy, configured_default, configured_effort, metadata)


def _is_native_agy_model(slug: str) -> bool:
    """Antigravity's own Gemini models, as opposed to the claude-*/gpt-* models it proxies."""
    return slug.startswith(("gemini-", "google-")) or slug == "gemini"


AGY_MODELS_RETRY_SECONDS = 3600
# Implicit lookups stay short so a slow `agy models` cannot stall routing.
# An explicit refresh (refresh_agy_models_cache / discover_available_models(refresh=True))
# waits long enough for the networked command, which regularly exceeds 4s and
# was falling back to the stale 10-02 cache.
AGY_MODELS_TIMEOUT_SECONDS = 4.0
AGY_MODELS_REFRESH_TIMEOUT_SECONDS = 60.0
# Refresh attempts of this process that left no new catalog, per cache file:
# (time.monotonic() of the attempt, cache state it saw). Keeps `agy models` to one
# run per process and back-off window even when the marker file misbehaves.
_AGY_FAILED_REFRESHES: Dict[str, Tuple[float, str]] = {}


def _agy_cache_state(cache_file: Path) -> str:
    """Identifies the catalog a refresh attempt saw ("missing" when there was none)."""
    try:
        st = cache_file.stat()
    except OSError:
        return "missing"
    return f"{st.st_mtime_ns}:{st.st_size}"


def _claim_agy_refresh(marker: Path, state: str, retry: float) -> bool:
    """
    Creates the attempt marker before `agy models` runs; False means do not run it.
    A marker younger than `retry` written for the current cache state belongs to a
    refresh that is running or failed recently. Older markers, markers dated in the
    future and markers written for a cache that has changed since (deleted to force
    a refresh, or replaced) are taken over. When no marker can be written, the
    refreshed catalog could not be stored either, so there is no refresh.
    """
    import time
    try:
        marker.parent.mkdir(parents=True, exist_ok=True)
    except OSError:
        return False
    for _ in range(2):
        try:
            fd = os.open(str(marker), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        except FileExistsError:
            try:
                modified = marker.stat().st_mtime
                # Observe the clock after stat: a concurrently published marker
                # must not look future-dated merely because it followed our clock read.
                age = time.time() - modified
            except OSError:
                continue  # removed meanwhile: create it again
            try:
                seen = marker.read_text(encoding="utf-8", errors="replace").strip()
            except OSError:
                seen = ""
            if 0 <= age < retry and seen in ("", state):  # "": its owner is still writing it
                return False
            try:
                marker.unlink()
            except FileNotFoundError:
                pass
            except OSError:
                return False
            continue
        except OSError:
            return False
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as handle:
                handle.write(state)
        except OSError:
            pass
        return True
    return False


def _sync_agy_models_cache(cache_file: Optional[Path] = None, timeout: float = AGY_MODELS_TIMEOUT_SECONDS, max_age: float = 86400, runner=None, output_text: Optional[str] = None, force: bool = False) -> Optional[Path]:
    """
    Syncs available models from `agy models` CLI if cache is absent or older than max_age (1 day, 86400s by default).
    `agy models` runs only after the attempt marker next to the cache has been
    created, so an unwritable cache directory never runs it for nothing. The marker
    stays after a failed refresh: the next attempt for the same cache state waits
    AGY_MODELS_RETRY_SECONDS (at most max_age), once per process at most, so a
    broken or slow `agy models` cannot stall every lookup. Output that names no
    gemini-/google- model ("No models available ...", "Error: ...") is a failed
    refresh and never replaces the previous catalog.
    The implicit path keeps the short timeout (4s). ``force=True`` is the explicit
    refresh: it ignores a fresh cache, the in-process back-off and a young
    ``.attempt`` marker, and the caller passes the 60s timeout.
    Never executes in test isolation mode (MAKEWAND_TEST_ISOLATION_ROOT).
    """
    from makewand.config import is_provider_enabled
    _injected = output_text is not None or runner is not None
    if not _injected and ("MAKEWAND_TEST_ISOLATION_ROOT" in os.environ or not is_provider_enabled("agy")):
        return None
    import shutil
    import subprocess
    import tempfile
    import time
    if not _injected and not shutil.which("agy"):
        return None

    if cache_file is None:
        cache_file = Path.home() / ".cache" / "makewand" / "agy_models_cache.json"

    def is_fresh():
        try:
            st = cache_file.stat()
        except OSError:
            return False
        return time.time() - st.st_mtime < max_age and st.st_size > 30

    if not force and is_fresh():
        return cache_file

    retry = min(max_age, AGY_MODELS_RETRY_SECONDS)
    state = _agy_cache_state(cache_file)
    attempt_marker = cache_file.with_name(cache_file.name + ".attempt")
    if not force:
        failed = _AGY_FAILED_REFRESHES.get(str(cache_file))
        if failed and failed[1] == state and time.monotonic() - failed[0] < retry:
            return None
        if not _claim_agy_refresh(attempt_marker, state, retry):
            return None
        if is_fresh():  # another process finished a refresh just before the claim
            try:
                attempt_marker.unlink()
            except OSError:
                pass
            return cache_file
    else:
        # Explicit refresh must not be held by a young .attempt marker. The
        # directory still has to be writable, otherwise the new catalog could
        # not be stored and running `agy models` would only block the caller.
        try:
            cache_file.parent.mkdir(parents=True, exist_ok=True)
        except OSError:
            return None
        try:
            attempt_marker.unlink()
        except OSError:
            pass
    _AGY_FAILED_REFRESHES[str(cache_file)] = (time.monotonic(), state)  # until the refresh succeeds

    tmp_name = None
    try:
        # Injectable sources for tests: output_text is used verbatim, runner
        # is called like subprocess.run; neither touches a real CLI.
        if output_text is not None:
            parsed = parse_agy_models_output(output_text)
            models_data = [{"id": slug, "description": desc} for slug, desc in parsed]
            fetch_ok = True
        elif runner is not None:
            res = runner(["agy", "models"], capture_output=True, text=True, timeout=timeout, cwd=tempfile.gettempdir())
            parsed = parse_agy_models_output(res.stdout) if getattr(res, "returncode", 1) == 0 and getattr(res, "stdout", None) else []
            models_data = [{"id": slug, "description": desc} for slug, desc in parsed]
            fetch_ok = getattr(res, "returncode", 1) == 0
        else:
            res = subprocess.run(
                ["agy", "models"],
                capture_output=True,
                text=True,
                timeout=timeout,
                cwd=tempfile.gettempdir(),
            )
            parsed = parse_agy_models_output(res.stdout) if res.returncode == 0 and res.stdout else []
            models_data = [{"id": slug, "description": desc} for slug, desc in parsed]
            fetch_ok = res.returncode == 0
        if fetch_ok and any(_is_native_agy_model(_split_agy_suffix(m["id"])[0]) for m in models_data):
            # A private temporary file per writer: concurrent refreshers never
            # replace the cache with each other's partial output.
            fd, tmp_name = tempfile.mkstemp(prefix=cache_file.name + ".", suffix=".tmp", dir=str(cache_file.parent))
            with os.fdopen(fd, "w", encoding="utf-8") as handle:
                json.dump({"updated_at": time.time(), "models": models_data}, handle, indent=2)
            os.replace(tmp_name, str(cache_file))
            tmp_name = None
            _AGY_FAILED_REFRESHES.pop(str(cache_file), None)
            try:
                attempt_marker.unlink()
            except OSError:
                pass
            return cache_file
    except Exception:
        pass
    finally:
        if tmp_name:
            try:
                os.unlink(tmp_name)
            except OSError:
                pass
    return None


_refresh_agy_models_cache = _sync_agy_models_cache


def refresh_agy_models_cache(cache_file: Optional[Path] = None, timeout: float = AGY_MODELS_REFRESH_TIMEOUT_SECONDS, max_age: float = 86400, runner=None, output_text: Optional[str] = None) -> Optional[Path]:
    """Explicit ``agy models`` refresh for ``makewand models --refresh``.

    Waits up to 60 seconds. A fresh cache does not skip the run, and a young
    ``.attempt`` marker (or this process's failed-refresh window) does not
    block it. The daily implicit lookup keeps the 4 second timeout.
    ``runner`` / ``output_text`` are the same test injections as
    ``_sync_agy_models_cache``: neither starts a real CLI by itself.
    """
    return _sync_agy_models_cache(
        cache_file=cache_file,
        timeout=timeout,
        max_age=max_age,
        runner=runner,
        output_text=output_text,
        force=True,
    )


class DiscoveredMuseModels(tuple):
    """Container for discovered Muse Code model catalog."""

    def __new__(cls, models: List[Tuple[str, str]], configured_default: Optional[str], configured_effort: Optional[str], efforts=None):
        result = super().__new__(cls, (models, configured_default, configured_effort))
        result.efforts = efforts or {}
        return result

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
    model_efforts = {}
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
                            if mid not in model_efforts and isinstance(r.get("reasoning_effort_variants"), list):
                                model_efforts[mid] = _effort_names(r["reasoning_effort_variants"])
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

    return DiscoveredMuseModels(discovered_muse, configured_default, configured_effort, model_efforts)


def _discover_local_grok_models() -> List[Tuple[str, str]]:
    """
    (model id, description) pairs from the Grok CLI cache ~/.grok/models_cache.json.
    Grok CLI 1.0.x nests each entry's metadata under "info"
    ({"models": {"grok-4.7": {"info": {"description": ...}}}}); older caches kept
    "description" directly in the entry. Both layouts are read.
    """
    discovered: List[Tuple[str, str]] = []
    try:
        data = json.loads((Path.home() / ".grok" / "models_cache.json").read_text(encoding="utf-8"))
    except Exception:  # absent, unreadable or malformed cache
        return discovered
    entries = data.get("models") if isinstance(data, dict) else None
    if not isinstance(entries, dict):
        return discovered
    for model_id, entry in entries.items():
        if not _valid_model_id(model_id):
            continue
        entry = entry if isinstance(entry, dict) else {}
        info = entry.get("info") if isinstance(entry.get("info"), dict) else {}
        desc = entry.get("description") or info.get("description") or ""
        discovered.append((model_id, desc if isinstance(desc, str) else ""))
    return discovered


def _tier_resolution(model, effort, *, is_dynamic, full_id=None, effort_source="builtin"):
    """Model provenance and effort provenance are independent facts.

    Builtin effort settings preserve historical defaults; they do not prove
    that a newly discovered commercial model supports those capabilities.
    """
    result = {"model": model.strip(), "effort": effort, "is_dynamic": is_dynamic,
              "source": "detected" if is_dynamic else "builtin", "effort_source": effort_source}
    if full_id is not None:
        result["full_id"] = full_id.strip()
    if not is_dynamic:
        result["selection_reason"] = f"未检测到可选择目录，使用内置兜底 {model.strip()}；目录模型未参与排名"
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

    # A price statement is not a capability: "2x the price" makes a variant the
    # wrong cheap/everyday pick and says nothing for the deep tier, and its words
    # ("2x the standard price") name no family.
    price = _price_signal(text)
    text = _without_price_statements(text)

    fast_fam = sum(1 for k in FAST_FAMILIES if _matches_token(k, text)) + (1 if _SPEED_VARIANT_RE.search(text) else 0)
    fast_kw = sum(1 for k in FAST_KEYWORDS if _matches_token(k, text))

    deep_fam = sum(1 for k in DEEP_FAMILIES if _matches_token(k, text))
    deep_kw = sum(1 for k in DEEP_KEYWORDS if _matches_token(k, text))
    premium_kw = sum(1 for k in PREMIUM_KEYWORDS if _matches_token(k, text))

    std_fam = sum(1 for k in STANDARD_FAMILIES if _matches_token(k, text))
    std_kw = sum(1 for k in STANDARD_KEYWORDS if _matches_token(k, text))

    tier = tier.lower()
    score = ver_score
    if tier == "fast":
        # The cheap tier: a surcharged speed variant gains nothing from being fast.
        if price <= 0:
            score += fast_fam * 200000 + fast_kw * 30000
        score -= (deep_fam * 250000 + deep_kw * 40000)
        score -= (std_fam * 100000)
        if price > 0:
            score -= 250000
        elif price < 0:
            score += 30000
        # A surcharge, or a deep family or top-of-line wording on a model that is
        # not itself a fast family, marks it premium. Versions order models within
        # the premium and the other class, never across them: a newer flagship
        # ("grok-5.0", "latest frontier model") is not the cheap pick over grok-4.6.
        if price > 0 or ((deep_fam or premium_kw) and not fast_fam):
            score -= _PREMIUM_IN_FAST_TIER_PENALTY
    elif tier == "deep":
        score += deep_fam * 200000 + deep_kw * 30000
        score -= (fast_fam * 250000 + fast_kw * 40000)
    else:  # standard
        score += std_fam * 200000 + std_kw * 30000
        score -= (fast_fam * 150000)
        score -= (deep_fam * 50000)
        if price > 0:
            score -= 150000
        if not fast_fam and not deep_fam and not std_fam:
            score += 15000
    return score


_MODEL_SIZE_RE = re.compile(r"[:\-_](\d+(?:\.\d+)?)[bB](?![a-zA-Z0-9])")


def _model_size(slug: str) -> float:
    """Parameter count in billions an id names ("llama3.1:70b", "gpt-oss-120b"), 0 when it names none."""
    match = _MODEL_SIZE_RE.search(slug)
    return float(match.group(1)) if match else 0.0


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
    # Equal scores go to the larger model for deep work and the smaller one for
    # the fast tier ("llama3.1:70b" / "llama3.1:8b"), then to the shorter id, the
    # base model rather than a variant of it ("grok-4.7" over
    # "grok-4.7-build-fast"), then to the id itself: the pick never depends on
    # the order a catalog happens to list models in.
    size_order = {"deep": -1, "fast": 1}.get(tier.lower(), 0)
    scored.sort(key=lambda x: (-x[0], size_order * _model_size(x[1]), len(x[1]), x[1]))
    return scored


def _effort_names(levels) -> List[str]:
    """Pull canonical effort ids out of a catalog's level list. Unknown names are dropped."""
    found: List[str] = []
    seen = set()
    for level in levels or []:
        if isinstance(level, dict):
            raw = level.get("effort") or level.get("id") or level.get("value") or level.get("tier")
        else:
            raw = level
        if not isinstance(raw, str):
            continue
        name = raw.strip().lower()
        if name in _EFFORT_ORDER and name not in seen:
            seen.add(name)
            found.append(name)
    return found


def _iter_codex_bases():
    if os.environ.get("CODEX_HOME"):
        yield Path(os.environ["CODEX_HOME"]).expanduser()
        return
    yield Path.home() / ".codex"


def _codex_effort_map() -> Dict[str, List[str]]:
    found: Dict[str, List[str]] = {}
    for base in _iter_codex_bases():
        try:
            data = json.loads((base / "models_cache.json").read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        for entry in data.get("models", []) if isinstance(data, dict) else []:
            if not isinstance(entry, dict) or entry.get("visibility") == "hide":
                continue
            slug = entry.get("slug") or entry.get("id")
            if not _valid_model_id(slug) or slug in found:
                continue
            found[str(slug)] = _effort_names(entry.get("supported_reasoning_levels") or [])
    return found


def _claude_effort_map() -> Dict[str, List[str]]:
    found: Dict[str, List[str]] = {}
    cat_dir = Path(os.environ.get("CLAUDE_CONFIG_DIR") or Path.home() / ".claude").expanduser() / "cache" / "model-catalog"
    try:
        files = sorted(cat_dir.glob("*.json"), key=lambda path: path.stat().st_mtime, reverse=True)
    except OSError:
        return found
    if not files:
        return found
    try:
        cat_data = json.loads(files[0].read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return found
    cfg_models = cat_data.get("catalog", {}).get("config", {}).get("models", []) if isinstance(cat_data, dict) else []
    for entry in cfg_models if isinstance(cfg_models, list) else []:
        if not isinstance(entry, dict):
            continue
        mid = entry.get("id")
        if not _valid_model_id(mid) or mid in found:
            continue
        thinking = entry.get("thinking") if isinstance(entry.get("thinking"), dict) else {}
        found[str(mid)] = _effort_names(thinking.get("effort_options") or [])
    return found


def _grok_effort_map() -> Dict[str, List[str]]:
    found: Dict[str, List[str]] = {}
    try:
        data = json.loads((Path.home() / ".grok" / "models_cache.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return found
    entries = data.get("models") if isinstance(data, dict) else None
    if not isinstance(entries, dict):
        return found
    for model_id, entry in entries.items():
        if not _valid_model_id(model_id):
            continue
        entry = entry if isinstance(entry, dict) else {}
        info = entry.get("info") if isinstance(entry.get("info"), dict) else {}
        levels = info.get("reasoning_efforts") or entry.get("reasoning_efforts") or []
        found[str(model_id)] = _effort_names(levels)
    return found


def _muse_effort_map() -> Dict[str, List[str]]:
    found: Dict[str, List[str]] = {}
    try:
        discovered = _discover_local_muse_models()
    except Exception:
        return found
    for mid, _desc in discovered.models:
        if _valid_model_id(mid) and mid not in found:
            found[str(mid)] = getattr(discovered, "efforts", {}).get(mid, [])
    return found


def _agy_effort_map() -> Dict[str, List[str]]:
    found: Dict[str, List[str]] = {}
    try:
        discovered = _discover_local_agy_models(sync=False)
    except Exception:
        return found
    for slug, _desc in discovered.models:
        if not _valid_model_id(slug):
            continue
        _base, suffix = _split_agy_suffix(slug)
        if suffix:
            found[str(slug)] = [suffix]
        elif slug not in found:
            found[str(slug)] = []
    return found


def discover_model_efforts() -> Dict[str, Dict[str, List[str]]]:
    """Local model → reasoning efforts for agy, claude, codex, muse and grok.

    Reads the same on-disk catalogs ``discover_available_models`` already uses.
    Does not run a provider CLI and does not refresh the agy cache (that stays
    on the short implicit timeout). A provider with nothing on disk is omitted
    so the routing catalog keeps its built-in default for that provider.
    """
    found: Dict[str, Dict[str, List[str]]] = {}
    for provider, reader in (
        ("codex", _codex_effort_map),
        ("claude", _claude_effort_map),
        ("grok", _grok_effort_map),
        ("muse", _muse_effort_map),
        ("agy", _agy_effort_map),
    ):
        try:
            models = reader()
        except Exception:
            continue
        if models:
            found[provider] = models
    return found


def _merge_model_label(entries_by_id: Dict[str, str], label: str) -> None:
    """Keep one entry per bare model ID, preferring a labeled/described string over a bare ID."""
    if not isinstance(label, str) or not label.strip():
        return
    clean = label.strip()
    mid = clean.split()[0]
    if not _valid_model_id(mid):
        return
    existing = entries_by_id.get(mid)
    if existing is None or (" (" not in existing and " (" in clean):
        entries_by_id[mid] = clean


def discover_available_models(refresh: bool = False) -> Dict[str, Any]:
    if refresh:
        # Explicit path only. The lookup below still uses the short implicit
        # sync, which no-ops once this refresh has written a fresh cache.
        refresh_agy_models_cache()
    models = {
        "claude": {"current_default": "claude-sonnet-5 (Sonnet 5)", "available": ["claude-fable-5-1[1m] (Fable 5.1)", "claude-opus-5-5 (Opus 5.5)", "claude-sonnet-5 (Sonnet 5)", "claude-haiku-4-5-20251001 (Haiku 4.5)"]},
        "codex": {"current_default": "gpt-6-astra", "available": []},
        "agy": {"current_default": "Gemini 3.8 Flash / Pro", "available": ["gemini-4-argon-low", "gemini-4-argon-medium", "gemini-4-argon-high", "gemini-3.8-flash-low", "gemini-3.8-flash-medium", "gemini-3.8-flash-high", "claude-opus-5-5-low", "claude-opus-5-5-medium", "claude-opus-5-5-high", "claude-sonnet-5-5-low", "claude-sonnet-5-5-medium", "claude-sonnet-5-5-high", "gpt-oss-120b-medium"]},
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
                catalog_by_id: Dict[str, str] = {}
                for m in cfg_models:
                    if not isinstance(m, dict):
                        continue
                    mid = m.get("id")
                    mname = m.get("name")
                    if not _valid_model_id(mid):
                        continue
                    lbl = f"{mid} ({mname})" if isinstance(mname, str) and mname.strip() else mid
                    _merge_model_label(catalog_by_id, lbl)
                if catalog_by_id:
                    models["claude"]["available"] = list(catalog_by_id.values())
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
            found_by_id: Dict[str, str] = {}
            if models["claude"]["source"] == "detected":
                for item in models["claude"]["available"]:
                    _merge_model_label(found_by_id, item)
            for m in data.get("additionalModelOptionsCache", []):
                val = m.get("value")
                lbl = m.get("label")
                if _valid_model_id(val) and val.startswith("claude-"):
                    _merge_model_label(found_by_id, f"{val} ({lbl})" if lbl else str(val))
            for m in re.findall(r'"model":\s*"([^"]+)"', raw):
                if _valid_model_id(m) and m.strip().startswith("claude-"):
                    _merge_model_label(found_by_id, m.strip())
            detected = sorted([m for m in found_by_id.values() if m.startswith("claude-")], reverse=True)
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
            codex_bases.append(Path.home() / ".codex")

        found_by_id: Dict[str, str] = {}
        configured_model = None

        for base in codex_bases:
            if not base.exists():
                continue

            # 1. Read models_cache.json (official model catalog from OpenAI)
            cache_file = base / "models_cache.json"
            cache_has_visible = False
            if cache_file.exists():
                try:
                    cdata = json.loads(cache_file.read_text(encoding="utf-8"))
                    for m in cdata.get("models", []):
                        if not isinstance(m, dict) or m.get("visibility") == "hide":
                            continue
                        slug = m.get("slug") or m.get("id")
                        desc = m.get("description", "")
                        if _valid_model_id(slug):
                            cache_has_visible = True
                            _merge_model_label(found_by_id, f"{slug} ({desc})" if desc else slug)
                except Exception:
                    pass

            # 2. Read config.toml
            codex_toml = base / "config.toml"
            if codex_toml.exists():
                try:
                    raw = codex_toml.read_text(encoding="utf-8")
                    if not cache_has_visible:
                        for m in re.findall(r'\[tui\.model_availability_nux\]\s*([\s\S]*?)(?=\n\[|$)', raw):
                            for line in m.splitlines():
                                if "=" in line:
                                    name = line.split("=")[0].strip().strip('"')
                                    if _valid_model_id(name):
                                        _merge_model_label(found_by_id, name)
                    for m in re.findall(r'model\s*=\s*"([^"]+)"', raw):
                        if _valid_model_id(m):
                            _merge_model_label(found_by_id, m.strip())
                    if not configured_model:
                        cfg_m = re.search(r'^\s*model\s*=\s*"([^"]+)"', raw, re.MULTILINE)
                        if cfg_m and _valid_model_id(cfg_m.group(1)):
                            configured_model = cfg_m.group(1).strip()
                except Exception:
                    pass

        if found_by_id:
            models["codex"]["available"] = sorted(
                list(found_by_id.values()),
                key=lambda x: (parse_semver(x.split()[0]), x),
                reverse=True
            )
            models["codex"]["source"] = "detected"
        if configured_model:
            models["codex"]["current_default"] = configured_model
            models["codex"]["default_source"] = "detected"
        elif found_by_id:
            ranked_std = rank_models_for_tier(list(found_by_id.values()), "standard")
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
                lbl = f"{mid} ({desc})" if desc and desc.strip() != mid else mid
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
        discovered_grok = _discover_local_grok_models()
        if discovered_grok:
            models["grok"]["available"] = sorted(
                [model for model, _ in discovered_grok],
                key=lambda x: (parse_semver(x.split()[0]), x),
                reverse=True
            )
            models["grok"]["source"] = "detected"
            # Ranked with descriptions, like get_provider_model_tier("grok", "standard").
            ranked_std = rank_models_for_tier(discovered_grok, "standard")
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
        selectable = [item for item in agy_res.models if agy_res.metadata.get(item[0], {}).get("auto_select", _is_native_agy_model(item[0]))]
        if agy_res.configured_default and (_is_native_agy_model(agy_res.configured_default) or agy_res.metadata.get(agy_res.configured_default, {}).get("auto_select")):
            models["agy"]["current_default"] = agy_res.configured_default
            models["agy"]["default_source"] = "detected"
        elif selectable:
            _native_bases = []
            _seen_bases = set()
            for _slug, _desc in selectable:
                _base, _suf = _split_agy_suffix(_slug)
                if _base in _seen_bases:
                    continue
                _seen_bases.add(_base)
                _native_bases.append((_base, _desc))
            ranked_std = rank_models_for_tier(_native_bases, "standard") if _native_bases else []
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

    from makewand.account_identity import account_identity
    for tool, entry in models.items():
        identity = account_identity(tool) if tool in ("claude", "codex") else None
        verified = _verified_model_ids(tool)
        entry["candidates"] = []
        for label in entry["available"]:
            mid = label.split()[0]
            origin = agy_res.metadata.get(mid, {}) if tool == "agy" and "agy_res" in locals() else {}
            entry["candidates"].append(dict(
                tool=tool, account_ref=identity["account_ref"] if identity and identity["identity_known"] else None,
                identity_known=bool(identity and identity["identity_known"]),
                pool_ref=identity["pool_ref"] if identity else None,
                model=mid, model_family=model_family(mid), actual_model=None,
                source=origin.get("source", entry["source"]), cached_at=origin.get("cached_at"),
                call_id_verified=mid in verified,
                verification_source="execution_evidence" if mid in verified else "unknown",
                auto_select=origin.get("auto_select", tool != "agy" or _is_native_agy_model(mid))))
    models["claude"]["tier_selections"] = {
        tier: get_provider_model_tier("claude", tier) for tier in ("fast", "standard", "deep")}
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
                            result = _tier_resolution(alias, effort, is_dynamic=True, full_id=top_id,
                                                      effort_source="detected" if efforts else "builtin")
                            result["selection_reason"] = f"tier={tier}: 按版本、家族和能力关键词评分，选择 {top_id} (score={ranked[0][0]})"
                            result["ranking"] = [dict(model=mid, score=score, selected=mid == top_id,
                                                      reason="本档位排名第一" if mid == top_id else f"本档位排名低于 {top_id}")
                                                 for score, mid, _ in ranked]
                            return result
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
        codex_efforts: Dict[str, List[str]] = {}
        configured_default = None
        configured_effort = None

        codex_bases = []
        if os.environ.get("CODEX_HOME"):
            codex_bases.append(Path(os.environ["CODEX_HOME"]).expanduser())
        else:
            codex_bases.append(Path.home() / ".codex")

        for base in codex_bases:
            if not base.exists():
                continue
            cache_file = base / "models_cache.json"
            if cache_file.exists() and not discovered_models:
                try:
                    cdata = json.loads(cache_file.read_text(encoding="utf-8"))
                    for m in cdata.get("models", []):
                        if not isinstance(m, dict) or m.get("visibility") == "hide":
                            continue
                        slug = m.get("slug") or m.get("id")
                        if _valid_model_id(slug):
                            discovered_models.append((slug, m.get("description", "")))
                            effs = _effort_names(m.get("supported_reasoning_levels") or [])
                            if effs:
                                codex_efforts[slug] = effs
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
            eff_src = "detected" if (configured_effort or codex_efforts.get(configured_default)) else "builtin"
            return _tier_resolution(configured_default, effort, is_dynamic=True, effort_source=eff_src)

        if discovered_models:
            ranked = rank_models_for_tier(discovered_models, tier)
            if ranked:
                top_id = ranked[0][1]
                eff_src = "detected" if (codex_efforts.get(top_id) or (tier == "standard" and configured_effort)) else "builtin"
                return _tier_resolution(top_id, effort, is_dynamic=True, effort_source=eff_src)

        fallback_models = {
            "fast": "gpt-6-luna",
            "deep": "gpt-6-astra",
            "standard": configured_default or "gpt-6.1-sol"
        }
        return _tier_resolution(fallback_models.get(tier, "gpt-6.1-sol"), effort, is_dynamic=False)

    elif provider == "grok":
        discovered_grok = _discover_local_grok_models()
        grok_efforts = _grok_effort_map()

        effort = "high" if tier == "deep" else ("low" if tier == "fast" else "medium")
        if discovered_grok:
            ranked = rank_models_for_tier(discovered_grok, tier)
            if ranked:
                top_id = ranked[0][1]
                eff_src = "detected" if grok_efforts.get(top_id) else "builtin"
                return _tier_resolution(top_id, effort, is_dynamic=True, effort_source=eff_src)

        # Without a catalog only the CLI's default model is known to exist. Its
        # speed variant grok-4.7-build-fast costs twice as much, so it is never
        # the cheap default; the fast tier runs grok-4.7 at low effort.
        return _tier_resolution("grok-4.7", effort, is_dynamic=False)

    elif provider == "muse":
        muse_res = _discover_local_muse_models()
        discovered_muse = muse_res.models
        configured_default = muse_res.configured_default
        configured_effort = muse_res.configured_effort
        muse_efforts = getattr(muse_res, "efforts", {}) or {}

        effort = configured_effort if (tier == "standard" and configured_effort) else ("max" if tier == "deep" else ("low" if tier == "fast" else "high"))

        # If user explicitly configured a model in settings.json and requested standard tier, honor it
        if tier == "standard" and configured_default:
            eff_src = "detected" if (configured_effort or muse_efforts.get(configured_default)) else "builtin"
            return _tier_resolution(configured_default, effort, is_dynamic=True, effort_source=eff_src)

        if discovered_muse:
            ranked = rank_models_for_tier(discovered_muse, tier)
            if ranked:
                top_id = ranked[0][1]
                eff_src = "detected" if (configured_effort or muse_efforts.get(top_id)) else "builtin"
                return _tier_resolution(top_id, effort, is_dynamic=True, effort_source=eff_src)

        if configured_default:
            eff_src = "detected" if (configured_effort or muse_efforts.get(configured_default)) else "builtin"
            return _tier_resolution(configured_default, effort, is_dynamic=True, effort_source=eff_src)

        # Static fallback when no dynamic catalog is detected
        fallback_models = {
            "fast": "muse-spark-1.2",
            "deep": "muse-spark-1.3",
            "standard": "muse-spark-1.3-contributor",
        }
        return _tier_resolution(fallback_models.get(tier, "muse-spark-1.3-contributor"), effort, is_dynamic=False)

    elif provider == "agy":
        agy_res = _discover_local_agy_models()
        discovered_agy = [item for item in agy_res.models
                          if agy_res.metadata.get(item[0], {}).get("auto_select", _is_native_agy_model(item[0]))]
        configured_default = agy_res.configured_default
        if configured_default and not (_is_native_agy_model(configured_default) or
                                      agy_res.metadata.get(configured_default, {}).get("auto_select")):
            configured_default = None
        configured_effort = agy_res.configured_effort

        effort = configured_effort if (tier == "standard" and configured_effort) else ("max" if tier == "deep" else ("low" if tier == "fast" else "high"))

        if tier == "standard" and configured_default:
            return _tier_resolution(configured_default, effort, is_dynamic=True,
                                    effort_source="detected" if configured_effort else "builtin")

        if discovered_agy:
            # Tier picks stay on native Gemini bases: suffixed variants are
            # collapsed to their base and foreign (claude-*/gpt-*) models
            # never hijack the tier (they would spend credits).
            _bases = []
            _seen_b = set()
            for _slug, _desc in discovered_agy:
                _base, _suf = _split_agy_suffix(_slug)
                if not _is_native_agy_model(_base) or _base in _seen_b:
                    continue
                _seen_b.add(_base)
                _bases.append((_base, _desc))
            ranked = rank_models_for_tier(_bases, tier) if _bases else []
            if ranked:
                return _tier_resolution(ranked[0][1], effort, is_dynamic=True,
                                        effort_source="detected" if configured_effort else "builtin")

        # Static fallback when no dynamic catalog is detected
        fallback_models = {
            "fast": "gemini-3.8-flash-high",
            "deep": "gemini-3.1-pro-high",
            "standard": configured_default or "gemini-3.8-flash-high",
        }
        fallback_effort = "max" if tier == "deep" else ("high" if tier == "standard" else "low")
        return _tier_resolution(fallback_models.get(tier, "gemini-3.8-flash-high"), fallback_effort, is_dynamic=False)

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


# Providers of the Go router whose every model must be priced (pricedProviders
# in router/strategy_tables.go): it ignores their discovered tiers without one.
GO_PRICED_PROVIDERS = frozenset({"claude", "gemini", "openai"})


def _go_default_priced_models() -> frozenset:
    """Model IDs the Go router's built-in table prices, read from router/defaults.json
    beside the package (a source checkout or an install); empty when it is absent."""
    path = Path(__file__).resolve().parent.parent / "router" / "defaults.json"
    try:
        costs = json.loads(path.read_text(encoding="utf-8")).get("costs")
    except (OSError, ValueError, AttributeError):
        return frozenset()
    return frozenset(costs) if isinstance(costs, dict) else frozenset()


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
            lookup_prov = "agy" if prov == "gemini" else prov
            res_cheap = get_provider_model_tier(lookup_prov, "fast")
            res_mid = get_provider_model_tier(lookup_prov, "standard")
            res_prem = get_provider_model_tier(lookup_prov, "deep")

            cheap_m = res_cheap.get("full_id") or res_cheap.get("model")
            mid_m = res_mid.get("full_id") or res_mid.get("model")
            prem_m = res_prem.get("full_id") or res_prem.get("model")

            # Clean any effort suffix from agy/gemini model names if present (e.g. gemini-3.1-pro-high -> gemini-3.1-pro)
            if lookup_prov == "agy":
                cheap_m, _ = _normalize_agy_model(cheap_m) if cheap_m else (cheap_m, None)
                mid_m, _ = _normalize_agy_model(mid_m) if mid_m else (mid_m, None)
                prem_m, _ = _normalize_agy_model(prem_m) if prem_m else (prem_m, None)


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

    # A priced provider's tier is exported only with a priced model: in these costs or
    # in the Go defaults. The Go router drops any other (pricedDiscoveredModels), so it
    # would only be noise, such as an agy model in the gemini tiers.
    priced = set(costs_map) | _go_default_priced_models()
    for prov in GO_PRICED_PROVIDERS:
        tiers = models_map.get(prov)
        if not isinstance(tiers, dict):
            continue
        kept = {tier: model for tier, model in tiers.items() if isinstance(model, str) and model in priced}
        if len(kept) != len(tiers):
            logger.debug("discovered.json: %s tiers without a price left to the Go defaults: %s",
                         prov, sorted(set(tiers) - set(kept)))
        models_map[prov] = kept

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

