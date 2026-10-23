"""Process-safe admission ledger for bounded model task dispatches.

Reservations count attempted Makewand dispatches, including failures. Vendor
CLI internal turns and token usage remain separately measured or unknown.
"""
import copy
import json
import math
import errno
import os
import re
import time
import uuid
from pathlib import Path
from contextlib import contextmanager
from makewand import accounting_files, filelock, layered_budget, account_leases, ledger_compaction


class BudgetError(RuntimeError):
    pass


class BudgetDeadlineError(BudgetError):
    pass


class ConcurrencyBusyError(BudgetError):
    """A known, uncharged capacity refusal; catch before BudgetError."""


class BudgetCancelledError(BudgetError):
    """Admission was cancelled before a provider started."""


class BudgetConfigError(BudgetError):
    """Invalid admission configuration, not exhausted call capacity."""


class LeaseWaitConfigError(BudgetConfigError):
    """An invalid MAKEWAND_LEASE_WAIT_SECONDS."""


_LEASE_WAIT_DECIMAL = re.compile(r"[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?")


def lease_wait_seconds(deadline_monotonic=None):
    value = os.environ.get("MAKEWAND_LEASE_WAIT_SECONDS")
    if not value:
        return 30
    try:
        value = value.strip(" \t\r\n\v\f")
        if not _LEASE_WAIT_DECIMAL.fullmatch(value):
            raise ValueError()
        seconds = float(value)
        if not math.isfinite(seconds) or seconds < 0 or seconds * 1e9 >= 2**63:
            raise ValueError()
        return seconds
    except ValueError as error:
        raise LeaseWaitConfigError("MAKEWAND_LEASE_WAIT_SECONDS must be a finite nonnegative decimal fitting int64 nanoseconds") from error


MAX_LEDGER_BYTES = 16 * 1024 * 1024


def _reject_nonfinite_json(_):
    raise BudgetError("model call budget contains a nonfinite JSON number")


def _admission_deadline():
    from makewand.execution_runtime import current_context
    return current_context().get("_deadline_monotonic")


@contextmanager
def _ledger(budget_file=None, max_model_calls=None, lock_deadline=None, migrate=True, admission=False):
    """Lock, read and validate the ledger. Without migrate, opening it writes nothing:
    a lifetime ledger is not converted and a tighter maximum is not persisted."""
    value = budget_file or os.environ.get("MAKEWAND_CALL_BUDGET_FILE")
    if not value and layered_budget.enabled():
        value = layered_budget.default_path()
    if not value:
        if max_model_calls is not None or os.environ.get("MAKEWAND_MAX_MODEL_CALLS"):
            raise BudgetError("model call maximum requires a shared ledger path")
        yield None, None
        return
    if lock_deadline is not None and time.monotonic() >= lock_deadline:
        # As Go's transaction gate: an already expired transaction does no I/O.
        raise BudgetDeadlineError("execution deadline expired while acquiring the model call budget")
    with accounting_files.pin(value) as path:
        descriptor = path.open(path.lock_name, create=True, writable=True)
        path.lock_fd = descriptor
        acquired = False
        # Accounting/cleanup uses its own bounded deadline after cancellation.
        lock_deadline = time.monotonic() + 5 if lock_deadline is None else lock_deadline
        try:
            while True:
                if admission:
                    _check_admission_cancelled()
                try:
                    filelock.flock(descriptor, filelock.LOCK_EX | filelock.LOCK_NB)
                    acquired = True
                    break
                except OSError as error:
                    if error.errno not in (errno.EACCES, errno.EAGAIN, errno.EWOULDBLOCK) and getattr(error, "winerror", None) not in (33, 158):
                        raise
                # The deadline bounds waiting for another holder, not this
                # call's own I/O: a free lock is taken (as in Go's lockSidecar).
                if time.monotonic() >= lock_deadline:
                    raise BudgetDeadlineError("execution deadline expired while acquiring the model call budget")
                time.sleep(min(.01, max(0, lock_deadline - time.monotonic())))
            path.assert_named(path.lock_name, descriptor)
            try:
                ledger_fd = path.open(path.name)
            except FileNotFoundError:
                limit = int(max_model_calls if max_model_calls is not None else os.environ.get("MAKEWAND_MAX_MODEL_CALLS", "1000" if layered_budget.enabled() else "0"))
                if limit <= 0 and not layered_budget.enabled():
                    raise BudgetError("model call budget requires a positive maximum")
                data = {"schema": 1, "maximum": limit, "attempts": []}
                if layered_budget.enabled():
                    layered_budget.initialize(data)
            else:
                with os.fdopen(ledger_fd, "rb") as stream:
                    raw = stream.read(MAX_LEDGER_BYTES + 1)
                    if len(raw) > MAX_LEDGER_BYTES:
                        raise BudgetError("model call budget ledger exceeds 16 MiB")
                data = json.loads(raw.decode("utf-8"), parse_constant=_reject_nonfinite_json)
            _validate(data)
            ledger_compaction.compact(data)
            if migrate and layered_budget.enabled() and "scopes" not in data:
                legacy = data["maximum"]
                layered_budget.initialize(data)
                data["legacy_maximum"] = legacy
                data["legacy_used"] = _charged(data)
                _save(path, data)
            maximum = data["maximum"] if "scopes" in data else _maximum(data, max_model_calls)
            if migrate and maximum < data["maximum"]:
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
    if type(data.get("archived_used", 0)) is not int or not 0 <= data.get("archived_used", 0) <= 9223372036854775807:
        raise BudgetError("invalid archived call count")
    account_leases.validate(data)
    identifiers = set()
    for entry in data["attempts"]:
        if (not isinstance(entry, dict) or not isinstance(entry.get("id"), str)
                or not entry["id"] or entry["id"] in identifiers):
            raise BudgetError("invalid model call budget attempts")
        identifiers.add(entry["id"])
        finished = entry.get("finished_at")
        if finished is not None and (type(finished) not in (int, float) or not math.isfinite(finished) or finished < 0):
            raise BudgetError("invalid attempt finish timestamp")
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
        created = hold.get("created_at", 0)  # absent in holds written before they were dated
        if (isinstance(created, bool) or not isinstance(created, (int, float))
                or (isinstance(created, int) and created > 9223372036854775807)
                or not math.isfinite(created) or created < 0):
            raise BudgetError("invalid model call capacity hold creation time")

    if "scopes" in data:
        layered_budget.validate(data)


def _charged(data):
    return data.get("archived_used", 0) + sum(e.get("status") != "released" for e in data["attempts"])


def _maximum(data, max_model_calls=None):
    configured = max_model_calls if max_model_calls is not None else os.environ.get("MAKEWAND_MAX_MODEL_CALLS", data["maximum"])
    if isinstance(configured, bool):
        raise BudgetError("model call maximum must be a positive integer")
    configured = int(configured)
    if configured <= 0:
        raise BudgetError("model call maximum must be a positive integer")
    return min(data["maximum"], configured)


def _active_holds(data):
    now = layered_budget.effective_clock(data)
    holds = data.get("holds", {})
    expired = [lease for lease, hold in holds.items() if hold["expires_at"] <= now]
    for lease in expired:
        del holds[lease]
    return holds


def _save(path, data):
    ledger_compaction.compact(data)
    encoded = (json.dumps(data, ensure_ascii=False, allow_nan=False, indent=2) + "\n").encode("utf-8")
    if len(encoded) > MAX_LEDGER_BYTES:
        raise BudgetError("model call budget ledger exceeds 16 MiB")
    path.save(encoded, path.lock_fd)


def reserve(engine, tier, model=None, readonly=False, *, lease_id=None, task_id=None,
            stage="provider", account_ref=None, api_policy="subscription_only",
            budget_file=None, max_model_calls=None, deadline_monotonic=None, pending=False,
            wait_for_capacity=True):
    deadline_monotonic = _admission_deadline() if deadline_monotonic is None else deadline_monotonic
    wait_seconds = lease_wait_seconds(deadline_monotonic)
    now = time.monotonic()
    wait_until = now + wait_seconds if wait_for_capacity else now
    if deadline_monotonic is not None:
        remaining = max(0, deadline_monotonic - now)
        # Leave one second for dispatch/cleanup, or half of a short request.
        wait_until = min(wait_until, deadline_monotonic - min(1, remaining / 2))
    from makewand.providers.base import get_current_contestant_scope
    scope = get_current_contestant_scope()
    delay = .05
    busy_error = None
    while True:
        _check_admission_cancelled()
        try:
            return _reserve_once(engine, tier, model, readonly, lease_id=lease_id, task_id=task_id,
                stage=stage, account_ref=account_ref, api_policy=api_policy,
                budget_file=budget_file, max_model_calls=max_model_calls,
                deadline_monotonic=deadline_monotonic, pending=pending)
        except BudgetDeadlineError as error:
            if busy_error is not None:
                raise busy_error from error
            raise
        except ConcurrencyBusyError as error:
            busy_error = error
            remaining = wait_until - time.monotonic()
            if remaining <= 0:
                raise
            # _reserve_once has exited _ledger: never sleep holding the lock.
            if scope is None:
                time.sleep(min(delay, remaining))
            else:
                scope.cancel_event.wait(min(delay, remaining))
            _check_admission_cancelled()
            delay = min(delay * 2, 1)
            if time.monotonic() >= wait_until:
                raise


def _check_admission_cancelled():
    from makewand.providers.base import get_current_contestant_scope
    scope = get_current_contestant_scope()
    if scope is not None and scope.cancel_event.is_set():
        raise BudgetCancelledError("cancelled while waiting for account capacity")


def _reserve_once(engine, tier, model=None, readonly=False, *, lease_id=None, task_id=None,
            stage="provider", account_ref=None, api_policy="subscription_only",
            budget_file=None, max_model_calls=None, deadline_monotonic=None, pending=False):
    try:
        deadline_monotonic = _admission_deadline() if deadline_monotonic is None else deadline_monotonic
        # Admission without an execution deadline waits for the ledger like Go's
        # Reserve. The fixed five-second lock budget bounds settlement; applied
        # here it reported an expired deadline that no caller had set.
        lock_deadline = math.inf if deadline_monotonic is None else deadline_monotonic
        with _ledger(budget_file, max_model_calls, lock_deadline, admission=True) as (path, data):
            _check_admission_cancelled()
            if data is None:
                return None
            maximum = data["maximum"] if "scopes" in data else _maximum(data, max_model_calls)
            holds = _active_holds(data)
            reserved = sum(hold["remaining"] for hold in holds.values())
            if lease_id is not None:
                hold = holds.get(lease_id)
                if hold is None or hold["remaining"] <= 0:
                    raise BudgetError("model call capacity hold expired or exhausted")
                reserved -= 1
            keys = None
            if "scopes" in data:
                keys = layered_budget.scopes(data, task_id, max_model_calls)
                _save(path, data)  # persist tightened scope limits even on refusal
                if lease_id and hold.get("scopes", [])[0:1] != keys[0:1]:
                    raise BudgetError("capacity hold belongs to another task")
                if layered_budget.available(data, keys, holds, hold if lease_id else None) <= 0:
                    raise BudgetError("model task budget exhausted (layered scope)")
            elif _charged(data) + reserved >= maximum:
                raise BudgetError(f"model task budget exhausted ({len(data['attempts'])}/{maximum})")
            account_lease = None
            if "scopes" in data:
                account_lease = account_leases.admission(data, engine, account_ref)
            if lease_id is not None:
                hold["remaining"] -= 1
            if keys:
                layered_budget.spend(data, keys)
            attempt = uuid.uuid4().hex
            if account_lease is not None:
                data["leases"][attempt] = account_lease
            data["attempts"].append({"id": attempt, "engine": engine, "tier": tier,
                                     "requested_model": model, "readonly": readonly,
                                     "benchmark_run": os.environ.get("MAKEWAND_BENCHMARK_RUN_ID"),
                                     "started_at": time.time(), "status": "reserved" if pending else "started",
                                     "scopes": keys,
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
             tokens=None, monetary_cost=None, budget_file=None, max_model_calls=None,
             evidence_failed=False):
    """Settle the attempt. Returns whether its account lease is released (None without one)."""
    if attempt is None:
        return None
    with _ledger(budget_file, max_model_calls) as (path, data):
        if data is None:
            raise BudgetError("model task reservation ledger disappeared")
        for entry in data["attempts"]:
            if entry["id"] == attempt:
                if entry["status"] not in ("completed", "released"):
                    entry.update(status="completed", success=bool(success), seconds=seconds,
                                 finished_at=time.time(), result_status=result_status,
                                 outcome_known=bool(outcome_known), tokens=tokens,
                                 monetary_cost=monetary_cost)
                if evidence_failed:
                    # Final evidence can fail after accounting released the
                    # lease. Downgrade the outcome without charging/refunding
                    # again or changing the original settlement timestamp.
                    entry.update(success=False, result_status="UNKNOWN", outcome_known=False)
                account_leases.settle(data, attempt)
                _save(path, data)
                return attempt not in data.get("leases", {})
        raise BudgetError("model task reservation disappeared")


def reserve_capacity(count, owner, purpose="review", ttl_seconds=600):
    """Hold future calls without recording an attempt that has not started."""
    if (type(count) is not int or count <= 0 or not isinstance(owner, str) or not owner
            or not isinstance(purpose, str) or not purpose
            or isinstance(ttl_seconds, bool) or not isinstance(ttl_seconds, (int, float))
            or not math.isfinite(ttl_seconds) or not 0 < ttl_seconds <= layered_budget.MAX_HOLD_SECONDS):
        raise BudgetError("invalid model call capacity reservation")
    try:
        with _ledger(lock_deadline=_admission_deadline()) as (path, data):
            if data is None:
                return None
            holds = _active_holds(data)
            available = _maximum(data) - _charged(data) - sum(h["remaining"] for h in holds.values())
            keys = None
            if "scopes" in data:
                from makewand.execution_runtime import task_id
                keys = layered_budget.scopes(data, task_id(), None)
                _save(path, data)
                available = layered_budget.available(data, keys, holds)
            if count > available:
                raise BudgetError(f"insufficient model task capacity ({available} available; {count} required)")
            lease = uuid.uuid4().hex
            # Dated from the clock that compaction expires holds by: from wall time, a
            # ledger clock within its permitted lead cut a hold short, even at this save.
            created = layered_budget.effective_clock(data)
            data.setdefault("holds", {})[lease] = dict(owner=owner, purpose=purpose, remaining=count,
                created_at=created, expires_at=created + ttl_seconds, scopes=keys)
            _save(path, data)
            return lease
    except BudgetError:
        raise
    except (OSError, ValueError, KeyError, TypeError, RuntimeError) as error:
        raise BudgetError(f"cannot reserve model task capacity: {error}") from error


def repair_clock(budget_file=None, apply=False):
    """Report a budget clock or day bucket that leads real time; apply=True brings it back.

    Runs under the shared ledger lock (layered_budget.repair_clock). Nothing is
    written without apply. Returns the report, with the ledger path and zone.
    """
    value = budget_file or os.environ.get("MAKEWAND_CALL_BUDGET_FILE") or layered_budget.default_path()
    report = {"path": str(value), "exists": os.path.exists(value), "layered": False, "applied": False,
              "timezone": os.environ.get(layered_budget.TIMEZONE_ENV, "").strip() or "local"}
    if not report["exists"]:
        return report
    try:
        with _ledger(value, migrate=False) as (path, data):
            if "scopes" not in data:
                return report  # a lifetime ledger has no daily clock
            report["layered"] = True
            report.update(layered_budget.repair_clock(data if apply else copy.deepcopy(data)))
            if apply and report["changed"]:
                _save(path, data)
                report["applied"] = True
    except BudgetError:
        raise
    except (OSError, ValueError, KeyError, TypeError, RuntimeError) as error:
        raise BudgetError(f"cannot read the model call budget ledger: {error}") from error
    return report


def release_capacity(lease_id):
    if lease_id is None:
        return
    with _ledger() as (path, data):
        if data is not None and lease_id in data.get("holds", {}):
            del data["holds"][lease_id]
            _save(path, data)


def remaining_capacity():
    with _ledger() as (path, data):
        if data is None:
            return None
        if "scopes" in data:
            from makewand.execution_runtime import task_id
            keys = layered_budget.scopes(data, task_id(), None)
            _save(path, data)
            return max(0, layered_budget.available(data, keys, _active_holds(data)))
        return max(0, _maximum(data) - _charged(data) - sum(
            h["remaining"] for h in _active_holds(data).values()))


def start(attempt, *, budget_file=None, max_model_calls=None):
    """Commit the launch boundary. A crash after this point never refunds."""
    _transition(attempt, False, budget_file, max_model_calls)


def release_unstarted(attempt, *, budget_file=None, max_model_calls=None):
    _transition(attempt, True, budget_file, max_model_calls)


def reject_unlaunched(attempt, *, budget_file=None, max_model_calls=None):
    """Trusted runtime account refusal before any transport invocation."""
    _transition(attempt, True, budget_file, max_model_calls, account_refusal=True)


def _transition(attempt, release, budget_file, maximum, account_refusal=False):
    if attempt is None:
        return
    with _ledger(budget_file, maximum) as (path, data):
        for entry in data["attempts"]:
            if entry["id"] != attempt:
                continue
            lease = data.get("leases", {}).get(attempt)
            if account_refusal and lease and (lease["launch_pending"] or lease["workers"]):
                raise BudgetError("cannot refund an invoked transport")
            if entry["status"] != "reserved" and not (account_refusal and entry["status"] == "started"):
                if not release and entry["status"] != "started":
                    raise BudgetError("cannot start a released or settled reservation")
                return
            if release:
                if entry.get("scopes"):
                    layered_budget.spend(data, entry["scopes"], -1)
                entry["status"] = "released"
                entry["finished_at"] = time.time()
                data.get("leases", {}).pop(attempt, None)
            else:
                entry["status"] = "started"
            _save(path, data)
            return
        raise BudgetError("model task reservation disappeared")


def lease_update(attempt, action="heartbeat", pid=None, *, budget_file=None, max_model_calls=None):
    if attempt is None:
        return
    with _ledger(budget_file, max_model_calls) as (path, data):
        lease = data.get("leases", {}).get(attempt)
        if lease is None:
            return
        now = layered_budget.effective_clock(data)
        lease.update(heartbeat_at=now, expires_at=now + account_leases.TTL)
        if action == "launch":
            lease["launch_pending"] = True
        elif action in ("worker", "job_worker"):
            ref = account_leases.process_ref(pid)
            if action == "job_worker":
                ref["job"] = "owner-kill-on-close"
            lease["workers"].append(ref)
            # If the launched process exited before we observed its identity,
            # no lifecycle proof is available; keep the uncertainty fence.
            lease["launch_pending"] = ref["token"] is None
        elif action == "launch_failed":
            lease["launch_pending"] = False
        elif action == "stopped":
            # Called only after provider runner tree cleanup and wait.
            if not Path("/proc").is_dir() or not any(account_leases.alive(w, True) for w in lease["workers"]):
                lease["workers"] = []
                lease["launch_pending"] = False
        elif action != "heartbeat":
            raise BudgetError("invalid lease update")
        if action == "stopped" and any(e["id"] == attempt and e["status"] == "completed" for e in data["attempts"]):
            account_leases.settle(data, attempt)
        _save(path, data)


def finish_transport(attempt, *, budget_file=None, max_model_calls=None):
    if attempt is None:
        return
    with _ledger(budget_file, max_model_calls) as (path, data):
        lease = data.get("leases", {}).get(attempt)
        if lease and (lease["launch_pending"] or any(account_leases.alive(w, True) for w in lease["workers"])):
            raise BudgetError("previous provider transport has not stopped")
        for entry in data["attempts"]:
            if entry["id"] == attempt:
                if entry["status"] not in ("completed", "released"):
                    entry.update(status="completed", success=False, seconds=0,
                                 finished_at=time.time(), result_status="FAILED", outcome_known=True)
                account_leases.settle(data, attempt)
                _save(path, data)
                return
        raise BudgetError("model task reservation disappeared")


def api_lease(attempt, engine, *, budget_file=None, max_model_calls=None):
    """Atomically move this attempt to the API capacity before transport."""
    if attempt is None:
        return
    with _ledger(budget_file, max_model_calls) as (path, data):
        leases = data.get("leases", {})
        old = leases.get(attempt)
        if old is None:
            return
        if old["launch_pending"] or old["workers"]:
            raise BudgetError("cannot change route while a CLI worker exists")
        del leases[attempt]
        new = account_leases.admission(data, engine + ":api", None)
        leases[attempt] = new
        _save(path, data)
