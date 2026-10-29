"""Optional host capacity policy, shared by every budget ledger and both CLIs.

Only admission takes this lock, before the call-budget lock. Calls remain
charged in their original ledger; this file contains leases and queue tickets,
never a second call counter. Provider sandboxes receive neither file.
"""
import copy
import contextvars
import functools
import inspect
import errno
import hashlib
import json
import math
import os
import sys
import time
import threading
import uuid
from contextlib import contextmanager
from pathlib import Path

from makewand import accounting_files, account_leases, filelock, layered_budget, ledger_registry

POLICY_NAME = "capacity-policy.json"
STATE_NAME = "shared-capacity.json"
MAX_BYTES = 16 * 1024 * 1024
_tool = contextvars.ContextVar("makewand_capacity_tool", default=None)


def _read(value):
    os.lstat(Path(value).expanduser())  # reads never recreate a missing source directory
    with accounting_files.pin(value) as path:
        return _read_pinned(path)


def _read_pinned(path):
    with os.fdopen(path.open(path.name), "rb") as stream:
        raw = stream.read(MAX_BYTES + 1)
    if len(raw) > MAX_BYTES:
        raise ValueError("capacity file exceeds 16 MiB")
    return json.loads(raw, parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite capacity number")))


def policy():
    from makewand.call_budget import BudgetConfigError
    try:
        value = layered_budget.default_path()
    except (OSError, RuntimeError) as error:
        # An unavailable implicit home is not an enabled policy. Keep the
        # legacy private-ledger path; explicit configuration stays closed.
        if not os.environ.get("MAKEWAND_CONFIG_DIR") and not os.environ.get("XDG_CONFIG_HOME"):
            return None
        raise BudgetConfigError(f"cannot locate host capacity policy: {error}") from error
    try:
        result = _read(Path(value).with_name(POLICY_NAME))
    except FileNotFoundError:
        return None
    except (OSError, ValueError, RuntimeError) as error:
        raise BudgetConfigError(f"cannot read host capacity policy: {error}") from error
    try:
        if result["schema"] != 1 or type(result["schema"]) is not int:
            raise ValueError("unsupported schema")
        for name in ("global_max", "project_max", "tool_max", "wait_seconds"):
            if type(result[name]) is not int or not 0 < result[name] <= 86400:
                raise ValueError(f"invalid {name}")
        if not isinstance(result["providers"], dict):
            raise ValueError("invalid providers")
        for name, maximum in result["providers"].items():
            if (not isinstance(name, str) or not name or type(maximum) is not int
                    or not 0 < maximum <= 86400 or account_leases.lease_provider(name) != name):
                raise ValueError("invalid provider limit")
    except (KeyError, TypeError, ValueError) as error:
        raise BudgetConfigError(f"invalid host capacity policy: {error}") from error
    return result


def project(working_directory=None):
    """Group ordinary Git worktrees without invoking repository code."""
    root = Path(os.environ.get("MAKEWAND_PROJECT_ROOT") or working_directory or os.getcwd()).expanduser().resolve()
    if not os.environ.get("MAKEWAND_PROJECT_ROOT"):
        for parent in (root, *root.parents):
            marker = parent / ".git"
            try:
                if marker.is_dir():
                    root = parent
                    break
                if marker.is_file():
                    raw = ledger_registry._read_bounded(marker).strip()
                    if raw.startswith("gitdir: "):
                        directory = (parent / raw[8:]).resolve()
                        common = ledger_registry._read_bounded(directory / "commondir").strip()
                        root = (directory / common).resolve().parent
                        break
            except (OSError, ValueError):
                continue
    value = str(root)
    return hashlib.sha256(value.encode()).hexdigest()[:32], root.name or value


@contextmanager
def locked(deadline=None, admission=True, *, state_path=None):
    from makewand.call_budget import BudgetDeadlineError, _check_admission_cancelled
    with accounting_files.pin(state_path or Path(layered_budget.default_path()).with_name(STATE_NAME)) as path:
        descriptor = path.open(path.lock_name, create=True, writable=True)
        path.lock_fd = descriptor
        acquired = False
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
                if deadline is not None and time.monotonic() >= deadline:
                    raise BudgetDeadlineError("execution deadline expired acquiring shared capacity")
                time.sleep(.01)
            path.assert_named(path.lock_name, descriptor)
            try:
                state = _read_pinned(path)
            except FileNotFoundError:
                state = {"schema": 1, "active": {}, "queue": []}
            validate(state)
            yield path, state
        finally:
            if acquired:
                filelock.flock(descriptor, filelock.LOCK_UN)
            os.close(descriptor)


def save(path, state):
    encoded = (json.dumps(state, ensure_ascii=False, allow_nan=False, indent=2) + "\n").encode()
    if len(encoded) > MAX_BYTES:
        raise ValueError("shared capacity exceeds 16 MiB")
    path.save(encoded, path.lock_fd)


def validate(state):
    if (not isinstance(state, dict) or type(state.get("schema")) is not int or state.get("schema") != 1
            or not isinstance(state.get("active"), dict) or not isinstance(state.get("queue"), list)):
        raise ValueError("invalid shared capacity state")
    if (type(state.get("turn", 0)) is not int or state.get("turn", 0) < 0
            or not isinstance(state.get("project_turns", {}), dict)
            or any(not isinstance(k, str) or type(v) is not int or v < 0
                   for k, v in state.get("project_turns", {}).items())):
        raise ValueError("invalid project queue turns")
    for row in [*state["active"].values(), *state["queue"]]:
        if not isinstance(row, dict) or not all(isinstance(row.get(k), str) for k in ("id", "project", "label")):
            raise ValueError("invalid shared capacity entry")
        account_leases.validate({"leases": {row["id"]: row.get("lease")}})
        if type(row.get("started_at")) not in (int, float) or not math.isfinite(row["started_at"]):
            raise ValueError("invalid capacity timestamp")
        if row.get("kind") not in ("model", "tool"):
            raise ValueError("invalid capacity kind")
        if not isinstance(row.get("ledger"), str) or not isinstance(row.get("attempt"), str):
            raise ValueError("invalid capacity source")
        if not isinstance(row.get("limits"), list) or len(row["limits"]) != 3 or any(type(v) is not int or v <= 0 for v in row["limits"]):
            raise ValueError("invalid capacity limits")


def referenced_attempts():
    """Lock-free atomic snapshot for compaction, which holds a budget lock.

    None means that history cannot safely be removed. Do not take the shared
    lock here: admissions already hold it before taking that budget lock.
    Read even with the policy disabled so a later re-enable can recover.
    """
    try:
        state = _read(Path(layered_budget.default_path()).with_name(STATE_NAME))
        validate(state)
    except FileNotFoundError:
        return set()
    except (OSError, RuntimeError, ValueError, KeyError, TypeError):
        return None
    return {row["attempt"] for row in state["active"].values() if row["kind"] == "model"}


def _live(lease, completed=False, now=None):
    # Keep precisely the existing account-lease recovery rule: time alone
    # never releases an owner, a child process, or an unobservable process.
    stopped = completed and not lease["launch_pending"]
    return not ((not account_leases.alive(lease["owner"]) or stopped)
                and not account_leases.workers_alive(lease)
                and lease["expires_at"] <= (time.time() if now is None else now))


def _source_rows(value):
    from makewand.call_budget import _validate
    data = _read(value)
    _validate(data)  # do not compact: completed rows prove safe settlement
    now = layered_budget.effective_clock(data)
    attempts = {row["id"]: row for row in data["attempts"]}
    rows = {}
    for identifier, lease in data.get("leases", {}).items():
        entry = attempts.get(identifier, {})
        if not _live(lease, entry.get("status") == "completed", now):
            attempts.setdefault(identifier, {})["_capacity_reaped"] = True
            continue
        project_id = entry.get("capacity_project")
        if not project_id:
            project_id = next((scope[1] for scope in entry.get("scopes") or [] if scope[0] == "projects"), "legacy:" + str(Path(value).parent))
        started = entry.get("started_at")
        if type(started) not in (int, float) or not math.isfinite(started) or started < 0:
            started = 0  # unknown legacy age; still count its live lease
        rows[identifier] = {"id": hashlib.sha256((value + "\0" + identifier).encode()).hexdigest(),
                            "kind": "model", "ledger": value, "attempt": identifier,
                            "project": project_id, "label": entry.get("capacity_label") or "existing task",
                            "task_id": entry.get("task_id"), "started_at": started,
                            "lease": copy.deepcopy(lease),
                            "limits": [data.get("lease_limits", {}).get(k, 2**63 - 1) for k in lease["keys"]]}
    return rows, attempts, now


def refresh(state, selected):
    paths = {layered_budget.default_path(), selected, *ledger_registry.known_ledgers(),
             *(row["ledger"] for row in state["active"].values() if row["kind"] == "model")}
    for value in sorted({str(accounting_files.canonical_path(path)) for path in paths}):
        previous = {row["attempt"]: row for row in state["active"].values() if row["kind"] == "model" and row["ledger"] == value}
        try:
            rows, attempts, now = _source_rows(value)
        except (OSError, ValueError, RuntimeError):
            # Never turn an unreadable active source into a free slot.
            continue
        for identifier, old in previous.items():
            if identifier in rows:
                continue
            entry = attempts.get(identifier, {})
            settled = entry.get("status") in ("completed", "released")
            # Old binaries removed dead leases without recording reaping.
            # Require the original attempt AND death/expiry of the cached
            # lifecycle; missing/reset/unreadable sources alone prove nothing.
            legacy_reaped = identifier in attempts and not _live(old["lease"], now=now)
            if settled or entry.get("lease_reaped") is True or entry.get("_capacity_reaped") or legacy_reaped:
                state["active"].pop(old["id"], None)
        for row in rows.values():
            state["active"][row["id"]] = row
    for identifier, row in list(state["active"].items()):
        if row["kind"] == "tool" and not _live(row["lease"], row.get("settled", False)):
            del state["active"][identifier]
    # Queue tickets are not launched calls. Their short heartbeat can safely
    # expire even when the waiting CLI is alive or its PID is unobservable.
    state["queue"] = [row for row in state["queue"] if row["lease"]["expires_at"] > time.time()
                      and account_leases.alive(row["lease"]["owner"])]


def limits(engine, current_policy):
    provider = account_leases.lease_provider(engine).removesuffix(":api")
    maximum = current_policy["providers"].get(provider, 1)
    result = [maximum] * 3
    for i, name in enumerate(("MAKEWAND_PROVIDER_CONCURRENCY", "MAKEWAND_ACCOUNT_CONCURRENCY", "MAKEWAND_POOL_CONCURRENCY")):
        if os.environ.get(name):
            result[i] = min(result[i], layered_budget.positive(os.environ[name]))
    return result


def fits(ticket, active, current_policy):
    if ticket["kind"] == "tool":
        if sum(row["kind"] == "tool" for row in active) >= current_policy["tool_max"]:
            return False
        return tool_memory_available()
    active = [row for row in active if row["kind"] == "model"]
    if len(active) >= current_policy["global_max"]:
        return False
    if sum(row["project"] == ticket["project"] for row in active) >= current_policy["project_max"]:
        return False
    if ticket.get("task_id") and any(row.get("task_id") == ticket["task_id"] for row in active):
        return False
    wanted = ticket["lease"]["keys"]
    provider = wanted[0][:-len(":provider")]
    for index, key in enumerate(wanted):
        conflicting = [row for row in active if
                       (index == 0 and provider.removesuffix(":api") == row["lease"]["keys"][0][:-9].removesuffix(":api")) or
                       key in row["lease"]["keys"] or
                       (index > 0 and wanted[0] == row["lease"]["keys"][0] and
                        (wanted[1] == provider + ":unknown" or row["lease"]["keys"][1] == provider + ":unknown"))]
        maximum = min([ticket["limits"][index], *(row["limits"][index] for row in conflicting)])
        if len(conflicting) >= maximum:
            return False
    return True


def diagnostic(state, ticket, current_policy):
    def age(row):
        return f"{max(0, int(time.time() - row['started_at']))}s" if row["started_at"] > 0 else "age unknown"
    running = ", ".join(f"{json.dumps(row['label'], ensure_ascii=False)} ({row['lease']['keys'][0][:-9]}, {age(row)})"
                        for row in state["active"].values()) or "none"
    models = sum(row["kind"] == "model" for row in state["active"].values())
    tools = len(state["active"]) - models
    return (f"shared capacity busy: models={models}/{current_policy['global_max']}; tools={tools}/{current_policy['tool_max']}; "
            f"project={json.dumps(ticket['label'], ensure_ascii=False)}; queue={len(state['queue'])}; occupied: {running}")


class Gate:
    def __init__(self, path, state, ticket):
        self.path, self.state, self.ticket = path, state, ticket

    def commit(self, value, attempt):
        if attempt is None:
            raise ValueError("host capacity requires a model call ledger")
        rows, _, _ = _source_rows(str(value))
        row = rows.get(attempt)
        if row is None:
            raise ValueError("admitted model call has no capacity lease")
        self.state["active"][row["id"]] = row
        self.state["turn"] = self.state.get("turn", 0) + 1
        self.state.setdefault("project_turns", {})[row["project"]] = self.state["turn"]
        self.state["queue"] = [r for r in self.state["queue"] if r["id"] != self.ticket["id"]]
        save(self.path, self.state)


@contextmanager
def admission(current_policy, engine, account, value, task_id, ticket_id, deadline=None, working_directory=None):
    from makewand.call_budget import BudgetError, ConcurrencyBusyError, BudgetConfigError
    if current_policy is None:
        yield None
        return
    value = value or os.environ.get("MAKEWAND_CALL_BUDGET_FILE") or layered_budget.default_path()
    value = str(accounting_files.canonical_path(value))
    project_id, label = project(working_directory)
    now = time.time()
    ticket = {"id": ticket_id, "kind": "model", "ledger": value, "attempt": "", "project": project_id,
              "label": label, "task_id": task_id, "started_at": now, "limits": limits(engine, current_policy),
              "lease": {"keys": account_leases.keys(engine, account), "owner": account_leases.process_ref(os.getpid()),
                        "workers": [], "launch_pending": False, "heartbeat_at": now, "expires_at": now + 30}}
    try:
        with locked(deadline) as (path, state):
            refresh(state, value)
            old = next((row for row in state["queue"] if row["id"] == ticket_id), None)
            if old:
                old["lease"]["expires_at"] = now + 30
                ticket = old
            else:
                state["queue"].append(ticket)
            active = list(state["active"].values())
            candidates = [row for row in state["queue"] if row["kind"] == "model" and fits(row, active, current_policy)]
            eligible = min(candidates, key=lambda row: state.get("project_turns", {}).get(row["project"], 0), default=None)
            if eligible is None or eligible["id"] != ticket_id:
                save(path, state)
                raise ConcurrencyBusyError(diagnostic(state, ticket, current_policy))
            yield Gate(path, state, ticket)
    except BudgetError:
        raise
    except (OSError, ValueError, RuntimeError, KeyError, TypeError) as error:
        # A provider callback is never reached if the durable capacity write
        # failed, including a failure after the call-budget write committed.
        raise BudgetConfigError(f"cannot coordinate shared capacity: {error}") from error


def cancel(ticket_id, current_policy):
    if current_policy is None:
        return
    try:
        with locked(time.monotonic() + 1, admission=False) as (path, state):
            before = len(state["queue"])
            state["queue"] = [row for row in state["queue"] if row["id"] != ticket_id]
            if len(state["queue"]) != before:
                save(path, state)
    except Exception:  # queue heartbeat expires; never mask an original outcome
        pass


def report_busy(error):
    print(f"makewand: waiting: {error}", file=sys.stderr)


def tool_memory_available():
    if sys.platform != "linux":
        return True
    try:
        available = next(int(line.split()[1]) * 1024 for line in
                         Path("/proc/meminfo").read_text().splitlines() if line.startswith("MemAvailable:"))
        return available >= 32 * 1024**3
    except (OSError, ValueError, StopIteration):
        return False


@contextmanager
def relocation(current_policy, engine, value, attempt):
    """Recheck an already charged CLI slot before switching to an API pool."""
    from makewand.call_budget import BudgetError, ConcurrencyBusyError
    if current_policy is None:
        yield None
        return
    value = str(accounting_files.canonical_path(value or os.environ.get("MAKEWAND_CALL_BUDGET_FILE") or layered_budget.default_path()))
    with locked(time.monotonic() + 5) as (path, state):
        refresh(state, value)
        old = next((row for row in state["active"].values() if row["kind"] == "model"
                    and row["ledger"] == value and row["attempt"] == attempt), None)
        if old is None:
            raise BudgetError("model slot disappeared before API relocation")
        ticket = copy.deepcopy(old)
        ticket["lease"]["keys"] = account_leases.keys(engine + ":api", None)
        ticket["limits"] = limits(engine, current_policy)
        if not fits(ticket, [row for row in state["active"].values() if row["id"] != old["id"]], current_policy):
            raise ConcurrencyBusyError(diagnostic(state, ticket, current_policy))
        yield Gate(path, state, ticket)


def tool_update(action, pid=None, identifier=None, *, state_path=None):
    from makewand.call_budget import BudgetError, BudgetConfigError
    try:
        _tool_update(action, pid, identifier, state_path=state_path)
    except BudgetError:
        raise
    except (OSError, RuntimeError, ValueError, KeyError, TypeError) as error:
        # Lifecycle failures must reach the trusted entry points as a known
        # unavailable result, including errors raised during final settlement.
        raise BudgetConfigError(f"cannot update local tool capacity: {error}") from error


def _tool_update(action, pid=None, identifier=None, *, state_path=None, missing_ok=False):
    identifier = identifier or _tool.get()
    if identifier is None:
        return
    with locked(time.monotonic() + 5, admission=False, state_path=state_path) as (path, state):
        row = state["active"].get(identifier)
        if row is None or row["kind"] != "tool":
            if missing_ok:
                return  # a deferred settlement was already durably applied
            raise RuntimeError("local tool capacity lease disappeared")
        lease = row["lease"]
        now = time.time()
        if action != "settle":
            lease.update(heartbeat_at=now, expires_at=now + account_leases.TTL)
        if action == "launch":
            lease["launch_pending"] = True
        elif action in ("worker", "job_worker"):
            ref = account_leases.process_ref(pid)
            if action == "job_worker":
                ref["job"] = "owner-kill-on-close"
            lease["workers"].append(ref)
            lease["launch_pending"] = ref.get("token") is None
        elif action in ("stopped", "launch_failed"):
            # Native Windows runners close the kill-on-close Job and wait
            # before reporting stopped. PID liveness alone cannot prove Job
            # descendants died, so use that trusted cleanup evidence here.
            if (action == "stopped" and not Path("/proc").is_dir()) or not account_leases.workers_alive(lease):
                lease["workers"], lease["launch_pending"] = [], False
        elif action == "settle":
            row["settled"] = True
            if not lease["launch_pending"] and not account_leases.workers_alive(lease):
                del state["active"][identifier]
        elif action != "heartbeat":
            raise ValueError("invalid local tool lease action")
        save(path, state)


def _defer_tool_settlement(identifier, state_path):
    """Retry only final accounting for this original tool and coordinator.

    Do not renew its heartbeat, launch work, or clear uncertain worker fences.
    A missing state/row needs no cleanup; invalid existing state stays closed.
    """
    def retry():
        waiter, delay = threading.Event(), .1
        while True:
            waiter.wait(delay)
            try:
                os.lstat(state_path)  # skip a missing coordinator
                _tool_update("settle", identifier=identifier, state_path=state_path, missing_ok=True)
                return
            except FileNotFoundError:
                return
            except (OSError, RuntimeError, ValueError, KeyError, TypeError):
                delay = min(delay * 2, 30)

    threading.Thread(target=retry, name="makewand-tool-settlement", daemon=True).start()


@contextmanager
def tool_scope(current_policy, deadline):
    from makewand.call_budget import BudgetDeadlineError, ConcurrencyBusyError, _check_admission_cancelled
    if deadline <= time.monotonic():
        raise BudgetDeadlineError("local tool deadline expired before admission")
    identifier = uuid.uuid4().hex
    state_path = str(accounting_files.canonical_path(Path(layered_budget.default_path()).with_name(STATE_NAME)))
    project_id, label = project()
    now = time.time()
    ticket = {"id": identifier, "kind": "tool", "ledger": "", "attempt": "", "project": project_id,
              "label": label, "task_id": None, "started_at": now, "limits": [1, 1, 1],
              "lease": {"keys": ["tool:provider", "tool:unknown", "tool:unknown:subscription"],
                        "owner": account_leases.process_ref(os.getpid()), "workers": [],
                        "launch_pending": False, "heartbeat_at": now, "expires_at": now + 30}}
    start = time.monotonic()
    wait_until = start + min(current_policy["wait_seconds"], max(0, deadline - start) / 3)
    next_report = 0
    try:
        while True:
            _check_admission_cancelled()
            with locked(wait_until, state_path=state_path) as (path, state):
                refresh(state, str(accounting_files.canonical_path(layered_budget.default_path())))
                existing = next((r for r in state["queue"] if r["id"] == identifier), None)
                if existing is None:
                    state["queue"].append(ticket)
                else:
                    existing["lease"]["expires_at"] = time.time() + 30
                active = list(state["active"].values())
                # Tools use FIFO in their own pool. Model project turns must
                # neither reorder them nor prevent a free tool slot being used.
                eligible = next((r for r in state["queue"] if r["kind"] == "tool"
                                 and fits(r, active, current_policy)), None)
                if eligible and eligible["id"] == identifier:
                    state["active"][identifier] = ticket
                    state["queue"] = [r for r in state["queue"] if r["id"] != identifier]
                    save(path, state)
                    break
                message = diagnostic(state, ticket, current_policy)
                save(path, state)
            if time.monotonic() >= wait_until:
                raise ConcurrencyBusyError(message)
            if time.monotonic() >= next_report:
                report_busy(message)
                next_report = time.monotonic() + 15
            time.sleep(min(.1, max(0, wait_until - time.monotonic())))
        token = _tool.set(identifier)
        stop = threading.Event()

        def heartbeat():
            while not stop.wait(5):
                try:
                    tool_update("heartbeat", identifier=identifier, state_path=state_path)
                except (OSError, RuntimeError, ValueError):
                    pass
        thread = threading.Thread(target=heartbeat, daemon=True)
        thread.start()
        try:
            yield
        finally:
            stop.set()
            thread.join(timeout=6)
            try:
                try:
                    tool_update("settle", identifier=identifier, state_path=state_path)
                except BaseException:
                    _defer_tool_settlement(identifier, state_path)
                    raise
            finally:
                _tool.reset(token)
    finally:
        cancel(identifier, current_policy)


def controlled_tool(function):
    """Limit the trusted sandbox entry point; external shells are not covered."""
    signature = inspect.signature(function)

    @functools.wraps(function)
    def wrapped(*args, **kwargs):
        values = signature.bind(*args, **kwargs)
        if values.arguments.get("is_provider", False) or _tool.get() is not None:
            return function(*args, **kwargs)
        from makewand.call_budget import BudgetError
        try:
            current_policy = policy()
            if current_policy is not None:
                deadline = time.monotonic() + values.arguments.get("timeout", 120)
                with tool_scope(current_policy, deadline):
                    values.arguments["timeout"] = max(.001, deadline - time.monotonic())
                    return function(*values.args, **values.kwargs)
        except (BudgetError, OSError, ValueError) as error:
            return -1, "", str(error), "LocalCapacityUnavailable"
        return function(*args, **kwargs)
    return wrapped
