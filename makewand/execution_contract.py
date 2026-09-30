"""Versioned execution outcomes shared by native, Python and IPC callers.

PASSED means the requested stage completed. Delivery still requires the sealed
test, review and apply gates. Costs remain null until actually observed.
"""
from __future__ import annotations
import math
import re
from dataclasses import asdict, dataclass

SCHEMA = 1
STATUS_CODES = {
    "PASSED": 0, "INTERNAL_ERROR": 1, "INVALID_REQUEST": 2, "FAILED": 10,
    "UNVERIFIED": 11, "CANCELLED": 12, "BUDGET_EXHAUSTED": 13,
    "APPLY_CONFLICT": 14, "SANDBOX_UNAVAILABLE": 15, "TIMEOUT": 16,
    "UNKNOWN": 17,
}
EXIT_PASSED = 0
EXIT_INTERNAL_ERROR = 1
EXIT_USAGE_ERROR = 2
EXIT_FAILED = 10
EXIT_UNVERIFIED = 11
EXIT_CANCELLED = 12
EXIT_BUDGET_EXHAUSTED = 13
EXIT_APPLY_CONFLICT = 14
EXIT_SANDBOX_UNAVAILABLE = 15
EXIT_TIMEOUT = 16
EXIT_UNKNOWN = 17


def identifier(value, field, nullable=False):
    if nullable and value is None:
        return
    if not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9_.:-]{1,128}", value):
        raise ValueError(f"invalid {field}")


def number(value, field, nullable=False, integer=False):
    if nullable and value is None:
        return
    if (isinstance(value, bool) or not isinstance(value, int if integer else (int, float))
            or value < 0 or (isinstance(value, float) and not math.isfinite(value))
            or (integer and value > 9223372036854775807)
            or (not integer and value > 1.7976931348623157e308)):
        raise ValueError(f"invalid {field}")


def text_value(value, field, nullable=True):
    if nullable and value is None:
        return
    if not isinstance(value, str) or "\0" in value:
        raise ValueError(f"invalid {field}")


@dataclass(frozen=True)
class ExecutionRequest:
    task_id: str
    stage: str
    engine: str
    tier: str = "standard"
    model: str | None = None
    account_ref: str | None = None
    readonly: bool = False
    repo_trust: str = "untrusted"
    api_policy: str = "subscription_only"
    deadline_unix_ms: int | None = None
    timeout_ms: int | None = None
    budget_file: str | None = None
    max_model_calls: int | None = None
    workflow: str | None = None
    risk: str | None = None
    prompt: str | None = None
    cwd: str | None = None
    schema: int = SCHEMA

    def __post_init__(self):
        if type(self.schema) is not int or self.schema != SCHEMA:
            raise ValueError("unsupported execution schema")
        for field in ("task_id", "stage", "engine", "tier"):
            identifier(getattr(self, field), field)
        identifier(self.account_ref, "account_ref", nullable=True)
        if type(self.readonly) is not bool:
            raise ValueError("readonly must be boolean")
        if self.repo_trust not in ("trusted", "untrusted"):
            raise ValueError("invalid repo_trust")
        if self.api_policy not in ("subscription_only", "allow_paid"):
            raise ValueError("invalid api_policy")
        for field in ("deadline_unix_ms", "timeout_ms", "max_model_calls"):
            value = getattr(self, field)
            number(value, field, nullable=True, integer=True)
            if value == 0:
                raise ValueError(f"{field} must be positive")
        for field in ("model", "budget_file", "prompt", "cwd"):
            text_value(getattr(self, field), field)
        if self.workflow not in (None, "auto", "single", "pipeline", "race", "review", "direct"):
            raise ValueError("invalid workflow")
        if self.risk not in (None, "auto", "low", "medium", "high", "unknown"):
            raise ValueError("invalid risk")

    def to_dict(self):
        return asdict(self)

    @classmethod
    def from_dict(cls, data):
        if not isinstance(data, dict) or type(data.get("schema")) is not int or data["schema"] != SCHEMA:
            raise ValueError("execution request must be an object")
        return cls(**data)


class ExecutionResult(tuple):
    """Three-value tuple compatibility with an explicit, serializable outcome."""
    def __new__(cls, success=None, output=None, error=None, *, status=None,
                task_id="unassigned", attempt_id=None, stage="provider", engine="unknown",
                account_ref=None, readonly=False, duration_ms=0, artifact_digest=None,
                tokens=None, monetary_cost=None, error_kind=None, outcome_known=None,
                exit_code=None, schema=SCHEMA):
        if status is None:
            if type(success) is not bool:
                raise ValueError("success must be boolean")
            status = "PASSED" if success else "FAILED"
        if not isinstance(status, str) or status not in STATUS_CODES:
            raise ValueError("invalid execution status")
        expected = status == "PASSED"
        if success is not None and (type(success) is not bool or success != expected):
            raise ValueError("success conflicts with execution status")
        if type(schema) is not int or schema != SCHEMA:
            raise ValueError("unsupported execution schema")
        if exit_code is not None and (type(exit_code) is not int or exit_code != STATUS_CODES[status]):
            raise ValueError("exit code conflicts with execution status")
        for field, value in (("task_id", task_id), ("stage", stage), ("engine", engine)):
            identifier(value, field)
        for field, value in (("attempt_id", attempt_id), ("account_ref", account_ref), ("error_kind", error_kind)):
            identifier(value, field, nullable=True)
        if outcome_known is None:
            outcome_known = status != "UNKNOWN"
        if type(readonly) is not bool or type(outcome_known) is not bool:
            raise ValueError("result flags must be boolean")
        if status == "UNKNOWN" and outcome_known:
            raise ValueError("unknown outcome cannot be claimed as known")
        number(duration_ms, "duration_ms", nullable=True, integer=True)
        number(tokens, "tokens", nullable=True, integer=True)
        number(monetary_cost, "monetary_cost", nullable=True)
        if artifact_digest is not None and (not isinstance(artifact_digest, str)
                or not re.fullmatch(r"[0-9a-f]{64}", artifact_digest)):
            raise ValueError("invalid artifact_digest")
        text_value(output, "output")
        text_value(error, "error")
        result = tuple.__new__(cls, (expected, output, error))
        result._metadata = dict(schema=schema, task_id=task_id, attempt_id=attempt_id,
            stage=stage, engine=engine, status=status, exit_code=STATUS_CODES[status],
            account_ref=account_ref, readonly=readonly, duration_ms=duration_ms,
            artifact_digest=artifact_digest, tokens=tokens, monetary_cost=monetary_cost,
            error_kind=error_kind, outcome_known=outcome_known)
        return result

    def __getattr__(self, name):
        try:
            return self._metadata[name]
        except KeyError:
            raise AttributeError(name) from None

    @property
    def success(self):
        return self[0]

    @property
    def output(self):
        return self[1]

    @property
    def error(self):
        return self[2]

    def to_dict(self):
        return dict(self._metadata, output=self.output, error=self.error)

    @classmethod
    def from_dict(cls, data):
        if not isinstance(data, dict) or type(data.get("schema")) is not int or data["schema"] != SCHEMA:
            raise ValueError("execution result must be an object")
        return cls(**data)
