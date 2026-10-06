"""Opt-in metadata-only execution spans; never log prompts or credentials."""
import json
import errno
import os
import stat
import sys
import time
import uuid
from contextlib import contextmanager
from pathlib import Path

from makewand import filelock
from makewand.execution_contract import SCHEMA, STATUS_CODES, identifier, number

EVENT_FIELDS = frozenset(("schema", "event_id", "task_id", "benchmark_run", "stage",
    "event", "engine", "attempt_id", "readonly", "status", "start_unix_ms",
    "duration_ms", "artifact_digest", "error_kind", "tokens", "monetary_cost",
    "peak_rss_bytes", "account_ref"))
_warned = False
STAGE_ALIASES = {"implementation": "generation", "test": "verification", "judge": "review"}


def validate_event(event):
    if not isinstance(event, dict) or set(event) != EVENT_FIELDS:
        raise ValueError("invalid execution event fields")
    if type(event["schema"]) is not int or event["schema"] != SCHEMA:
        raise ValueError("unsupported execution event schema")
    for field in ("event_id", "task_id", "stage"):
        identifier(event[field], field)
    for field in ("benchmark_run", "engine", "attempt_id", "account_ref", "error_kind"):
        identifier(event[field], field, nullable=True)
    if type(event["readonly"]) is not bool or event["event"] not in ("start", "end"):
        raise ValueError("invalid execution event flags")
    number(event["start_unix_ms"], "start_unix_ms", integer=True)
    for field in ("duration_ms", "tokens", "peak_rss_bytes"):
        number(event[field], field, nullable=True, integer=True)
    number(event["monetary_cost"], "monetary_cost", nullable=True)
    if event["event"] == "start":
        if event["status"] is not None or event["duration_ms"] is not None:
            raise ValueError("start event cannot claim completion")
    elif event["status"] not in STATUS_CODES or event["duration_ms"] is None:
        raise ValueError("end event requires an execution outcome")
    digest = event["artifact_digest"]
    if digest is not None:
        import re
        if not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
            raise ValueError("invalid artifact digest")
    return event


def emit_event(event):
    """Observation failures never cause an already completed task to replay."""
    value = os.environ.get("MAKEWAND_EXECUTION_EVENTS_FILE")
    if not value:
        return False
    lock = None
    acquired = False
    try:
        path = Path(value).expanduser().absolute()
        validate_event(event)
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        lock = os.open(str(path) + ".lock", os.O_CREAT | os.O_RDWR | getattr(os, "O_NOFOLLOW", 0), 0o600)
        lock_deadline = time.monotonic() + .1
        while True:
            try:
                filelock.flock(lock, filelock.LOCK_EX | filelock.LOCK_NB)
                acquired = True
                break
            except OSError as error:
                if error.errno not in (errno.EACCES, errno.EAGAIN, errno.EWOULDBLOCK) and getattr(error, "winerror", None) not in (33, 158):
                    raise
                remaining = lock_deadline - time.monotonic()
                if remaining <= 0:
                    raise OSError("execution event lock unavailable") from None
                time.sleep(min(.01, remaining))
        descriptor = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0), 0o600)
        with os.fdopen(descriptor, "wb") as stream:
            if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
                raise OSError("event sink must be a regular file")
            if hasattr(os, "fchmod"):
                os.fchmod(stream.fileno(), 0o600)
            else:
                os.chmod(path, 0o600)
            stream.write((json.dumps(event, ensure_ascii=True, allow_nan=False, separators=(",", ":")) + "\n").encode())
            stream.flush()
        return True
    except (OSError, ValueError, TypeError, RuntimeError):
        global _warned
        if not _warned:
            _warned = True
            try:
                print("makewand: execution telemetry unavailable; task outcome preserved", file=sys.stderr)
            except Exception:
                pass
        return False
    finally:
        if acquired:
            try:
                filelock.flock(lock, filelock.LOCK_UN)
            except OSError:
                pass
        if lock is not None:
            try:
                os.close(lock)
            except OSError:
                pass


class StageSpan:
    def __init__(self, name, engine=None, readonly=False, attempt_id=None, account_ref=None):
        from makewand.execution_runtime import task_id
        self.started = time.monotonic()
        self.event = dict(schema=SCHEMA, event_id=uuid.uuid4().hex, task_id=task_id(),
            benchmark_run=os.environ.get("MAKEWAND_BENCHMARK_RUN_ID"), stage=name,
            event="start", engine=engine, attempt_id=attempt_id, readonly=readonly,
            status=None, start_unix_ms=int(time.time() * 1000), duration_ms=None,
            artifact_digest=None, error_kind=None, tokens=None, monetary_cost=None,
            peak_rss_bytes=None, account_ref=account_ref)
        self.finished = False
        self.status = "PASSED"
        self.metadata = {}
        emit_event(self.event)

    def finish(self, status="PASSED", error_kind=None, artifact_digest=None, **metadata):
        if status not in STATUS_CODES:
            raise ValueError("invalid stage status")
        allowed = {"attempt_id", "account_ref", "tokens", "monetary_cost", "peak_rss_bytes"}
        if set(metadata) - allowed:
            raise ValueError("unsafe stage metadata")
        self.status = status
        self.metadata.update(metadata, error_kind=error_kind, artifact_digest=artifact_digest)

    def close(self):
        if self.finished:
            return
        self.finished = True
        event = dict(self.event, **self.metadata)
        event.update(event="end", status=self.status, duration_ms=max(0, int((time.monotonic() - self.started) * 1000)))
        emit_event(event)


@contextmanager
def stage(name, engine=None, readonly=False, *, attempt_id=None, account_ref=None):
    from makewand.execution_runtime import execution_context
    name = STAGE_ALIASES.get(name, name)
    with execution_context(stage=name, readonly=readonly):
        span = StageSpan(name, engine, readonly, attempt_id, account_ref)
        try:
            yield span
        except BaseException as error:
            interrupted = isinstance(error, (KeyboardInterrupt, SystemExit))
            try:
                status = getattr(error, "execution_status", None) or getattr(error, "status", None)
            except BaseException:
                status = None
            # Preserve typed failures without copying raw exception text into
            # events or allowing malformed metadata to replace the exception.
            if not isinstance(status, str) or status not in STATUS_CODES or status == "PASSED":
                status = "CANCELLED" if interrupted else "FAILED"
            span.finish(status, error_kind="interrupted" if interrupted else "stage_exception")
            raise
        finally:
            span.close()
