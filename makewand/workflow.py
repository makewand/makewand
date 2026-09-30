"""Conservative workflow policy. Selection never waives verification gates."""
from dataclasses import dataclass
import math
from contextvars import ContextVar

_last_result = ContextVar("makewand_workflow_result", default=None)


def remember_result(result):
    _last_result.set(result)
    return result


def last_result():
    """Typed outcome of the last workflow in this execution context."""
    return _last_result.get()


def provider_outcome(value):
    """Recognize runner-owned legacy errors without guessing from model text."""
    from makewand.execution_contract import ExecutionResult
    import re
    if (isinstance(value, ExecutionResult) or not isinstance(value, (tuple, list))
            or len(value) != 3 or value[0] is not False or not isinstance(value[2], str)):
        return value
    error = value[2]
    status = getattr(error, "execution_status", None)
    if status:
        pass  # Structured runner evidence has priority over compatibility text.
    elif re.fullmatch(r"Command timed out after [0-9.]+ seconds", error) or error.startswith("Total timeout exceeded: API call did not finish within "):
        status = "TIMEOUT"
    elif (re.fullmatch(r"(?:Command|Claude|Codex|Grok|Muse|AGY|Aider) (?:returned exit|exited with) code -?[1-9]\d*", error)
          or re.match(r"^\[Errno (?:32|104)\]", error)
          or error.startswith(("Network/URL Error:", "Execution Exception:", "JSON parse error:", "Truncated stream:", "Stream ended without terminal event:"))):
        status = "UNKNOWN"
    elif ("Bubblewrap" in error or "bwrap" in error or "沙箱构建失败" in error) and ("拒绝执行" in error or "未检测到" in error):
        status = "SANDBOX_UNAVAILABLE"
    return ExecutionResult(False, value[1], str(error), status=status, outcome_known=status in ("FAILED", "SANDBOX_UNAVAILABLE")) if status else value


@dataclass(frozen=True)
class WorkflowPlan:
    workflow: str
    risk: str
    cross_review_required: bool
    reason: str


def choose_workflow(workflow="auto", risk="auto", evidence=None):
    if workflow not in ("auto", "single", "pipeline", "race"):
        raise ValueError("workflow must be auto, single, pipeline or race")
    if risk not in ("auto", "low", "high"):
        raise ValueError("risk must be auto, low or high")
    # Auto evidence is supplied by a trusted caller, never extracted from prompt
    # keywords. Only a complete explicit scope can establish low risk.
    resolved_risk = "medium" if risk == "auto" else risk
    if risk == "auto" and isinstance(evidence, dict):
        if evidence.get("sensitive_interfaces") is True or evidence.get("external_side_effects") is True:
            resolved_risk = "high"
        elif (evidence.get("scope_complete") is True
              and evidence.get("sensitive_interfaces") is False
              and evidence.get("external_side_effects") is False
              and isinstance(evidence.get("changed_files"), int)
              and not isinstance(evidence.get("changed_files"), bool)
              and 0 < evidence["changed_files"] <= 2
              and isinstance(evidence.get("changed_lines"), int)
              and not isinstance(evidence.get("changed_lines"), bool)
              and 0 < evidence["changed_lines"] <= 100):
            resolved_risk = "low"
    selected = ("single" if resolved_risk == "low" else "pipeline") if workflow == "auto" else workflow
    if selected == "single" and resolved_risk != "low":
        raise ValueError("single workflow requires explicit low risk or complete trusted low-risk scope evidence")
    cross_review = resolved_risk == "high" or selected in ("pipeline", "race")
    return WorkflowPlan(selected, resolved_risk, cross_review,
                        "explicit workflow" if workflow != "auto" else "verified low-risk scope" if resolved_risk == "low" else "risk unknown or elevated; cross review required")


def judge_reserve(total_seconds, requested=None):
    if isinstance(total_seconds, bool) or not isinstance(total_seconds, (int, float)) or not math.isfinite(total_seconds) or total_seconds <= 0:
        raise ValueError("total timeout must be a finite positive number")
    reserve = min(60.0, total_seconds * .25) if requested is None else requested
    if isinstance(reserve, bool) or not isinstance(reserve, (int, float)) or not math.isfinite(reserve) or not 0 <= reserve < total_seconds:
        raise ValueError("judge reserve must be finite, nonnegative and less than the total timeout")
    return float(reserve)
