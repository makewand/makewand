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


def _live_refusals(info, identity):
    """The stored refusals of this account that have not expired."""
    from makewand.health import is_reset_time_passed, status_age_seconds
    for row in info["quota_limits"]:
        if not isinstance(row, dict) or row.get("status") != "limited":
            continue
        scope = row.get("quota_scope")
        if not isinstance(scope, dict) or scope.get("account_ref") != identity["account_ref"]:
            continue
        # Known account aliases share a pool; unknown directories do not.
        if not identity["identity_known"] and scope.get("credential_root") != identity["credential_root"]:
            continue
        if status_age_seconds(row) is None or is_reset_time_passed(
                row.get("resets_at"), row.get("updated_at", ""), row.get("hold_seconds")):
            continue
        yield row


def refused_models(provider, info):
    """(model, reason) of each model this account's provider is currently refusing."""
    if not isinstance(info, dict) or not isinstance(info.get("quota_limits"), list):
        return []
    identity = selected_scope(provider)
    return [(row["quota_scope"]["model"], str(row.get("reason") or "供应商拒绝") + _hold_note(row))
            for row in _live_refusals(info, identity) if row["quota_scope"].get("model")]


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
    matches = [row for row in _live_refusals(info, identity)
               if not row["quota_scope"].get("model")
               or model_matches(row["quota_scope"]["model"], canonical_model(provider, model))]
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


def _names_no_reset(row):
    return row.get("resets_at") in (None, "", "待重置")


def hold_after_refusal(provider, entry):
    """How long this refusal holds when it names no reset time.

    A pool that refuses again right after its hold ended gets the next longer
    hold, so callers stop spending calls on a known-empty pool. A real success
    on that model, or a quiet day after the hold ended, starts over.
    """
    from datetime import datetime
    from makewand import health, usage
    holds = health.UNKNOWN_RESET_HOLDS
    if not _names_no_reset(entry):
        return holds[0]
    info = health.load_status_cache().get(provider)
    rows = info.get("quota_limits") if isinstance(info, dict) else None
    scope = entry["quota_scope"]
    key = (scope.get("account_ref"), scope.get("credential_root"), scope.get("model"))
    previous = next((row for row in reversed(rows if isinstance(rows, list) else [])
                     if isinstance(row, dict) and isinstance(row.get("quota_scope"), dict)
                     and (row["quota_scope"].get("account_ref"), row["quota_scope"].get("credential_root"),
                          row["quota_scope"].get("model")) == key), None)
    observed = health._written_at(previous) if previous is not None else None
    if observed is None or not _names_no_reset(previous):
        return holds[0]
    last = health.unknown_reset_hold(previous.get("hold_seconds"))
    elapsed = (datetime.now().astimezone() - observed).total_seconds()
    if elapsed < last:
        # A call that started before the hold, not a new try after it.
        return last
    if elapsed > last + 24 * 3600 or any(
            model_matches(scope.get("model"), canonical_model(provider, model))
            for model in usage.successes_since(provider, observed)):
        return holds[0]
    return holds[min(holds.index(last) + 1, len(holds) - 1)]


def _hold_note(info):
    """When a repeatedly refused pool comes back, since the provider did not say."""
    from datetime import timedelta
    from makewand import health
    hold = health.unknown_reset_hold(info.get("hold_seconds"))
    observed = health._written_at(info)
    if hold == health.UNKNOWN_RESET_HOLDS[0] or observed is None or not _names_no_reset(info):
        return ""
    return f"（连续被拒，约 {(observed + timedelta(seconds=hold)).strftime('%m-%d %H:%M')} 前不再派发）"


def _number(value):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    try:
        return float(value) if math.isfinite(value) else None
    except OverflowError:
        return None


def _reset_credit_redeemed_at(provider):
    """Quota evidence from before a redeemed Codex reset no longer describes the account."""
    if provider != "codex":
        return 0
    from makewand.codex_reset_credit import redeemed_at
    return redeemed_at(selected_scope(provider)["account_ref"])


def limit_cleared_by_reset(provider, info):
    """Whether this cached refusal predates a redeemed reset of the same account."""
    from datetime import datetime
    reset_at = _reset_credit_redeemed_at(provider)
    if not reset_at or not isinstance(info, dict):
        return False
    try:
        return datetime.fromisoformat(str(info.get("updated_at")).replace("Z", "+00:00")).timestamp() <= reset_at
    except (ValueError, OverflowError, OSError):
        return False


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
    reset_at = _reset_credit_redeemed_at(provider)
    if isinstance(info, dict) and info.get("status") == "limited" and not limit_cleared_by_reset(provider, info):
        return "当前账号/模型额度受限：" + str(info.get("reason") or "供应商拒绝") + _hold_note(info)
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
            # A redeemed reset answers for the weekly window it cleared, nothing else.
            if at <= reset_at and window.get("window_minutes") == 10080 and not window.get("model"):
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
    if reason and provider == "codex":
        from makewand.codex_reset_credit import restore_quota
        if restore_quota("before_launch"):
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
