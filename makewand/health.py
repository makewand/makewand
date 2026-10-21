"""
Health probing, quota monitoring, and status cache management.
"""

import os
import re
import json
import math
import stat
import warnings
from makewand import filelock as fcntl
import uuid
import tempfile
from datetime import datetime, timedelta, timezone
from functools import lru_cache
from typing import Dict, Any, Optional, Iterator, Tuple
from pathlib import Path
from makewand.config import (
    STATUS_CACHE_FILE,
    LEGACY_TRIO_CACHE,
    ensure_config_dir,
    API_ONLY_ENGINES,
)
from makewand.providers.base import check_cli_installed, run_subprocess
from makewand.providers.agy import parse_agy_quota
from makewand.providers.claude import parse_claude_quota
from makewand.providers.codex import parse_codex_quota
from makewand.providers.muse import parse_muse_quota
from makewand.providers.grok import parse_grok_quota

DEFAULT_CACHE = {
    "agy": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""},
    "claude": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""},
    "codex": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""},
    "muse": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""},
    "grok": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""},
    "local": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""}
}

STATUS_LOCK_FILE = STATUS_CACHE_FILE.parent / ".status.lock"

# A cached "healthy"/"warning" verdict older than this is no longer evidence of
# anything: it is treated as neutral and the user is asked to re-probe.
STATUS_STALE_SECONDS = 6 * 3600
# Cache writers share this host's clock; an updated_at further ahead of now than
# this allowance is damaged, not a fresh observation, and counts as no timestamp.
STATUS_CLOCK_SKEW_SECONDS = 5 * 60
# Failure verdicts self-heal back to neutral ("unknown") after a TTL so that one
# probe timeout or one failed login check can never exclude a provider forever.
STATUS_TTL_SECONDS = {
    "error": 30 * 60,
    "needs_auth": 30 * 60,
}
# Geographic/account-level blocks rarely change within minutes; re-check later.
REGION_BLOCK_TTL_SECONDS = 6 * 3600
# The real agy probe is pinned to a native Gemini model: probing with an
# Opus/Sonnet proxy model would spend the wrong quota and prove nothing
# about the native Gemini path.
AGY_PROBE_MODEL = "gemini-3.8-flash-low"
# Upper bound on an entry's own "ttl_seconds" (the longest any writer sets is
# REGION_BLOCK_TTL_SECONDS): a damaged value cannot exclude a provider forever.
FAILURE_TTL_MAX_SECONDS = 24 * 3600


QUOTA_RESERVE_ENV = "MAKEWAND_QUOTA_RESERVE_PERCENT"
DEFAULT_QUOTA_RESERVE_PERCENT = 8.0
QUOTA_RESERVE_ERROR = (
    "MAKEWAND_QUOTA_RESERVE_PERCENT must be a finite number between 0 and 100; "
    "using the default 8% reserve"
)


def _quota_reserve_setting():
    """An explicit per-process buffer; invalid input cannot lower the default."""
    raw = os.environ.get(QUOTA_RESERVE_ENV)
    if raw is None:
        return DEFAULT_QUOTA_RESERVE_PERCENT, None
    try:
        reserve = float(raw)
        if not math.isfinite(reserve) or not 0 <= reserve <= 100:
            raise ValueError("invalid reserve")
    except (TypeError, ValueError, OverflowError):
        # Never echo the environment value, which need not be safe to log.
        warnings.warn(QUOTA_RESERVE_ERROR, RuntimeWarning, stacklevel=2)
        return DEFAULT_QUOTA_RESERVE_PERCENT, QUOTA_RESERVE_ERROR
    return reserve, None


def _quota_status(remaining, reserve, *, healthy_at=25, inclusive=True):
    # Disabling the buffer never treats actually exhausted quota as usable.
    if remaining <= 0 or remaining < reserve:
        return "limited"
    healthy = remaining >= healthy_at if inclusive else remaining > healthy_at
    return "healthy" if healthy else "warning"


def _quota_reserve_metadata(reserve, error):
    return {"reserve_percent": reserve, "reserve_error": error}


def _quota_reserve_description(description, reserve, error):
    # Keep the unset/default description intact; explicit policy is visible.
    if QUOTA_RESERVE_ENV in os.environ:
        description += f" · 配额保留阈值: {reserve:g}%"
    if error:
        description += f" · {error}"
    return description


def _is_quota_reserve_gate(info):
    # Migrate only our exact legacy buffer verdict, never a provider 429/auth
    # verdict. Subsequent real dispatch/probe updates replace the whole entry.
    reason = str(info.get("reason") or "")
    return info.get("quota_reserve_gate") is True or bool(
        re.match(r"^保护阈值拦截 \(剩余 \d+(?:\.\d+)?% < 8%\):", reason)
        or re.fullmatch(r"配额保护阈值拦截 \((?:剩余 \d+(?:\.\d+)?% < \d+(?:\.\d+)?%|剩余额度耗尽或无法确认)\)", reason)
    )

REAUTH_HINTS = {
    "claude": "运行 'claude' 并在会话内执行 /login",
    "codex": "运行 'codex login'",
    "muse": "运行 'muse login'",
    "grok": "运行 'grok' 按提示登录，或设置 XAI_API_KEY",
    "agy": "运行 'agy' 按提示登录 Google 账号",
}


def get_reauth_hint(provider: str) -> str:
    """Actionable re-login instruction for a provider in needs_auth state."""
    name = (provider or "").lower().strip()
    step = REAUTH_HINTS.get(name, f"重新登录 {name} CLI 或配置其 API Key")
    return f"{name} 需要重新登录：{step}，完成后运行 'makewand probe' 刷新状态"


def _parse_timestamp(value: Any) -> Optional[datetime]:
    if not value or not isinstance(value, str):
        return None
    try:
        clean = value.replace("Z", "+00:00") if value.endswith("Z") else value
        return datetime.fromisoformat(clean)
    except (TypeError, ValueError):
        return None


# ---------------------------------------------------------------------------
# Quota reset times. One parser serves the status cache (is_reset_time_passed)
# and pacing (parse_reset_time_to_seconds_left), so both read a provider's
# "resets ..." text the same way.
# ---------------------------------------------------------------------------
_ANSI_ESCAPE_RE = re.compile(r"\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]")
_RESET_MONTHS = ("jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec")
_RESET_MONTH = (r"(?:jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|june?|july?|aug(?:ust)?"
                r"|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)")
# "Claude AI usage limit reached|1760000000": Unix seconds after a pipe.
_RESET_EPOCH_RE = re.compile(r"(?:^|\|)\s*(\d{10})(?:\.\d+)?\s*$")
_RESET_ISO_RE = re.compile(
    r"(?<![\d.])(\d{4})-(\d{1,2})-(\d{1,2})"
    r"(?:[T ](\d{1,2}):(\d{2})(?::(\d{2})(?:[.,](\d{1,6})\d*)?)?\s*(Z|[+-]\d{2}(?::?\d{2})?)?)?(?![\d.])",
    re.IGNORECASE)
_RESET_NAMED_DATE_RE = re.compile(
    rf"\b(?P<mon>{_RESET_MONTH})\.?\s+(?P<day>\d{{1,2}})(?:st|nd|rd|th)?\b(?:\s*,?\s*(?P<year>\d{{4}})\b)?"
    rf"|\b(?P<day2>\d{{1,2}})(?:st|nd|rd|th)?\s+(?:of\s+)?(?P<mon2>{_RESET_MONTH})\b\.?(?:\s*,?\s*(?P<year2>\d{{4}})\b)?",
    re.IGNORECASE)
# "10-04 10:40": month-day in ISO order without the year; only with a clock time.
_RESET_NUMERIC_DATE_RE = re.compile(r"(?<![\w/:.-])(\d{1,2})-(\d{1,2})(?![\w/:.-])")
_RESET_CLOCK_RE = re.compile(
    r"(?<![\w:.])(?P<h>\d{1,2})(?::(?P<m>\d{2}))?(?::(?P<s>\d{2}))?\s*(?P<ap>[ap])\.?\s?m\b\.?"
    r"|(?<![\w:.])(?P<H>\d{1,2}):(?P<M>\d{2})(?::(?P<S>\d{2}))?(?![\d:])",
    re.IGNORECASE)
_RESET_DURATION_RE = re.compile(
    r"(?<![\w.-])((?:\d+(?:\.\d+)?\s*(?:days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s|天|小时|分钟|秒)"
    r"(?![a-z])\s*,?\s*(?:and\s+)?)+)",
    re.IGNORECASE)
_RESET_ZONE_RE = re.compile(
    r"\b(?P<iana>[A-Z][A-Za-z]+(?:/[A-Za-z][A-Za-z0-9_+-]*){1,2})"
    r"|\b(?P<fixed>UTC|GMT)(?:\s*(?P<sign>[+-])\s*(?P<oh>\d{1,2})(?::?(?P<om>\d{2}))?)?\b")


def _as_local_aware(value: datetime) -> datetime:
    """Naive timestamps in the cache and in provider output are local wall-clock time."""
    return value.astimezone() if value.tzinfo is None else value


def _reset_zone(text: str):
    """The zone a reset string names ("(Asia/Shanghai)", "UTC+8") and the text without it."""
    for match in _RESET_ZONE_RE.finditer(text):
        if match.group("iana"):
            try:
                from zoneinfo import ZoneInfo
                zone = ZoneInfo(match.group("iana"))
            except Exception:  # unknown key ("Pro/Max"), or no tz database (Windows without tzdata)
                continue
        else:
            minutes = 0
            if match.group("sign"):
                minutes = int(match.group("oh")) * 60 + int(match.group("om") or 0)
                if minutes > 14 * 60:
                    continue
                minutes = -minutes if match.group("sign") == "-" else minutes
            zone = timezone(timedelta(minutes=minutes))
        return zone, f"{text[:match.start()]} {text[match.end():]}"
    return None, text


def _reset_moment(year, month, day, hour, minute, second, zone) -> datetime:
    naive = datetime(year, month, day, hour, minute, second)
    return naive.replace(tzinfo=zone) if zone is not None else naive.astimezone()


def _first_clock(text: str, exclude=None) -> Optional[Tuple[int, int, int]]:
    for match in _RESET_CLOCK_RE.finditer(text):
        if exclude and match.start() < exclude[1] and match.end() > exclude[0]:
            continue
        if match.group("ap"):
            hour, minute, second = int(match.group("h")), int(match.group("m") or 0), int(match.group("s") or 0)
            if not 1 <= hour <= 12:
                continue
            hour = hour % 12 + (12 if match.group("ap").lower() == "p" else 0)
        else:
            hour, minute, second = int(match.group("H")), int(match.group("M")), int(match.group("S") or 0)
            if hour > 23:
                continue
        if minute <= 59 and second <= 59:
            return hour, minute, second
    return None


def _nearest_occurrence(month, day, hour, minute, second, zone, anchor: datetime) -> Optional[datetime]:
    """A month-day without a year is the occurrence closest to when it was observed.

    Reset windows are days or weeks long, so the nearest candidate is the right
    year: a reset that passed an hour ago is in the past, not 11 months ahead,
    and "Jan 1" observed on Dec 31 is next year.
    """
    base_year = (anchor.astimezone(zone) if zone is not None else anchor.astimezone()).year
    best = None
    for year in (base_year - 1, base_year, base_year + 1):
        try:
            candidate = _reset_moment(year, month, day, hour, minute, second, zone)
        except ValueError:  # Feb 29 outside leap years
            continue
        if best is None or abs((candidate - anchor).total_seconds()) < abs((best - anchor).total_seconds()):
            best = candidate
    return best


def _next_clock_occurrence(hour, minute, second, zone, anchor: datetime) -> datetime:
    """A bare clock time is its first occurrence at or after the observation."""
    day = (anchor.astimezone(zone) if zone is not None else anchor.astimezone()).date()
    candidate = anchor
    for extra in (0, 1, 2):
        current = day + timedelta(days=extra)
        candidate = _reset_moment(current.year, current.month, current.day, hour, minute, second, zone)
        if candidate >= anchor:
            break
    return candidate


def _start_of_day(moment: datetime, zone) -> datetime:
    day = (moment.astimezone(zone) if zone is not None else moment.astimezone()).date()
    return _reset_moment(day.year, day.month, day.day, 0, 0, 0, zone)


def parse_reset_datetime(text: Any, observed_at: Any = None, now: Optional[datetime] = None) -> Optional[datetime]:
    """Absolute, timezone-aware moment a quota window resets, or None.

    Understands what the provider CLIs print: "Oct 20, 8pm (Asia/Shanghai)",
    "Oct 20th, 2026 8:00 PM", "8:30pm", "20:00", "10-04 10:40", ISO-8601,
    relative "in 3h 20m" / "2 hours", and "...limit reached|<unix seconds>". A
    trailing "(Area/City)" or "UTC+8" zone is honoured (zoneinfo), otherwise
    local time. Times without a date, durations, and dates without a year are
    anchored on when the message was observed (observed_at, else now). Without
    a usable observation (missing, unparseable, or outside the platform's time
    range) a bare clock time is today's occurrence, so a limit stored without a
    timestamp still heals once that time has passed.
    """
    if not isinstance(text, str) or not text.strip():
        return None
    s = _ANSI_ESCAPE_RE.sub("", text).strip()
    try:
        now_dt = _as_local_aware(now if isinstance(now, datetime) else datetime.now())
        observed = observed_at if isinstance(observed_at, datetime) else _parse_timestamp(observed_at)
        try:
            anchor = _as_local_aware(observed) if observed is not None else None
        except (ValueError, OverflowError, OSError):  # "0001-01-01T00:00:00", "9999-12-31T23:59:59"
            anchor = None
        epoch = _RESET_EPOCH_RE.search(s)
        if epoch:
            return datetime.fromtimestamp(int(epoch.group(1)), timezone.utc)
        zone, s = _reset_zone(s)

        iso = _RESET_ISO_RE.search(s)
        if iso:
            year, month, day = int(iso.group(1)), int(iso.group(2)), int(iso.group(3))
            if iso.group(4) is None:
                # "10:58 (2026-09-27)": a date with the clock time elsewhere.
                hour, minute, second = _first_clock(s, exclude=iso.span()) or (0, 0, 0)
                return _reset_moment(year, month, day, hour, minute, second, zone)
            naive = datetime(year, month, day, int(iso.group(4)), int(iso.group(5)), int(iso.group(6) or 0),
                             int((iso.group(7) or "0").ljust(6, "0")))
            offset = iso.group(8)
            if offset and offset.upper() == "Z":
                return naive.replace(tzinfo=timezone.utc)
            if offset:
                digits = offset[1:].replace(":", "")
                delta = timedelta(hours=int(digits[:2]), minutes=int(digits[2:4] or 0))
                return naive.replace(tzinfo=timezone(-delta if offset[0] == "-" else delta))
            return naive.replace(tzinfo=zone) if zone is not None else naive.astimezone()

        named = _RESET_NAMED_DATE_RE.search(s)
        numeric = None if named else _RESET_NUMERIC_DATE_RE.search(s)
        date_match = named or numeric
        if date_match:
            if named:
                month = _RESET_MONTHS.index((named.group("mon") or named.group("mon2"))[:3].lower()) + 1
                day = int(named.group("day") or named.group("day2"))
                year_text = named.group("year") or named.group("year2")
            else:
                month, day, year_text = int(numeric.group(1)), int(numeric.group(2)), None
            clock = _first_clock(s, exclude=date_match.span())
            if named or clock is not None:
                hour, minute, second = clock or (0, 0, 0)
                if year_text:
                    return _reset_moment(int(year_text), month, day, hour, minute, second, zone)
                return _nearest_occurrence(month, day, hour, minute, second, zone, anchor or now_dt)

        clock = _first_clock(s)
        if clock is not None:
            return _next_clock_occurrence(*clock, zone, anchor or _start_of_day(now_dt, zone))

        duration = _RESET_DURATION_RE.search(s)
        if duration:
            seconds = 0.0
            for amount, unit in re.findall(r"(\d+(?:\.\d+)?)\s*([a-z]+|天|小时|分钟|秒)", duration.group(1), re.IGNORECASE):
                unit = unit.lower()
                factor = (86400 if unit[0] in "d天" else 3600 if unit[0] in "h小" else
                          60 if unit[0] in "m分" else 1)
                seconds += float(amount) * factor
            return (anchor or now_dt) + timedelta(seconds=seconds)
    except (ValueError, OverflowError, OSError):
        return None
    return None


_RESET_PHRASE_RE = re.compile(
    r"\b(?:resets?|reset\s+time|renews?|try\s+again|retry|available\s+again)\b(?:\s+(?:at|on))?\s*:?\s*"
    r"(?P<when>(?:in\s+)?[^·•∙|\n]+)",
    re.IGNORECASE)


def extract_reset_phrase(text: str) -> Optional[str]:
    """The reset time a quota message announces, as printed.

    "Weekly limit reached · resets Oct 20, 8pm (Asia/Shanghai)" -> "Oct 20, 8pm
    (Asia/Shanghai)"; "Try again in 2 hours." -> "in 2 hours". The phrase ends
    at the next separator or sentence; only text parse_reset_datetime
    understands is returned, so callers can fall back to a default window.
    """
    for match in _RESET_PHRASE_RE.finditer(_ANSI_ESCAPE_RE.sub("", text or "")):
        when = re.split(r"\.\s+(?=[A-Z])|;|\s+or\s+|,\s+(?:or|and)\s+|\s+-\s+", match.group("when"))[0]
        when = when.strip().rstrip(".,;: ").strip()
        if when and parse_reset_datetime(when) is not None:
            return when
    return None


# ---------------------------------------------------------------------------
# Provider CLI messages. A quota, rate-limit, login or region failure is read
# only from a message the provider CLI prints itself, at the start of a line
# the CLI writes (find_cli_messages). The same words in the model's answer or
# in tool output relayed into a transcript ("gh: API rate limit exceeded",
# "npm ERR! 429", "429 passed", "expected 401 Unauthorized", "Too many requests
# from a single IP are now rejected") are task content and never disable a
# provider.
#
# The catalog is data, cli_message_catalog.json next to this module, which
# router/cli.go mirrors byte for byte; its "_doc" describes the format. It
# holds what each CLI really prints, taken from the strings of the installed
# binaries and bundles (Claude Code 2.1.296, codex-linux-x64 0.162.1, Grok
# Build 1.0.50, Muse Code 1.4.6, agy 1.3.2, Gemini CLI 0.63.0), plus the short
# forms existing callers rely on, and how the lines a CLI writes itself are
# told apart from the answer:
#   own_streams     where the CLI writes its own messages when stdout and
#                   stderr are captured apart (claude -p: its result text on
#                   stdout; codex exec, grok -p, agy -p, gemini -p: stderr;
#                   Muse Code's split is unknown, so both)
#   selection       "lines" (every own line), "final_report" (the error report
#                   the CLI ends with, for text that may also hold the answer)
#                   or "transcript" (codex exec's transcript sections, and the
#                   fatal error report its main ends with)
#   json_reports    the CLI prints JSON error reports over several lines
#                   (gemini --output-format json): each is read as one line
#   kinds           quota, rate, overload, auth and region patterns; a
#                   "<kind>.marked" pattern counts only behind an error marker
# Every pattern is RE2 compatible and matches at the start of normalize_cli_line()
# text, case-insensitively for ASCII letters only: Python compiles it with
# re.IGNORECASE | re.ASCII (cli_catalog_regex), Go with cliCatalogRegexp in
# router/cli.go, which anchors it (`^(?:` + p + `)`) and spells out both cases of
# each ASCII letter instead of using (?i), whose Unicode folding (the Kelvin sign
# for k, the long s for s) Python's re.ASCII does not do.
# ---------------------------------------------------------------------------
CLI_MESSAGE_CATALOG_FILE = Path(__file__).with_name("cli_message_catalog.json")


def cli_catalog_regex(pattern: str):
    """A catalog pattern as both readers compile it: case-insensitive, ASCII
    character classes, anchored at the start of the text by match()."""
    return re.compile(pattern, re.IGNORECASE | re.ASCII)


def _load_cli_catalog() -> Dict[str, Any]:
    with open(CLI_MESSAGE_CATALOG_FILE, encoding="utf-8") as handle:
        catalog = json.load(handle)
    for provider, entry in catalog["providers"].items():
        own, answer = entry["own_streams"], entry["answer_streams"]
        if any(stream not in ("stdout", "stderr") for stream in (*own, *answer)):
            raise ValueError(f"{provider}: unknown stream")
        selection = entry.get("selection", {})
        if selection.get("merged", "lines") == "lines" and answer:
            raise ValueError(f"{provider}: selection.merged 'lines' reads answer_streams")
        if selection.get("apart", "lines") == "lines" and set(own).intersection(answer):
            raise ValueError(f"{provider}: selection.apart 'lines' reads answer_streams")
    return catalog


_CLI_CATALOG = _load_cli_catalog()
_CLI_PROVIDERS: Dict[str, Dict[str, Any]] = _CLI_CATALOG["providers"]
CLI_MESSAGE_CATALOG: Dict[str, Dict[str, Tuple[str, ...]]] = {
    provider: {kind: tuple(patterns) for kind, patterns in entry["kinds"].items()}
    for provider, entry in _CLI_PROVIDERS.items()}
# Names a CLI prints in front of its own lines ("grok: ...", "muse: ...").
CLI_NAMES = {provider: tuple(entry["names"]) for provider, entry in _CLI_PROVIDERS.items()}
CLI_OWN_STREAMS = {provider: tuple(entry["own_streams"]) for provider, entry in _CLI_PROVIDERS.items()}

CLI_MESSAGE_MAX_CHARS = 2000
_APOSTROPHES = str.maketrans({"\u2019": "'", "\u2018": "'", "\u02bc": "'", "\u2032": "'"})
_WHITESPACE_RUN_RE = re.compile(r"\s+")
_CLI_LINES = {name: cli_catalog_regex(pattern) for name, pattern in _CLI_CATALOG["lines"].items()}
# Go glog diagnostics agy writes to stderr ("E1012 08:11:57.633062  188 errorreport.go:224] ...").
GLOG_HEADER_RE = _CLI_LINES["glog_header"]
_GLOG_ANYWHERE_RE = re.compile(r"(?m)^\s*(?:\x1b\[[0-9;]*m)?[IWEF]\d{4} +\d{2}:\d{2}:\d{2}\.")
_JSON_LINE_START = tuple('{}[]",')
_JSON_SCALAR_LINE_RE = re.compile(r"(?:-?\d[\d.eE+-]*|true|false|null),?$")
_FENCE_PREFIXES = ("```", "~~~")


def cli_catalog_entry(provider: str) -> Dict[str, Any]:
    """`provider`'s entry of the CLI message catalog (empty for unknown CLIs)."""
    return _CLI_PROVIDERS.get(provider, {})


def _normalize_bare(text: str) -> str:
    return _WHITESPACE_RUN_RE.sub(" ", text[:CLI_MESSAGE_MAX_CHARS].translate(_APOSTROPHES)).strip()


def _strip_ansi(text: str) -> str:
    return _ANSI_ESCAPE_RE.sub("", text) if "\x1b" in text else text


def normalize_cli_line(line: str) -> str:
    """A line as the catalog patterns read it: ANSI stripped, typographic
    apostrophes as "'", whitespace runs collapsed to one space."""
    return _normalize_bare(_strip_ansi((line or "")[:4 * CLI_MESSAGE_MAX_CHARS]))


def _bracket_depth_delta(text: str) -> int:
    depth, in_string, escaped = 0, False, False
    for char in text:
        if in_string:
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                in_string = False
        elif char == '"':
            in_string = True
        elif char in "{[":
            depth += 1
        elif char in "}]":
            depth -= 1
    return depth


def _json_body_line(stripped: str) -> bool:
    """A line a logged JSON body may consist of: blank, bracket- or quote-led, or a bare scalar."""
    return not stripped or stripped.startswith(_JSON_LINE_START) or bool(_JSON_SCALAR_LINE_RE.match(stripped))


def glog_entry_lines(lines, *, continuation: str = "colon") -> set:
    """Indexes of the glog diagnostics in `lines`: each header and its continuation.

    A logged JSON body continues while its brackets are open, on lines shaped
    like JSON (a bracket- or quote-led line, a bare scalar, a blank line). A
    header ending in ":" logs an error whose %v starts on the next line: with
    continuation="colon" (classification) the plain lines up to the next blank
    line, glog header or top-level line ("Error:", "warning:", "Caused by:")
    belong to it; with "value" (reading the answer) only the one line after it
    does, so the model's text right after a log line is never dropped.
    """
    skipped = set()
    depth, follow = 0, 0  # follow: continuation lines still allowed, -1 without limit
    for index, raw in enumerate(lines):
        line = _strip_ansi(raw).rstrip()
        stripped = line.strip()
        header = GLOG_HEADER_RE.match(stripped) if stripped[:1] in ("I", "W", "E", "F") else None
        if header:
            message = stripped[header.end():]
            depth = max(0, _bracket_depth_delta(message))
            follow = (-1 if continuation == "colon" else 1) if message.endswith(":") else 0
            skipped.add(index)
        elif depth > 0 and _json_body_line(stripped):
            depth = max(0, depth + _bracket_depth_delta(line))
            skipped.add(index)
        elif follow and stripped and not _CLI_LINES["top_level"].match(_normalize_bare(stripped)):
            depth = max(0, depth + _bracket_depth_delta(line))
            follow -= follow > 0
            skipped.add(index)
        else:
            depth, follow = 0, 0
    return skipped


def cli_own_output(provider: str, output: str, stderr: Optional[str] = None) -> str:
    """The text in which `provider`'s CLI prints its own messages.

    stderr=None means `output` is one merged stream (stream mode when the runner
    merged stderr into stdout, probes): any of its lines may be the CLI's. With
    stdout and stderr captured apart only the streams in CLI_OWN_STREAMS count,
    so the model's answer on stdout is never read as the CLI's verdict.
    """
    if stderr is None:
        return output or ""
    streams = CLI_OWN_STREAMS.get(provider, ("stdout", "stderr"))
    return "\n".join(part for name, part in (("stdout", output), ("stderr", stderr))
                     if name in streams and part)


def _cli_name_regex(cli_names: Tuple[str, ...]):
    if not cli_names:
        return None
    return cli_catalog_regex("(?:%s)%s" % ("|".join(re.escape(name) for name in cli_names),
                                           _CLI_CATALOG["lines"]["name_separator"]))


def _message_starts(message: str, marked: bool, line: str, own) -> Iterator[Tuple[str, bool, str]]:
    rest = message
    for _ in range(8):
        if not rest:
            return
        yield rest, marked, line
        match = own.match(rest) if own is not None else None
        if match is None:
            match = _CLI_LINES["marker"].match(rest)
            if match is not None:
                bracket = match.group("bracket")
                marked = marked or bool(match.group("glyph") or match.group("error")
                                        or (bracket and _CLI_LINES["marked_bracket"].match(bracket)))
            elif marked:
                # Behind an error marker, a message may open with lead-in clauses:
                # "Error: failed to send message: rpc error: code = X desc = <message>"
                match = _CLI_LINES["lead_in"].match(rest)
                if match is None:
                    return
            else:
                return
        rest = rest[match.end():]


def _starts_marked(message: str, own) -> bool:
    """Whether an error marker (glyph, "Error:", "[ERROR]" ...) leads the message."""
    return any(marked for _rest, marked, _line in _message_starts(message, False, message, own))


class _Transcript:
    """codex exec's transcript structure (catalog providers.codex.transcript)."""

    def __init__(self, spec: Dict[str, Any]):
        self.banner = cli_catalog_regex(spec["banner"])
        self.section = cli_catalog_regex(spec["section"])
        self.prompt_echo = cli_catalog_regex(spec["prompt_echo"])
        self.exec_result = cli_catalog_regex(spec["exec_result"])
        self.error_event = cli_catalog_regex(spec["error_event"])
        self.own_error = cli_catalog_regex("|".join("(?:%s)" % pattern for pattern in spec["own_error"]))
        self.tracing_target = cli_catalog_regex(spec["tracing_target"])
        self.fatal_error = cli_catalog_regex(spec["fatal_error"]) if spec.get("fatal_error") else None
        self.fatal_after = cli_catalog_regex(spec["fatal_after"]) if spec.get("fatal_after") else None
        own_warning = spec.get("own_warning") or ()
        self.own_warning = (cli_catalog_regex("|".join("(?:%s)" % pattern for pattern in own_warning))
                            if own_warning else None)

    def starts_section(self, line: str) -> bool:
        return bool(self.banner.match(line) or self.section.match(line) or self.exec_result.match(line))

    def fatal_report_start(self, lines, normalized) -> int:
        """Index of the fatal report the text ends with, len(lines) without one.

        A Rust program's main prints the error it returns after everything
        else: "Error: <message>", for an anyhow error followed by a blank line,
        "Caused by:" and the indented chain entries. codex exec ends a failed
        run with it, after the agent's last message or the "tokens used" count
        (fatal_after says after which sections it may follow).
        """
        if self.fatal_error is None or self.fatal_after is None:
            return len(lines)
        index = len(lines) - 1
        while index >= 0 and not normalized[index]:
            index -= 1
        entry = index
        while entry >= 0 and normalized[entry] and _strip_ansi(lines[entry]).startswith(("\t", "  ")):
            entry -= 1
        if entry < index:
            # The text ends with indented lines: only an anyhow chain qualifies.
            if entry < 0 or not _CLI_LINES["caused_by"].match(normalized[entry] or ""):
                return len(lines)
            index = entry - 1
            if index < 0 or normalized[index]:
                return len(lines)  # anyhow puts a blank line before "Caused by:"
            while index >= 0 and not normalized[index]:
                index -= 1
        if (index >= 0 and normalized[index] and not _strip_ansi(lines[index]).startswith(("\t", "  "))
                and self.fatal_error.match(normalized[index])):
            return index
        return len(lines)

    def own_error_message(self, line: str) -> Optional[str]:
        """The message of a line that is codex's own error event ("ERROR: <own error>")."""
        event = self.error_event.match(line)
        if event and self.own_error.match(line, event.end()):
            return line[event.end():]
        return None

    def prompt_echoes(self, lines, normalized, prompt: Optional[str]) -> Dict[int, int]:
        """For each prompt echo header followed by the prompt as sent, the index after the echo."""
        wanted = [line.rstrip() for line in (prompt or "").splitlines()]
        echoes: Dict[int, int] = {}
        if not wanted:
            return echoes
        for index, line in enumerate(normalized):
            if line and self.prompt_echo.match(line):
                block = [_strip_ansi(raw).rstrip() for raw in lines[index + 1:index + 1 + len(wanted)]]
                if block == wanted:
                    echoes[index] = index + 1 + len(wanted)
        return echoes


@lru_cache(maxsize=8)
def _transcript_rules(provider: str) -> Optional[_Transcript]:
    spec = cli_catalog_entry(provider).get("transcript")
    return _Transcript(spec) if spec else None


def _json_report_lines(lines, skipped: set) -> Dict[int, str]:
    """The JSON objects a CLI prints over several lines, each as one line.

    A report starts on a line that does not start with whitespace, ends in "{"
    or "[" and leaves brackets open ("[ERROR] {", "{"); the JSON-shaped lines
    after it belong to it until its brackets close. Returns, for each report,
    its first index -> its lines stripped and joined by one space; the other
    lines of a report are added to `skipped`.
    """
    joined: Dict[int, str] = {}
    index = 0
    while index < len(lines):
        head = _strip_ansi(lines[index]).rstrip()
        depth = 0
        if index not in skipped and head.endswith(("{", "[")) and not head[:1].isspace():
            depth = _bracket_depth_delta(head)
        end = index + 1
        while depth > 0 and end < len(lines) and end not in skipped:
            stripped = _strip_ansi(lines[end]).strip()
            if not _json_body_line(stripped):
                break
            depth += _bracket_depth_delta(stripped)
            end += 1
        if end > index + 1:
            joined[index] = " ".join([head] + [_strip_ansi(raw).strip() for raw in lines[index + 1:end]])
            skipped.update(range(index + 1, end))
        index = end
    return joined


def _cli_lines(output: str, transcript: Optional[_Transcript] = None, own=None,
               prompt: Optional[str] = None, *, json_reports: bool = False, warnings: bool = False) -> list:
    """(message, marked, line, chain) for every line the CLI may have written itself.

    Skipped as diagnostics or task content: glog entries, lines inside closed
    markdown fences, indented lines (code, tool results) and tracing lines
    below ERROR. The indented entries of an anyhow "Caused by:" chain (chain)
    and ERROR tracing lines count as marked. With `json_reports` a JSON object
    printed over several lines is one line (_json_report_lines). With
    `transcript` (codex exec), a text with transcript structure keeps only
    codex's marked lines outside the sections, plus codex's own error events,
    which end a section, and the fatal report the text ends with; the echo of
    `prompt` (the prompt as sent) is skipped whatever it contains. `warnings`
    adds the lines that are one of codex's own warnings (transcript
    own_warning) anywhere outside the prompt echo; find_cli_messages never
    reads them.
    """
    lines = (output or "").splitlines()
    skipped = glog_entry_lines(lines) if _GLOG_ANYWHERE_RE.search(output or "") else set()
    # Markdown code fences belong to the model's answer: what it quotes inside a
    # closed fence is task content. An unclosed (truncated) fence hides nothing.
    fences = [index for index, raw in enumerate(lines) if raw.lstrip().startswith(_FENCE_PREFIXES)]
    for opening, closing in zip(fences[0::2], fences[1::2]):
        skipped.update(range(opening, closing + 1))
    reports = _json_report_lines(lines, skipped) if json_reports else {}
    normalized = [None if index in skipped else
                  _normalize_bare(reports[index][:4 * CLI_MESSAGE_MAX_CHARS] if index in reports
                                  else _strip_ansi(raw[:4 * CLI_MESSAGE_MAX_CHARS]))
                  for index, raw in enumerate(lines)]
    if transcript is not None and not any(line and transcript.starts_section(line) for line in normalized):
        transcript = None
    echoes = transcript.prompt_echoes(lines, normalized, prompt) if transcript is not None else {}
    fatal = transcript.fatal_report_start(lines, normalized) if transcript is not None else len(lines)
    own_warning = transcript.own_warning if warnings and transcript is not None else None
    selected = []
    chain = section = False
    header = ""  # the header of the current section
    skip_until = 0
    for index, raw in enumerate(lines):
        line = normalized[index]
        if index < skip_until:
            continue
        if index == fatal and section and transcript.fatal_after.match(header):
            section = False  # the fatal report the text ends with is the CLI's own
        if not line:
            chain = False
            continue
        indented = _strip_ansi(raw).startswith(("\t", "  "))
        if chain and indented:
            entry = _CLI_LINES["caused_by_entry"].match(line)
            selected.append((line[entry.end():] if entry else line, True, line, True))
            continue
        chain = False
        tracing = _CLI_LINES["tracing"].match(line) if line[:1].isdigit() else None
        if transcript is not None:
            if index in echoes:
                skip_until, section = echoes[index], False
                continue
            if transcript.starts_section(line):
                section, header = True, line
                continue
            if (own_warning is not None and not indented and own_warning.match(line)
                    and not (section and transcript.prompt_echo.match(header))):
                selected.append((line, False, line, False))
                continue
            if section:
                # codex prints its events unindented; an indented copy is content.
                message = None if _strip_ansi(raw)[:1].isspace() else transcript.own_error_message(line)
                if message is None:
                    if (tracing and tracing.group("level") == "ERROR"
                            and transcript.tracing_target.match(tracing.group("target"))):
                        selected.append((line[tracing.end():], True, line, False))
                    continue
                section = False
                selected.append((message, True, line, False))
                continue
        if indented:
            continue
        if _CLI_LINES["caused_by"].match(line):
            chain = True
            continue
        if tracing:
            if tracing.group("level") == "ERROR":
                selected.append((line[tracing.end():], True, line, False))
            continue
        if transcript is not None and not _starts_marked(line, own):
            continue  # codex's own lines outside the sections carry its markers
        selected.append((line, False, line, False))
    return selected


def _final_report(selected: list, own) -> list:
    """The error report a CLI ends with: its last line and its last marked line,
    each with the "Caused by:" chain entries that follow it.

    The answer (and the tool output it quotes) comes before the CLI's verdict,
    so a phrase earlier in the text is not the CLI's.
    """
    if not selected:
        return []
    last = len(selected) - 1
    while last > 0 and selected[last][3]:  # a chain entry: the report starts at its head
        last -= 1
    heads = {last}
    marked = [index for index, (message, is_marked, _line, chain) in enumerate(selected)
              if not chain and (is_marked or _starts_marked(message, own))]
    if marked:
        heads.add(marked[-1])
    report = []
    for head in sorted(heads):
        end = head + 1
        while end < len(selected) and selected[end][3]:
            end += 1
        report.extend(selected[head:end])
    return report


def _cli_selected_lines(provider: str, output: str, stderr: Optional[str], prompt: Optional[str] = None,
                        *, warnings: bool = False):
    entry = cli_catalog_entry(provider)
    own = _cli_name_regex(CLI_NAMES.get(provider, (provider,)))
    json_reports = entry.get("json_reports") is True
    selection = entry.get("selection", {}).get("merged" if stderr is None else "apart", "lines")
    if selection == "final_report":
        # Every stream ends with its own last lines: with the streams captured
        # apart, the report each of the CLI's own streams ends with counts.
        streams = CLI_OWN_STREAMS.get(provider, ("stdout", "stderr"))
        parts = [output] if stderr is None else [part for name, part in (("stdout", output), ("stderr", stderr))
                                                 if name in streams]
        return [line for part in parts
                for line in _final_report(_cli_lines(part, None, own, json_reports=json_reports), own)], own
    transcript = _transcript_rules(provider) if selection == "transcript" else None
    return _cli_lines(cli_own_output(provider, output, stderr), transcript, own, prompt,
                      json_reports=json_reports, warnings=warnings), own


@lru_cache(maxsize=64)
def _catalog_patterns(provider: str, kind: str):
    entries = CLI_MESSAGE_CATALOG.get(provider, {})

    def build(name):
        found = entries.get(name, ())
        return cli_catalog_regex("|".join("(?:%s)" % pattern for pattern in found)) if found else None

    return build(kind), build(kind + ".marked")


def find_cli_messages(provider: str, kinds: Tuple[str, ...], output: str,
                      stderr: Optional[str] = None, *, prompt: Optional[str] = None) -> Dict[str, str]:
    """For each of `kinds`, the first line on which `provider`'s CLI prints one of
    its own messages of that kind, in the order they were found.

    `output` and `stderr` are the captured streams (see cli_own_output; stderr
    None means one merged stream); the catalog's selection for `provider` picks
    the lines. `prompt`, the prompt as sent, lets a transcript skip its echo.
    Lines come back normalized (normalize_cli_line).
    """
    patterns = [(kind,) + _catalog_patterns(provider, kind) for kind in kinds]
    found: Dict[str, str] = {}
    selected, own = _cli_selected_lines(provider, output, stderr, prompt)
    for message, marked, line, _chain in selected:
        for start, start_marked, _line in _message_starts(message, marked, line, own):
            for kind, standalone, marked_only in patterns:
                if kind not in found and ((standalone is not None and standalone.match(start))
                                          or (start_marked and marked_only is not None and marked_only.match(start))):
                    found[kind] = line
            if len(found) == len(patterns):
                return found
    return found


def find_cli_message(provider: str, kinds: Tuple[str, ...], output: str,
                     stderr: Optional[str] = None) -> Optional[str]:
    """The first line on which `provider`'s CLI prints one of its own `kinds` messages."""
    return next(iter(find_cli_messages(provider, kinds, output, stderr).values()), None)


def cli_own_lines(provider: str, output: str, stderr: Optional[str] = None, *,
                  prompt: Optional[str] = None, warnings: bool = False) -> Iterator[str]:
    """The (normalized) lines of `provider`'s CLI that find_cli_messages reads;
    with `warnings` also the CLI's own warning lines (transcript own_warning)."""
    selected, _own = _cli_selected_lines(provider, output, stderr, prompt, warnings=warnings)
    for _message, _marked, line, _chain in selected:
        yield line


def status_age_seconds(info: Dict[str, Any]) -> Optional[float]:
    """Seconds since a cache entry was written, or None when unknown: no
    timestamp, an unparseable one, or one more than STATUS_CLOCK_SKEW_SECONDS
    in the future."""
    if not isinstance(info, dict):
        return None
    stamp = _parse_timestamp(info.get("updated_at"))
    if stamp is None:
        return None
    now = datetime.now(stamp.tzinfo) if stamp.tzinfo is not None else datetime.now()
    age = (now - stamp).total_seconds()
    return None if age < -STATUS_CLOCK_SKEW_SECONDS else age


def _written_at(info: Any) -> Optional[datetime]:
    """When a cache entry was written (timezone-aware), or None when its
    updated_at is no evidence of that (see status_age_seconds)."""
    if status_age_seconds(info) is None:
        return None
    try:
        return _as_local_aware(_parse_timestamp(info.get("updated_at")))
    except (ValueError, OverflowError, OSError):
        return None


def _entry_ttl_seconds(info: Dict[str, Any]) -> Optional[float]:
    explicit = info.get("ttl_seconds")
    if isinstance(explicit, (int, float)) and not isinstance(explicit, bool) and explicit > 0:
        try:
            return min(float(explicit), FAILURE_TTL_MAX_SECONDS)
        except OverflowError:  # an integer beyond float range
            return FAILURE_TTL_MAX_SECONDS
    return STATUS_TTL_SECONDS.get(info.get("status"))


def failure_verdict_expired(info: Any) -> bool:
    """True once an error / needs_auth verdict has outlived its TTL.

    load_status_cache and engine selection apply this same rule. A verdict
    without a usable timestamp (missing, unparseable, or further in the future
    than STATUS_CLOCK_SKEW_SECONDS) is damaged evidence and counts as expired,
    so it can never exclude a provider forever.
    """
    if not isinstance(info, dict) or info.get("status") not in STATUS_TTL_SECONDS:
        return False
    ttl = _entry_ttl_seconds(info)
    age = status_age_seconds(info)
    return ttl is not None and (age is None or age >= ttl)


def _observed_longer_than(updated_at: str, seconds: float) -> bool:
    observed = _parse_timestamp(updated_at)
    if observed is None:
        return False
    try:
        return (datetime.now().astimezone() - _as_local_aware(observed)).total_seconds() > seconds
    except (ValueError, OverflowError, OSError):
        # Outside the platform's time range ("0001-01-01T00:00:00"): compare wall-clock times.
        try:
            now = datetime.now(observed.tzinfo) if observed.tzinfo is not None else datetime.now()
            return (now - observed).total_seconds() > seconds
        except (ValueError, OverflowError, OSError):
            return False


def is_reset_time_passed(resets_at: Optional[str], updated_at: str = "") -> bool:
    """True once a cached limit's reset moment has passed.

    resets_at is parsed by parse_reset_datetime (the parser pacing uses too),
    anchored on updated_at. A missing, placeholder or unparseable reset falls
    back to two hours after the observation so a limit can never stick forever.
    """
    if not resets_at or resets_at == "待重置":
        return bool(updated_at) and _observed_longer_than(updated_at, 2 * 3600)
    reset = parse_reset_datetime(resets_at, observed_at=updated_at or None)
    if reset is not None:
        return datetime.now().astimezone() >= reset
    return bool(updated_at) and _observed_longer_than(updated_at, 2 * 3600)


_RELATIVE_RESET_RE = re.compile(
    r"(?:in\s+|after\s+)?(?:\d+(?:\.\d+)?\s*(?:days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)\s*,?\s*(?:and\s+)?)+",
    re.IGNORECASE)


# Failure verdicts read from a real call's own output: a dispatch (the adapters
# write region and login failures back) or the real agy probe.
_DEFINITE_FAILURE_SOURCES = ("dispatch", "probe")


def _dispatch_failure_superseded(engine: str, info: Dict[str, Any]) -> bool:
    """A real CLI dispatch of `engine` succeeded after this failure verdict.

    The usage ledger is written after every dispatch, with the route its adapter
    reported (usage.observe_dispatch_route) or, when the adapter reports none, the
    one the configuration allowed at that time (usage.dispatch_route: "cli" when
    no API relay was possible). A success served by the cloud API ("route":
    "api") says nothing about the CLI. While a needs_auth verdict is live the
    adapters may relay work to the API, so with an API key configured only a
    success recorded as a CLI run ("route": "cli") clears it: an entry without a
    route was written when a relay was possible (or by an older version) and
    counts only without an API key. The current API policy does not decide
    this; the policy at the time of the dispatch is in the entry's route.
    """
    if info.get("source") not in _DEFINITE_FAILURE_SOURCES:
        return False
    try:
        failed_at = _parse_timestamp(info.get("updated_at"))
        if failed_at is None:
            return False
        failed_at = _as_local_aware(failed_at)
    except (ValueError, OverflowError, OSError):
        return False
    try:
        cli_only = False
        if info.get("status") == "needs_auth":
            from makewand.config import has_api_configured
            cli_only = has_api_configured(engine)
        from makewand.usage import get_dispatch_records
        records = get_dispatch_records(engine, max_age_days=2.0)
    except Exception:
        return False
    for record in reversed(records):
        route = record.get("route")
        if record.get("success") is not True or route == "api" or (cli_only and route != "cli"):
            continue
        try:
            stamp = _parse_timestamp(record.get("timestamp"))
            if stamp is not None and _as_local_aware(stamp) > failed_at:
                return True
        except (ValueError, OverflowError, OSError):
            continue
    return False


def _sanitize_cache(cache: Dict[str, Any]) -> Dict[str, Any]:
    now_dt = datetime.now()
    for model_name, info in cache.items():
        if isinstance(info, dict) and info.get("status") == "limited":
            resets_at = info.get("resets_at")
            updated_at = info.get("updated_at", "")
            # Normalize once to an absolute deadline. Rewriting the duration
            # without its reference time causes every read/save to subtract
            # the same elapsed interval again.
            if resets_at and updated_at and _RELATIVE_RESET_RE.fullmatch(str(resets_at).strip()):
                deadline = parse_reset_datetime(str(resets_at), observed_at=updated_at)
                if deadline is not None:
                    resets_at = deadline.isoformat()
                    info["resets_at"] = resets_at
            if is_reset_time_passed(resets_at, updated_at):
                info["status"] = "healthy"
                info["reason"] = f"已过配额重置窗口 ({resets_at})，已自动恢复待命"
                info["resets_at"] = None
                info["updated_at"] = now_dt.isoformat()
        elif isinstance(info, dict) and info.get("status") in STATUS_TTL_SECONDS:
            ttl = _entry_ttl_seconds(info)
            expired = failure_verdict_expired(info)
            if not expired and _dispatch_failure_superseded(model_name, info):
                previous = info.get("status")
                info["status"] = "healthy"
                info["verified"] = True
                info["superseded"] = previous
                info["reason"] = f"派发失败 ({previous}) 之后已有真实派发成功，失败记录已清除"
                info["resets_at"] = None
                info.pop("ttl_seconds", None)
                info["source"] = "dispatch"
                info["updated_at"] = now_dt.isoformat()
            elif expired:
                previous = info.get("status")
                old_reason = str(info.get("reason") or "").strip()
                why = (f"已超过 {int(ttl // 60)} 分钟有效期" if status_age_seconds(info) is not None
                       else "缺少有效的记录时间")
                info["status"] = "unknown"
                info["expired_from"] = previous
                info["reason"] = (
                    f"上次状态 {previous}（{old_reason[:80] or '无详情'}）{why}，"
                    "按中性处理；运行 'makewand probe' 重新探测"
                )
                info["resets_at"] = None
                info.pop("ttl_seconds", None)
                info["updated_at"] = now_dt.isoformat()
        # Only fresh, account-bound official evidence can re-evaluate a
        # buffer verdict. Real provider limits and authentication TTLs remain.
        if isinstance(info, dict) and model_name in ("codex", "claude"):
            reserve_gate = _is_quota_reserve_gate(info)
            if info.get("status") in ("healthy", "warning", "unknown") or (info.get("status") == "limited" and reserve_gate):
                off_q = _get_official_subscription_quota(model_name)
                if reserve_gate and (not off_q or off_q.get("source") != "official"):
                    # A derived gate is valid only while its account-bound
                    # source can still authorize it. Re-read also enforces TTL.
                    info["status"] = "unknown"
                    info["reason"] = "原账号额度证据已失效；可用性待实际派发验证"
                    info["resets_at"] = None
                    info["updated_at"] = now_dt.isoformat()
                    info.pop("quota_gate_evidence", None)
                if off_q and off_q.get("source") == "official":
                    if off_q.get("status") == "limited":
                        reserve = off_q["reserve_percent"]
                        remaining = off_q["percentage"]
                        condition = (f"剩余 {remaining}% < {reserve:g}%" if remaining > 0
                                     else "官方额度已耗尽，剩余 0%")
                        info["status"] = "limited"
                        info["reason"] = f"保护阈值拦截 ({condition}): {off_q.get('desc')}"
                        info["resets_at"] = off_q.get("resets_at")
                        info["updated_at"] = now_dt.isoformat()
                        info["quota_reserve_gate"] = True
                        info["quota_gate_evidence"] = {key: off_q.get(key) for key in
                            ("account_ref", "pool_ref", "observed_at", "credential_root")}
                    elif reserve_gate and info.get("status") == "limited":
                        # Sufficient quota is not a new successful health probe.
                        info["status"] = "unknown"
                        info["reason"] = f"官方额度达到当前保护阈值；可用性待实际派发验证: {off_q.get('desc')}"
                        info["resets_at"] = None
                        info["updated_at"] = now_dt.isoformat()
                        info["quota_reserve_gate"] = True
        if isinstance(info, dict):
            age = status_age_seconds(info)
            info["stale"] = bool(
                info.get("status") in ("healthy", "warning")
                and age is not None
                and age >= STATUS_STALE_SECONDS
            )
    return cache


def is_status_stale(info: Optional[Dict[str, Any]]) -> bool:
    """True when a healthy/warning verdict is too old to be trusted."""
    if not isinstance(info, dict) or info.get("status") not in ("healthy", "warning"):
        return False
    age = status_age_seconds(info)
    return age is not None and age >= STATUS_STALE_SECONDS

def load_status_cache() -> Dict[str, Any]:
    ensure_config_dir()
    cache = {k: dict(v) for k, v in DEFAULT_CACHE.items()}
    loaded = False
    lock_file = STATUS_CACHE_FILE.parent / ".status.lock"
    try:
        with open(lock_file, "a+") as lock_f:
            try:
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_SH)
                if STATUS_CACHE_FILE.exists():
                    with open(STATUS_CACHE_FILE, "r", encoding="utf-8") as f:
                        data = json.load(f)
                        cache.update(data)
                        loaded = True
            finally:
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_UN)
    except Exception:
        pass

    # Fallback to legacy trio status if available
    if not loaded and LEGACY_TRIO_CACHE.exists():
        try:
            with open(LEGACY_TRIO_CACHE, "r", encoding="utf-8") as f:
                try:
                    fcntl.flock(f.fileno(), fcntl.LOCK_SH)
                    data = json.load(f)
                    cache.update(data)
                finally:
                    fcntl.flock(f.fileno(), fcntl.LOCK_UN)
        except Exception:
            pass

    return _sanitize_cache(cache)

def save_status_cache(cache: Dict[str, Any]):
    ensure_config_dir()
    lock_file = STATUS_CACHE_FILE.parent / ".status.lock"
    try:
        with open(lock_file, "w") as lock_f:
            fcntl.flock(lock_f.fileno(), fcntl.LOCK_EX)
            try:
                # Merge with current on-disk data so concurrent probes don't clobber each other
                disk_data = {k: dict(v) for k, v in DEFAULT_CACHE.items()}
                if STATUS_CACHE_FILE.exists():
                    try:
                        with open(STATUS_CACHE_FILE, "r", encoding="utf-8") as f:
                            loaded = json.load(f)
                            if isinstance(loaded, dict):
                                disk_data.update(loaded)
                    except Exception:
                        pass

                for k, v in cache.items():
                    if k not in disk_data:
                        disk_data[k] = v
                    elif isinstance(v, dict):
                        disk_item = disk_data.get(k, {})
                        if isinstance(disk_item, dict):
                            # If incoming status is 'unknown' while disk has active status ('limited' or 'healthy'), preserve disk
                            if v.get("status") == "unknown" and disk_item.get("status") in ("limited", "healthy"):
                                continue
                            # An older snapshot never overwrites a newer verdict. A disk
                            # timestamp that is no evidence of when it was written (e.g.
                            # far in the future) never blocks an update; an incoming one
                            # that is present but no such evidence loses to a valid one.
                            disk_time = _written_at(disk_item)
                            if disk_time is not None and v.get("updated_at"):
                                inc_time = _written_at(v)
                                if inc_time is None or disk_time > inc_time:
                                    continue
                        disk_data[k] = v
                    else:
                        disk_data[k] = v

                merged = _sanitize_cache(disk_data)

                tmp_file = STATUS_CACHE_FILE.parent / f".status_{os.getpid()}_{datetime.now().timestamp()}_{uuid.uuid4().hex[:8]}.tmp"
                with open(tmp_file, "w", encoding="utf-8") as f:
                    json.dump(merged, f, indent=2, ensure_ascii=False)
                    f.flush()
                    os.fsync(f.fileno())
                os.replace(tmp_file, STATUS_CACHE_FILE)

                # Also update legacy trio cache file for backward compatibility
                if LEGACY_TRIO_CACHE.parent.exists():
                    tmp_legacy = LEGACY_TRIO_CACHE.parent / f".trio_{os.getpid()}_{datetime.now().timestamp()}_{uuid.uuid4().hex[:8]}.tmp"
                    with open(tmp_legacy, "w", encoding="utf-8") as f:
                        json.dump(merged, f, indent=2, ensure_ascii=False)
                        f.flush()
                        os.fsync(f.fileno())
                    os.replace(tmp_legacy, LEGACY_TRIO_CACHE)
            finally:
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_UN)
    except Exception:
        pass

def record_engine_failure(
    engine: str,
    status: str,
    reason: str,
    ttl_seconds: Optional[float] = None,
    source: str = "dispatch",
) -> None:
    """
    Writes a non-quota failure observed during a real dispatch (region block,
    authentication failure, broken CLI) into the shared status cache with a TTL.
    After the TTL the entry decays to neutral "unknown" (see _sanitize_cache).
    """
    if status not in STATUS_TTL_SECONDS:
        raise ValueError(f"unsupported failure status: {status}")
    model_name = engine.lower().strip()
    entry: Dict[str, Any] = {
        "status": status,
        "reason": reason,
        "resets_at": None,
        "updated_at": datetime.now().isoformat(),
        "source": source,
    }
    if ttl_seconds:
        entry["ttl_seconds"] = float(ttl_seconds)
    save_status_cache({model_name: entry})


def record_engine_limit(engine: str, reason: str, resets_at: Optional[str] = None) -> None:
    """
    Directly writes a live rate-limit/429 status event into the shared status cache.
    Allows immediate cross-session visibility without waiting for periodic polling probes.
    Login/credential failures reported through this legacy entry point are stored as
    needs_auth with a TTL instead of a fake quota limit.
    """
    model_name = engine.lower().strip()
    if resets_at == "需登录授权" or "登录" in str(reason or ""):
        record_engine_failure(model_name, "needs_auth", f"{reason} · {get_reauth_hint(model_name)}")
        return
    now = datetime.now().isoformat()
    status_entry = {
        model_name: {
            "status": "limited",
            "reason": reason,
            "resets_at": resets_at,
            "updated_at": now
        }
    }
    save_status_cache(status_entry)

class ProbePolicyError(ValueError):
    """Live host inference cannot satisfy an untrusted repository policy."""


def _validate_probe_policy(repo_trust):
    if repo_trust not in ("trusted", "untrusted"):
        raise ValueError("invalid repository trust policy")
    if repo_trust == "untrusted":
        raise ProbePolicyError("不可信仓库禁止宿主模型探活；请在可信目录显式运行 probe。缓存状态读取不需要模型调用。")


def _run_model_probe(engine: str, command: str, timeout: int, *, repo_trust="trusted", cwd=None):
    """Health echoes use the shared runtime, ledger and parent deadline."""
    from makewand.config import get_api_policy
    from makewand.execution_contract import ExecutionRequest
    from makewand.execution_runtime import execute, task_id, current_context
    context = current_context()
    captured = []
    try:
        _validate_probe_policy(repo_trust)
    except (ProbePolicyError, ValueError) as error:
        from makewand.providers.base import ProcessExecutionError
        return 127, "", "", ProcessExecutionError(str(error), "UNVERIFIED" if isinstance(error, ProbePolicyError) else "INVALID_REQUEST")

    def invoke(remaining):
        from makewand.execution_runtime import mark_provider_invocation
        from makewand.providers.base import model_process_failure, is_process_timeout, ProcessExecutionError
        mark_provider_invocation()
        # Generated echo probes do not need project files or project hooks.
        # Give even trusted probes a disposable neutral working directory.
        with tempfile.TemporaryDirectory(prefix="makewand-probe-") as neutral:
            if engine == "agy" and isinstance(command, (list, tuple)):
                # The agy probe must read the sanitized private-home settings
                # (useG1Credits=false), never the host settings: go through
                # the same sandbox launch as real dispatches. Without a
                # sandbox that launch fails closed (see sandbox.P0 guard).
                from makewand.sandbox import (
                    SandboxConfigError,
                    SandboxUnavailableError,
                    provider_sandbox_launch,
                )
                try:
                    with provider_sandbox_launch(
                        "agy", list(command), neutral,
                        readonly=True, repo_root=neutral, repo_trust=repo_trust,
                    ) as launch:
                        raw = run_subprocess(
                            launch.cmd, timeout=remaining, cwd=neutral,
                            pass_fds=launch.pass_fds, extra_env=launch.env,
                            separate_stderr=True,
                        )
                except (SandboxUnavailableError, SandboxConfigError) as exc:
                    raw = (127, "", "", ProcessExecutionError(str(exc), "FAILED"))
                    captured.append(raw)
                    return model_process_failure(engine, raw[0], raw[1], raw[2], raw[3], readonly=True)
            else:
                raw = run_subprocess(command, timeout=remaining, cwd=neutral)
        captured.append(raw)
        if raw[0] == 0:
            return True, raw[1], None
        parser = {"claude": parse_claude_quota, "codex": parse_codex_quota,
                  "muse": parse_muse_quota, "grok": parse_grok_quota,
                  "agy": parse_agy_quota}.get(engine)
        if raw[0] > 0 and not is_process_timeout(raw[3]) and parser is not None:
            text = f"{raw[1]}\n{raw[2]}"
            refused = parser(text)[0]
            if not refused and engine == "agy":
                # A region block or login rejection is an explicit refusal too.
                from makewand.providers.agy import classify_agy_failure
                refused = classify_agy_failure(text) is not None
            if refused:
                return False, raw[1], raw[3] or raw[2] or "model probe explicitly refused"
        return model_process_failure(engine, raw[0], raw[1], raw[2], raw[3], readonly=True)

    request = ExecutionRequest(task_id=task_id(), stage="probe", engine=engine,
        tier="probe", readonly=True, repo_trust=repo_trust, cwd=cwd or os.getcwd(), api_policy=get_api_policy(),
        deadline_unix_ms=context.get("deadline_unix_ms"), timeout_ms=max(1, int(timeout * 1000)))
    result = execute(request, invoke)
    if captured and result.status in ("PASSED", "FAILED"):
        return captured[0]
    return (124 if result.status == "TIMEOUT" else 127), "", "", result.error


def _probe_timed_out(code: int, error: Any) -> bool:
    """A probe that ran out of time: the runner's timeout evidence, or the shared
    runtime's TIMEOUT outcome (_run_model_probe returns 124 with "execution
    deadline expired" once the process outlives the deadline)."""
    from makewand.providers.base import is_process_timeout
    text = str(error or "").lower()
    return is_process_timeout(error) or "timed out" in text or (code == 124 and "deadline expired" in text)


def _resolve_probe_timeout(
    model_name: str,
    base_timeout: int,
    tier: Optional[str] = None,
    effort: Optional[str] = None,
) -> int:
    """Dynamically adapt probe timeouts based on model reasoning effort, tier, or env settings.

    Prevents deep reasoning models (e.g. Muse with reasoning_effort: max, Codex deep)
    from being misclassified as dead due to short echo probe deadlines.
    """
    env_override = os.environ.get(f"MAKEWAND_PROBE_TIMEOUT_{model_name.upper()}")
    if env_override:
        try:
            return max(5, int(env_override))
        except ValueError:
            pass
    if "MAKEWAND_PROBE_TIMEOUT" in os.environ:
        try:
            return max(5, int(os.environ["MAKEWAND_PROBE_TIMEOUT"]))
        except ValueError:
            pass

    # Generous base timeouts for cold starts, container init and handshake
    relaxed_base = {
        "claude": 30,
        "codex": 45,
        "muse": 45,
        "grok": 35,
        "agy": 15,
        "aider": 15,
    }.get(model_name, max(base_timeout, 30))
    timeout = max(base_timeout, relaxed_base)

    effective_effort = effort
    effective_tier = tier or "standard"
    if not effective_effort:
        try:
            from makewand.discovery import resolve_model_and_effort
            res = resolve_model_and_effort(model_name, tier=effective_tier)
            effective_effort = res.get("effort")
        except Exception:
            pass

    eff_lower = (effective_effort or "").lower()
    if eff_lower in ("max", "xhigh") or effective_tier == "deep":
        timeout = max(timeout, 90)
    elif eff_lower == "high":
        timeout = max(timeout, 60)
    elif eff_lower == "medium":
        timeout = max(timeout, 45)

    return timeout


def probe_model(
    model_name: str,
    *,
    repo_trust: str = "trusted",
    cwd: Optional[str] = None,
    tier: Optional[str] = "standard",
    effort: Optional[str] = None,
    timeout: Optional[int] = None,
) -> Dict[str, Any]:
    now = datetime.now().isoformat()
    from makewand.config import api_relay_available, api_relay_blocked_note, has_subscription_configured, is_provider_enabled

    if not is_provider_enabled(model_name):
        return {
            "status": "disabled",
            "reason": f"用户已在配置中手动禁用此引擎 (运行 'makewand enable {model_name}' 重新开启)",
            "resets_at": None,
            "updated_at": now,
            "mode": "disabled"
        }

    # 1. Local self-hosted models (Ollama, vLLM, LocalAI)
    if model_name in ("local", "ollama"):
        from makewand.providers.local import is_local_model_available, get_default_local_model, list_local_models
        ok, reason, models = is_local_model_available()
        if ok:
            active_m = get_default_local_model()
            all_m = list_local_models() or []
            m_desc = f"{active_m}" + (f" (共 {len(all_m)} 个本地模型)" if len(all_m) > 1 else "")
            return {
                "status": "healthy",
                "reason": f"本地私有模型就绪 (当前: {m_desc}, 0 Token 成本)",
                "resets_at": None,
                "updated_at": now,
                "mode": "local"
            }
        return {
            "status": "missing",
            "reason": reason or "本地 Ollama / vLLM (http://localhost:11434) 未响应或未检测到可用模型",
            "resets_at": None,
            "updated_at": now,
            "mode": "local"
        }

    # The adapters relay to the cloud API only with a key AND an API policy that
    # allows paid calls; a key alone is no fallback (config.api_relay_available).
    api_ready = api_relay_available(model_name)
    if model_name in API_ONLY_ENGINES:
        blocked = api_relay_blocked_note(model_name)
        return {
            "status": "healthy" if api_ready else "missing",
            "reason": (f"{model_name.upper()} 云端 API 就绪" if api_ready else
                       blocked or f"未配置 {model_name.upper()} API Key (设置环境变量即可激活)"),
            "resets_at": None,
            "updated_at": now,
            "mode": "api" if api_ready else "none",
        }
    sub_configured = has_subscription_configured(model_name)

    # 2. Subscription CLI not installed
    if not sub_configured:
        if api_ready:
            return {
                "status": "healthy",
                "reason": f"未安装订阅 CLI，已启用纯 API Key 模式",
                "resets_at": None,
                "updated_at": now,
                "mode": "api"
            }
        blocked = api_relay_blocked_note(model_name)
        return {
            "status": "missing",
            "reason": (f"CLI 命令 '{model_name}' 未在系统 PATH 中找到；{blocked}" if blocked
                       else f"CLI 命令 '{model_name}' 未在系统 PATH 中找到，且未配置备用 API Key"),
            "resets_at": None,
            "updated_at": now,
            "mode": "none"
        }

    # 3. Subscription Probes
    mode = "hybrid" if api_ready else "subscription"
    if model_name in ("claude", "codex", "muse", "grok"):
        try:
            _validate_probe_policy(repo_trust)
        except (ProbePolicyError, ValueError) as error:
            return {"status": "unknown", "reason": str(error), "resets_at": None,
                    "updated_at": now, "mode": mode, "verified": False,
                    "execution_status": "UNVERIFIED" if isinstance(error, ProbePolicyError) else "INVALID_REQUEST"}

    if model_name == "claude":
        cmd = 'claude -p "echo ok"'
        t = _resolve_probe_timeout(model_name, timeout or 30, tier=tier, effort=effort)
        code, out, err, ex = _run_model_probe(model_name, cmd, timeout=t, repo_trust=repo_trust, cwd=cwd)
        combined = f"{out}\n{err}"
        is_limited, reason, resets = parse_claude_quota(combined)
        if is_limited:
            reason_str = f"{reason} (已就绪备用 API 兜底接力)" if api_ready else reason
            return {"status": "limited", "reason": reason_str, "resets_at": resets, "updated_at": now, "mode": mode, "api_fallback": api_ready}
        if code == 0:
            sub_desc = "Claude Code 订阅运行正常" + (" (已配置 API 备用兜底)" if api_ready else "")
            return {"status": "healthy", "reason": sub_desc, "resets_at": None, "updated_at": now, "mode": mode}
        if _probe_timed_out(code, ex):
            err_lower = (combined + " " + str(ex)).lower()
            if any(term in err_lower for term in ("login", "oauth", "auth", "授权", "登录")):
                return {"status": "needs_auth", "reason": "等待 OAuth 登录 (请运行 'claude login')", "resets_at": None, "updated_at": now, "mode": mode}
            return {"status": "warning", "reason": f"Claude 探活超时 ({t}s，可能正处于深度思考中，执行已延时放宽)", "resets_at": None, "updated_at": now, "mode": mode}
        if ex:
            return {"status": "error", "reason": ex, "resets_at": None, "updated_at": now, "mode": mode}
        return {"status": "healthy" if "ok" in out.lower() else "warning", "reason": combined.strip()[:120], "resets_at": None, "updated_at": now, "mode": mode}

    elif model_name == "codex":
        cmd = 'codex exec --sandbox read-only --skip-git-repo-check "echo ok"'
        t = _resolve_probe_timeout(model_name, timeout or 45, tier=tier, effort=effort)
        code, out, err, ex = _run_model_probe(model_name, cmd, timeout=t, repo_trust=repo_trust, cwd=cwd)
        combined = f"{out}\n{err}"
        is_limited, reason, resets = parse_codex_quota(combined)
        if is_limited:
            reason_str = f"{reason} (已就绪备用 API 兜底接力)" if api_ready else reason
            return {"status": "limited", "reason": reason_str, "resets_at": resets, "updated_at": now, "mode": mode, "api_fallback": api_ready}
        if code == 0:
            sub_desc = "Codex 订阅运行正常" + (" (已配置 API 备用兜底)" if api_ready else "")
            return {"status": "healthy", "reason": sub_desc, "resets_at": None, "updated_at": now, "mode": mode}
        if _probe_timed_out(code, ex):
            err_lower = (combined + " " + str(ex)).lower()
            if any(term in err_lower for term in ("login", "oauth", "auth", "授权", "登录")):
                return {"status": "needs_auth", "reason": "等待 OAuth 浏览器授权登录 (请运行 'codex login')", "resets_at": None, "updated_at": now, "mode": mode}
            return {"status": "warning", "reason": f"Codex 探活超时 ({t}s，可能正处于深度推理中，执行已延时放宽)", "resets_at": None, "updated_at": now, "mode": mode}
        if ex:
            return {"status": "error", "reason": ex, "resets_at": None, "updated_at": now, "mode": mode}
        return {"status": "warning", "reason": combined.strip()[:120], "resets_at": None, "updated_at": now, "mode": mode}

    elif model_name == "agy":
        # `agy --version` only proves the binary is installed. Account, region and
        # quota availability are learned from real dispatches (providers/agy.py
        # writes region/auth failures back with a TTL) and the usage ledger; while
        # such a failure (or a quota limit) is live, get_or_update_status adds one
        # real echo call (_probe_agy_for_real) so a re-login, a lifted block or a
        # quota reset can clear it.
        with tempfile.TemporaryDirectory(prefix="makewand-version-") as neutral:
            code, out, err, ex = run_subprocess(["agy", "--version"], timeout=timeout or 5, cwd=neutral)
        if code == 0:
            version = out.strip() or "v1.x"
            sub_desc = (f"Antigravity 已安装 ({version})；仅版本检测，账号/地区可用性未经真实调用验证"
                        + (" (已配置 API 备用兜底)" if api_ready else ""))
            return {"status": "healthy", "reason": sub_desc, "resets_at": None, "updated_at": now,
                    "mode": mode, "verified": False}
        return {"status": "warning", "reason": err.strip()[:100] or "agy 版本检测异常", "resets_at": None, "updated_at": now, "mode": mode}

    elif model_name == "muse":
        cmd = 'muse exec --disable-write --trust-workspace "echo ok"'
        t = _resolve_probe_timeout(model_name, timeout or 45, tier=tier, effort=effort)
        code, out, err, ex = _run_model_probe(model_name, cmd, timeout=t, repo_trust=repo_trust, cwd=cwd)
        combined = f"{out}\n{err}"
        is_limited, reason, resets = parse_muse_quota(combined)
        if is_limited:
            reason_str = f"{reason} (已就绪备用 API 兜底接力)" if api_ready else reason
            return {"status": "needs_auth" if "登录" in reason else "limited", "reason": reason_str, "resets_at": resets, "updated_at": now, "mode": mode, "api_fallback": api_ready}
        if code == 0:
            sub_desc = "Muse Code (Meta 订阅) 运行正常" + (" (已配置 API 备用兜底)" if api_ready else "")
            return {"status": "healthy", "reason": sub_desc, "resets_at": None, "updated_at": now, "mode": mode}
        if _probe_timed_out(code, ex):
            err_lower = (combined + " " + str(ex)).lower()
            if any(term in err_lower for term in ("login", "oauth", "auth", "授权", "登录")):
                return {"status": "needs_auth", "reason": "等待 OAuth 浏览器授权登录 (请运行 'muse login')", "resets_at": None, "updated_at": now, "mode": mode}
            return {"status": "warning", "reason": f"Muse 探活超时 ({t}s，可能正处于深度推理中，执行已延时放宽)", "resets_at": None, "updated_at": now, "mode": mode}
        return {"status": "warning", "reason": combined.strip()[:120] or ex or "Muse 状态未知", "resets_at": None, "updated_at": now, "mode": mode}

    elif model_name == "grok":
        cmd = 'grok -p "echo ok" --output-format plain'
        t = _resolve_probe_timeout(model_name, timeout or 35, tier=tier, effort=effort)
        code, out, err, ex = _run_model_probe(model_name, cmd, timeout=t, repo_trust=repo_trust, cwd=cwd)
        combined = f"{out}\n{err}"
        is_limited, reason, resets = parse_grok_quota(combined)
        if is_limited:
            reason_str = f"{reason} (已就绪备用 API 兜底接力)" if api_ready else reason
            return {"status": "needs_auth" if "登录" in reason else "limited", "reason": reason_str, "resets_at": resets, "updated_at": now, "mode": mode, "api_fallback": api_ready}
        if code == 0:
            sub_desc = "Grok Build (xAI 订阅) 运行正常" + (" (已配置 API 备用兜底)" if api_ready else "")
            return {"status": "healthy", "reason": sub_desc, "resets_at": None, "updated_at": now, "mode": mode}
        if _probe_timed_out(code, ex):
            err_lower = (combined + " " + str(ex)).lower()
            if any(term in err_lower for term in ("login", "oauth", "auth", "授权", "登录")):
                return {"status": "needs_auth", "reason": "等待授权登录", "resets_at": None, "updated_at": now, "mode": mode}
            return {"status": "warning", "reason": f"Grok 探活超时 ({t}s，可能正处于推理中，执行已延时放宽)", "resets_at": None, "updated_at": now, "mode": mode}
        if ex:
            return {"status": "error", "reason": ex, "resets_at": None, "updated_at": now, "mode": mode}
        return {"status": "warning", "reason": combined.strip()[:120] or "Grok 探活异常退出", "resets_at": None, "updated_at": now, "mode": mode}
    elif model_name == "aider":
        with tempfile.TemporaryDirectory(prefix="makewand-version-") as neutral:
            code, out, err, ex = run_subprocess(["aider", "--version"], timeout=5, cwd=neutral)
        if code == 0:
            version = out.strip().replace("aider ", "v")
            sub_desc = f"Aider AI Pair Programmer ({version}) 运行就绪" + (" (已配置 API 备用兜底)" if api_ready else "")
            return {"status": "healthy", "reason": sub_desc, "resets_at": None, "updated_at": now, "mode": mode}
        return {"status": "warning", "reason": err.strip()[:100] or "aider 版本检测异常", "resets_at": None, "updated_at": now, "mode": mode}

    return {"status": "unknown", "reason": "Unknown model", "resets_at": None, "updated_at": now, "mode": "none"}


def check_grok_health(*args, **kwargs) -> Dict[str, Any]:
    """Helper alias to probe Grok health status."""
    return probe_model("grok", *args, **kwargs)


PROBE_REUSE_SECONDS = 120
# Joins a kept failure verdict's reason and what the latest probe saw.
_PROBE_NOTE_MARK = " · 最近一次探活"


def _is_live_definite_failure(info: Any) -> bool:
    """A live failure verdict that only a verified success or a new definite verdict replaces.

    Quota limits stand until their reset; error / needs_auth verdicts read from a
    real call's own output stand until their TTL, whether a dispatch or the real
    agy probe recorded them.
    """
    if not isinstance(info, dict):
        return False
    if info.get("status") == "limited":
        return not is_reset_time_passed(info.get("resets_at"), info.get("updated_at") or "")
    return (info.get("source") in _DEFINITE_FAILURE_SOURCES and info.get("status") in STATUS_TTL_SECONDS
            and not failure_verdict_expired(info))


# Probe verdicts that are evidence on their own and replace an earlier failure.
_DEFINITIVE_PROBE_STATUSES = ("limited", "needs_auth", "error", "missing", "disabled")


def _merge_probe_result(model: str, previous: Any, probed: Dict[str, Any]) -> Dict[str, Any]:
    """Only a verified success or a new definite verdict replaces a live failure verdict.

    A version-only probe ("verified": False), a real probe that timed out or
    failed in an unrecognised way, or another inconclusive verdict (warning)
    proves nothing about the account, so the earlier failure is kept until it
    expires; its reason then also says what the latest probe saw.
    """
    if not _is_live_definite_failure(previous):
        return probed
    status = probed.get("status")
    if (status == "healthy" and probed.get("verified") is not False) or status in _DEFINITIVE_PROBE_STATUSES:
        return probed
    if status == "healthy":
        seen = "仅为版本检测，只证明已安装"
    else:
        seen = "未能确认恢复：" + (str(probed.get("reason") or "").strip()[:160] or f"状态 {status or 'unknown'}")
    if previous.get("status") == "limited":
        keep = "保留额度受限记录直到重置"
    else:
        keep = f"保留{'真实探活' if previous.get('source') == 'probe' else '真实派发'}失败记录直到过期"
    kept = dict(previous)
    reason = str(previous.get("reason") or "").split(_PROBE_NOTE_MARK)[0]
    kept["reason"] = f"{reason}{_PROBE_NOTE_MARK}{seen}，{keep}"
    return kept


def _probe_agy_for_real(*, repo_trust: str = "trusted", cwd: Optional[str] = None,
                        timeout: Optional[int] = None) -> Dict[str, Any]:
    """One real `agy -p` echo through the shared runtime, ledger and deadline.

    `agy --version` cannot tell whether a re-login fixed an authentication
    failure, a region block ended or a quota reset, so `makewand probe` runs this
    whenever such a failure is live (the re-login hint tells users to probe).
    Its verdicts carry "source": "probe".
    """
    from makewand.providers.agy import classify_agy_failure, _agy_user_facing
    from makewand.config import api_relay_available
    now = datetime.now().isoformat()
    mode = "hybrid" if api_relay_available("agy") else "subscription"
    t = _resolve_probe_timeout("agy", timeout or 60, tier="fast", effort="low")
    command = ["agy", "-p", "echo ok", "--print-timeout", f"{t}s", "--disable-slash-commands", "--mode", "plan",
               "--model", AGY_PROBE_MODEL]
    code, out, err, ex = _run_model_probe("agy", command, timeout=t, repo_trust=repo_trust, cwd=cwd)
    combined = f"{out}\n{err}"
    limited, reason, resets = parse_agy_quota(combined)
    if limited:
        return {"status": "limited", "reason": reason, "resets_at": resets, "updated_at": now,
                "mode": mode, "source": "probe", "verified": True}
    failure = classify_agy_failure(combined)
    if failure:
        status, fail_reason, ttl = failure
        entry = {"status": status, "reason": fail_reason.replace("真实派发", "真实探活"), "resets_at": None,
                 "updated_at": now, "mode": mode, "source": "probe", "verified": True}
        if ttl:
            entry["ttl_seconds"] = float(ttl)
        return entry
    sandbox_detail = str(ex or "").strip()
    if sandbox_detail and ("useG1Credits" in sandbox_detail or "沙箱" in sandbox_detail):
        # No usable sandbox (and host credits not proven off): the probe did
        # not run on the host at all (fail closed). Report that, unverified.
        return {"status": "warning", "reason": sandbox_detail[:300], "resets_at": None, "updated_at": now,
                "mode": mode, "source": "probe", "verified": False}
    if code == 0 and _agy_user_facing(out).strip():
        return {"status": "healthy", "reason": "Antigravity 真实探活成功 (账号/地区/登录可用)", "resets_at": None,
                "updated_at": now, "mode": mode, "source": "probe", "verified": True}
    # Inconclusive: neither clears nor replaces a more specific earlier failure.
    if _probe_timed_out(code, ex):
        reason = f"Antigravity 真实探活超时 ({t}s)，结果不确定"
    elif code == 0:
        reason = "Antigravity 真实探活执行完成但未返回有效输出，结果不确定"
    else:
        detail = str(ex or "").strip() or _agy_user_facing(err).strip()[:120] or f"exit code {code}"
        reason = f"Antigravity 真实探活失败，原因未识别: {detail[:120]}"
    return {"status": "warning", "reason": reason, "resets_at": None, "updated_at": now, "mode": mode,
            "source": "probe", "verified": False}


def _probed_since(info: Any, started: datetime) -> bool:
    # A timestamp far in the future is damaged, not the result of a fresh probe.
    if status_age_seconds(info) is None:
        return False
    stamp = _parse_timestamp(info.get("updated_at"))
    if stamp is None:
        return False
    try:
        if stamp.tzinfo is not None:
            stamp = stamp.astimezone().replace(tzinfo=None)
    except (ValueError, OverflowError, OSError):
        return False
    return stamp >= started - timedelta(seconds=PROBE_REUSE_SECONDS)


def get_or_update_status(force_probe: bool = False, *, repo_trust="trusted", cwd=None) -> Dict[str, Any]:
    from makewand.config import get_all_supported_providers
    if force_probe:
        _validate_probe_policy(repo_trust)
    cache = load_status_cache()
    if not force_probe:
        return cache
    # Probes of claude/codex/muse/grok are real model calls. Serialize them across
    # processes so two concurrent `makewand probe` runs do not double the spend: a
    # waiter reuses the fresh result of the probe that held the lock.
    ensure_config_dir()
    probe_lock = STATUS_CACHE_FILE.parent / ".probe.lock"
    started = datetime.now()
    try:
        lock_f = open(probe_lock, "a+")
    except OSError:
        lock_f = None
    try:
        if lock_f is not None:
            try:
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            except OSError:
                print("⏳ 另一个 makewand 探测正在进行，等待其结果以避免重复消耗额度...", flush=True)
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_EX)
                cache = load_status_cache()
                if all(_probed_since(cache.get(m), started) for m in get_all_supported_providers()):
                    return cache
        cache = load_status_cache()
        for model in get_all_supported_providers():
            previous = cache.get(model)
            probed = (probe_model(model) if repo_trust == "trusted" and cwd is None
                      else probe_model(model, repo_trust=repo_trust, cwd=cwd))
            if (model == "agy" and probed.get("status") == "healthy" and probed.get("verified") is False
                    and _is_live_definite_failure(previous)):
                # The installed CLI may have been fixed (re-login, region, quota
                # reset): only a real call can retire the live failure, whether a
                # dispatch or an earlier real probe recorded it.
                probed = _probe_agy_for_real(repo_trust=repo_trust, cwd=cwd)
            cache[model] = _merge_probe_result(model, previous, probed)
        save_status_cache(cache)
        return cache
    finally:
        if lock_f is not None:
            try:
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_UN)
            except OSError:
                pass
            lock_f.close()

def format_quota_bar(percentage: int, width: int = 20, colorize: bool = True) -> str:
    """
    Renders a colored progress bar matching modern CLI tools:
    [████████████████████] 100% (Green)
    [████████████░░░░░░░░]  60% (Yellow/Cyan)
    [████░░░░░░░░░░░░░░░░]  20% (Yellow/Red)
    [░░░░░░░░░░░░░░░░░░░░]   0% (Red)
    """
    pct = max(0, min(100, int(percentage)))
    filled = int(round(width * pct / 100.0))
    empty = width - filled

    if not colorize:
        bar = "█" * filled + "░" * empty
        return f"[{bar}] {pct:>3}%"

    from makewand.config import COLOR_GREEN, COLOR_YELLOW, COLOR_RED, COLOR_RESET, COLOR_BOLD
    if pct >= 50:
        bar_color = COLOR_GREEN
    elif pct >= 20:
        bar_color = COLOR_YELLOW
    else:
        bar_color = COLOR_RED

    bar = f"{bar_color}{'█' * filled}{COLOR_RESET}{'░' * empty}"
    pct_str = f"{bar_color}{COLOR_BOLD}{pct:>3}%{COLOR_RESET}"
    return f"[{bar}] {pct_str}"

QUOTA_SOURCE_LABELS = {
    "official": "官方报告",
    "local_estimate": "本地调用计数估算，非真实配额",
    "assumed": "无额度信号，未做估算",
    "status": "由健康状态推定",
    "unknown": "未探测",
}


def _get_official_subscription_quota(provider: str, resets_at: Optional[str] = None) -> Optional[Dict[str, Any]]:
    """Read recent quota evidence for the CLI's selected account root.

    Windows expire at their reset or after 15 minutes without an observation.
    Newer evidence replaces only the same account/model window. Unbound
    history is never current admission evidence, including at the default root.
    No cache, authentication state or provider is changed.
    """
    import math
    import time
    from datetime import timezone

    reserve, reserve_error = _quota_reserve_setting()
    now = time.time()
    ttl = 15 * 60
    evidence = []
    reasons = set()
    provider = provider.lower().strip()

    def timestamp(value):
        if value is None or isinstance(value, bool) or str(value).startswith("0001"):
            return None
        try:
            if isinstance(value, (int, float)) or str(value).replace(".", "", 1).isdigit():
                parsed = float(value)
            else:
                parsed = datetime.fromisoformat(str(value).replace("Z", "+00:00")).timestamp()
            return parsed if math.isfinite(parsed) and parsed > 0 else None
        except (ValueError, TypeError, OverflowError, OSError):
            return None

    def observed_at(modified, *values):
        # Event timestamps prevent an unrelated log append or file touch from
        # renewing old data; mtime bounds legacy records without timestamps.
        parsed = [timestamp(value) for value in values]
        if any(value is not None and value > now + 60 for value in parsed):
            return now + ttl + 60
        return min([now, modified] + [value for value in parsed if value is not None])

    def add(used, minutes, reset, observed, origin, window_key, scoped=None, model=""):
        try:
            if isinstance(used, bool):
                return
            used = float(used)
            if not math.isfinite(used) or not 0 <= used <= 100:
                reasons.add("invalid_percentage")
                return
            reset_time = timestamp(reset)
            if reset not in (None, "", "0001-01-01T00:00:00Z") and reset_time is None:
                reasons.add("invalid_reset")
                return
            if reset_time is not None and reset_time <= now:
                reasons.add("window_reset")
                return
            if now - observed > ttl:
                reasons.add("stale_observation")
                return
            if observed > now + 60:
                reasons.add("future_observation")
                return
            label = ("每周窗口" if minutes == 10080 else "5 小时滚动窗口" if minutes == 300
                     else "30 天窗口" if minutes == 43200 else f"{minutes} 分钟窗口" if minutes else "未知时间窗口")
            evidence.append({"used_percent": used, "window_minutes": minutes,
                             "window": label, "window_key": window_key,
                             "resets_at": datetime.fromtimestamp(reset_time, timezone.utc).isoformat() if reset_time else None,
                             "observed": observed, "selected_source": origin,
                             "scoped_percent": scoped, "model": model, "pool_ref": identity["pool_ref"]})
        except (ValueError, TypeError, OverflowError, OSError):
            reasons.add("invalid_window")

    from makewand.account_identity import account_identity, evidence_matches, event_account_ref
    identity = account_identity(provider)
    codex_home = identity["credential_root"] if provider == "codex" else None
    cache_path = os.path.expanduser("~/.cache/makewand/quota-snapshot.json")
    try:
        with open(cache_path, "r", encoding="utf-8") as stream:
            raw = stream.read(1024 * 1024 + 1)
        if len(raw) > 1024 * 1024:
            raise ValueError("quota snapshot exceeds read limit")
        snapshot = json.loads(raw)
        modified = os.path.getmtime(cache_path)
        for entry in snapshot.get("providers", []):
            if not isinstance(entry, dict) or str(entry.get("Provider", "")).lower() != provider or not entry.get("HasData"):
                continue
            if snapshot.get("version") != 2 or not evidence_matches(entry, identity):
                reasons.add("unbound_account_identity")
                continue
            if timestamp(entry.get("SourceAt")) is None:
                reasons.add("missing_observation_time")
                continue
            observed = observed_at(modified, snapshot.get("taken_at"), entry.get("SourceAt"))
            if entry.get("Windows"):
                for window in entry["Windows"]:
                    if not isinstance(window, dict) or window.get("PoolRef") != identity["pool_ref"]:
                        reasons.add("unbound_pool")
                        continue
                    if timestamp(window.get("SourceAt")) is None:
                        reasons.add("missing_observation_time")
                        continue
                    minutes = window.get("WindowMinutes") or None
                    model = window.get("Model") or ""
                    add(window.get("UsedPercent"), minutes, window.get("ResetAt"),
                        observed_at(modified, window.get("SourceAt"), snapshot.get("taken_at")),
                        "quota_snapshot", window.get("WindowKey") or model + "|" + str(minutes), model=model)
            else:
                for key, minutes, reset_key in (("WeeklyPct", 10080, "WeeklyResetAt"),
                                               ("FiveHourPct", 300, "FiveHourResetAt"),
                                               ("ThirtyDayPct", 43200, "ThirtyDayResetAt")):
                    if entry.get(key) is None:
                        continue
                    reset = entry.get(reset_key)
                    if not reset or str(reset).startswith("0001"):
                        reset = entry.get("ResetAt") if key == "WeeklyPct" or entry.get("WeeklyPct") is None else None
                    add(entry[key], minutes, reset, observed, "quota_snapshot", "|" + str(minutes), entry.get("ScopedPct"))
    except FileNotFoundError:
        pass
    except (OSError, ValueError, TypeError, AttributeError):
        reasons.add("invalid_snapshot")

    if provider == "codex":
        # Follow exactly the official CLI's account-root selection. Never fall
        # back to another account's home or combine its session observations.
        sessions = Path(codex_home) / "sessions"
        paths = []
        if sessions.is_dir():
            for path in sessions.rglob("*.jsonl"):
                try:
                    status = path.stat()
                except OSError:
                    continue
                # Only regular files compete for the newest eight: opening a
                # FIFO would block, and no other kind of file holds a log.
                if stat.S_ISREG(status.st_mode):
                    paths.append((status.st_mtime, path))
        for modified, path in sorted(paths, reverse=True)[:8]:
            try:
                with path.open("rb") as stream:
                    size = stream.seek(0, 2)
                    start = max(0, size - 4 * 1024 * 1024)
                    stream.seek(start)
                    if start:
                        stream.readline()
                    lines = stream.read(4 * 1024 * 1024).splitlines()
                    if start:
                        # The tail leaves out the first line, where Codex writes
                        # the session_meta that binds tail events naming no
                        # account. It is read when complete within 64 KiB, as Go
                        # reads it.
                        stream.seek(0)
                        first, newline, _ = stream.read(64 * 1024).partition(b"\n")
                        if newline:
                            lines.insert(0, first)
                session_ref = None
                for line in lines:
                    if b"rate_limits" not in line and b"session_meta" not in line:
                        continue
                    try:
                        record = json.loads(line)
                        payload = record.get("payload") or {}
                        explicit_ref = event_account_ref(record)
                        if record.get("type") == "session_meta":
                            session_ref = explicit_ref
                            continue
                        if not identity["identity_known"] or (explicit_ref or session_ref) != identity["account_ref"]:
                            if b"rate_limits" in line:
                                reasons.add("unbound_account_identity")
                            continue
                        limits = payload.get("rate_limits") or record.get("rate_limits")
                        if not isinstance(limits, dict):
                            continue
                        if timestamp(record.get("timestamp")) is None:
                            reasons.add("missing_observation_time")
                            continue
                        observed = observed_at(modified, record.get("timestamp"), payload.get("timestamp"))
                        model = limits.get("model") or ""
                        limit_id = limits.get("limit_id")
                        if not model and limit_id and limit_id != "codex":
                            model = limit_id
                        for slot in ("primary", "secondary"):
                            window = limits.get(slot)
                            if not isinstance(window, dict) or "used_percent" not in window:
                                continue
                            duration = window.get("window_minutes", window.get("limit_window_minutes"))
                            try:
                                duration = int(duration) if duration is not None and not isinstance(duration, bool) else None
                                if duration is not None and duration <= 0:
                                    duration = None
                            except (TypeError, ValueError, OverflowError):
                                duration = None
                            add(window["used_percent"], duration, window.get("resets_at"), observed,
                                "codex_session", model + "|" + (str(duration) if duration else "unknown_" + slot), model=model)
                        # Merge each independently observed account/model window.
                    except (ValueError, TypeError, AttributeError):
                        continue
            except OSError:
                continue

    # Private provider homes are gone by this point; consume their sanitized
    # observations only when bound to the selected actual account and pool.
    from makewand.execution_evidence import read_recent
    # This account's newest records only: other accounts' traffic cannot push them out.
    records = (read_recent(engine=provider, account_ref=identity["account_ref"], pool_ref=identity["pool_ref"])
               if identity and identity["identity_known"] else ())
    for record in records:
        if (record.get("engine") != provider or record.get("identity_known") is not True
                or record.get("account_ref") != identity["account_ref"]
                or record.get("pool_ref") != identity["pool_ref"]):
            continue
        for window in record.get("windows") or []:
            try:
                at = window["observed_at_ms"] / 1000
                if at <= 0:
                    continue
                add(window.get("used_percent"), window.get("window_minutes"),
                    window.get("reset_at_ms", 0) / 1000 or None, at,
                    "execution_evidence", window.get("window_key"), model=window.get("model") or "")
            except (ValueError, TypeError, KeyError, AttributeError):
                continue

    latest_identity = account_identity(provider)
    if identity and latest_identity and identity["account_ref"] != latest_identity["account_ref"]:
        evidence.clear()
        reasons.add("account_changed_during_read")
    identity = latest_identity or identity
    if not evidence:
        if reasons:
            return {"percentage": None, "status": "unknown", "desc": "官方额度数据陈旧、过期或无法关联当前认证目录；按未知处理",
                    "resets_at": None, "is_unlimited": False, "source": "unknown",
                    "selected_source": None, "windows": [], "ttl_seconds": ttl,
                    "unverified_reasons": sorted(reasons), **(identity or {})}
        return None
    windows = {}
    for candidate in evidence:
        key = candidate["window_key"]
        previous = windows.get(key)
        if previous is None or (candidate["observed"], candidate["selected_source"] == "codex_session") >= (previous["observed"], previous["selected_source"] == "codex_session"):
            windows[key] = candidate
    shared = [entry for entry in windows.values() if not entry["model"]]
    if not shared:
        return {"percentage": None, "status": "unknown", "source": "unknown",
                "desc": "只有模型子限额，账号共享额度未知", "is_unlimited": False,
                "resets_at": None, "windows": list(windows.values()), **identity}
    worst = max(shared, key=lambda entry: entry["used_percent"])
    remaining = round(100 - worst["used_percent"], 2)
    scoped = max([entry["used_percent"] for entry in windows.values() if entry["model"]] + [worst.get("scoped_percent") or 0])
    scoped_message = ""
    if isinstance(scoped, (int, float)) and not isinstance(scoped, bool) and scoped >= 99:
        scoped_message = " · 顶配模型周限额已达 100%（仅提示）"
    return {"percentage": remaining, "status": _quota_status(remaining, reserve),
            **_quota_reserve_metadata(reserve, reserve_error),
            "desc": _quota_reserve_description(f"官方报告{worst['window']}剩余额度: {remaining:g}% (已用 {worst['used_percent']:g}%){scoped_message}", reserve, reserve_error),
            "resets_at": worst["resets_at"], "is_unlimited": False, "source": "official",
            **identity, "selected_source": worst["selected_source"], "window": worst["window"],
            "window_minutes": worst["window_minutes"], "ttl_seconds": ttl,
            "observed_at": datetime.fromtimestamp(worst["observed"], timezone.utc).isoformat(),
            "age_seconds": max(0, now - worst["observed"]),
            "windows": [{key: value for key, value in entry.items() if key not in ("observed", "window_key", "scoped_percent")}
                        for entry in windows.values()]}


def calculate_provider_quota(provider: str, info: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
    """
    Returns a quota *indicator* (0-100) for display and pacing.

    Only percentages parsed from provider output ("NN% left") are official
    ("source": "official"). Everything else is either inferred from the health
    status or estimated from makewand's own local call counts
    ("source": "local_estimate"); callers must not present those as real quota.
    "unknown" (never probed) is neutral: it is not an exhausted quota.
    """
    if info is None:
        info = load_status_cache().get(provider, {})

    status = info.get("status", "unknown")
    reason = info.get("reason", "")
    resets_at = info.get("resets_at")
    updated_at = info.get("updated_at", "")

    reserve, reserve_error = _quota_reserve_setting()
    reserve_quota = None
    if _is_quota_reserve_gate(info) and status in ("limited", "unknown", "healthy", "warning"):
        official = _get_official_subscription_quota(provider, resets_at=resets_at)
        if official and official.get("source") == "official":
            reserve_quota = official

    # CLI health/version checks establish availability, never a quota percentage.
    if provider == "agy":
        res = {
            "percentage": None, "status": status, "resets_at": resets_at,
            "is_unlimited": False, "source": "unknown",
            "desc": f"额度未知 · 来源: agy 无额度查询接口 · 观察时间: {updated_at or '未知'}",
        }
    # A buffer verdict must display the real remaining quota, not a fake 0%.
    elif reserve_quota is not None:
        res = reserve_quota
    # 1. Limited / Exhausted (0%)
    elif status == "limited":
        reset_desc = f" (预计解封: {resets_at})" if resets_at else " (已达当前限额)"
        res = {
            "percentage": 0,
            "status": "limited",
            "desc": f"额度已耗尽{reset_desc}",
            "resets_at": resets_at,
            "is_unlimited": False,
            "source": "status",
        }
    # 2. Never probed / expired verdict: neutral, not "0% left"
    elif status == "unknown":
        res = {
            "percentage": None,
            "status": "unknown",
            "desc": reason or "未探测 (中性处理；运行 'makewand probe' 获取实时状态)",
            "resets_at": None,
            "is_unlimited": False,
            "source": "unknown",
        }
    # 3. Disabled / Missing / Needs Auth / Error
    elif status in ("disabled", "needs_auth", "error", "missing"):
        desc = reason or "未就绪或未授权"
        if status == "needs_auth" and "重新登录" not in desc:
            desc = f"{desc} · {get_reauth_hint(provider)}"
        res = {
            "percentage": 0,
            "status": status,
            "desc": desc,
            "resets_at": None,
            "is_unlimited": False,
            "source": "status",
        }
    # 4. Unlimited local models or enterprise tiers
    elif provider == "local":
        res = {
            "percentage": 100,
            "status": "healthy",
            "desc": "本地私有模型 · 无云端额度 · 0 Token 成本",
            "resets_at": None,
            "is_unlimited": True,
            "source": "assumed",
        }
    elif reason and re.search(r"(\d+)\s*%\s*(?:left|remaining|剩余)", reason, re.IGNORECASE):
        pct_match = re.search(r"(\d+)\s*%\s*(?:left|remaining|剩余)", reason, re.IGNORECASE)
        pct = int(pct_match.group(1))
        res = {
            "percentage": max(0, min(100, pct)),
            "status": _quota_status(pct, reserve, healthy_at=20, inclusive=False),
            **_quota_reserve_metadata(reserve, reserve_error),
            "desc": _quota_reserve_description(f"官方报告剩余额度: {pct}%", reserve, reserve_error),
            "resets_at": resets_at,
            "is_unlimited": False,
            "source": "official",
        }
    else:
        # Check official snapshot/session quota before falling back to local counter estimate
        official_quota = _get_official_subscription_quota(provider, resets_at=resets_at)
        if official_quota:
            res = official_quota
        else:
            # 5. Estimate from rolling usage and burn rate penalty
            try:
                from makewand.usage import get_burn_rate_penalty, get_engine_usage_stats
                penalty, pen_reason = get_burn_rate_penalty(provider)
                u24 = get_engine_usage_stats(window_hours=24.0).get(provider, {}).get("total", 0)

                if penalty <= -4.0:
                    pct = 5
                    desc = f"高频调用削峰保护中 (24h 调用: {u24}次)"
                elif penalty <= -3.0:
                    pct = 20
                    desc = f"额度消耗较快 (24h 调用: {u24}次)"
                elif penalty <= -2.0:
                    pct = 40
                    desc = f"滑动窗口用量活跃 (24h 调用: {u24}次)"
                elif penalty <= -1.0:
                    pct = 65
                    desc = f"滑动窗口运行平稳 (24h 调用: {u24}次)"
                else:
                    if u24 == 0:
                        pct = 100
                        desc = "额度充沛 · 滑动窗口无压力"
                    else:
                        pct = max(75, 100 - min(25, u24 * 2))
                        desc = f"额度充沛 · 运行健康 (24h 调用: {u24}次)"

                res = {
                    "percentage": pct,
                    "status": "healthy" if pct >= 25 else "warning",
                    "desc": f"{desc} [本地调用计数估算，非真实配额]",
                    "resets_at": resets_at,
                    "is_unlimited": False,
                    "source": "local_estimate",
                }
            except Exception:
                res = {
                    "percentage": 85,
                    "status": "healthy",
                    "desc": "运行健康 [本地调用计数估算，非真实配额]",
                    "resets_at": resets_at,
                    "is_unlimited": False,
                    "source": "local_estimate",
                }

    res.setdefault("reserve_percent", reserve)
    res.setdefault("reserve_error", reserve_error)
    if reserve_error and reserve_error not in res["desc"]:
        res["desc"] += f" · {reserve_error}"
    res["updated_at"] = updated_at
    res["source_label"] = QUOTA_SOURCE_LABELS.get(res.get("source"), "")
    try:
        from makewand.usage import get_predictive_pacing_status
        res["predictive_pacing"] = get_predictive_pacing_status(provider)
    except Exception:
        pass
    if is_status_stale(info):
        res["stale"] = True
        res["desc"] = f"{res['desc']} · 状态缓存已超过 {STATUS_STALE_SECONDS // 3600} 小时未刷新，按中性处理 (运行 'makewand probe')"
    return res
