"""
Makewand Dynamic Quota Pacing & Adaptive Effort Modulation Engine.
Aligns token/quota burn rate with the calendar progression of subscription cycles:
- Prevents early starvation (automatically throttles/downgrades effort when burning too fast)
- Prevents end-of-cycle waste (automatically upgrades models and maximizes reasoning depth when surplus quota is about to reset)
- Eliminates brittle static heuristics with dynamic pacing curves.
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
from makewand.health import load_status_cache, calculate_provider_quota
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
    clean_time = re.sub(r"\(.*?\)", "", s).strip().rstrip(".,")
    clean_time = re.sub(r"^(?:at|in)\s+", "", clean_time, flags=re.IGNORECASE).strip().rstrip(".,")
    for fmt in ("%I:%M %p", "%I %p", "%H:%M", "%I:%M%p", "%I%p"):
        try:
            t = datetime.strptime(clean_time, fmt).time()
            reset_dt = datetime.combine(now.date(), t)
            if reset_dt < now:
                reset_dt += timedelta(days=1)
            diff = (reset_dt - now).total_seconds()
            return max(0.0, diff)
        except Exception:
            continue

    return None


def calculate_dynamic_pacing(
    provider: str,
    info: Optional[Dict[str, Any]] = None,
    cache: Optional[Dict[str, Any]] = None
) -> Dict[str, Any]:
    """
    Computes real-time mathematical pacing for a provider:
    - delta = consumed_ratio - time_elapsed_ratio
    - Determines optimal tier, effort, and router score multiplier.
    """
    if info is None:
        if cache is None:
            cache = load_status_cache()
        info = cache.get(provider, {})

    quota_data = calculate_provider_quota(provider, info)
    percentage = quota_data.get("percentage", 100)
    status = quota_data.get("status", "unknown")
    resets_at = quota_data.get("resets_at")
    is_unlimited = quota_data.get("is_unlimited", False)

    # Hard-limited or broken (takes precedence over is_unlimited / agy / local)
    if status in ("limited", "needs_auth", "error", "disabled") or percentage <= 0:
        reset_hint = f" (解封时间: {resets_at})" if resets_at else ""
        return {
            "provider": provider,
            "pacing_state": PACING_LIMITED,
            "quota_percentage": 0,
            "recommended_tier": "fast",
            "recommended_effort": "low",
            "routing_boost": -999.0,
            "reason": f"{provider.upper()} 额度当前已耗尽/限流{reset_hint}，自动熔断避让",
            "delta": 1.0,
        }

    # Unlimited providers (local, agy base tier)
    if is_unlimited or provider in ("local", "agy"):
        return {
            "provider": provider,
            "pacing_state": PACING_BALANCED,
            "quota_percentage": 100,
            "recommended_tier": "deep" if provider == "agy" else "standard",
            "recommended_effort": "high",
            "routing_boost": 0.5 if provider == "agy" else 0.0,
            "reason": f"{provider.upper()} 算力充沛无硬限额，全天候平稳就绪",
            "delta": 0.0,
        }

    cycle_total = PROVIDER_DEFAULT_CYCLES.get(provider, CYCLE_7_DAYS)
    updated_at = quota_data.get("updated_at") or info.get("updated_at")
    seconds_left = parse_reset_time_to_seconds_left(resets_at, updated_at=updated_at)
    has_explicit_anchor = False

    if seconds_left is not None:
        has_explicit_anchor = True
        if seconds_left > cycle_total:
            cycle_total = max(cycle_total, seconds_left)
        time_elapsed_ratio = max(0.0, min(1.0, 1.0 - (seconds_left / cycle_total)))
    if not has_explicit_anchor:
        return {
            "provider": provider,
            "pacing_state": PACING_BALANCED,
            "quota_percentage": percentage,
            "recommended_tier": "standard",
            "recommended_effort": "high",
            "routing_boost": 0.0,
            "reason": f"{provider.upper()} 运行健康平稳 (自适应基准调步)",
            "delta": 0.0,
            "seconds_left": cycle_total * 0.5,
        }

    consumed_ratio = max(0.0, min(1.0, 1.0 - (percentage / 100.0)))
    delta = consumed_ratio - time_elapsed_ratio

    # 1. Harvest Condition: In the last 12% of cycle with explicit anchor confirmed, with >15% quota remaining
    is_harvest_window = has_explicit_anchor and (0 < seconds_left <= (0.12 * cycle_total))
    if is_harvest_window and percentage >= 15:
        hrs_left = round(seconds_left / 3600.0, 1)
        return {
            "provider": provider,
            "pacing_state": PACING_HARVEST,
            "quota_percentage": percentage,
            "recommended_tier": "deep",
            "recommended_effort": "max",
            "routing_boost": 3.0,
            "reason": f"⚡ {provider.upper()} 临界冲刺收割期 (重置仅剩 {hrs_left}h，尚余 {percentage}% 额度)：顶格启用旗舰模型与 max effort 深度推理，杜绝过期浪费！",
            "delta": delta,
            "seconds_left": seconds_left,
        }

    # 2. Under-burned Condition (Surplus Quota)
    if delta < -0.15:
        surplus_pct = int(abs(delta) * 100)
        return {
            "provider": provider,
            "pacing_state": PACING_UNDER_BURNED,
            "quota_percentage": percentage,
            "recommended_tier": "deep",
            "recommended_effort": "high",
            "routing_boost": 2.2,
            "reason": f"📈 {provider.upper()} 配额充裕富余 (消耗落后进度 {surplus_pct}%)：自动升档至 Deep 深度推理模式，榨取最大订阅价值",
            "delta": delta,
            "seconds_left": seconds_left,
        }

    # 3. Over-burned Condition (Burning Too Fast)
    if delta > 0.15:
        over_pct = int(delta * 100)
        return {
            "provider": provider,
            "pacing_state": PACING_OVER_BURNED,
            "quota_percentage": percentage,
            "recommended_tier": "fast",
            "recommended_effort": "low",
            "routing_boost": -2.0,
            "reason": f"🛡️ {provider.upper()} 消耗超前预警 (消耗超前进度 {over_pct}%)：自动降档为 Fast 轻量模型以防提前熔断，次要流量转移",
            "delta": delta,
            "seconds_left": seconds_left,
        }

    # 4. Balanced Condition
    return {
        "provider": provider,
        "pacing_state": PACING_BALANCED,
        "quota_percentage": percentage,
        "recommended_tier": "standard",
        "recommended_effort": "medium",
        "routing_boost": 0.0,
        "reason": f"✔ {provider.upper()} 处于匀速平衡态 (剩余 {percentage}%)：标准模型与标准推理深度平稳运行",
        "delta": delta,
        "seconds_left": seconds_left,
    }


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
