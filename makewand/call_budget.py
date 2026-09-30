"""Process-safe admission ledger for bounded model task dispatches.

Reservations count attempted Makewand dispatches, including failures. Vendor
CLI internal turns and token usage remain separately measured or unknown.
"""
import json
import math
import errno
import stat
import os
import tempfile
import time
import uuid
from contextlib import contextmanager
from pathlib import Path
from makewand import filelock


class BudgetError(RuntimeError):
    pass


class BudgetDeadlineError(BudgetError):
    pass


MAX_LEDGER_BYTES = 16 * 1024 * 1024


def _reject_nonfinite_json(_):
    raise BudgetError("model call budget contains a nonfinite JSON number")


def _admission_deadline():
    from makewand.execution_runtime import current_context
    return current_context().get("_deadline_monotonic")


@contextmanager
def _ledger(budget_file=None, max_model_calls=None, lock_deadline=None):
    value = budget_file or os.environ.get("MAKEWAND_CALL_BUDGET_FILE")
    if not value:
        if max_model_calls is not None or os.environ.get("MAKEWAND_MAX_MODEL_CALLS"):
            raise BudgetError("model call maximum requires a shared ledger path")
        yield None, None
        return
    path = Path(value).expanduser().resolve()
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    descriptor = os.open(str(path) + ".lock", os.O_CREAT | os.O_RDWR | getattr(os, "O_NOFOLLOW", 0), 0o600)
    acquired = False
    # Accounting/cleanup uses its own bounded deadline after cancellation.
    lock_deadline = time.monotonic() + 5 if lock_deadline is None else lock_deadline
    try:
        if not stat.S_ISREG(os.fstat(descriptor).st_mode):
            raise BudgetError("model call budget lock must be a regular file")
        while True:
            if time.monotonic() >= lock_deadline:
                raise BudgetDeadlineError("execution deadline expired while acquiring the model call budget")
            try:
                filelock.flock(descriptor, filelock.LOCK_EX | filelock.LOCK_NB)
                acquired = True
                break
            except OSError as error:
                if error.errno not in (errno.EACCES, errno.EAGAIN, errno.EWOULDBLOCK) and getattr(error, "winerror", None) not in (33, 158):
                    raise
                time.sleep(min(.01, max(0, lock_deadline - time.monotonic())))
        try:
            ledger_fd = os.open(path, os.O_RDONLY | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_NOFOLLOW", 0))
        except FileNotFoundError:
            limit = int(max_model_calls if max_model_calls is not None else os.environ.get("MAKEWAND_MAX_MODEL_CALLS", "0"))
            if limit <= 0:
                raise BudgetError("model call budget requires a positive maximum")
            data = {"schema": 1, "maximum": limit, "attempts": []}
        else:
            with os.fdopen(ledger_fd, "rb") as stream:
                if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
                    raise BudgetError("model call budget ledger must be a regular file")
                raw = stream.read(MAX_LEDGER_BYTES + 1)
                if len(raw) > MAX_LEDGER_BYTES:
                    raise BudgetError("model call budget ledger exceeds 16 MiB")
            data = json.loads(raw.decode("utf-8"), parse_constant=_reject_nonfinite_json)
        _validate(data)
        maximum = _maximum(data, max_model_calls)
        if maximum < data["maximum"]:
            data["maximum"] = maximum
            _save(path, data)
        yield path, data
    finally:
        if acquired:
            filelock.flock(descriptor, filelock.LOCK_UN)
        os.close(descriptor)


def _validate(data):
    if (not isinstance(data, dict) or type(data.get("schema")) is not int or data["schema"] != 1
            or type(data.get("maximum")) is not int or not 0 < data["maximum"] <= 9223372036854775807
            or not isinstance(data.get("attempts"), list)):
        raise BudgetError("invalid model call budget ledger")
    identifiers = set()
    for entry in data["attempts"]:
        if (not isinstance(entry, dict) or not isinstance(entry.get("id"), str)
                or not entry["id"] or entry["id"] in identifiers):
            raise BudgetError("invalid model call budget attempts")
        identifiers.add(entry["id"])
    holds = data.get("holds", {})
    if not isinstance(holds, dict):
        raise BudgetError("invalid model call capacity holds")
    for lease, hold in holds.items():
        if (not isinstance(lease, str) or not lease or not isinstance(hold, dict)
                or type(hold.get("remaining")) is not int or not 0 <= hold["remaining"] <= 9223372036854775807
                or isinstance(hold.get("expires_at"), bool)
                or not isinstance(hold.get("expires_at"), (int, float))
                or (isinstance(hold["expires_at"], int) and abs(hold["expires_at"]) > 9223372036854775807)
                or not math.isfinite(hold["expires_at"])):
            raise BudgetError("invalid model call capacity hold")


def _maximum(data, max_model_calls=None):
    configured = max_model_calls if max_model_calls is not None else os.environ.get("MAKEWAND_MAX_MODEL_CALLS", data["maximum"])
    if isinstance(configured, bool):
        raise BudgetError("model call maximum must be a positive integer")
    configured = int(configured)
    if configured <= 0:
        raise BudgetError("model call maximum must be a positive integer")
    return min(data["maximum"], configured)


def _active_holds(data):
    now = time.time()
    holds = data.get("holds", {})
    expired = [lease for lease, hold in holds.items() if hold["expires_at"] <= now]
    for lease in expired:
        del holds[lease]
    return holds


def _save(path, data):
    encoded = (json.dumps(data, ensure_ascii=False, allow_nan=False, indent=2) + "\n").encode("utf-8")
    if len(encoded) > MAX_LEDGER_BYTES:
        raise BudgetError("model call budget ledger exceeds 16 MiB")
    descriptor, name = tempfile.mkstemp(prefix=".call-budget-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(encoded)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        if os.name != "nt":
            directory_fd = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def reserve(engine, tier, model=None, readonly=False, *, lease_id=None, task_id=None,
            stage="provider", account_ref=None, api_policy="subscription_only",
            budget_file=None, max_model_calls=None, deadline_monotonic=None):
    try:
        deadline_monotonic = _admission_deadline() if deadline_monotonic is None else deadline_monotonic
        with _ledger(budget_file, max_model_calls, deadline_monotonic) as (path, data):
            if data is None:
                return None
            maximum = _maximum(data, max_model_calls)
            holds = _active_holds(data)
            reserved = sum(hold["remaining"] for hold in holds.values())
            if lease_id is not None:
                hold = holds.get(lease_id)
                if hold is None or hold["remaining"] <= 0:
                    raise BudgetError("model call capacity hold expired or exhausted")
                reserved -= 1
            if len(data["attempts"]) + reserved >= maximum:
                raise BudgetError(f"model task budget exhausted ({len(data['attempts'])}/{maximum})")
            if lease_id is not None:
                hold["remaining"] -= 1
            attempt = uuid.uuid4().hex
            data["attempts"].append({"id": attempt, "engine": engine, "tier": tier,
                                     "requested_model": model, "readonly": readonly,
                                     "benchmark_run": os.environ.get("MAKEWAND_BENCHMARK_RUN_ID"),
                                     "started_at": time.time(), "status": "started",
                                     "task_id": task_id, "stage": stage,
                                     "account_ref": account_ref, "api_policy": api_policy,
                                     "result_status": None, "outcome_known": None,
                                     "tokens": None, "monetary_cost": None})
            _save(path, data)
            return attempt
    except BudgetError:
        raise
    except (OSError, ValueError, KeyError, TypeError, RuntimeError) as error:
        raise BudgetError(f"cannot reserve model task budget: {error}") from error


def complete(attempt, success, seconds, *, result_status=None, outcome_known=True,
             tokens=None, monetary_cost=None, budget_file=None, max_model_calls=None):
    if attempt is None:
        return
    with _ledger(budget_file, max_model_calls) as (path, data):
        if data is None:
            raise BudgetError("model task reservation ledger disappeared")
        for entry in data["attempts"]:
            if entry["id"] == attempt:
                entry.update(status="completed", success=bool(success), seconds=seconds,
                             finished_at=time.time(), result_status=result_status,
                             outcome_known=bool(outcome_known), tokens=tokens,
                             monetary_cost=monetary_cost)
                _save(path, data)
                return
        raise BudgetError("model task reservation disappeared")


def reserve_capacity(count, owner, purpose="review", ttl_seconds=600):
    """Hold future calls without recording an attempt that has not started."""
    if (type(count) is not int or count <= 0 or not isinstance(owner, str) or not owner
            or not isinstance(purpose, str) or not purpose
            or isinstance(ttl_seconds, bool) or not isinstance(ttl_seconds, (int, float))
            or not math.isfinite(ttl_seconds) or not 0 < ttl_seconds <= 86400):
        raise BudgetError("invalid model call capacity reservation")
    try:
        with _ledger(lock_deadline=_admission_deadline()) as (path, data):
            if data is None:
                return None
            holds = _active_holds(data)
            available = _maximum(data) - len(data["attempts"]) - sum(h["remaining"] for h in holds.values())
            if count > available:
                raise BudgetError(f"insufficient model task capacity ({available} available; {count} required)")
            lease = uuid.uuid4().hex
            data.setdefault("holds", {})[lease] = dict(owner=owner, purpose=purpose,
                remaining=count, expires_at=time.time() + ttl_seconds)
            _save(path, data)
            return lease
    except BudgetError:
        raise
    except (OSError, ValueError, KeyError, TypeError, RuntimeError) as error:
        raise BudgetError(f"cannot reserve model task capacity: {error}") from error


def release_capacity(lease_id):
    if lease_id is None:
        return
    with _ledger() as (path, data):
        if data is not None and lease_id in data.get("holds", {}):
            del data["holds"][lease_id]
            _save(path, data)


def remaining_capacity():
    with _ledger() as (_, data):
        if data is None:
            return None
        return max(0, _maximum(data) - len(data["attempts"]) - sum(
            h["remaining"] for h in _active_holds(data).values()))
