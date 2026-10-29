"""Read-only signs that a task went past the per-request call rules.

The rules (default 3 calls per user request; a retry after UNKNOWN or TIMEOUT
needs the user's approval) are enforced by callers, not by the ledger: a cap
can be raised and a failed call redispatched. This lists, for `makewand
observe`, tasks in the default and registered ledgers whose cap is above the
default, and tasks that dispatched again after an UNKNOWN or TIMEOUT call.
Showing them is all it does; whether they were authorized is for the user.
"""
import json
import math
import time

from makewand import layered_budget, ledger_registry

DEFAULT_TASK_CAP = 3
_FAILED = ("UNKNOWN", "TIMEOUT")


def _ledgers():
    yield layered_budget.default_path()
    yield from ledger_registry.known_ledgers()


def _load(path):
    try:
        data = json.loads(ledger_registry._read_bounded(path))
    except (OSError, ValueError, RecursionError):
        return None
    return data if isinstance(data, dict) else None


def _number(value):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    try:
        return value if math.isfinite(value) else None
    except OverflowError:  # JSON integers can exceed the float range.
        return None


def _ended(attempt):
    """When an attempt ended, from finished_at or started_at + seconds; None if unknown."""
    finished = _number(attempt.get("finished_at"))
    if finished is not None:
        return finished
    seconds = _number(attempt.get("seconds"))
    return attempt["started_at"] + seconds if seconds is not None else None


# A redispatch starts after the failure has ended; peers of one parallel
# ensemble or race start together, so a later start within this gap is a peer.
RETRY_GAP_SECONDS = 2.0


def flags(window_seconds=86400, now=None):
    """[{task, ledger, kind: "raised_cap"|"retry_after_failure", detail}], read without locks."""
    cutoff = (time.time() if now is None else now) - window_seconds
    found = []
    for path in _ledgers():
        data = _load(path)
        if data is None:
            continue
        scopes = data.get("scopes")
        tasks = scopes.get("tasks") if isinstance(scopes, dict) else None
        for task, counter in (tasks.items() if isinstance(tasks, dict) else ()):
            if not isinstance(counter, dict):
                continue
            maximum, used, last = (_number(counter.get(k)) for k in ("maximum", "used", "last_used"))
            if maximum is not None and maximum > DEFAULT_TASK_CAP and last is not None and last >= cutoff:
                found.append({"task": str(task), "ledger": str(path), "kind": "raised_cap",
                              "detail": f"上限 {int(maximum)}（默认 {DEFAULT_TASK_CAP}），已用 {int(used or 0)}"})
        attempts = data.get("attempts")
        by_task = {}
        for attempt in attempts if isinstance(attempts, list) else ():
            if (isinstance(attempt, dict) and isinstance(attempt.get("task_id"), str) and attempt["task_id"]
                    and _number(attempt.get("started_at")) is not None):
                by_task.setdefault(attempt["task_id"], []).append(attempt)
        for task, runs in by_task.items():
            runs.sort(key=lambda a: a["started_at"])
            for index, failed in enumerate(runs):
                ended = _ended(failed)
                if failed.get("result_status") not in _FAILED or ended is None:
                    continue
                later = [a for a in runs[index + 1:] if a["started_at"] >= ended + RETRY_GAP_SECONDS]
                if later and max(ended, later[-1]["started_at"]) >= cutoff:
                    found.append({"task": task, "ledger": str(path), "kind": "retry_after_failure",
                                  "detail": f"{failed.get('engine')} {failed.get('result_status')} 后又派 {len(later)} 次"
                                            f"（{', '.join(sorted({str(a.get('engine')) for a in later}))}）"})
                    break
    return found
