"""Shared Python admission, deadlines, typed outcomes and stage telemetry."""
import contextvars
import hashlib
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

_context = contextvars.ContextVar("makewand_execution", default=None)


class _ProviderInvocation:
    def __init__(self):
        self.used = False
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


def mark_provider_invocation():
    """Mark the actual CLI launch already admitted by the enclosing SDK call."""
    claim_provider_invocation()


def current_context():
    return dict(_context.get() or {})


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


@contextmanager
def execution_context(**values):
    context = current_context()
    allowed = {"task_id", "stage", "deadline_unix_ms", "workflow", "risk", "readonly", "lease_id"}
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
    """Opaque credential-root binding, not an assertion of account identity."""
    if engine == "codex":
        path = os.environ.get("CODEX_HOME") or str(Path.home() / ".codex")
    elif engine == "claude":
        path = os.environ.get("CLAUDE_CONFIG_DIR") or str(Path.home() / ".claude")
    else:
        return None
    root = str(Path(path).expanduser().resolve())
    return engine + ":" + hashlib.sha256(root.encode()).hexdigest()[:16]


def _result(request, status, output=None, error=None, **metadata):
    return ExecutionResult(output=output, error=error, status=status,
        task_id=request.task_id, stage=request.stage, engine=request.engine,
        readonly=request.readonly, account_ref=request.account_ref, **metadata)


@contextmanager
def _request_scope(request, deadline):
    """Publish this request's computed deadline to nested SDK invocations."""
    with execution_context(task_id=request.task_id):
        context = current_context()
        context["_request"] = request
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
    if request.account_ref is None:
        try:
            request = replace(request, account_ref=account_reference(request.engine))
        except (OSError, ValueError, RuntimeError):
            return _result(request, "INVALID_REQUEST", error="selected credential root is invalid", error_kind="account_configuration")
    context = current_context()
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
            deadline_monotonic=deadline)
    except call_budget.BudgetDeadlineError:
        return _result(request, "TIMEOUT", error="execution deadline expired during model admission",
            error_kind="deadline", outcome_known=True)
    except call_budget.BudgetError as error:
        return _result(request, "BUDGET_EXHAUSTED", error=str(error), error_kind="budget_admission")
    # An admitted invocation has an identity even when no hard limit is enabled.
    attempt = reservation or uuid.uuid4().hex
    from makewand.telemetry import stage
    interruption = None
    with _request_scope(request, deadline):
        with stage("provider", request.engine, request.readonly, attempt_id=attempt,
                   account_ref=request.account_ref) as span:
            started = time.monotonic()
            remaining = max(0, deadline - started) if deadline is not None else None
            if remaining is not None and remaining <= 0:
                result = _result(request, "TIMEOUT", error="execution deadline expired before provider invocation",
                    attempt_id=attempt, error_kind="deadline", outcome_known=True)
            else:
                try:
                    value = callback(remaining)
                    elapsed = max(0, int((time.monotonic() - started) * 1000))
                    result = _normalize(request, value, elapsed, attempt)
                    if deadline is not None and time.monotonic() > deadline:
                        result = _result(request, "TIMEOUT", output=result.output,
                            error="execution deadline expired", attempt_id=attempt,
                            duration_ms=elapsed, error_kind="deadline", outcome_known=False)
                except (TimeoutError, subprocess.TimeoutExpired):
                    result = _result(request, "TIMEOUT", error="provider execution timed out", attempt_id=attempt,
                        duration_ms=max(0, int((time.monotonic() - started) * 1000)), error_kind="deadline", outcome_known=False)
                except (KeyboardInterrupt, SystemExit) as error:
                    interruption = error
                    result = _result(request, "CANCELLED", error="provider execution interrupted", attempt_id=attempt,
                        duration_ms=max(0, int((time.monotonic() - started) * 1000)), error_kind="interrupted", outcome_known=False)
                except Exception:
                    result = _result(request, "UNKNOWN", error="provider outcome unavailable after dispatch", attempt_id=attempt,
                        duration_ms=max(0, int((time.monotonic() - started) * 1000)), error_kind="provider_exception", outcome_known=False)
            try:
                call_budget.complete(reservation, result.success, result.duration_ms / 1000,
                    result_status=result.status, outcome_known=result.outcome_known,
                    tokens=result.tokens, monetary_cost=result.monetary_cost,
                    budget_file=request.budget_file, max_model_calls=request.max_model_calls)
            except (call_budget.BudgetError, OSError, ValueError, TypeError, RuntimeError):
                result = _result(request, "UNKNOWN", output=result.output, error="model call accounting unavailable",
                    attempt_id=attempt, duration_ms=result.duration_ms, error_kind="accounting", outcome_known=False)
            span.finish(result.status, error_kind=result.error_kind, artifact_digest=result.artifact_digest,
                tokens=result.tokens, monetary_cost=result.monetary_cost)
    if interruption is not None:
        raise interruption
    return result
