"""Account/model-scoped quota evidence shared by routing and CLI admission.

An unknown login directory is an isolation boundary, never proof of another
subscription. Local usage counts are deliberately absent from these decisions.
"""
import hashlib
import math
import os
import re
from pathlib import Path


def selected_scope(provider):
    from makewand.account_identity import account_identity
    identity = account_identity(provider)
    if identity is not None:
        return identity
    home = Path.home()
    roots = {"agy": home / ".gemini", "grok": home / ".grok",
             "muse": Path(os.environ.get("XDG_CONFIG_HOME") or home / ".config") / "muse"}
    root = roots.get(provider, home / ("." + provider)).expanduser().resolve()
    return {"credential_root": str(root), "account_ref": provider + ":unknown:" +
            hashlib.sha256(str(root).encode()).hexdigest()[:32],
            "identity_known": False, "pool_ref": None}


def model_matches(scope, model):
    """Only explicit exact IDs or named Claude families may match a sublimit."""
    if not scope:
        return True
    if not isinstance(scope, str) or not isinstance(model, str) or not model:
        return False
    scope, model = scope.lower(), re.sub(r"\[1m\]$", "", model.lower())
    if scope == model:
        return True
    if scope == "gemini":
        return model.startswith("gemini-")
    if scope in ("haiku", "sonnet", "opus", "fable"):
        return model.startswith("claude-" + scope + "-")
    return scope == model


def canonical_model(provider, model):
    if not isinstance(model, str):
        return model
    model = re.sub(r"\[1m\]$", "", model, flags=re.IGNORECASE)
    if provider == "agy":
        model = re.sub(r"-(?:minimal|low|medium|high|xhigh|max|ultra)$", "", model)
    return model


def merge_limits(previous, incoming):
    """Merge independent observations under the status file's existing lock."""
    from makewand.health import _written_at
    rows = {}
    previous = previous if isinstance(previous, list) else []
    incoming = incoming if isinstance(incoming, list) else []
    for entry in (*previous, *incoming):
        if not isinstance(entry, dict) or not isinstance(entry.get("quota_scope"), dict):
            continue
        scope = entry["quota_scope"]
        key = (scope.get("account_ref"), scope.get("credential_root"), scope.get("model"))
        if (not all(isinstance(value, str) for value in key[:2])
                or key[2] is not None and not isinstance(key[2], str)):
            continue
        at = _written_at(entry)
        if at is None:
            continue
        old = rows.get(key)
        if old is None or at >= _written_at(old):
            rows[key] = entry
    return sorted(rows.values(), key=_written_at)[-128:]


def projected_status(provider, info, model=None, identity=None):
    """Project a scoped refusal without changing its observation or other rows."""
    if not isinstance(info, dict):
        return info
    from makewand.health import is_reset_time_passed, status_age_seconds
    # This legacy message was emitted by the native Google quota parser. It
    # supplies no evidence about AGY's Claude pools. Preserve the observation
    # and its time, while ceasing to advertise a tool-wide refusal.
    legacy = info.get("legacy_native_limit")
    legacy_is_current = (provider == "agy" and info.get("status") == "limited"
                         and info.get("reason") == "Google AI 配额暂时耗尽"
                         and not info.get("quota_scope"))
    if legacy_is_current:
        legacy = {key: info.get(key) for key in ("status", "reason", "resets_at", "updated_at")}
    if provider == "agy" and isinstance(legacy, dict):
        result = dict(info)
        active = (status_age_seconds(legacy) is not None and
                  not is_reset_time_passed(legacy.get("resets_at"), legacy.get("updated_at", "")))
        if active:
            result["legacy_native_limit"] = legacy
        else:
            result.pop("legacy_native_limit", None)
        # Neutralize only the old unscoped Google verdict. Its retained
        # observation must never erase a later login, error, health or
        # unrelated tool-wide limit verdict during a cache read/save.
        if legacy_is_current:
            result.update(status="unknown", verified=False, resets_at=None,
                          reason="旧Google模型限流记录不作为AGY Claude模型的额度证据；具体余量未知")
        if (active and model_matches("gemini", model)
                and (legacy_is_current or info.get("status") == "unknown" or info.get("quota_scope"))):
            result.update(legacy)
            result.pop("quota_scope", None)
        info = result
    if not isinstance(info.get("quota_limits"), list):
        return info
    identity = identity or selected_scope(provider)
    matches = []
    for row in info["quota_limits"]:
        if not isinstance(row, dict) or row.get("status") != "limited":
            continue
        scope = row.get("quota_scope")
        if not isinstance(scope, dict) or scope.get("account_ref") != identity["account_ref"]:
            continue
        # Known account aliases share a pool; unknown directories do not.
        if not identity["identity_known"] and scope.get("credential_root") != identity["credential_root"]:
            continue
        if scope.get("model") and not model_matches(scope["model"], canonical_model(provider, model)):
            continue
        age = status_age_seconds(row)
        if age is None or is_reset_time_passed(row.get("resets_at"), row.get("updated_at", "")):
            continue
        matches.append(row)
    result = dict(info)
    if matches:
        from makewand.health import _written_at
        chosen = max(matches, key=_written_at)
        result.update({key: value for key, value in chosen.items() if key != "quota_limits"})
    elif info.get("quota_scope"):
        result.update(status="unknown", verified=False, resets_at=None,
                      reason="当前账号/模型没有有效的额度拒绝证据；余量未知")
        result.pop("quota_scope", None)
    return result


def refusal(provider, reason, reset, model=None):
    from datetime import datetime
    if provider == "agy" and reason == "Google AI 配额暂时耗尽" and model is None:
        model = "gemini"
    scope = dict(selected_scope(provider), model=canonical_model(provider, model))
    return {"status": "limited", "reason": reason, "resets_at": reset,
            "updated_at": datetime.now().isoformat(), "source": "dispatch",
            "quota_scope": scope}


def _number(value):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    try:
        return float(value) if math.isfinite(value) else None
    except OverflowError:
        return None


def quota_reason(provider, model=None, *, quota=None, info=None, policy=None):
    """The same deterministic quota gate for a route row and a CLI launch.

    Legacy numeric entries mean operator-supplied weekly usage, as before.
    Structured windows require current account binding and observation checks
    from the official reader; a model-specific window never blocks siblings.
    """
    from datetime import datetime, timezone
    from makewand import health
    if info is None:
        info = health.load_status_cache().get(provider, {})
    info = projected_status(provider, info, model)
    if isinstance(info, dict) and info.get("status") == "limited":
        return "当前账号/模型额度受限：" + str(info.get("reason") or "供应商拒绝")
    if policy is None:
        from makewand.route_select import load_policy
        policy = load_policy()
    weekly = getattr(policy, provider + "_max_weekly", None)
    if quota is None and provider in ("codex", "claude"):
        quota = health._get_official_subscription_quota(provider)
    if not isinstance(quota, dict):
        if quota is None or weekly is None:
            return None
        from makewand.route_select import _coerce_quota_entry, _format_percent
        used, reset = _coerce_quota_entry(quota)
        if used is not None and (used >= weekly or used >= 100):
            return f"周额度已用 {_format_percent(used)}%，达到阈值 {_format_percent(weekly)}%" + (f"（{reset}）" if reset else "")
        return None
    identity = selected_scope(provider)
    if (not identity["identity_known"] or quota.get("account_ref") != identity["account_ref"]
            or quota.get("pool_ref") != identity["pool_ref"]):
        return None
    reserve, _ = health._quota_reserve_setting()
    now = datetime.now(timezone.utc).timestamp()
    for window in quota.get("windows") or []:
        if (not isinstance(window, dict) or window.get("scope") == "unknown"
                or window.get("pool_ref") != identity["pool_ref"]
                or not model_matches(window.get("model"), model)):
            continue
        try:
            at = datetime.fromisoformat(window["observed_at"].replace("Z", "+00:00")).timestamp()
            if not math.isfinite(at) or not -60 <= now - at <= 900:
                continue
            reset = window.get("resets_at")
            if reset and datetime.fromisoformat(reset.replace("Z", "+00:00")).timestamp() <= now:
                continue
        except (KeyError, AttributeError, TypeError, ValueError, OverflowError, OSError):
            continue
        used = _number(window.get("used_percent"))
        if used is None or not 0 <= used <= 100:
            continue
        minutes = window.get("window_minutes")
        if used >= 100 or 100 - used < reserve:
            return f"当前账号/模型的{minutes or '未知'}分钟额度窗口已达保护阈值（已用 {used:g}%）"
        if minutes == 10080 and weekly is not None and used >= weekly:
            return f"周额度已用 {used:g}%，达到阈值 {weekly:g}%"
    return None


def before_cli_launch(provider, model, *, relay=None, cwd=None):
    """Record the CLI resolution; enforce quota or use an explicitly allowed relay."""
    from makewand.execution_runtime import current_context
    context = current_context()
    evidence = context.get("_evidence")
    if isinstance(model, str) and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:/\[\]-]{0,199}", model):
        if evidence is not None:
            evidence["resolved_model"] = model
        attempt = context.get("_attempt")
        if attempt is not None:
            from makewand import call_budget
            call_budget.note_resolved_model(attempt, model, budget_file=context.get("_budget_file"),
                                            max_model_calls=context.get("_max_model_calls"))
    reason = quota_reason(provider, model)
    if reason:
        if relay is not None and relay.available:
            return relay(f"[Makewand -> {provider}] {reason}；按已允许的 API 接力策略执行", cwd=cwd)
        return refuse_before_launch(reason)
    return None


def refuse_before_launch(reason):
    from makewand.execution_runtime import current_context
    invocation = current_context().get("_provider_invocation")
    if invocation is not None and not invocation.used:
        invocation.preflight_refused = True
    return False, None, reason
