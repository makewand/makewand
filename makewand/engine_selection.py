"""
Makewand Candidate Selection & Multi-Model Engine Pairing:
Intelligent scoring, domain affinity, pacing alignment, reliability weighting, and judge selection.
"""

import re
import sys
from typing import Optional, Tuple, List, Dict, Any

import makewand.config as config
from makewand.config import (
    c,
    COLOR_BOLD,
    COLOR_RED,
    COLOR_YELLOW,
)
from makewand.task_admission import classify_prompt_intent

# ---------------------------------------------------------------------------
# Routing penalty math (bounded, continuous).
# ---------------------------------------------------------------------------
BURN_PENALTY_FULL_SCALE = 4.0
BURN_PENALTY_MIN_FACTOR = 0.45
RELIABILITY_GOOD_RATE = 0.8
RELIABILITY_BAD_RATE = 0.2
RELIABILITY_MIN_FACTOR = 0.5
ENGINES_WITHOUT_EXECUTOR = frozenset({"cursor", "copilot"})
NON_AGENTIC_CHAT_MODELS = frozenset({"local", "ollama", "deepseek", "qwen", "glm", "kimi", "openrouter", "siliconflow"})

_UNUSABLE_ENGINE_STATUSES = ("limited", "needs_auth", "missing", "disabled")
_RACE_JUDGE_ORDER = ("agy", "codex", "claude", "grok", "muse", "local")


def _match_domain_keywords(keywords: List[str], text: str) -> List[str]:
    """
    Matches keywords against text.
    For ASCII/English keywords (containing alphanumeric/hyphen/underscore/plus), enforces word boundaries.
    For non-ASCII / CJK keywords, uses substring inclusion.
    """
    matched = []
    for k in keywords:
        if re.match(r'^[a-zA-Z0-9_\-\+]+$', k):
            pattern = r'(?<![a-zA-Z0-9_])' + re.escape(k) + r'(?![a-zA-Z0-9_])'
            if re.search(pattern, text, re.IGNORECASE):
                matched.append(k)
        else:
            if k in text:
                matched.append(k)
    return matched


def burn_rate_factor(pen: float) -> float:
    """Multiplier in [BURN_PENALTY_MIN_FACTOR, 1] for a burn-rate penalty <= 0."""
    try:
        pen = float(pen)
    except (TypeError, ValueError):
        return 1.0
    if pen != pen or pen >= 0.0:  # NaN or no penalty
        return 1.0
    severity = min(1.0, -pen / BURN_PENALTY_FULL_SCALE)
    return max(BURN_PENALTY_MIN_FACTOR, 1.0 - (1.0 - BURN_PENALTY_MIN_FACTOR) * severity)


def apply_burn_rate_penalty(score: float, pen: float) -> float:
    """Bounded soft down-weighting: score*MIN_FACTOR <= result <= score for score > 0."""
    if score <= 0:
        return score
    return score * burn_rate_factor(pen)


def reliability_factor(rate: Optional[float]) -> float:
    """Multiplier in [RELIABILITY_MIN_FACTOR, 1]; None (insufficient evidence) is neutral."""
    if rate is None:
        return 1.0
    try:
        rate = float(rate)
    except (TypeError, ValueError):
        return 1.0
    if rate != rate:
        return 1.0
    if rate >= RELIABILITY_GOOD_RATE:
        return 1.0
    if rate <= RELIABILITY_BAD_RATE:
        return RELIABILITY_MIN_FACTOR
    span = (rate - RELIABILITY_BAD_RATE) / (RELIABILITY_GOOD_RATE - RELIABILITY_BAD_RATE)
    return RELIABILITY_MIN_FACTOR + (1.0 - RELIABILITY_MIN_FACTOR) * span


def _engine_usable(engine: str, cache: Optional[Dict[str, Any]], require_healthy: bool = False) -> Tuple[bool, str]:
    """An engine may be selected only if the user has not disabled it and its cached health allows it."""
    orch = sys.modules.get("makewand.orchestrator")
    if orch is not None:
        custom = getattr(orch, "_engine_usable", None)
        if custom is not None and custom is not _engine_usable:
            return custom(engine, cache, require_healthy=require_healthy)
    from makewand.config import is_provider_enabled
    if not is_provider_enabled(engine):
        return False, f"已被用户禁用 (makewand enable {engine} 可重新开启)"
    status = ((cache or {}).get(engine) or {}).get("status")
    if require_healthy and status != "healthy":
        return False, f"健康状态为 {status or 'unknown'}"
    if status in _UNUSABLE_ENGINE_STATUSES:
        return False, f"健康状态为 {status}"
    return True, ""


def _select_race_judge(cache: Optional[Dict[str, Any]], contestants: Tuple[Optional[str], ...]) -> Optional[str]:
    """Antigravity first; otherwise an enabled/usable non-contestant, and only then a contestant (blind A/B)."""
    orch = sys.modules.get("makewand.orchestrator")
    if orch is not None:
        custom = getattr(orch, "_select_race_judge", None)
        if custom is not None and custom is not _select_race_judge:
            return custom(cache, contestants)
    usable_fn = getattr(orch, "_engine_usable", None) if orch else None
    if usable_fn is None or usable_fn is _engine_usable:
        usable_fn = _engine_usable
    usable = [e for e in _RACE_JUDGE_ORDER if usable_fn(e, cache)[0]]
    if "agy" in usable:
        return "agy"
    taken = {e for e in contestants if e}
    for e in usable:
        if e not in taken:
            return e
    return usable[0] if usable else None


def _no_provider_detected(forced_engine: Optional[str], route_meta: Dict[str, Any]) -> bool:
    """True when routing fell back to a placeholder engine with zero active providers (N=0)."""
    if forced_engine and forced_engine != "auto":
        return False
    if route_meta.get("no_active_providers"):
        return True
    if not route_meta.get("single_tool_mode"):
        return False
    try:
        from makewand.config import get_active_providers
        return not get_active_providers()
    except Exception:
        return False


def _print_no_provider_guidance() -> None:
    print(c("❌ [Makewand Setup] 未检测到任何可用的 AI 编码工具 (0 个)，任务未执行，工作区未做任何改动。", COLOR_RED + COLOR_BOLD))
    print("   接入引导：")
    print("   • 安装并登录任一订阅 CLI：claude / codex / agy (Gemini) / grok / muse；")
    print("   • 或配置 API Key（如 ANTHROPIC_API_KEY / OPENAI_API_KEY），并设置 MAKEWAND_API_POLICY=allow_paid 允许按量计费；")
    print("   • 或启用本地模型：makewand enable local（需本机 Ollama / vLLM）。")
    print("   完成后运行 makewand status 查看检测结果。")


def select_optimal_engine_pair(
    prompt: str,
    tier: str = "standard",
    cache: Optional[Dict[str, Any]] = None,
    boost: bool = False,
    require_file_editing: Optional[bool] = None
) -> Tuple[List[str], List[str], Dict[str, Any]]:
    """
    Intelligently scores and pairs engines for (Implementation, Red-team Review)
    based on task domain affinity and quota window dynamics.
    Supports user forced overclocking (--boost) and low-usage performance harvesting.
    Returns: (ordered_coders, ordered_reviewers, meta_info)
    """
    orch = sys.modules.get("makewand.orchestrator")
    if orch is not None:
        custom = getattr(orch, "select_optimal_engine_pair", None)
        if custom is not None and custom is not select_optimal_engine_pair:
            return custom(prompt, tier=tier, cache=cache, boost=boost, require_file_editing=require_file_editing)

    from makewand.config import normalize_tier
    if cache is None:
        status_fn = getattr(orch, "get_or_update_status", None) if orch else None
        if status_fn is None:
            from makewand.health import get_or_update_status
            status_fn = get_or_update_status
        cache = status_fn(force_probe=False)

    p_lower = prompt.lower()
    reasons = []

    if boost:
        tier = "deep"
        reasons.append("⚡ [Boost Overclock] 用户显式启用强制超频模式：穿透所有软削峰与限流惩罚，全力调度最强旗舰模型！")
    else:
        tier = normalize_tier(tier)

    # Base scores:
    # Claude: primary general software development & engineering (2.0)
    # Codex: short rolling window (3-4h resets) & red-team specialist (1.8)
    # Grok: high-reasoning frontier models & rapid agile coding (1.7)
    # Antigravity: continuous high-capacity reasoning anchor (1.4)
    # Muse: secondary alternative (0.8)
    scores = {
        "claude": 2.0,
        "codex": 1.8,
        "grok": 1.7,
        "deepseek": 1.6,
        "aider": 1.5,
        "agy": 1.4,
        "qwen": 1.3,
        "local": 1.2,
        "openrouter": 1.1,
        "siliconflow": 1.1,
        "cursor": 1.0,
        "glm": 0.9,
        "kimi": 0.9,
        "muse": 0.8,
        "copilot": 0.7
    }

    # 1. Semantic Domain Keywords
    # Local offline / private model affinity
    local_keywords = ["本地", "local", "私有", "离线", "offline", "免费", "ollama", "vllm"]
    matched_local = _match_domain_keywords(local_keywords, p_lower)
    if matched_local:
        scores["local"] += 3.0
        reasons.append(f"命中本地私有模型偏好 ({', '.join(matched_local[:3])}) -> Local 模型大幅加权")

    # Algorithmic, Concurrency, Low-level, Security -> Codex affinity
    algo_keywords = [
        "算法", "algorithm", "leetcode", "二叉树", "binary tree", "动态规划", "dynamic programming",
        "排序", "sort", "图论", "graph", "hash", "哈希", "并发", "concurrency", "死锁", "deadlock",
        "race condition", "竞态", "mutex", "channel", "goroutine", "thread", "asyncio", "锁",
        "性能", "benchmark", "优化", "optimize", "内存", "memory leak", "底层", "kernel",
        "protocol", "协议", "socket", "tcp", "udp", "汇编", "assembly", "c++", "rust",
        "unsafe", "位运算", "bitwise", "逆向", "reverse engineering"
    ]
    matched_algo = _match_domain_keywords(algo_keywords, p_lower)
    if matched_algo:
        scores["codex"] += 2.5
        reasons.append(f"命中算法与底层并发特征 ({', '.join(matched_algo[:3])}) -> Codex 专精大幅加权")

    # Deep reasoning, Logic exploration, Prototyping, xAI -> Grok affinity
    grok_keywords = [
        "grok", "xai", "探索", "头脑风暴", "推演", "数学", "math", "快速原型", "prototype",
        "mock", "演进", "前沿", "高并发设计", "多维分析", "因果", "逻辑"
    ]
    matched_grok = _match_domain_keywords(grok_keywords, p_lower)
    if matched_grok:
        scores["grok"] += 2.5
        reasons.append(f"命中深度推理与逻辑探索特征 ({', '.join(matched_grok[:3])}) -> Grok 专精大幅加权")

    # Refactoring, UI/Frontend, Web, Docs, Types -> Claude affinity
    refactor_keywords = [
        "重构", "refactor", "整理", "clean code", "rename", "拆分", "前端", "frontend",
        "react", "vue", "svelte", "nextjs", "component", "组件", "css", "html", "tailwind",
        "ui", "ux", "web", "django", "fastapi", "flask", "express", "spring", "crud",
        "rest", "api", "endpoint", "controller", "view", "typescript", "ts", "文档",
        "docstring", "readme", "markdown", "unittest", "pytest", "mock", "测试用例"
    ]
    matched_refactor = _match_domain_keywords(refactor_keywords, p_lower)
    if matched_refactor:
        scores["claude"] += 2.5
        reasons.append(f"命中工程重构/前端/框架特征 ({', '.join(matched_refactor[:3])}) -> Claude 专精大幅加权")

    # Architecture, Full repo, Monorepo, Global Plan -> Antigravity affinity
    arch_keywords = [
        "全仓", "跨项目", "全局架构", "architecture", "跨模块", "system design", "总体设计",
        "全链路", "全工程", "monorepo", "超长上下文", "long context", "综合分析", "技术选型",
        "方案对比", "tradeoff", "可行性"
    ]
    matched_arch = _match_domain_keywords(arch_keywords, p_lower)
    if matched_arch:
        scores["agy"] += 3.0
        reasons.append(f"命中全局架构/跨模块/全仓设计特征 ({', '.join(matched_arch[:3])}) -> Antigravity 架构师加权")

    # Tier adjustments
    if tier == "deep":
        scores["codex"] += 0.8
        scores["agy"] += 0.8
        scores["grok"] += 0.8
    elif tier == "fast":
        scores["claude"] += 0.8
        scores["grok"] += 0.5

    # Low-usage Performance Harvesting (Surplus Milking Bonus)
    try:
        from makewand.usage import get_engine_usage_stats
        usage_stats = get_engine_usage_stats(window_hours=24.0)
        for m in ["codex", "claude", "grok", "muse"]:
            if scores.get(m, 0) > 0 and usage_stats.get(m, {}).get("total", 0) <= 2:
                scores[m] += 0.5
                reasons.append(f"{m.upper()} 过去 24 小时处于低频空闲窗口，增加低频性能榨取放量加权 (+0.5)")
    except Exception:
        pass

    # 2. Sliding Window Quota Burn-Rate Adjustment (bounded soft down-weighting)
    try:
        from makewand.usage import get_burn_rate_penalty
        is_explain_query = (classify_prompt_intent(prompt) == "explain")
        for model_name in list(scores.keys()):
            if scores[model_name] > 0:
                pen, pen_reason = get_burn_rate_penalty(model_name)
                if pen != 0.0:
                    if boost or is_explain_query:
                        if pen_reason and boost:
                            reasons.append(f"{model_name.upper()} {pen_reason} [已由用户 --boost 强制穿透豁免]")
                        pen = 0.0
                    else:
                        suffix = ""
                        if tier == "deep" and pen > -2.5:
                            pen *= 0.5
                            suffix = " [已触发 Deep 穿透豁免减半]"
                        factor = burn_rate_factor(pen)
                        scores[model_name] = apply_burn_rate_penalty(scores[model_name], pen)
                        if pen_reason:
                            reasons.append(f"{pen_reason}{suffix} -> 软降权 ×{factor:.2f} (有界，不排除该引擎)")
    except Exception:
        pass

    # 2.5 Dynamic Quota Pacing & Calendar Progression Alignment
    pacings = {}
    try:
        from makewand.pacing import get_all_providers_pacing
        pacings = get_all_providers_pacing(cache=cache)
        for model_name, p_data in pacings.items():
            if model_name in scores and scores[model_name] > -500:
                p_boost = p_data.get("routing_boost", 0.0)
                if p_boost != 0.0 and not boost:
                    scores[model_name] += p_boost
                p_reason = p_data.get("reason")
                if p_reason and (p_data.get("pacing_state") in ("harvest", "under_burned", "over_burned")
                                 or p_data.get("signal") == "reliability"):
                    reasons.append(p_reason)
    except Exception:
        pass

    if tier == "auto":
        try:
            from makewand.pacing import describe_auto_tier_signal
            reasons.append(f"tier={describe_auto_tier_signal(cache, pacings=pacings or None)}")
        except Exception:
            pass

    # 3. Quota Health & Active Tool Filter
    from makewand.config import has_api_configured, is_provider_enabled, get_active_providers
    active_pool = set(get_active_providers())
    _usable_fn = getattr(orch, "_engine_usable", None) if orch else None
    if _usable_fn is None or _usable_fn is _engine_usable:
        _usable_fn = _engine_usable

    from makewand.health import get_reauth_hint, is_status_stale
    stale_engines = []
    for model_name in list(scores.keys()):
        if not is_provider_enabled(model_name):
            scores[model_name] = -999.0
            reasons.append(f"{model_name} 已由用户在配置中手动禁用 (disabled)")
            continue
        if model_name not in active_pool:
            scores[model_name] = -999.0
            continue
        if model_name in ENGINES_WITHOUT_EXECUTOR:
            scores[model_name] = -999.0
            reasons.append(f"{model_name} 已检测到但暂无执行适配，不参与派发")
            continue
        if cache and model_name not in cache:
            status = "missing"
        else:
            status = cache.get(model_name, {}).get("status", "unknown")
        if is_status_stale(cache.get(model_name) if cache else None):
            stale_engines.append(model_name)
        api_ok = has_api_configured(model_name)
        if status == "limited":
            if api_ok:
                scores[model_name] -= 1.0
                reasons.append(f"{model_name} 订阅额度受限，已自动启用备用 API 兜底 (轻微降权 -1.0)")
            else:
                scores[model_name] = -999.0
                reasons.append(f"{model_name} 当前额度受限 (limited)")
        elif status in ("needs_auth", "missing"):
            if api_ok and model_name not in ("local", "ollama"):
                scores[model_name] -= 0.5
                reasons.append(f"{model_name} 未配置 CLI 订阅，当前使用纯 API 模式")
            else:
                scores[model_name] = -999.0
                if status == "needs_auth":
                    reasons.append(get_reauth_hint(model_name))
    if stale_engines:
        reasons.append(f"引擎状态缓存已超过 6 小时未刷新 ({', '.join(sorted(stale_engines))})，按中性处理；建议运行 'makewand probe'")

    # 3.5 Real-dispatch reliability
    reliability = {}
    try:
        from makewand.usage import get_all_engine_reliability
        reliability = get_all_engine_reliability(list(scores.keys()))
        for model_name, (rate, weight, _raw) in reliability.items():
            factor = reliability_factor(rate)
            if factor < 1.0 and scores.get(model_name, 0) > 0:
                scores[model_name] *= factor
                reasons.append(f"{model_name.upper()} 近 7 天真实派发成功率 {rate:.0%} (有效样本 {weight:g})，按可靠性软降权 ×{factor:.2f}")
    except Exception:
        reliability = {}

    if require_file_editing is None:
        require_file_editing = (classify_prompt_intent(prompt) == "code")

    # Sort coder candidates
    available_coders = [m for m, sc in sorted(scores.items(), key=lambda x: x[1], reverse=True) if sc > 0]
    if require_file_editing:
        filtered_coders = [m for m in available_coders if m not in NON_AGENTIC_CHAT_MODELS]
        if filtered_coders:
            available_coders = filtered_coders
        else:
            reasons.append("⚠️ 无可用自主工具 Agent 候选，降级保留 API/Local 引擎")

    if not available_coders:
        available_coders = sorted(
            (m for m in active_pool if is_provider_enabled(m) and m not in ENGINES_WITHOUT_EXECUTOR
             and (_usable_fn(m, cache)[0] or has_api_configured(m))),
            key=lambda m: (-scores.get(m, -999.0), m),
        ) or ["agy"]
        if require_file_editing:
            filtered_coders = [m for m in available_coders if m not in NON_AGENTIC_CHAT_MODELS]
            if filtered_coders:
                available_coders = filtered_coders

    primary_coder = available_coders[0]

    # Reviewer candidates
    reviewer_base_scores = {
        "codex": 2.2,
        "deepseek": 2.1,
        "agy": 2.0,
        "grok": 1.9,
        "claude": 1.6,
        "aider": 1.5,
        "qwen": 1.4,
        "local": 1.0,
        "openrouter": 1.0,
        "siliconflow": 1.0,
        "glm": 0.8,
        "kimi": 0.8,
        "muse": 0.5,
        "cursor": 0.5,
        "copilot": 0.5
    }
    reviewer_base_scores.pop(primary_coder, None)
    for model_name in list(reviewer_base_scores.keys()):
        if not is_provider_enabled(model_name):
            reviewer_base_scores[model_name] = -999.0
            continue
        if model_name not in active_pool:
            reviewer_base_scores[model_name] = -999.0
            continue
        if cache and model_name not in cache:
            status = "missing"
        else:
            status = cache.get(model_name, {}).get("status", "unknown")
        api_ok = has_api_configured(model_name)
        if status in ("limited", "needs_auth", "missing"):
            if api_ok and model_name not in ("local", "ollama"):
                reviewer_base_scores[model_name] -= 0.8
            else:
                reviewer_base_scores[model_name] = -999.0
        else:
            if model_name in ENGINES_WITHOUT_EXECUTOR:
                reviewer_base_scores[model_name] = -999.0
                continue
            if not boost:
                try:
                    from makewand.usage import get_burn_rate_penalty
                    pen, _ = get_burn_rate_penalty(model_name)
                    if pen != 0.0:
                        reviewer_base_scores[model_name] = apply_burn_rate_penalty(reviewer_base_scores[model_name], pen)
                except Exception:
                    pass
            rate = (reliability.get(model_name) or (None,))[0]
            reviewer_base_scores[model_name] = (
                reviewer_base_scores[model_name] * reliability_factor(rate)
                if reviewer_base_scores[model_name] > 0 else reviewer_base_scores[model_name]
            )

    available_reviewers = [m for m, sc in sorted(reviewer_base_scores.items(), key=lambda x: x[1], reverse=True) if sc > 0]
    single_tool_mode = False
    if len(active_pool) <= 1:
        available_reviewers = [primary_coder]
        single_tool_mode = True
        reasons.append(f"当前系统仅检测到 1 个活跃可用工具 ({primary_coder.upper()})，已自动切换为单工具实现 + 独立沙箱自审闭环模式")
    elif not available_reviewers:
        other_active = [m for m in active_pool if m != primary_coder and is_provider_enabled(m)
                        and m not in ENGINES_WITHOUT_EXECUTOR
                        and (_usable_fn(m, cache)[0] or has_api_configured(m))]
        if other_active:
            available_reviewers = sorted(other_active, key=lambda m: (-reviewer_base_scores.get(m, -999.0), m))
            reasons.append(f"由于削峰保护，备用审查员降级由活跃工具接管: {available_reviewers[0]}")
        else:
            available_reviewers = [primary_coder]
            single_tool_mode = True

    meta_info = {
        "scores": scores,
        "reasons": reasons,
        "primary_coder": primary_coder,
        "primary_reviewer": available_reviewers[0] if available_reviewers else primary_coder,
        "single_tool_mode": single_tool_mode,
        "pacings": pacings
    }

    return available_coders, available_reviewers, meta_info
