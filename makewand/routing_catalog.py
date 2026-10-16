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
"""

from __future__ import annotations

import json
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

_DIFFICULTY_TIER = {"mechanical": 1, "normal": 2, "hard": 3}
# Pressed-down map for test/doc when the difficulty is not hard.
_PRESSED_TIER = {"mechanical": 1, "normal": 1}
_CANDIDATE_KEYS = ("provider", "args", "label")
_TIER_KEYS = ("1", "2", "3")


@dataclass(frozen=True)
class TierCandidate:
    """One ordered dispatch candidate inside a tier."""

    provider: str
    args: tuple
    label: str

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


def candidates(tier: int, catalog: Optional[Mapping[int, Sequence[TierCandidate]]] = None) -> tuple:
    """Return the ordered candidates for ``tier``.

    ``catalog`` defaults to the built-in table. Pass the dict from
    ``load_catalog`` to read an override.
    """
    tier = _require_tier(tier)
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
