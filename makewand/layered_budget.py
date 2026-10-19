"""Three independent counters, committed together by call_budget's sidecar lock.

The file is the transaction boundary, not the budget scope. Legacy ledgers
remain lifetime caps; they are never silently reset or weakened on migration.
"""
import hashlib
import math
import os
import time
from datetime import datetime, timezone, timedelta
from pathlib import Path

DEFAULT_MACHINE = 1000
DEFAULT_PROJECT = 200


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
    from makewand.call_budget import BudgetError
    s = data["scopes"]
    now = time.time()
    day = datetime.fromtimestamp(now, timezone(timedelta(hours=8))).strftime("%Y-%m-%d")
    if day < s["day"]:
        raise BudgetError("budget clock moved back across an Asia/Shanghai day")
    s["day"], s["clock"] = day, max(now, s["clock"])
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


def available(data, keys, holds, claim=None):
    from makewand.call_budget import BudgetError
    result = []
    for group, key in keys:
        counter = data["scopes"][group][key]
        held = sum(h["remaining"] for h in holds.values() if [group, key] in h.get("scopes", []))
        if claim is not None and [group, key] in claim.get("scopes", []):
            held -= 1
        result.append(counter["maximum"] - counter["used"] - held)
    if data.get("legacy_maximum") is not None:
        legacy_used = data.get("legacy_used", 0)
        result = [value - legacy_used for value in result]
        result.append(data["legacy_maximum"] - (data.get("archived_used", 0) + sum(e.get("status") != "released" for e in data["attempts"])) -
                      sum(h["remaining"] for h in holds.values()) + (1 if claim else 0))
    return min(result)


def spend(data, keys, delta=1):
    for group, key in keys:
        data["scopes"][group][key]["used"] += delta
        data["scopes"][group][key]["last_used"] = max(time.time(), data["scopes"]["clock"])
