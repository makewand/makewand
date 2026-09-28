"""
Makewand quota pacing and `--tier auto` resolution.

Scope, stated honestly:
- Calendar pacing (harvest / under-burned / over-burned) only activates when a
  provider reports an *official* remaining percentage ("NN% left") together
  with a reset anchor. The Python probes rarely record either, so in practice
  pacing is usually neutral and says so.
- Without official signals, `--tier auto` falls back to makewand's local
  call-count burn-rate estimate (heavy local burn -> "fast"; otherwise
  "standard") and the reason text labels it as an estimate, not real quota.
- Adjustments are continuous with a dead band around balanced consumption, so
  a tiny change of the inputs never flips the routing score by several points.
- The local burn-rate estimate is never converted back into a quota percentage
  here; the router applies it once as a bounded soft penalty (no double count).
- "unknown" (never probed) and stale verdicts are neutral, not exhausted.
"""

import os
import re
from datetime import datetime, timedelta
from typing import Dict, Any, Optional, Tuple

from makewand.config import (
    c,
    COLOR_BOLD,
    COLOR_GREEN,
    COLOR_YELLOW,
    COLOR_RED,
    COLOR_CYAN,
    COLOR_PURPLE,
    COLOR_RESET,
)
from makewand.health import load_status_cache, calculate_provider_quota, get_reauth_hint, STATUS_STALE_SECONDS
from makewand.discovery import get_provider_model_tier

# Pacing States
PACING_HARVEST = "harvest"          # Final 10% of cycle with surplus quota -> max effort harvest
PACING_UNDER_BURNED = "under_burned"# Consuming slower than time progression -> boost tier
PACING_BALANCED = "balanced"        # Consumption tracks time progression -> standard tier
PACING_OVER_BURNED = "over_burned"  # Consuming faster than time progression -> downgrade tier
PACING_LIMITED = "limited"          # Quota exhausted or account rate-limited

# Standard cycle durations (seconds)
CYCLE_5_HOURS = 5 * 3600
CYCLE_24_HOURS = 24 * 3600
CYCLE_7_DAYS = 7 * 24 * 3600

PROVIDER_DEFAULT_CYCLES = {
    "claude": CYCLE_7_DAYS,   # Primary weekly budget (with 5h burst window)
    "codex": CYCLE_7_DAYS,    # 3-account weekly pool
    "grok": CYCLE_24_HOURS,   # Daily allowance
    "muse": CYCLE_24_HOURS,   # Daily allowance
    "agy": CYCLE_24_HOURS,    # High capacity rolling window
}

# Continuous pacing curve (replaces the former bang-bang +-0.15 switch):
# |delta| <= DEAD_BAND -> no adjustment; linear ramp up to FULL_BAND; saturate.
PACING_DEAD_BAND = 0.05
PACING_FULL_BAND = 0.15
UNDER_BURN_MAX_BOOST = 2.2
OVER_BURN_MAX_PENALTY = -2.0
# Harvest: last 12% of the cycle with surplus. Ramps in over the first quarter of
# the window and between 10% and 20% remaining quota.
HARVEST_WINDOW_RATIO = 0.12
HARVEST_RAMP_RATIO = 0.03
HARVEST_MIN_PCT = 10.0
HARVEST_FULL_PCT = 20.0
HARVEST_MAX_BOOST = 3.0
# Tier changes only once the continuous adjustment is at least half saturated.
TIER_SWITCH_FRACTION = 0.5

AGY_DEFAULT_BOOST = 0.5
# agy loses its default preference once real dispatches mostly fail.
AGY_BONUS_MIN_SUCCESS_RATE = 0.5
# Without official quota signals, a local burn-rate estimate at least this
# severe makes `--tier auto` pick the cheaper tier.
AUTO_FAST_BURN_PENALTY = -2.5


def _ramp(value: float, start: float, full: float) -> float:
    if full <= start:
        return 1.0 if value >= full else 0.0
    return max(0.0, min(1.0, (value - start) / (full - start)))


def _neutral(provider: str, reason: str, percentage: Optional[int] = None, signal: str = "none",
             tier: str = "standard", effort: str = "high") -> Dict[str, Any]:
    return {
        "provider": provider,
        "pacing_state": PACING_BALANCED,
        "quota_percentage": percentage,
        "recommended_tier": tier,
        "recommended_effort": effort,
        "routing_boost": 0.0,
        "reason": reason,
        "delta": 0.0,
        "signal": signal,
    }


def _agy_reliability() -> Tuple[Optional[float], float]:
    try:
        from makewand.usage import get_engine_reliability
        rate, weight, _ = get_engine_reliability("agy")
        return rate, weight
    except Exception:
        return None, 0.0


def _local_burn_penalty(provider: str) -> Tuple[float, Optional[str]]:
    try:
        from makewand.usage import get_burn_rate_penalty
        return get_burn_rate_penalty(provider)
    except Exception:
        return 0.0, None


def parse_reset_time_to_seconds_left(resets_at_str: Optional[str], updated_at: Optional[str] = None) -> Optional[float]:
    """
    Parses resets_at string (e.g., '2026-09-28 07:53', '10:58 (2026-09-27)', 'Sep 27th, 2026 10:58 AM', '8pm (Asia/Shanghai)', 'in 2 hours')
    into seconds remaining from now, subtracting elapsed time if updated_at is provided.
    """
    if not resets_at_str or not isinstance(resets_at_str, str):
        return None

    s = resets_at_str.strip()
    now = datetime.now()

    elapsed = 0.0
    if updated_at:
        try:
            clean_up = updated_at.replace("Z", "+00:00") if updated_at.endswith("Z") else updated_at
            up_dt = datetime.fromisoformat(clean_up)
            now_comp = datetime.now(up_dt.tzinfo) if up_dt.tzinfo is not None else now
            elapsed = max(0.0, (now_comp - up_dt).total_seconds())
        except Exception:
            elapsed = 0.0

    # Pattern 1: ISO or standard YYYY-MM-DD HH:MM (with timezone support)
    try:
        clean_s = s.replace("Z", "+00:00")
        dt = datetime.fromisoformat(clean_s)
        now_tz = datetime.now(dt.tzinfo) if dt.tzinfo is not None else now
        diff = (dt - now_tz).total_seconds()
        return max(0.0, diff)
    except Exception:
        pass

    m = re.search(r"(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2})(?::(\d{2}))?", s)
    if m:
        try:
            year, month, day, hour, minute = int(m.group(1)), int(m.group(2)), int(m.group(3)), int(m.group(4)), int(m.group(5))
            second = int(m.group(6)) if m.group(6) else 0
            dt = datetime(year, month, day, hour, minute, second)
            diff = (dt - now).total_seconds()
            return max(0.0, diff)
        except Exception:
            pass

    # Pattern 2: HH:MM (YYYY-MM-DD)
    m2 = re.search(r"(\d{1,2}):(\d{2})\s*\((?:.+)?(\d{4})-(\d{2})-(\d{2})\)", s)
    if m2:
        try:
            hour, minute, year, month, day = int(m2.group(1)), int(m2.group(2)), int(m2.group(3)), int(m2.group(4)), int(m2.group(5))
            dt = datetime(year, month, day, hour, minute)
            diff = (dt - now).total_seconds()
            return max(0.0, diff)
        except Exception:
            pass

    # Pattern 3: Month-day date support (e.g. "Sep 27th, 2026 10:58 AM", "Oct 3 at 2pm")
    date_match = re.search(r"([A-Za-z]{3,9})\s+(\d{1,2})(?:st|nd|rd|th)?(?:\s*,\s*(\d{4}))?(?:\s+at\s+|\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?", s, re.IGNORECASE)
    if date_match:
        try:
            month_str = date_match.group(1).capitalize()[:3]
            day_val = int(date_match.group(2))
            year_val = int(date_match.group(3)) if date_match.group(3) else now.year
            hour_val = int(date_match.group(4))
            min_val = int(date_match.group(5)) if date_match.group(5) else 0
            ampm = date_match.group(6).lower() if date_match.group(6) else ""
            if ampm == "pm" and hour_val < 12:
                hour_val += 12
            elif ampm == "am" and hour_val == 12:
                hour_val = 0
            months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"]
            if month_str in months:
                month_val = months.index(month_str) + 1
                reset_dt = datetime(year_val, month_val, day_val, hour_val, min_val)
                diff = (reset_dt - now).total_seconds()
                return max(0.0, diff)
        except Exception:
            pass

    # Pattern 4: Relative hours/minutes e.g. "3h 20m" or "45m" or "in 2 hours"
    m_hours = re.search(r"(\d+(?:\.\d+)?)\s*(?:h|hr|hours?|小时)", s, re.IGNORECASE)
    m_mins = re.search(r"(\d+)\s*(?:m|min|minutes?|分钟)", s, re.IGNORECASE)
    if m_hours or m_mins:
        secs = 0.0
        if m_hours:
            secs += float(m_hours.group(1)) * 3600
        if m_mins:
            secs += float(m_mins.group(1)) * 60
        secs = max(0.0, secs - elapsed)
        return secs

    # Pattern 5: Time of day with stripped timezone e.g. "8pm (Asia/Shanghai)", "10:58 AM", "at 2:00 PM"
    # The clock time refers to the first such moment *after the observation*
    # (updated_at), exactly like health.is_reset_time_passed. Anchoring on
    # "now" instead would push an already-passed reset to tomorrow.
    clean_time = re.sub(r"\(.*?\)", "", s).strip().rstrip(".,")
    clean_time = re.sub(r"^(?:at|in)\s+", "", clean_time, flags=re.IGNORECASE).strip().rstrip(".,")
    anchor = None
    if updated_at:
        try:
            clean_up = updated_at.replace("Z", "+00:00") if updated_at.endswith("Z") else updated_at
            anchor = datetime.fromisoformat(clean_up)
            if anchor.tzinfo is not None:
                anchor = anchor.astimezone().replace(tzinfo=None)
        except Exception:
            anchor = None
    for fmt in ("%I:%M %p", "%I %p", "%H:%M", "%I:%M%p", "%I%p"):
        try:
            t = datetime.strptime(clean_time, fmt).time()
        except Exception:
            continue
        if anchor is not None:
            reset_dt = datetime.combine(anchor.date(), t)
            if reset_dt < anchor:
                reset_dt += timedelta(days=1)
        else:
            reset_dt = datetime.combine(now.date(), t)
            if reset_dt < now:
                reset_dt += timedelta(days=1)
        return max(0.0, (reset_dt - now).total_seconds())

    return None


def calculate_dynamic_pacing(
    provider: str,
    info: Optional[Dict[str, Any]] = None,
    cache: Optional[Dict[str, Any]] = None
) -> Dict[str, Any]:
    """
    Computes the pacing verdict for a provider:
    - hard exclusion (-999) only for confirmed limited / needs_auth / error / disabled / missing
    - neutral for unknown or stale status
    - calendar pacing only with an official remaining percentage and reset anchor,
      as a continuous curve with a dead band (delta = consumed_ratio - elapsed_ratio)
    - otherwise a local burn-rate estimate may lower `--tier auto` to "fast"
    """
    if info is None:
        if cache is None:
            cache = load_status_cache()
        info = cache.get(provider, {})

    quota_data = calculate_provider_quota(provider, info)
    percentage = quota_data.get("percentage")
    status = quota_data.get("status", "unknown")
    source = quota_data.get("source")
    resets_at = quota_data.get("resets_at")
    is_unlimited = quota_data.get("is_unlimited", False)

    # Confirmed hard limits / broken or unavailable providers.
    official_empty = source == "official" and percentage is not None and percentage <= 0
    if status in ("limited", "needs_auth", "error", "disabled", "missing") or official_empty:
        reset_hint = f" (解封时间: {resets_at})" if resets_at else ""
        if status == "needs_auth":
            reason = f"{provider.upper()} 未通过登录校验，暂不派发：{get_reauth_hint(provider)}"
        elif status == "error":
            reason = f"{provider.upper()} 最近一次探测/派发异常，暂不派发（异常状态有效期过后自动恢复为中性，可运行 'makewand probe' 立即复查）"
        elif status in ("disabled", "missing"):
            reason = f"{provider.upper()} 未启用或未安装"
        else:
            reason = f"{provider.upper()} 额度当前已耗尽/限流{reset_hint}，自动熔断避让"
        return {
            "provider": provider,
            "pacing_state": PACING_LIMITED,
            "quota_percentage": 0,
            "recommended_tier": "fast",
            "recommended_effort": "low",
            "routing_boost": -999.0,
            "reason": reason,
            "delta": 1.0,
            "signal": "status",
        }

    # Never probed, expired failure verdict, or stale healthy verdict: neutral.
    if status == "unknown":
        return _neutral(provider, f"{provider.upper()} 状态未知 (未探测或旧状态已过期)，按中性处理；建议运行 'makewand probe'")
    if quota_data.get("stale"):
        return _neutral(provider, f"{provider.upper()} 状态缓存已超过 {STATUS_STALE_SECONDS // 3600} 小时未刷新，按中性处理；建议运行 'makewand probe'",
                        percentage=percentage)

    if provider == "agy":
        rate, weight = _agy_reliability()
        if rate is not None and rate < AGY_BONUS_MIN_SUCCESS_RATE:
            return _neutral(provider, f"AGY 近期真实派发成功率仅 {rate:.0%} (有效样本 {weight:g})，取消默认 +{AGY_DEFAULT_BOOST} 偏好加成",
                            percentage=percentage, signal="reliability")
        return {
            "provider": provider,
            "pacing_state": PACING_BALANCED,
            "quota_percentage": percentage,
            "recommended_tier": "deep",
            "recommended_effort": "high",
            "routing_boost": AGY_DEFAULT_BOOST,
            "reason": f"AGY 无额度查询信号 (未做估算)，保留默认 +{AGY_DEFAULT_BOOST} 偏好；可用性以真实派发结果为准",
            "delta": 0.0,
            "signal": "none",
        }
    if is_unlimited or provider == "local":
        return _neutral(provider, f"{provider.upper()} 无云端额度约束", percentage=percentage)

    cycle_total = PROVIDER_DEFAULT_CYCLES.get(provider, CYCLE_7_DAYS)
    updated_at = quota_data.get("updated_at") or info.get("updated_at")
    seconds_left = parse_reset_time_to_seconds_left(resets_at, updated_at=updated_at) if resets_at else None
    has_official_pct = source == "official" and percentage is not None

    if not has_official_pct or seconds_left is None:
        # No official quota signal: no calendar pacing and no routing boost (the
        # router already applies the burn-rate estimate once). The estimate may
        # only lower `--tier auto` to the cheaper tier, and says so.
        pen, _ = _local_burn_penalty(provider)
        if pen <= AUTO_FAST_BURN_PENALTY:
            return _neutral(
                provider,
                f"{provider.upper()} 无官方额度/重置时间信号；本地调用计数估算显示消耗偏快 ({pen})，"
                "tier=auto 降为 fast (估算，非真实配额)",
                percentage=percentage, signal="local_estimate", tier="fast", effort="low")
        return _neutral(
            provider,
            f"{provider.upper()} 无官方额度/重置时间信号，未进行动态调步；tier=auto 使用 standard",
            percentage=percentage, signal="none")

    if seconds_left > cycle_total:
        cycle_total = seconds_left
    time_elapsed_ratio = max(0.0, min(1.0, 1.0 - (seconds_left / cycle_total)))
    consumed_ratio = max(0.0, min(1.0, 1.0 - (percentage / 100.0)))
    delta = consumed_ratio - time_elapsed_ratio
    base = {
        "provider": provider,
        "quota_percentage": percentage,
        "delta": delta,
        "seconds_left": seconds_left,
        "signal": "official",
    }

    strength = _ramp(abs(delta), PACING_DEAD_BAND, PACING_FULL_BAND)
    under_boost = UNDER_BURN_MAX_BOOST * strength if delta < 0 else 0.0

    # 1. Harvest: last part of the cycle with surplus quota (continuous ramp).
    # Both harvest and under-burn signal surplus; take the larger so that the
    # hand-over between the two is continuous as well.
    window = HARVEST_WINDOW_RATIO * cycle_total
    if 0 < seconds_left <= window:
        w_time = _ramp(window - seconds_left, 0.0, HARVEST_RAMP_RATIO * cycle_total)
        w_pct = _ramp(float(percentage), HARVEST_MIN_PCT, HARVEST_FULL_PCT)
        harvest_boost = HARVEST_MAX_BOOST * w_time * w_pct
        if harvest_boost > 0 and harvest_boost >= under_boost:
            boost = round(harvest_boost, 3)
            hrs_left = round(seconds_left / 3600.0, 1)
            strong = boost >= HARVEST_MAX_BOOST * TIER_SWITCH_FRACTION
            return dict(base, **{
                "pacing_state": PACING_HARVEST,
                "recommended_tier": "deep" if strong else "standard",
                "recommended_effort": "max" if strong else "high",
                "routing_boost": boost,
                "reason": f"⚡ {provider.upper()} 临近重置 (剩 {hrs_left}h) 仍余 {percentage}% 官方额度：加权 +{boost}，优先使用剩余额度",
            })

    # 2./3. Under- or over-burned relative to the calendar (dead band + ramp).
    if delta < 0 and strength > 0:
        boost = round(under_boost, 3)
        strong = strength >= TIER_SWITCH_FRACTION
        return dict(base, **{
            "pacing_state": PACING_UNDER_BURNED,
            "recommended_tier": "deep" if strong else "standard",
            "recommended_effort": "high",
            "routing_boost": boost,
            "reason": f"📈 {provider.upper()} 官方额度消耗落后进度 {int(abs(delta) * 100)}%：加权 +{boost}",
        })
    if delta > 0 and strength > 0:
        boost = round(OVER_BURN_MAX_PENALTY * strength, 3)
        strong = strength >= TIER_SWITCH_FRACTION
        return dict(base, **{
            "pacing_state": PACING_OVER_BURNED,
            "recommended_tier": "fast" if strong else "standard",
            "recommended_effort": "low" if strong else "medium",
            "routing_boost": boost,
            "reason": f"🛡️ {provider.upper()} 官方额度消耗超前进度 {int(delta * 100)}%：降权 {boost}",
        })

    # 4. Balanced (inside the dead band)
    return dict(base, **{
        "pacing_state": PACING_BALANCED,
        "recommended_tier": "standard",
        "recommended_effort": "medium",
        "routing_boost": 0.0,
        "reason": f"✔ {provider.upper()} 官方额度与周期进度基本匹配 (剩余 {percentage}%)",
    })


def get_all_providers_pacing(cache: Optional[Dict[str, Any]] = None) -> Dict[str, Dict[str, Any]]:
    """Evaluates pacing across all five core subscription engines."""
    if cache is None:
        cache = load_status_cache()

    providers = ["claude", "codex", "grok", "muse", "agy"]
    results = {}
    for p in providers:
        results[p] = calculate_dynamic_pacing(p, cache.get(p, {}), cache=cache)
    return results


def resolve_dynamic_tier_and_effort(
    provider: str,
    requested_tier: str = "auto",
    cache: Optional[Dict[str, Any]] = None
) -> Tuple[str, str, str]:
    """
    Resolves the final (effective_tier, model_name, effort) for a provider execution.
    If requested_tier == 'auto', adapts tier based on pacing.
    Returns: (tier, model_name, effort)
    """
    pacing = calculate_dynamic_pacing(provider, cache=cache)
    if requested_tier == "auto" or not requested_tier:
        effective_tier = pacing.get("recommended_tier", "standard")
    else:
        effective_tier = requested_tier

    resolved = get_provider_model_tier(provider, effective_tier)
    model_name = resolved.get("model", "default")
    # If pacing mandates max effort during harvest, elevate effort
    if pacing.get("pacing_state") == PACING_HARVEST and effective_tier == "deep":
        effort = "max"
    else:
        effort = resolved.get("effort", pacing.get("recommended_effort", "medium"))

    return effective_tier, model_name, effort


def describe_auto_tier_signal(cache: Optional[Dict[str, Any]] = None,
                              pacings: Optional[Dict[str, Dict[str, Any]]] = None) -> str:
    """
    One-line, honest description of what `--tier auto` does with the signals
    available right now (for UI text such as the pipeline stage header).
    """
    if pacings is None:
        try:
            pacings = get_all_providers_pacing(cache=cache)
        except Exception:
            pacings = {}
    official = sorted(p for p, d in pacings.items() if d.get("signal") == "official")
    if official:
        return f"auto (按官方额度信号调步: {', '.join(official)})"
    estimated = sorted(p for p, d in pacings.items() if d.get("signal") == "local_estimate")
    if estimated:
        return f"auto (无官方额度信号；按本地调用计数估算将 {', '.join(estimated)} 降为 fast，其余 standard)"
    return "auto (无官方额度信号，未做动态调步：按 standard 执行)"
