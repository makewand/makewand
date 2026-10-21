"""Three independent counters, committed together by call_budget's sidecar lock.

The file is the transaction boundary, not the budget scope. Legacy ledgers
remain lifetime caps; they are never silently reset or weakened on migration.

Clock rule, shared with execution/layered.go: daily buckets use the local zone,
or MAKEWAND_BUDGET_TIMEZONE (an IANA name). A day that moves back keeps counting
into the latest bucket already seen, and until the real day reaches it the daily
caps count every bucket from today to that one. scopes.clock is a ratchet against
a clock that steps back, but it is trusted, and stored, at most CLOCK_SLACK
seconds ahead of real time, so a ledger written while the clock was ahead cannot
stall lease cleanup. Capacity holds are dated from that clock, and one created
ahead of it is re-dated to it, keeping its duration.
"""
import hashlib
import math
import os
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

from makewand.account_leases import TTL

DEFAULT_MACHINE = 1000
DEFAULT_PROJECT = 200
CLOCK_SLACK = 300
TIMEZONE_ENV = "MAKEWAND_BUDGET_TIMEZONE"
# reserve_capacity refuses longer holds; a hold without created_at counts as this long.
MAX_HOLD_SECONDS = 86400
_REPAIR_HINT = "repair: makewand budget clock --repair"
_warned = set()


def _warn_once(kind, message):
    if kind not in _warned:
        _warned.add(kind)
        print(f"makewand: warning: {message} ({_REPAIR_HINT})", file=sys.stderr)


def effective_clock(data, now=None):
    """Real time, or the persisted clock when it is ahead, by at most CLOCK_SLACK."""
    now = time.time() if now is None else now
    clock = data.get("scopes", {}).get("clock", 0)
    if clock > now + CLOCK_SLACK:
        _warn_once("clock", f"call budget clock is {clock - now:.0f}s ahead of real time; "
                            f"lease and hold expiry use at most {CLOCK_SLACK}s of it")
        return now + CLOCK_SLACK
    return max(now, clock)


def _hold_dates(hold):
    """(created, duration) of a capacity hold. One written before holds carried
    created_at counts as the longest a hold can be."""
    if "created_at" in hold:
        return hold["created_at"], hold["expires_at"] - hold["created_at"]
    return hold["expires_at"] - MAX_HOLD_SECONDS, MAX_HOLD_SECONDS


def redate_hold(hold, clock):
    """Re-date a hold created after `clock` to it, keeping its duration, as scopes()
    re-dates a lease: one made while the clock was ahead then lasts as long as asked
    from real time, not until real time catches up. Returns whether it moved."""
    created, duration = _hold_dates(hold)
    if created <= clock:
        return False
    hold["expires_at"] = clock + duration
    if "created_at" in hold:
        hold["created_at"] = clock
    return True


def day_bucket(moment):
    """The daily bucket of an epoch time; execution/layered.go computes the same."""
    name = os.environ.get(TIMEZONE_ENV, "").strip()
    if not name:
        return datetime.fromtimestamp(moment).strftime("%Y-%m-%d")
    if name == "UTC":
        # Go's LoadLocation needs no time zone database for UTC; zoneinfo does (Windows without tzdata).
        return datetime.fromtimestamp(moment, timezone.utc).strftime("%Y-%m-%d")
    from makewand.call_budget import BudgetConfigError
    try:
        from zoneinfo import ZoneInfo
        zone = ZoneInfo(name)
    except (ValueError, KeyError, OSError) as error:  # ZoneInfoNotFoundError is a KeyError
        raise BudgetConfigError(f"{TIMEZONE_ENV} must name an IANA time zone such as Asia/Shanghai or UTC") from error
    return datetime.fromtimestamp(moment, zone).strftime("%Y-%m-%d")


def enabled(data=None):
    return bool(data and "scopes" in data) or any(os.environ.get(k) for k in (
        "MAKEWAND_LAYERED_BUDGET", "MAKEWAND_MACHINE_DAILY_CALLS", "MAKEWAND_PROJECT_DAILY_CALLS"))


def positive(value):
    from makewand.call_budget import BudgetError
    if isinstance(value, bool):
        raise BudgetError("budget maximum must be a positive integer")
    result = int(value)
    if str(result) != str(value) or not 0 < result <= 9223372036854775807:
        raise BudgetError("budget maximum must be a positive integer")
    return result


def project_ref():
    # Canonical invocation directory; callers can supply a canonical project
    # root to group worktrees. Never derive identity from provider homes.
    root = str(Path(os.environ.get("MAKEWAND_PROJECT_ROOT") or os.getcwd()).resolve())
    return hashlib.sha256(root.encode()).hexdigest()[:32]


def default_path():
    root = Path(os.environ.get("MAKEWAND_CONFIG_DIR") or
                Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config") / "makewand")
    return str(root / "call-budget.json")


def initialize(data):
    data["scopes"] = {"tasks": {}, "projects": {}, "machine": {}, "day": "", "clock": 0}
    data["maximum"] = positive(os.environ.get("MAKEWAND_MACHINE_DAILY_CALLS", DEFAULT_MACHINE))


def validate(data):
    from makewand.call_budget import BudgetError
    s = data["scopes"]
    if (not isinstance(s, dict) or not isinstance(s.get("day"), str)
            or type(s.get("clock")) not in (int, float)
            or not math.isfinite(s["clock"]) or s["clock"] < 0):
        raise BudgetError("invalid layered budget state")
    if "legacy_maximum" in data:
        if (type(data["legacy_maximum"]) is not int or not 0 < data["legacy_maximum"] <= 9223372036854775807
                or type(data.get("legacy_used", 0)) is not int
                or not 0 <= data.get("legacy_used", 0) <= 9223372036854775807):
            raise BudgetError("invalid legacy budget cap")
    for group in ("tasks", "projects", "machine"):
        if not isinstance(s.get(group), dict):
            raise BudgetError("invalid budget scope map")
        for key, counter in s[group].items():
            if (not isinstance(key, str) or not isinstance(counter, dict)
                    or type(counter.get("maximum")) is not int or counter["maximum"] <= 0
                    or counter["maximum"] > 9223372036854775807
                    or type(counter.get("used")) is not int or not 0 <= counter["used"] <= 9223372036854775807):
                raise BudgetError("invalid budget scope counter")
            touched = counter.get("last_used")
            if touched is not None and (type(touched) not in (int, float) or not math.isfinite(touched) or touched < 0):
                raise BudgetError("invalid scope last-used timestamp")

    for entry in [*data["attempts"], *data.get("holds", {}).values()]:
        keys = entry.get("scopes")
        if keys is None:
            continue
        if not isinstance(keys, list) or len(keys) != 3:
            raise BudgetError("invalid budget scope references")
        for pair, group in zip(keys, ("tasks", "projects", "machine")):
            if (not isinstance(pair, list) or len(pair) != 2 or pair[0] != group
                    or not isinstance(pair[1], str) or pair[1] not in s[group]):
                raise BudgetError("invalid budget scope reference")


def scopes(data, task, maximum):
    s = data["scopes"]
    now = time.time()
    day = day_bucket(now)
    if day < s["day"]:
        # A clock that moved back keeps counting into the latest bucket already seen:
        # refusing would stop every dispatch, an earlier bucket would reset counts.
        _warn_once("day", f"call budget day {s['day']} is ahead of today's bucket {day}; counting into {s['day']}")
        day = s["day"]
    # Persisting the capped clock heals a skewed ledger on its next admission.
    s["day"], s["clock"] = day, effective_clock(data, now)
    # A lease dated past what this clock allows was written while the clock was
    # ahead. Date it like a fresh heartbeat: a live owner renews it, a dead
    # owner's lease expires one TTL from now instead of when real time catches up.
    # A hold created after this clock keeps its duration from it. Callers persist
    # both even when they refuse the call.
    for lease in data.get("leases", {}).values():
        lease["expires_at"] = min(lease["expires_at"], s["clock"] + TTL)
    for hold in data.get("holds", {}).values():
        redate_hold(hold, s["clock"])
    task = task or os.environ.get("MAKEWAND_TASK_ID") or "legacy-task"
    project = project_ref()
    keys = [("tasks", task), ("projects", project + ":" + day), ("machine", day)]
    limits = [positive(maximum if maximum is not None else os.environ.get("MAKEWAND_MAX_MODEL_CALLS", DEFAULT_PROJECT)),
              positive(os.environ.get("MAKEWAND_PROJECT_DAILY_CALLS", DEFAULT_PROJECT)),
              positive(os.environ.get("MAKEWAND_MACHINE_DAILY_CALLS", DEFAULT_MACHINE))]
    for (group, key), limit in zip(keys, limits):
        counter = s[group].setdefault(key, {"maximum": limit, "used": 0})
        # Tightening only changes this scope, never the machine or another task.
        counter["maximum"] = min(counter["maximum"], limit)
        counter["last_used"] = s["clock"]
    return [[group, key] for group, key in keys]


def _day_span(s, group, key, today):
    """The buckets one scope's cap counts. While the real day is earlier than the
    latest bucket (the clock moved back), the calls charged into the later buckets
    were made today: a daily scope counts every bucket from today to the latest."""
    if group == "tasks" or today >= s["day"]:
        return [key]
    prefix = key.rpartition(":")[0]
    return [key] + [k for k in s[group] if k != key and k.rpartition(":")[0] == prefix
                    and today <= k.rpartition(":")[2] <= s["day"]]


def _occupies(entry, references):
    return any(reference in (entry.get("scopes") or []) for reference in references)


def available(data, keys, holds, claim=None, now=None):
    s = data["scopes"]
    today = day_bucket(time.time() if now is None else now)
    result = []
    for group, key in keys:
        span = [[group, k] for k in _day_span(s, group, key, today)]
        held = sum(h["remaining"] for h in holds.values() if _occupies(h, span))
        if claim is not None and _occupies(claim, span):
            held -= 1
        counters = [s[group][k] for _, k in span]
        result.append(min(c["maximum"] for c in counters) - sum(c["used"] for c in counters) - held)
    if data.get("legacy_maximum") is not None:
        legacy_used = data.get("legacy_used", 0)
        result = [value - legacy_used for value in result]
        result.append(data["legacy_maximum"] - (data.get("archived_used", 0) + sum(e.get("status") != "released" for e in data["attempts"])) -
                      sum(h["remaining"] for h in holds.values()) + (1 if claim else 0))
    return min(result)


def spend(data, keys, delta=1):
    for group, key in keys:
        data["scopes"][group][key]["used"] += delta
        data["scopes"][group][key]["last_used"] = effective_clock(data)


def repair_clock(data, now=None):
    """Bring a persisted clock and day that lead real time back to it; report what changed.

    Counts are never lowered: calls counted in a later day's bucket stay there and are
    charged to today's bucket as well, so moving the day back cannot reset a limit;
    holds counted there move to today's buckets with them. Live lease owners renew
    within seconds; a dead owner's lease expires one TTL from now. A hold dated in the
    future moves back by its lead, keeping its duration.
    """
    now = time.time() if now is None else now
    s = data["scopes"]
    today = day_bucket(now)
    leases = list(data.get("leases", {}).values())
    holds = list(data.get("holds", {}).values())
    report = {
        "clock_lead_seconds": max(0.0, s["clock"] - now), "day": s["day"], "today": today,
        # No writer that follows the clock rule leaves these this far ahead.
        "skewed_leases": sum(lease["expires_at"] > now + CLOCK_SLACK + TTL for lease in leases),
        "skewed_holds": sum(_hold_dates(hold)[0] > now + CLOCK_SLACK for hold in holds),
        "carried_buckets": 0, "moved_holds": 0, "changed": False,
    }
    report["skewed"] = bool(report["clock_lead_seconds"] > CLOCK_SLACK or s["day"] > today
                            or report["skewed_leases"] or report["skewed_holds"])

    def clamp(record, field, limit):
        if record.get(field, 0) > limit:
            record[field] = limit
            report["changed"] = True

    clamp(s, "clock", now)
    for group in ("tasks", "projects", "machine"):
        for counter in s[group].values():
            clamp(counter, "last_used", now)
    for lease in leases:
        clamp(lease, "expires_at", now + TTL)
        clamp(lease, "heartbeat_at", now)
    for hold in holds:
        if redate_hold(hold, now):
            report["changed"] = True
    if s["day"] > today:
        moved = {}
        for group, today_key in (("machine", lambda key: today),
                                 ("projects", lambda key: key.rpartition(":")[0] + ":" + today)):
            for key, counter in list(s[group].items()):
                if key.rpartition(":")[2] > today:
                    moved[group, key] = today_key(key)
                    target = s[group].setdefault(moved[group, key], {"maximum": counter["maximum"], "used": 0})
                    target["used"] += counter["used"]
                    target["maximum"] = min(target["maximum"], counter["maximum"])
                    target["last_used"] = now
                    report["carried_buckets"] += 1
        # Admission counts a hold in the buckets it names: left in a later day's, it
        # would reserve nothing today and another call could take its capacity.
        for hold in holds:
            references = hold.get("scopes") or []
            if any(tuple(reference) in moved for reference in references):
                hold["scopes"] = [[group, moved.get((group, key), key)] for group, key in references]
                report["moved_holds"] += 1
        s["day"] = today
        report["changed"] = True
    return report
