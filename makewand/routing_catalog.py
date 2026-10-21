"""Tier catalog and starting-tier rules for automatic makewand dispatch.

Pure functions. This module does not read the environment, write state, call a
model, or touch the network. Callers pass an optional JSON file when they want
to replace one or more tiers.

A tier is 1 (cheap and fast), 2 (middle), or 3 (strong, deep reasoning).
Each candidate records the provider, the argument list passed to makewand, and
a Chinese label. agy is pinned to a Gemini model: its own catalog also offers
Claude Opus, and that model spends credits.

Starting tier, applied in this order (a later rule wins):

- difficulty: mechanical → 1, normal → 2, hard → 3
- task type test or doc, and the difficulty is not hard: press down to
  tier 1 (cheapest adequate first; escalate if verification fails); hard is not pressed
- task type review and difficulty hard: start at 3
- risk high: always start at 3

``escalate(tier)`` returns the next tier, or None when the tier is already 3.

Override file shape (UTF-8 JSON object). Keys that are present replace that
tier's whole candidate list; omitted tiers keep the built-in list::

    {
      "1": [
        {"provider": "agy", "args": ["--model", "gemini-3.8-flash-medium"], "label": "便宜快 · Gemini Flash"}
      ]
    }

Legal task types are review, implement, test, research, doc.
Legal difficulties are mechanical, normal, hard.
Legal risks are low, high.
An illegal value or a malformed file raises ValueError and names the legal values.

Full catalog
------------
``build_full_catalog(discovered)`` expands every provider model × reasoning
effort into one candidate. ``discovered`` maps provider → model → efforts and
is what discovery already knows how to read (see
``makewand.discovery.discover_model_efforts``). A missing provider, or a model
with no listed efforts, falls back to the built-in defaults below.

``candidates(tier)`` is unchanged (the slim table). ``candidates(tier,
full=True)`` returns this tier of the full catalog. Legacy models are in the
catalog but left out of that list, and out of ``select(..., full=True)``,
unless ``include_legacy=True``.

Tier table (first match wins; effort bands are low=none/minimal/low,
medium=medium, high=high/xhigh/max/ultra). Reasons:

codex
    cheap (luna/mini/nano/reserve), any effort → 1
        The cheap model stays on the cheap tier at every depth.
    middle (sol/terra), low → 1; medium → 2; high → 3
        sol medium is the workhorse; sol high/xhigh is deep; low effort is cheap.
    strong (astra/ultra), low → 1; medium or high → 3
        astra is the frontier model, so anything above low reasoning is tier 3.
        Low reasoning still starts cheap because it barely spends thinking quota.
claude
    cheap (haiku), any effort → 1
    middle (sonnet), low → 1; medium or high → 2
        Sonnet high stays on the middle tier. Tier 3 is opus/fable high/max.
    strong (opus) / fable, low → 1; medium → 2; high → 3
        fable sorts after opus: it draws the subscription down faster.
agy  (suffixed model id, no ``--effort``; credits are forced off, so a proxied
      opus is allowed and simply fails when that pool is empty)
    flash, low or medium → 1; high → 2
    argon, low → 1; medium → 2; high → 3
    proxied haiku, any effort → 1
    proxied sonnet, low → 1; medium or high → 2
    proxied opus/pro, low → 1; medium → 2; high → 3
        ``claude-opus-5-5-high`` is the tier-3 proxied combo.
grok
    build-fast, any effort → 1
        It is the fast variant, but it costs about twice grok-4.7, so it sorts
        after grok-4.7 at low effort.
    grok-4.7 (and other grok ids), low → 1; medium → 2; high → 3
muse
    spark, low → 1; medium or high → 2
        Spark high stays middle. Tier 3 is wonder's high reasoning.
    wonder, low → 1; medium → 2; high → 3
anything else
    low → 1; medium → 2; high → 3

Legacy (kept, not ranked unless asked): codex major < 6 (gpt-5.6 and older),
claude major ≤ 4, gemini major < 4 (gemini 3.x), grok older than 4.7, muse
spark older than 1.3. The same tier rules apply; ``legacy`` is an extra flag.

Within a tier, cheaper subscription use comes first. agy Gemini is the
abundant pool, then grok, muse, codex (weekly gate 90), claude (weekly gate
85, so scarcer). Inside a provider, the cheaper family comes first (haiku
before sonnet before opus before fable; luna before sol before astra), then
the shallower effort. Proxied agy models (claude/gpt on agy) sort after the
native subscriptions because that pool fails closed when it is empty. Legacy
rows sort after every current row in the same tier.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Mapping, Optional, Sequence, Union

TASK_TYPES = ("review", "implement", "test", "research", "doc")
DIFFICULTIES = ("mechanical", "normal", "hard")
RISKS = ("low", "high")
TIERS = (1, 2, 3)

# Provider → model family. Order is the order named in the design.
PROVIDER_FAMILIES = {
    "codex": "openai",
    "claude": "anthropic",
    "agy": "google",
    "grok": "xai",
    "muse": "meta",
}

# Provider → the lineage of its own models, as discovery.model_family names it.
# agy also carries other vendors' models (claude-*, gpt-oss); route --not/--self
# compare a candidate's model with this so such a row counts as that vendor's.
PROVIDER_MODEL_FAMILIES = {
    "codex": "gpt",
    "claude": "claude",
    "agy": "gemini",
    "grok": "grok",
    "muse": "muse",
}

_DIFFICULTY_TIER = {"mechanical": 1, "normal": 2, "hard": 3}
# Pressed-down map for test/doc when the difficulty is not hard.
_PRESSED_TIER = {"mechanical": 1, "normal": 1}
_CANDIDATE_KEYS = ("provider", "args", "label")
_TIER_KEYS = ("1", "2", "3")


@dataclass(frozen=True)
class TierCandidate:
    """One ordered dispatch candidate inside a tier.

    The slim table only fills provider, args and label. Full-catalog rows also
    fill model, effort, ``candidate_id`` (``provider/model/effort``),
    ``tier_class`` and ``legacy``.
    """

    provider: str
    args: tuple
    label: str
    model: Optional[str] = None
    effort: Optional[str] = None
    candidate_id: Optional[str] = None
    tier_class: Optional[int] = None
    legacy: bool = False

    def __post_init__(self) -> None:
        args = self.args
        if isinstance(args, (str, bytes)) or not isinstance(args, (tuple, list)):
            raise ValueError("candidate args must be a sequence of strings")
        object.__setattr__(self, "args", tuple(args))


def _legal(allowed: Sequence[str]) -> str:
    return ", ".join(allowed)


def _choice(value, allowed: Sequence[str], kind: str) -> str:
    """Return ``value`` when it is one of ``allowed``; otherwise ValueError."""
    if not isinstance(value, str) or value not in allowed:
        raise ValueError(f"invalid {kind} {value!r}; legal values: {_legal(allowed)}")
    return value


def _require_tier(tier) -> int:
    if isinstance(tier, bool) or not isinstance(tier, int) or tier not in TIERS:
        raise ValueError(f"invalid tier {tier!r}; legal values: {_legal(str(item) for item in TIERS)}")
    return tier


def family(provider: str) -> str:
    """Return the model family for a dispatch provider."""
    if not isinstance(provider, str):
        raise ValueError(
            f"invalid provider {provider!r}; legal values: {_legal(PROVIDER_FAMILIES)}"
        )
    key = provider.strip().lower()
    try:
        return PROVIDER_FAMILIES[key]
    except KeyError:
        raise ValueError(
            f"invalid provider {provider!r}; legal values: {_legal(PROVIDER_FAMILIES)}"
        ) from None


def _reject_agy_credits(candidate: TierCandidate, where: str) -> None:
    """agy must name a Gemini model and must not name Claude or Opus."""
    if candidate.provider != "agy":
        return
    blob = " ".join(candidate.args).lower()
    if "claude" in blob or "opus" in blob or "gemini" not in blob:
        raise ValueError(
            f"{where}: agy must use a fixed gemini model and must not name claude or opus"
        )


def _builtin_rows() -> dict:
    rows = {
        1: (
            TierCandidate("agy", ("--model", "gemini-3.8-flash-medium"), "便宜快 · Gemini Flash"),
            TierCandidate("codex", ("--tier", "fast", "--effort", "low"), "便宜快 · 低推理"),
            TierCandidate("grok", ("--tier", "fast", "--effort", "low"), "便宜快 · 低推理"),
            TierCandidate("claude", ("--tier", "fast", "--effort", "low"), "便宜快 · 低推理"),
        ),
        2: (
            TierCandidate("codex", ("--tier", "standard", "--effort", "medium"), "中档 · 中等推理"),
            TierCandidate("agy", ("--model", "gemini-4-argon-medium"), "中档 · Gemini Argon"),
            TierCandidate("grok", ("--tier", "standard", "--effort", "medium"), "中档 · 中等推理"),
            TierCandidate("claude", ("--tier", "standard", "--effort", "medium"), "中档 · 中等推理"),
            TierCandidate("muse", ("--tier", "standard"), "中档"),
        ),
        3: (
            TierCandidate("codex", ("--tier", "deep", "--effort", "high"), "强档 · 深推理"),
            TierCandidate("agy", ("--model", "gemini-4-argon-high"), "强档 · Gemini Argon"),
            TierCandidate("claude", ("--tier", "deep", "--effort", "high"), "强档 · 深推理"),
            TierCandidate("grok", ("--tier", "deep", "--effort", "high"), "强档 · 深推理"),
        ),
    }
    for tier, items in rows.items():
        for index, item in enumerate(items):
            _reject_agy_credits(item, f"built-in tier {tier} candidate {index}")
    return rows


_BUILTIN = _builtin_rows()


def candidates(
    tier: int,
    catalog: Optional[Mapping[int, Sequence[TierCandidate]]] = None,
    *,
    full: bool = False,
    include_legacy: bool = False,
    discovered: Optional[Mapping] = None,
) -> tuple:
    """Return the ordered candidates for ``tier``.

    ``catalog`` defaults to the built-in slim table. Pass the dict from
    ``load_catalog`` to read an override. That path is unchanged.

    ``full=True`` returns this tier of ``build_full_catalog(discovered)``
    instead. The slim ``catalog`` override is not applied. Legacy rows are
    omitted unless ``include_legacy`` is true.
    """
    tier = _require_tier(tier)
    if full:
        rows = build_full_catalog(discovered)[tier]
        if not include_legacy:
            rows = tuple(item for item in rows if not item.legacy)
        return rows
    table = _BUILTIN if catalog is None else catalog
    try:
        rows = table[tier]
    except (KeyError, TypeError) as error:
        raise ValueError(f"invalid tier {tier!r}; legal values: {_legal(str(item) for item in TIERS)}") from error
    return tuple(rows)


def load_catalog(path: Optional[Union[str, Path]] = None) -> dict:
    """Return the tier table, with tiers listed in ``path`` replaced.

    ``path`` None returns the built-in table. A missing file, invalid JSON, or
    a candidate that does not match the schema raises ValueError. The returned
    dict is a new mapping; mutating it does not change later calls.
    """
    catalog = {tier: _BUILTIN[tier] for tier in TIERS}
    if path is None:
        return catalog
    catalog.update(_read_overrides(path))
    return catalog


def starting_tier(task_type: str, difficulty: str, risk: str = "low") -> int:
    """Return the tier a task starts on.

    ``risk`` defaults to low. Illegal task type, difficulty, or risk raises
    ValueError and lists the legal values. Inputs are checked before any rule
    is applied, so a high risk does not hide an illegal type or difficulty.
    """
    task_type = _choice(task_type, TASK_TYPES, "task type")
    difficulty = _choice(difficulty, DIFFICULTIES, "difficulty")
    risk = _choice(risk, RISKS, "risk")

    tier = _DIFFICULTY_TIER[difficulty]
    # test/doc is pressed down only when the work is not hard.
    if task_type in ("test", "doc") and difficulty != "hard":
        tier = _PRESSED_TIER[difficulty]
    if task_type == "review" and difficulty == "hard":
        tier = 3
    # High risk always starts on the strong tier, including pressed task types.
    if risk == "high":
        tier = 3
    return tier


def escalate(tier: int) -> Optional[int]:
    """Return tier + 1, or None when ``tier`` is already 3."""
    tier = _require_tier(tier)
    if tier == 3:
        return None
    return tier + 1


def _read_overrides(path: Union[str, Path]) -> dict:
    file_path = Path(path)
    where = f"routing catalog: {file_path}"
    try:
        text = file_path.read_text(encoding="utf-8")
    except FileNotFoundError as error:
        raise ValueError(f"{where}: file not found") from error
    except IsADirectoryError as error:
        raise ValueError(f"{where}: not a file") from error
    except UnicodeError as error:
        raise ValueError(f"{where}: not valid UTF-8: {error}") from error
    except OSError as error:
        raise ValueError(f"{where}: cannot read file: {error}") from error

    try:
        data = json.loads(text)
    except json.JSONDecodeError as error:
        raise ValueError(f"{where}: invalid JSON: {error}") from error
    if not isinstance(data, dict):
        raise ValueError(f"{where}: must be a JSON object; legal tier keys: {_legal(_TIER_KEYS)}")

    unknown = [key for key in data if key not in _TIER_KEYS]
    if unknown:
        raise ValueError(
            f"{where}: unknown tier {unknown[0]!r}; legal values: {_legal(_TIER_KEYS)}"
        )

    overrides = {}
    for key, raw_rows in data.items():
        tier = int(key)
        if not isinstance(raw_rows, list) or not raw_rows:
            raise ValueError(
                f"{where}: tier {key!r} must be a non-empty list of candidates"
            )
        parsed = []
        for index, item in enumerate(raw_rows):
            parsed.append(_parse_candidate(item, f"{where}: tier {key} candidate {index}"))
        overrides[tier] = tuple(parsed)
    return overrides


def _parse_candidate(item, where: str) -> TierCandidate:
    if not isinstance(item, dict):
        raise ValueError(f"{where}: candidate must be a JSON object with provider, args and label")
    missing = [key for key in _CANDIDATE_KEYS if key not in item]
    extra = [key for key in item if key not in _CANDIDATE_KEYS]
    if missing or extra:
        detail = []
        if missing:
            detail.append("missing " + ", ".join(missing))
        if extra:
            detail.append("unknown " + ", ".join(extra))
        raise ValueError(
            f"{where}: candidate keys must be provider, args, label ({'; '.join(detail)})"
        )

    provider = item["provider"]
    if not isinstance(provider, str) or provider.strip().lower() not in PROVIDER_FAMILIES:
        raise ValueError(
            f"{where}: invalid provider {provider!r}; legal values: {_legal(PROVIDER_FAMILIES)}"
        )
    provider = provider.strip().lower()

    args = item["args"]
    if (
        not isinstance(args, list)
        or not args
        or any(not isinstance(part, str) or part == "" for part in args)
    ):
        raise ValueError(f"{where}: args must be a non-empty list of strings")

    label = item["label"]
    if not isinstance(label, str) or label.strip() == "":
        raise ValueError(f"{where}: label must be a non-empty string")

    candidate = TierCandidate(provider, tuple(args), label)
    _reject_agy_credits(candidate, where)
    return candidate


# Effort bands used by the tier table. Order is also the canonical listing order.
_EFFORT_ORDER = ("none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra")
_EFFORT_COST = {name: index for index, name in enumerate(_EFFORT_ORDER)}
# The efforts `makewand <provider> --effort` and `makewand outcome --effort` accept.
# A discovered "none"/"minimal" would produce a command the CLI rejects.
_CLI_EFFORTS = frozenset(("low", "medium", "high", "xhigh", "max", "ultra"))
_LOW_BAND = {"none", "minimal", "low"}
_HIGH_BAND = {"high", "xhigh", "max", "ultra"}

# Cheaper subscription use sorts first. Codex's weekly gate is 90 and Claude's
# is 85, so Claude is scarcer and sorts later. Proxied models on agy fail
# closed when that pool is empty, so they sort after the native subscriptions.
_PROVIDER_COST = {"agy": 0, "grok": 20, "muse": 30, "codex": 40, "claude": 50}
_AGY_PROXY_COST = 60
_MODEL_COST = {
    "cheap": 0,
    "flash": 0,
    "spark": 0,
    "grok": 0,
    "fastvar": 12,  # grok-4.7-build-fast is about 2x the price of grok-4.7
    "middle": 10,
    "argon": 10,
    "wonder": 10,
    "other": 10,
    "strong": 20,
    "fable": 30,  # draws the subscription down faster than opus
}

# provider, family, effort band ("*" = any), tier, reason.
# First match wins. Kept as a table so the ranking rules stay readable.
TIER_CLASS_TABLE = (
    ("codex", "cheap", "*", 1, "luna / mini / nano / reserve：便宜模型，各推理深度都在第 1 档"),
    ("codex", "middle", "low", 1, "sol 的低推理消耗很少，归便宜档"),
    ("codex", "middle", "medium", 2, "gpt-6.1-sol 的 medium 是中档工作马"),
    ("codex", "middle", "high", 3, "sol 的 high/xhigh 推理深，进强档"),
    ("codex", "strong", "low", 1, "astra 低推理仍按便宜档，几乎不烧推理额度"),
    ("codex", "strong", "medium", 3, "astra 是前沿模型，中等及以上推理都在强档"),
    ("codex", "strong", "high", 3, "astra 的 high/xhigh/ultra 是强档"),
    ("claude", "cheap", "*", 1, "haiku 是便宜模型，各推理深度都在第 1 档"),
    ("claude", "middle", "low", 1, "sonnet 低推理按便宜档"),
    ("claude", "middle", "medium", 2, "sonnet 的 medium 是中档"),
    ("claude", "middle", "high", 2, "sonnet 的高推理仍是中档模型，强档留给 opus/fable"),
    ("claude", "strong", "low", 1, "opus 低推理按便宜档"),
    ("claude", "strong", "medium", 2, "opus 的 medium 还不是 high/max"),
    ("claude", "strong", "high", 3, "opus 的 high/max 是强档"),
    ("claude", "fable", "low", 1, "fable 低推理按便宜档"),
    ("claude", "fable", "medium", 2, "fable 的 medium 还不是 high/max"),
    ("claude", "fable", "high", 3, "fable 的 high/max 是强档，且比 opus 更吃额度"),
    ("agy", "flash", "low", 1, "gemini flash 的 low 是便宜档"),
    ("agy", "flash", "medium", 1, "gemini flash 的 medium 仍是便宜档"),
    ("agy", "flash", "high", 2, "flash-high 比 flash medium 深一档，但还不是 argon high"),
    ("agy", "argon", "low", 1, "argon 低推理按便宜档"),
    ("agy", "argon", "medium", 2, "gemini-4-argon-medium 是中档"),
    ("agy", "argon", "high", 3, "gemini-4-argon-high 是强档"),
    ("agy", "cheap", "*", 1, "agy 上的 haiku 类模型按便宜档"),
    ("agy", "middle", "low", 1, "agy 上的 sonnet 低推理按便宜档"),
    ("agy", "middle", "medium", 2, "agy 上的 sonnet medium 是中档"),
    ("agy", "middle", "high", 2, "agy 上的 sonnet 高推理仍是中档，强档留给 opus high"),
    ("agy", "strong", "low", 1, "agy 上的 opus/pro 低推理按便宜档"),
    ("agy", "strong", "medium", 2, "agy 上的 opus medium 是中档"),
    ("agy", "strong", "high", 3, "claude-opus-*-high 在 agy 上是强档；credits 已强制关闭，额度用尽直接失败"),
    ("agy", "fable", "low", 1, "agy 上的 fable 低推理按便宜档"),
    ("agy", "fable", "medium", 2, "agy 上的 fable medium 是中档"),
    ("agy", "fable", "high", 3, "agy 上的 fable high/max 是强档"),
    ("grok", "fastvar", "*", 1, "grok-4.7-build-fast 是快变体，归便宜档；排序在 grok-4.7 低推理之后（约 2 倍价钱）"),
    ("grok", "grok", "low", 1, "grok-4.7 低推理是便宜档"),
    ("grok", "grok", "medium", 2, "grok-4.7 medium 是中档"),
    ("grok", "grok", "high", 3, "grok-4.7 的 high/xhigh 是强档"),
    ("muse", "spark", "low", 1, "spark 的低推理是便宜档"),
    ("muse", "spark", "medium", 2, "spark 的中推理是中档"),
    ("muse", "spark", "high", 2, "spark 的高推理仍是较小模型，强档留给 wonder"),
    ("muse", "wonder", "low", 1, "wonder 低推理按便宜档"),
    ("muse", "wonder", "medium", 2, "wonder 的中推理是中档"),
    ("muse", "wonder", "high", 3, "wonder 的高推理是强档"),
)

_TIER_LABEL = {1: "便宜快", 2: "中档", 3: "强档"}

# Used when discovery does not list the provider. agy values are base ids;
# build_full_catalog appends the effort suffix and does not pass --effort.
_BUILTIN_MODELS = {
    "codex": {
        "gpt-6-luna": ("low", "medium", "high"),
        "gpt-6.1-sol": ("low", "medium", "high", "xhigh"),
        "gpt-6-sol": ("low", "medium", "high", "xhigh"),
        "gpt-6-astra": ("low", "medium", "high", "xhigh", "ultra"),
        "gpt-5.6": ("low", "medium", "high", "xhigh"),
        "gpt-5.6-luna": ("low", "medium", "high"),
        "gpt-5.6-sol": ("low", "medium", "high", "xhigh"),
    },
    "claude": {
        "claude-haiku-5-5": ("low", "medium", "high", "xhigh", "max"),
        "claude-sonnet-5": ("low", "medium", "high", "xhigh", "max"),
        "claude-opus-5-5": ("low", "medium", "high", "xhigh", "max"),
        "claude-fable-5-1": ("low", "medium", "high", "xhigh", "max"),
        "claude-haiku-4-5-20251001": ("low", "medium", "high", "xhigh", "max"),
        "claude-sonnet-4-6": ("low", "medium", "high", "xhigh", "max"),
        "claude-opus-4-6": ("low", "medium", "high", "xhigh", "max"),
    },
    "agy": {
        "gemini-4-argon": ("low", "medium", "high"),
        "gemini-3.8-flash": ("low", "medium", "high"),
        "gemini-3.1-pro": ("low", "high"),
        "claude-opus-5-5": ("low", "medium", "high"),
        "claude-sonnet-5-5": ("low", "medium", "high"),
        "gpt-oss-120b": ("medium",),
    },
    "grok": {
        "grok-4.7": ("low", "medium", "high", "xhigh"),
        "grok-4.7-build-fast": ("low", "medium", "high", "xhigh"),
        "grok-4.6": ("low", "medium", "high"),
        "grok-4.5": ("low", "medium", "high"),
    },
    "muse": {
        "muse-spark-1.3": ("low", "medium", "high", "xhigh", "max"),
        "muse-spark-1.3-contributor": ("low", "medium", "high", "xhigh", "max"),
        "muse-spark-1.2": ("low", "medium", "high", "xhigh", "max"),
        "muse-wonder": ("low", "medium", "high", "xhigh", "max"),
    },
}

_PROVIDER_DEFAULT_EFFORTS = {
    "codex": ("low", "medium", "high", "xhigh"),
    "claude": ("low", "medium", "high", "xhigh", "max"),
    "agy": ("low", "medium", "high"),
    "grok": ("low", "medium", "high", "xhigh"),
    "muse": ("low", "medium", "high", "xhigh", "max"),
}

_AGY_SUFFIXES = ("xhigh", "ultra", "medium", "high", "low", "max", "minimal")


def _split_effort_suffix(model_id: str):
    """Split a trailing effort suffix. Same order as discovery._split_agy_suffix.

    ``-xhigh`` is checked before ``-high`` so the longer suffix wins.
    """
    if not isinstance(model_id, str) or not model_id:
        return model_id or "", None
    lower = model_id.lower()
    for eff in _AGY_SUFFIXES:
        if lower.endswith("-" + eff):
            return model_id[: -len(eff) - 1], eff
    return model_id, None


def _effort_band(effort: str) -> str:
    if effort in _LOW_BAND:
        return "low"
    if effort == "medium":
        return "medium"
    return "high"


def _model_family(provider: str, model: str) -> str:
    name = (model or "").lower()
    if provider == "agy":
        name, _suffix = _split_effort_suffix(name)
    tokens = set(re.split(r"[^a-z0-9]+", name))
    if "build-fast" in name:
        return "fastvar"
    if tokens & {"fable", "mythos"}:
        return "fable"
    if "flash" in tokens:
        return "flash"
    if "argon" in tokens:
        return "argon"
    if "wonder" in tokens:
        return "wonder"
    if "spark" in tokens:
        return "spark"
    if tokens & {"haiku", "luna", "mini", "nano", "reserve"}:
        return "cheap"
    if tokens & {"opus", "astra", "ultra", "pro"}:
        return "strong"
    if tokens & {"sonnet", "sol", "terra"}:
        return "middle"
    if provider == "grok":
        return "grok"
    return "other"


def _claude_major(name: str):
    match = re.search(r"claude-(?:haiku|sonnet|opus|fable|mythos)-(\d+)", name)
    if match is None:
        return None
    return int(match.group(1))


def _gpt_major(name: str):
    match = re.search(r"(?:^|[^a-z])gpt-(\d+)", name)
    if match is None:
        return None
    return int(match.group(1))


def _first_version(name: str):
    match = re.search(r"(\d+)(?:\.(\d+))?", name)
    if match is None:
        return None
    minor = int(match.group(2)) if match.group(2) is not None else 0
    return (int(match.group(1)), minor)


def _is_legacy(provider: str, model: str) -> bool:
    """Old model lines stay in the catalog but do not rank unless asked.

    codex major < 6 (gpt-5.6 and older), claude major ≤ 4, gemini major < 4,
    grok older than 4.7, muse spark older than 1.3.
    """
    name = (model or "").lower()
    if provider == "agy":
        name, _suffix = _split_effort_suffix(name)
        if name.startswith("claude-"):
            major = _claude_major(name)
            return major is not None and major <= 4
        if name.startswith("gpt-"):
            major = _gpt_major(name)
            return major is not None and major < 6
        if name.startswith("gemini-") or name.startswith("google-"):
            version = _first_version(name)
            return version is not None and version[0] < 4
        return False
    if provider == "codex":
        major = _gpt_major(name)
        return major is not None and major < 6
    if provider == "claude":
        major = _claude_major(name)
        return major is not None and major <= 4
    if provider == "grok":
        version = _first_version(name)
        return version is not None and version < (4, 7)
    if provider == "muse" and "spark" in name:
        version = _first_version(name)
        return version is not None and version < (1, 3)
    return False


def _is_native_gemini(model: str) -> bool:
    name = (model or "").lower()
    return name.startswith("gemini-") or name.startswith("google-") or name == "gemini"


def _tier_class(provider: str, model: str, effort: str) -> int:
    family_name = _model_family(provider, model)
    band = _effort_band(effort)
    for rule_provider, rule_family, rule_band, tier, _reason in TIER_CLASS_TABLE:
        if rule_provider != provider or rule_family != family_name:
            continue
        if rule_band not in (band, "*"):
            continue
        return tier
    return {"low": 1, "medium": 2, "high": 3}[band]


def _price_rank(provider: str, model: str):
    if provider == "agy" and not _is_native_gemini(model):
        provider_cost = _AGY_PROXY_COST
    else:
        provider_cost = _PROVIDER_COST.get(provider, 70)
    return provider_cost, _MODEL_COST.get(_model_family(provider, model), 10)


def _cost_key(item: TierCandidate):
    provider_cost, model_cost = _price_rank(item.provider, item.model or "")
    return (
        1 if item.legacy else 0,
        provider_cost,
        model_cost,
        _EFFORT_COST.get(item.effort or "", 99),
        item.model or "",
        item.effort or "",
        item.provider,
    )


def _clean_efforts(efforts) -> tuple:
    if efforts is None:
        return ()
    if isinstance(efforts, str):
        efforts = (efforts,)
    found = set()
    for item in efforts:
        if not isinstance(item, str):
            continue
        name = item.strip().lower()
        if name in _CLI_EFFORTS:
            found.add(name)
    return tuple(name for name in _EFFORT_ORDER if name in found)


def _bare_model_id(text: str) -> str:
    name = text.strip()
    if " (" in name:
        name = name.split(" (", 1)[0].strip()
    return name


def _efforts_for(provider: str, model: str, efforts) -> tuple:
    cleaned = _clean_efforts(efforts)
    if cleaned:
        return cleaned
    if provider == "agy":
        _base, suffix = _split_effort_suffix(model)
        if suffix:
            return (suffix,)
    builtin = _BUILTIN_MODELS.get(provider, {}).get(model)
    if builtin:
        return tuple(builtin)
    return _PROVIDER_DEFAULT_EFFORTS[provider]


def _coerce_provider_models(provider: str, value) -> dict:
    """Normalize one provider's discovery blob to {model: efforts-or-empty}."""
    models = {}

    def add(model, efforts):
        model = _bare_model_id(model) if isinstance(model, str) else ""
        if not model or model.startswith("-"):
            return
        models[model] = efforts

    if isinstance(value, Mapping) and isinstance(value.get("available"), (list, tuple)):
        for item in value["available"]:
            if isinstance(item, str):
                add(item, ())
        return models
    if isinstance(value, Mapping):
        for model, efforts in value.items():
            if not isinstance(model, str) or model in ("current_default", "source", "default_source"):
                continue
            add(model, efforts)
        return models
    if isinstance(value, (list, tuple)):
        for item in value:
            if isinstance(item, str):
                add(item, ())
            elif isinstance(item, (list, tuple)) and item and isinstance(item[0], str):
                add(item[0], item[1] if len(item) > 1 else ())
        return models
    return {}


def _resolve_model_efforts(discovered: Optional[Mapping]) -> dict:
    """Provider → model → efforts. Discovered providers replace that provider's defaults."""
    resolved = {
        provider: {model: tuple(efforts) for model, efforts in models.items()}
        for provider, models in _BUILTIN_MODELS.items()
    }
    if discovered is None:
        return resolved
    if not isinstance(discovered, Mapping):
        raise ValueError(
            "discovered model efforts must be a mapping of provider → model → efforts; "
            f"legal providers: {_legal(PROVIDER_FAMILIES)}"
        )
    for raw_provider, value in discovered.items():
        if not isinstance(raw_provider, str):
            continue
        provider = raw_provider.strip().lower()
        if provider not in PROVIDER_FAMILIES:
            continue
        models = _coerce_provider_models(provider, value)
        if not models:
            continue
        resolved[provider] = {
            model: _efforts_for(provider, model, efforts) for model, efforts in models.items()
        }
    return resolved


def _expand_combos(provider: str, model: str, efforts) -> list:
    """Yield (model id passed to the CLI, effort) pairs.

    agy always uses the suffixed id and never a separate effort flag.
    """
    if provider == "agy":
        base, suffix = _split_effort_suffix(model)
        if suffix:
            return [(model, suffix)]
        return [(f"{base}-{effort}", effort) for effort in efforts]
    return [(model, effort) for effort in efforts]


def _make_full_candidate(provider: str, model: str, effort: str) -> TierCandidate:
    if provider == "agy":
        args = ("--model", model)
    else:
        args = ("--model", model, "--effort", effort)
    tier = _tier_class(provider, model, effort)
    legacy = _is_legacy(provider, model)
    prefix = "旧版 · " if legacy else ""
    label = f"{prefix}{_TIER_LABEL[tier]} · {provider} {model} · {effort}"
    return TierCandidate(
        provider=provider,
        args=args,
        label=label,
        model=model,
        effort=effort,
        candidate_id=f"{provider}/{model}/{effort}",
        tier_class=tier,
        legacy=legacy,
    )


def build_full_catalog(discovered: Optional[Mapping] = None) -> dict:
    """Return ``{1: tuple, 2: tuple, 3: tuple}`` covering every model × effort.

    Each tier is ordered cheaper-first (see the module docstring). Legacy
    combinations are included and flagged; they sort after the current rows.
    ``discovered`` None, or a provider with no models, uses ``_BUILTIN_MODELS``.
    A provider that discovery did list replaces that provider's defaults, so a
    stale built-in id is not mixed into a live catalog.
    """
    efforts = _resolve_model_efforts(discovered)
    buckets = {tier: {} for tier in TIERS}
    for provider in ("agy", "grok", "muse", "codex", "claude"):
        for model, model_efforts in efforts.get(provider, {}).items():
            for full_model, effort in _expand_combos(provider, model, model_efforts):
                candidate = _make_full_candidate(provider, full_model, effort)
                # First occurrence wins if discovery lists both the base and the suffixed id.
                buckets[candidate.tier_class].setdefault(candidate.candidate_id, candidate)
    return {
        tier: tuple(sorted(rows.values(), key=_cost_key))
        for tier, rows in buckets.items()
    }
