"""Durable, metadata-only provider observations, shared with the native runner.

Protocol message bodies, arbitrary strings, tokens and credential documents are
never serialized. Missing observations stay unknown; argv is not an observation.
"""
import json
import math
import os
import re
import sys
import time
from datetime import datetime
from pathlib import Path

from makewand.account_identity import account_identity, event_account_ref
from makewand.execution_contract import identifier


def directory():
    value = os.environ.get("MAKEWAND_EXECUTION_EVIDENCE_DIR")
    if value:
        return Path(value)
    if sys.platform == "win32":
        cache = os.environ.get("LocalAppData") or os.environ.get("LOCALAPPDATA")
        if not cache:
            raise OSError("provider evidence requires LocalAppData")
        cache = Path(cache)
    elif sys.platform == "darwin":
        cache = Path.home() / "Library" / "Caches"
    else:
        cache = Path(os.environ.get("XDG_CACHE_HOME") or Path.home() / ".cache")
        if not cache.is_absolute():
            raise OSError("provider evidence requires an absolute cache directory")
    return cache / "makewand" / "execution-evidence"


def new(request, attempt):
    identity = account_identity(request.engine) if request.engine in ("codex", "claude") else None
    return dict(schema=1, task_id=request.task_id, attempt_id=attempt, engine=request.engine,
                account_ref=identity["account_ref"] if identity else "",
                identity_known=bool(identity and identity["identity_known"]),
                pool_ref=identity["pool_ref"] if identity and identity["pool_ref"] else "",
                source="selected_login" if identity else "unavailable", observed_at_ms=int(time.time() * 1000),
                windows=[], result_status="UNKNOWN", outcome_known=False, tokens=None, process_exit_code=None)


def save(evidence):
    from makewand.accounting_files import pin
    from makewand import filelock
    identifier(evidence["attempt_id"], "attempt_id")
    evidence["observed_at_ms"] = int(time.time() * 1000)
    encoded = json.dumps(evidence, allow_nan=False, separators=(",", ":")).encode()
    with pin(directory() / (evidence["attempt_id"] + ".json")) as files:
        fd = files.open(files.lock_name, create=True, writable=True)
        try:
            deadline = time.monotonic() + 1
            while True:
                try:
                    filelock.flock(fd, filelock.LOCK_EX | filelock.LOCK_NB)
                    break
                except BlockingIOError:
                    if time.monotonic() >= deadline:
                        raise OSError("provider evidence lock unavailable") from None
                    time.sleep(.01)
            try:
                files.save(encoded, fd)
            finally:
                filelock.flock(fd, filelock.LOCK_UN)
        finally:
            os.close(fd)


def _model(value):
    return value if isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_.:-]{1,128}", value) else None


def _timestamp(value):
    try:
        if isinstance(value, bool) or value is None:
            return None
        if isinstance(value, (int, float)) or isinstance(value, str) and value.replace(".", "", 1).isdigit():
            parsed = float(value)
        else:
            parsed = datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
        return int(parsed * 1000) if math.isfinite(parsed) and parsed > 0 else None
    except (ValueError, TypeError, OverflowError, AttributeError):
        return None


def protocol(evidence, raw):
    """Inspect CLI-owned envelopes only, never item text or tool output."""
    engine = evidence["engine"]
    session_ref = None
    for line in raw.splitlines():
        if len(line) > 1024 * 1024:
            continue
        try:
            obj = json.loads(line)
            if not isinstance(obj, dict):
                continue
            kind = obj.get("type")
            payload = obj.get("payload")
            payload = payload if isinstance(payload, dict) else {}
            ref = event_account_ref(obj) if engine == "codex" else None
            if kind == "session_meta":
                session_ref = ref
            if (ref or session_ref) and (ref or session_ref) != evidence["account_ref"]:
                continue
            fields = (payload if engine == "codex" and kind in ("session_meta", "turn_context")
                      else obj if engine == "claude" and kind in ("system", "result") else {})
            if engine == "claude" and kind == "assistant" and isinstance(obj.get("message"), dict):
                fields = obj["message"]
            model = _model(fields.get("model"))
            if model:
                evidence["actual_model"] = model
            if engine == "claude" and kind == "result":
                models = obj.get("modelUsage")
                if isinstance(models, dict) and len(models) == 1 and _model(next(iter(models))):
                    evidence["actual_model"] = next(iter(models))
            usage = obj.get("usage") if ((engine == "codex" and kind == "turn.completed")
                                              or (engine == "claude" and kind == "result")) else None
            if isinstance(usage, dict):
                counts = [usage.get("input_tokens"), usage.get("output_tokens")]
                if all(type(value) is int and 0 <= value <= 9223372036854775807 for value in counts) and sum(counts) <= 9223372036854775807:
                    evidence["tokens"] = sum(counts)
            if engine != "codex" or not evidence["identity_known"]:
                continue
            if kind != "rate_limits.updated" and not (kind == "event_msg" and payload.get("type") == "token_count"):
                continue
            at = _timestamp(obj.get("timestamp"))
            limits = obj.get("rate_limits") or payload.get("rate_limits")
            if not at or at > time.time() * 1000 + 60000 or not isinstance(limits, dict):
                continue
            scoped = limits.get("model") or (limits.get("limit_id") if limits.get("limit_id") != "codex" else "") or ""
            if scoped and not _model(scoped):
                continue
            for slot, key in enumerate(("primary", "secondary")):
                window = limits.get(key)
                if not isinstance(window, dict):
                    continue
                used = window.get("used_percent")
                if type(used) not in (int, float) or not math.isfinite(used) or not 0 <= used <= 100:
                    continue
                minutes = window.get("window_minutes") or window.get("limit_window_minutes") or 0
                if type(minutes) is not int or not 0 <= minutes <= 525600:
                    continue
                reset = _timestamp(window.get("resets_at"))
                if window.get("resets_at") is not None and reset is None:
                    continue
                window_key = scoped + "|" + str(minutes) + ("|unknown_" + str(slot) if minutes == 0 else "")
                candidate = dict(model=scoped, window_key=window_key, window_minutes=minutes,
                                 used_percent=used, observed_at_ms=at, reset_at_ms=reset or 0)
                existing = next((w for w in evidence["windows"] if w["window_key"] == window_key), None)
                if existing is not None:
                    if existing["observed_at_ms"] <= at:
                        existing.update(candidate)
                elif len(evidence["windows"]) < 32:
                    evidence["windows"].append(candidate)
        except (ValueError, TypeError, AttributeError, RecursionError):
            continue


def private_home(evidence, home):
    """Use only the selected copied root; never follow worker-created symlinks."""
    from makewand.account_identity import _plain_identifier, account_ref
    from makewand.sandbox import _read_private_nofollow
    engine = evidence["engine"]
    if engine not in ("codex", "claude"):
        return
    # ProviderHome copies exactly one selected Codex/Claude root. Do not
    # re-resolve the host login here: its symlink may change during execution.
    root = None
    for cred in home.credentials:
        if engine == "claude" and cred.rel == ".credentials.json":
            root = Path(cred.private_root)
        if engine == "codex" and cred.rel == "auth.json":
            root = Path(cred.private_root)
    ref = None
    if home.identity_file is not None:
        found = _read_private_nofollow(*home.identity_file)
        if found[0] is not None:
            try:
                ref = account_ref(engine, _plain_identifier(found[0].decode(),
                    "tokens" if engine == "codex" else "oauthAccount",
                    "account_id" if engine == "codex" else "accountUuid"))
            except (ValueError, UnicodeError, RecursionError):
                pass
    if ref != evidence["account_ref"]:
        evidence["windows"] = []
    evidence.update(account_ref=ref or "", identity_known=bool(ref), pool_ref=ref + ":subscription" if ref else "")
    evidence["source"] = "private_home+cli_json" if "cli_json" in evidence["source"] else "private_home"
    if root is None:
        return
    count = 0
    for parent, dirs, files in os.walk(root / ("sessions" if engine == "codex" else "projects"), followlinks=False):
        count += 1
        if count > 256:
            return
        dirs[:] = [name for name in dirs if not (Path(parent) / name).is_symlink()]
        for name in files:
            count += 1
            if count > 256:
                return
            if not name.endswith(".jsonl"):
                continue
            found = _read_private_nofollow(str(root), str((Path(parent) / name).relative_to(root)))
            if found[0] is not None:
                protocol(evidence, found[0].decode("utf-8", errors="replace"))


def read_recent():
    """Small bounded reader used by quota binding; expired data is filtered there."""
    from makewand.accounting_files import pin
    try:
        paths = sorted(directory().glob("*.json"), key=lambda p: p.stat().st_mtime, reverse=True)[:64]
    except OSError:
        return
    for path in paths:
        try:
            if path.is_symlink():
                continue
            with pin(path) as files:
                fd = files.open(files.name)
                with os.fdopen(fd, "rb") as stream:
                    raw = stream.read(1024 * 1024 + 1)
            if len(raw) <= 1024 * 1024:
                value = json.loads(raw)
                if isinstance(value, dict) and value.get("schema") == 1:
                    yield value
        except (OSError, ValueError, TypeError):
            continue


def api_transport():
    """An API key does not establish a subscription account or quota pool."""
    from makewand.execution_runtime import current_context
    context = current_context()
    from makewand import call_budget
    request = context.get("_request")
    if request is not None:
        call_budget.api_lease(context.get("_attempt"), request.engine,
            budget_file=context.get("_budget_file"), max_model_calls=context.get("_max_model_calls"))
    evidence = current_context().get("_evidence")
    if evidence is not None:
        evidence.update(account_ref="", identity_known=False, pool_ref="", windows=[], tokens=None, source="api")
        evidence.pop("actual_model", None)
