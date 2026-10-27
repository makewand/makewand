"""Best-effort registry of call budget ledgers other than the default one.

A dispatch may name its own ledger (--call-budget-file or
MAKEWAND_CALL_BUDGET_FILE). Its task cap still applies, but its machine and
project daily counters are separate and the default ledger never sees its
calls. Admission notes such a ledger here, after releasing the ledger lock,
so `makewand observe` can show it. Recording never affects admission: every
error is ignored. Go's execution/registry.go writes the same line format.
"""
import json
import os
import stat
import sys
import time
from pathlib import Path

from makewand import layered_budget

REGISTRY_NAME = "ledger-registry.jsonl"
# A registry line is one small O_APPEND write, so concurrent writers never interleave.
_MAX_LINE = 4096
# Reads are bounded like call_budget's own ledger reads.
_MAX_READ = 16 * 1024 * 1024
_noted = set()
_announced = set()


def registry_path():
    return Path(layered_budget.default_path()).with_name(REGISTRY_NAME)


def _resolved(value):
    try:
        return str(Path(value).expanduser().resolve())
    except (OSError, ValueError, RuntimeError):
        return None


def _open_regular(path, flags, mode=0o600):
    """Open without following a symlink or blocking on a FIFO; refuse non-regular files."""
    flags |= getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_CLOEXEC", 0)
    descriptor = os.open(path, flags, mode)
    try:
        if not stat.S_ISREG(os.fstat(descriptor).st_mode):
            raise OSError(f"{path} is not a regular file")
    except BaseException:
        os.close(descriptor)
        raise
    return descriptor


def _read_bounded(path):
    descriptor = _open_regular(path, os.O_RDONLY)
    with os.fdopen(descriptor, "rb") as handle:
        data = handle.read(_MAX_READ + 1)
    if len(data) > _MAX_READ:
        raise ValueError(f"{path} is larger than {_MAX_READ} bytes")
    return data.decode("utf-8", errors="replace")


def note(ledger_path):
    """Record a non-default ledger once per process and say so once on stderr."""
    try:
        path = _resolved(ledger_path)
        if path is None or path in _noted or path == _resolved(layered_budget.default_path()):
            return
        if path not in _announced:
            _announced.add(path)
            print(f"makewand: note: using call budget ledger {path}, not the default; "
                  "its daily machine/project counters are separate from the default ledger "
                  "(registered for `makewand observe`)", file=sys.stderr)
        encoded = (json.dumps({"path": path, "ts": time.time()}, ensure_ascii=False) + "\n").encode()
        if len(encoded) > _MAX_LINE:
            return
        registry = registry_path()
        registry.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        descriptor = _open_regular(registry, os.O_WRONLY | os.O_APPEND | os.O_CREAT)
        try:
            os.write(descriptor, encoded)
        finally:
            os.close(descriptor)
        _noted.add(path)  # only once written, so a later admission retries a failed one
    except Exception:  # noqa: BLE001 - visibility only, never an admission failure
        return


def known_ledgers():
    """Registered ledger paths, newest registration first."""
    latest = {}
    try:
        text = _read_bounded(registry_path())
    except (OSError, ValueError):
        return []
    for raw in text.splitlines():
        try:
            row = json.loads(raw)
            path, ts = row["path"], float(row["ts"])
        except (ValueError, KeyError, TypeError, RecursionError):
            continue
        if isinstance(path, str) and ts > latest.get(path, -1.0):
            latest[path] = ts
    default = _resolved(layered_budget.default_path())
    return [p for p, _ in sorted(latest.items(), key=lambda item: -item[1]) if p != default]


def recent_activity(window_seconds=86400, now=None):
    """Calls each registered ledger admitted in the window. Read-only and lock-free:
    ledgers are replaced atomically, and one that cannot be read (gone, not a
    regular file, too large) is skipped."""
    cutoff = (time.time() if now is None else now) - window_seconds
    summary = []
    for path in known_ledgers():
        try:
            attempts = list(json.loads(_read_bounded(path)).get("attempts", []))
        except (OSError, ValueError, AttributeError, TypeError, RecursionError):
            continue
        recent = [a for a in attempts if isinstance(a, dict)
                  and isinstance(a.get("started_at"), (int, float)) and a["started_at"] >= cutoff]
        if recent:
            summary.append({
                "path": path,
                "calls": len(recent),
                "tasks": sorted({str(a.get("task_id")) for a in recent if a.get("task_id")}),
                "engines": sorted({str(a.get("engine")) for a in recent if a.get("engine")}),
                "abnormal": sum(1 for a in recent if a.get("result_status") in ("UNKNOWN", "TIMEOUT", "FAILED")),
            })
    return summary
