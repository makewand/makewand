"""Shared Python admission, deadlines, typed outcomes and stage telemetry."""
import contextvars
import os
import subprocess
import threading
import time
import uuid
from contextlib import contextmanager
from dataclasses import replace
from pathlib import Path

from makewand import call_budget
from makewand.execution_contract import ExecutionRequest, ExecutionResult, identifier

TIMEOUT_SOFT_GRACE = float(os.environ.get("MAKEWAND_TIMEOUT_SOFT_GRACE", "0.02"))
_context = contextvars.ContextVar("makewand_execution", default=None)


class _ProviderInvocation:
    def __init__(self):
        self.used = False
        self.account_refused = False
        self.preflight_refused = False
        self.settled = False
        self.lock = threading.Lock()


def claim_provider_invocation():
    """Claim an enclosing admission at an explicit model operation boundary.

    Auxiliary subprocesses and endpoint discovery must not call this helper.
    The mutable token is shared when a context is copied to another thread.
    """
    invocation = (_context.get() or {}).get("_provider_invocation")
    if invocation is None:
        return False
    with invocation.lock:
        if invocation.used:
            return False
        invocation.used = True
        return True


def finish_provider_attempt():
    """Settle a known, stopped refusal before a sequential retry or relay."""
    context = current_context()
    invocation = context.get("_provider_invocation")
    if invocation is None or invocation.settled:
        return
    attempt = context.get("_attempt")
    call_budget.finish_transport(attempt, budget_file=context.get("_budget_file"),
                                 max_model_calls=context.get("_max_model_calls"))
    evidence = context.get("_evidence")
    if evidence is not None:
        from makewand import execution_evidence
        # finish_transport has settled the ledger entry and released its lease.
        evidence.update(result_status="FAILED", outcome_known=True, settled=True)
        execution_evidence.save(evidence)
    invocation.settled = True


def mark_provider_invocation():
    """Mark the actual CLI launch already admitted by the enclosing SDK call."""
    claim_provider_invocation()


def current_context():
    return dict(_context.get() or {})


@contextmanager
def auxiliary_process_scope():
    """Keep a no-model helper out of the enclosing provider's process evidence.

    Deadlines and cancellation still apply. This neither admits a model nor
    refunds any reservation; it only isolates helper subprocess observations.
    """
    context = current_context()
    context.update(_attempt=None, _evidence=None, _provider_invocation=None)
    token = _context.set(context)
    try:
        yield
    finally:
        _context.reset(token)


def task_id():
    context = current_context()
    value = context.get("task_id") or os.environ.get("MAKEWAND_TASK_ID") or os.environ.get("MAKEWAND_DAEMON_REQUEST_ID")
    if value is not None:
        identifier(value, "task_id")
        return value
    value = uuid.uuid4().hex
    context["task_id"] = value
    _context.set(context)
    return value


def initialize_cli_task():
    """A new entry point owns a task ID; daemon/child entry points inherit it."""
    value = os.environ.get("MAKEWAND_TASK_ID") or uuid.uuid4().hex
    identifier(value, "task_id")
    os.environ["MAKEWAND_TASK_ID"] = value
    context = current_context()
    context["task_id"] = value
    _context.set(context)
    return value


@contextmanager
def execution_context(**values):
    context = current_context()
    allowed = {"task_id", "stage", "deadline_unix_ms", "workflow", "risk", "readonly", "lease_id", "wait_for_capacity"}
    if set(values) - allowed:
        raise ValueError("unsupported execution context field")
    # None inherits the parent, except lease_id where None explicitly releases
    # a stage-local hold before entering an unreserved stage.
    context.update({key: value for key, value in values.items() if value is not None or key == "lease_id"})
    if "task_id" not in context:
        context["task_id"] = task_id()
    identifier(context["task_id"], "task_id")
    if values.get("deadline_unix_ms") is not None:
        deadline = values["deadline_unix_ms"]
        from makewand.execution_contract import number
        number(deadline, "deadline_unix_ms", integer=True)
        parent = (_context.get() or {}).get("_deadline_monotonic")
        derived = time.monotonic() + max(0, (deadline - time.time() * 1000) / 1000)
        context["_deadline_monotonic"] = min(parent, derived) if parent is not None else derived
        if (_context.get() or {}).get("deadline_unix_ms") is not None:
            context["deadline_unix_ms"] = min(deadline, _context.get()["deadline_unix_ms"])
    token = _context.set(context)
    try:
        yield dict(context)
    finally:
        _context.reset(token)


def account_reference(engine):
    """Actual account binding when known, otherwise explicit directory isolation."""
    from makewand.account_identity import account_identity
    identity = account_identity(engine)
    return identity["account_ref"] if identity else None


def _result(request, status, output=None, error=None, **metadata):
    return ExecutionResult(output=output, error=error, status=status,
        task_id=request.task_id, stage=request.stage, engine=request.engine,
        readonly=request.readonly, account_ref=request.account_ref, **metadata)


@contextmanager
def _request_scope(request, deadline, reservation=None, evidence=None, binding=None):
    """Publish this request's computed deadline to nested SDK invocations."""
    with execution_context(task_id=request.task_id):
        context = current_context()
        context["_request"] = request
        context["_credential_binding"] = binding
        context["_attempt"] = reservation
        context["_evidence"] = evidence
        context["_provider_invocation"] = _ProviderInvocation()
        context["_budget_file"] = request.budget_file or os.environ.get("MAKEWAND_CALL_BUDGET_FILE")
        maximum = request.max_model_calls
        if maximum is None and os.environ.get("MAKEWAND_MAX_MODEL_CALLS"):
            maximum = int(os.environ["MAKEWAND_MAX_MODEL_CALLS"])
        context["_max_model_calls"] = maximum
        if deadline is not None:
            context["_deadline_monotonic"] = deadline
            serialized = int(time.time() * 1000 + max(0, deadline - time.monotonic()) * 1000)
            parent = context.get("deadline_unix_ms")
            context["deadline_unix_ms"] = min(serialized, parent) if parent is not None else serialized
        token = _context.set(context)
        try:
            yield
        finally:
            _context.reset(token)


def _normalize(request, value, elapsed_ms, attempt):
    # This is the enclosing invocation's first admission ID. A sequential API
    # retry has its own ledger/evidence record; returning its final result must
    # not relabel or overwrite the first, already-settled refusal.
    if isinstance(value, ExecutionResult):
        return _result(request, value.status, value.output, value.error,
            attempt_id=attempt, duration_ms=elapsed_ms, artifact_digest=value.artifact_digest,
            tokens=value.tokens, monetary_cost=value.monetary_cost,
            error_kind=value.error_kind, outcome_known=value.outcome_known)
    if (not isinstance(value, (tuple, list)) or len(value) != 3
            or type(value[0]) is not bool
            or any(item is not None and not isinstance(item, str) for item in value[1:])):
        return _result(request, "UNKNOWN", error="provider returned an invalid outcome",
            attempt_id=attempt, duration_ms=elapsed_ms, error_kind="invalid_provider_result", outcome_known=False)
    return _result(request, "PASSED" if value[0] else "FAILED", value[1], value[2],
        attempt_id=attempt, duration_ms=elapsed_ms,
        error_kind=None if value[0] else "provider_rejected", outcome_known=True)


def execute(request, callback):
    """Admit one dispatch and call callback(effective_timeout_seconds) once.

    Exceptions after admission have an unknown remote outcome. No reservation
    is refunded and this function never retries the callback.
    """
    if not isinstance(request, ExecutionRequest):
        raise TypeError("execute requires an ExecutionRequest")
    from makewand.account_identity import credential_binding
    try:
        binding = credential_binding(request.engine)
    except (OSError, ValueError, RuntimeError):
        return _result(request, "INVALID_REQUEST", error="selected credential root is invalid", error_kind="account_configuration")
    try:
        selected_ref = account_reference(request.engine)
        if request.account_ref is None:
            request = replace(request, account_ref=selected_ref)
        elif selected_ref and ":account:" in selected_ref and request.account_ref != selected_ref and ":api:" not in request.account_ref:
            return _result(request, "INVALID_REQUEST", error="admission account differs from selected login", error_kind="account_configuration")
    except (OSError, ValueError, RuntimeError):
        return _result(request, "INVALID_REQUEST", error="selected credential root is invalid", error_kind="account_configuration")
    try:
        if binding != credential_binding(request.engine):
            return _result(request, "INVALID_REQUEST", error="selected login changed during admission", error_kind="account_configuration")
    except (OSError, ValueError, RuntimeError):
        return _result(request, "INVALID_REQUEST", error="selected credential root is invalid", error_kind="account_configuration")
    context = current_context()
    if context.get("_request") is not None:
        request = replace(request, task_id=context["_request"].task_id)
    if request.budget_file is None and not os.environ.get("MAKEWAND_CALL_BUDGET_FILE"):
        from makewand import layered_budget
        if layered_budget.enabled():
            request = replace(request, budget_file=layered_budget.default_path())
    inherited_file = context.get("_budget_file")
    inherited_maximum = context.get("_max_model_calls")
    if inherited_file is not None and request.budget_file is not None:
        try:
            same_ledger = Path(inherited_file).expanduser().resolve() == Path(request.budget_file).expanduser().resolve()
        except (OSError, ValueError, RuntimeError):
            same_ledger = False
        if not same_ledger:
            return _result(request, "INVALID_REQUEST", error="child execution cannot switch its parent's call budget ledger",
                error_kind="budget_configuration")
    if request.budget_file is None and inherited_file is not None:
        request = replace(request, budget_file=inherited_file)
    if inherited_maximum is not None:
        request = replace(request, max_model_calls=min(request.max_model_calls, inherited_maximum)
                          if request.max_model_calls is not None else inherited_maximum)
    now = time.monotonic()
    deadlines = []
    if request.timeout_ms is not None:
        deadlines.append(now + request.timeout_ms / 1000)
    if request.deadline_unix_ms is not None:
        deadlines.append(now + (request.deadline_unix_ms - time.time() * 1000) / 1000)
    if context.get("_deadline_monotonic") is not None:
        deadlines.append(context["_deadline_monotonic"])
    deadline = min(deadlines) if deadlines else None
    if deadline is not None and deadline <= now:
        return _result(request, "TIMEOUT", error="execution deadline expired before dispatch",
            error_kind="deadline", outcome_known=True)
    try:
        reservation = call_budget.reserve(request.engine, request.tier, request.model, request.readonly,
            lease_id=context.get("lease_id"), task_id=request.task_id, stage=request.stage,
            account_ref=request.account_ref, api_policy=request.api_policy,
            budget_file=request.budget_file, max_model_calls=request.max_model_calls,
            deadline_monotonic=deadline, pending=True,
            wait_for_capacity=context.get("wait_for_capacity", True), working_directory=request.cwd)
    except call_budget.BudgetCancelledError as error:
        return _result(request, "CANCELLED", error=str(error), error_kind="cancelled", outcome_known=True)
    except call_budget.BudgetDeadlineError:
        return _result(request, "TIMEOUT", error="execution deadline expired during model admission",
            error_kind="deadline", outcome_known=True)
    except call_budget.ConcurrencyBusyError as error:
        return _result(request, "FAILED", error=str(error), error_kind="concurrency_busy", outcome_known=True)
    except call_budget.BudgetConfigError as error:
        return _result(request, "INTERNAL_ERROR", error=str(error), error_kind="budget_configuration", outcome_known=True)
    except call_budget.BudgetError as error:
        return _result(request, "BUDGET_EXHAUSTED", error=str(error), error_kind="budget_admission")
    # An admitted invocation has an identity even when no hard limit is enabled.
    attempt = reservation or uuid.uuid4().hex
    from makewand.telemetry import stage
    interruption = None
    from makewand import execution_evidence
    publication_failed = False
    try:
        evidence = execution_evidence.new(request, attempt)
        execution_evidence.save(evidence)
    except execution_evidence.PublishedWriteError:
        publication_failed = True
        evidence["persistence_failed"] = True
    except (OSError, ValueError, RuntimeError):
        # No record exists and nothing has started (an unwritable directory, a
        # full disk): the call proceeds with degraded telemetry. Nothing on disk
        # can contradict its outcome, so it is not an unknown one.
        evidence = None
        execution_evidence.warn_unavailable()
    with _request_scope(request, deadline, reservation, evidence, binding), _lease_heartbeat(reservation, request):
        with stage("provider", request.engine, request.readonly, attempt_id=attempt,
                   account_ref=request.account_ref) as span:
            started = time.monotonic()
            remaining = max(0, deadline - started) if deadline is not None else None
            if publication_failed:
                result = _result(request, "UNKNOWN", error="provider evidence publication incomplete before launch",
                    attempt_id=attempt, error_kind="accounting", outcome_known=False)
            elif remaining is not None and remaining <= 0:
                result = _result(request, "TIMEOUT", error="execution deadline expired before provider invocation",
                    attempt_id=attempt, error_kind="deadline", outcome_known=True)
            else:
                try:
                    call_budget.start(reservation, budget_file=request.budget_file, max_model_calls=request.max_model_calls)
                    value = callback(remaining)
                    elapsed = max(0, int((time.monotonic() - started) * 1000))
                    result = _normalize(request, value, elapsed, attempt)
                    if deadline is not None and time.monotonic() > deadline + TIMEOUT_SOFT_GRACE:
                        sanitized_out = result.output.replace("\0", "") if isinstance(result.output, str) else result.output
                        result = _result(request, "TIMEOUT", output=sanitized_out,
                            error="execution deadline expired", attempt_id=attempt,
                            duration_ms=elapsed, error_kind="deadline", outcome_known=False)
                except (TimeoutError, subprocess.TimeoutExpired) as exc:
                    output = (
                        getattr(exc, "output", None)
                        or getattr(exc, "stdout", None)
                        or getattr(exc, "partial_output", None)
                        or getattr(exc, "text", None)
                    )
                    if isinstance(output, bytes):
                        output = output.decode("utf-8", errors="replace")
                    if isinstance(output, str):
                        output = output.replace("\0", "")
                    error = "provider execution timed out"
                    stderr = getattr(exc, "stderr", None)
                    if isinstance(stderr, bytes):
                        stderr = stderr.decode("utf-8", errors="replace")
                    if isinstance(stderr, str):
                        stderr = stderr.replace("\0", "").strip()
                    if stderr:
                        error = f"{error}: {stderr}"
                    result = _result(request, "TIMEOUT", output=output, error=error, attempt_id=attempt,
                        duration_ms=max(0, int((time.monotonic() - started) * 1000)), error_kind="deadline", outcome_known=False)
                except (KeyboardInterrupt, SystemExit) as error:
                    interruption = error
                    result = _result(request, "CANCELLED", error="provider execution interrupted", attempt_id=attempt,
                        duration_ms=max(0, int((time.monotonic() - started) * 1000)), error_kind="interrupted", outcome_known=False)
                except Exception:
                    result = _result(request, "UNKNOWN", error="provider outcome unavailable after dispatch", attempt_id=attempt,
                        duration_ms=max(0, int((time.monotonic() - started) * 1000)), error_kind="provider_exception", outcome_known=False)
            invocation = current_context()["_provider_invocation"]
            if (invocation.account_refused or invocation.preflight_refused) and not invocation.used:
                try:
                    call_budget.reject_unlaunched(reservation, budget_file=request.budget_file,
                                                  max_model_calls=request.max_model_calls)
                    if invocation.account_refused:
                        result = _result(request, "INVALID_REQUEST", error="selected login changed before launch",
                                         attempt_id=attempt, error_kind="account_configuration", outcome_known=True)
                    else:
                        result = _result(request, "FAILED", error=result.error,
                                         attempt_id=attempt, error_kind="quota_preflight", outcome_known=True)
                except (OSError, RuntimeError, ValueError):
                    result = _result(request, "UNKNOWN", error="account refusal accounting unavailable",
                                     attempt_id=attempt, error_kind="accounting", outcome_known=False)
            recorded = False
            try:
                if evidence is not None:
                    if evidence.pop("persistence_failed", False):
                        result = _result(request, "UNKNOWN", output=result.output, error="provider evidence incomplete",
                            attempt_id=attempt, duration_ms=result.duration_ms, error_kind="accounting", outcome_known=False)
                    if result.tokens is None and evidence["tokens"] is not None:
                        result = ExecutionResult.from_dict(dict(result.to_dict(), tokens=evidence["tokens"]))
                    if not current_context()["_provider_invocation"].settled:
                        # Without a ledger nothing else can still hold this attempt open.
                        evidence.update(result_status=result.status if reservation is None else "UNKNOWN",
                                        outcome_known=result.outcome_known if reservation is None else False,
                                        tokens=result.tokens, settled=reservation is None)
                        execution_evidence.save(evidence)
                    recorded = True
            except (OSError, ValueError, RuntimeError):
                result = _result(request, "UNKNOWN", output=result.output, error="provider evidence unavailable",
                    attempt_id=attempt, duration_ms=result.duration_ms, error_kind="accounting", outcome_known=False)
            released = False
            try:
                call_budget.release_unstarted(reservation, budget_file=request.budget_file, max_model_calls=request.max_model_calls)
                released = call_budget.complete(reservation, result.success, result.duration_ms / 1000,
                    result_status=result.status, outcome_known=result.outcome_known,
                    tokens=result.tokens, monetary_cost=result.monetary_cost,
                    budget_file=request.budget_file, max_model_calls=request.max_model_calls) is not False
            except (call_budget.BudgetError, OSError, ValueError, TypeError, RuntimeError):
                result = _result(request, "UNKNOWN", output=result.output, error="model call accounting unavailable",
                    attempt_id=attempt, duration_ms=result.duration_ms, error_kind="accounting", outcome_known=False)
            if recorded:
                try:
                    if released:
                        evidence.update(result_status=result.status, outcome_known=result.outcome_known)
                    execution_evidence.settle(evidence, ledger_settled=released)
                except (OSError, ValueError, RuntimeError):
                    result = _result(request, "UNKNOWN", output=result.output, error="provider evidence settlement unavailable",
                        attempt_id=attempt, duration_ms=result.duration_ms, error_kind="accounting", outcome_known=False)
                    # Accounting already released the lease: only downgrade
                    # its outcome, retaining its charge and finish timestamp.
                    try:
                        call_budget.complete(reservation, False, result.duration_ms / 1000,
                            result_status="UNKNOWN", outcome_known=False, evidence_failed=True,
                            budget_file=request.budget_file, max_model_calls=request.max_model_calls)
                    except (call_budget.BudgetError, OSError, ValueError, TypeError, RuntimeError):
                        pass
                    evidence.update(result_status="UNKNOWN", outcome_known=False, settled=released)
                    try:
                        execution_evidence.save(evidence)
                    except (OSError, ValueError, RuntimeError):
                        evidence["settled"] = False
            span.finish(result.status, error_kind=result.error_kind, artifact_digest=result.artifact_digest,
                tokens=result.tokens, monetary_cost=result.monetary_cost)
    if interruption is not None:
        raise interruption
    return result


@contextmanager
def _lease_heartbeat(attempt, request):
    stopped = threading.Event()
    def beat():
        while not stopped.wait(5):
            try:
                call_budget.lease_update(attempt, budget_file=request.budget_file, max_model_calls=request.max_model_calls)
            except (OSError, RuntimeError, ValueError):
                # A missed beat never frees capacity while lifecycle is live.
                pass
    worker = threading.Thread(target=beat, daemon=True)
    if attempt:
        worker.start()
    try:
        yield
    finally:
        stopped.set()
        if attempt:
            worker.join(timeout=.1)


def provider_process_lease(action, pid=None):
    from makewand import shared_capacity
    shared_capacity.tool_update(action, pid)
    context = current_context()
    for retry in range(3):
        try:
            call_budget.lease_update(context.get("_attempt"), action, pid,
                                    budget_file=context.get("_budget_file"), max_model_calls=context.get("_max_model_calls"))
            return
        except (OSError, RuntimeError):
            if action not in ("launch_failed", "stopped") or retry == 2:
                raise
            time.sleep(.05 * (retry + 1))
