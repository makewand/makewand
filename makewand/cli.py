"""
Makewand CLI: Command-line interface and subcommand parsers.
"""

import os
import sys
import argparse
import json
import math
import time
from pathlib import Path
from typing import List, Optional, Dict, Any
from makewand import __version__
from makewand.command_contract import ARCHIVED_COMMANDS, PYTHON_VALUE_FLAGS, PYTHON_BOOLEAN_FLAGS, SHARED_VALUE_FLAGS, WORKER_QUERY_COMMANDS, WORKER_STATIC_ARGS
from makewand.config import c, COLOR_BOLD, COLOR_CYAN, COLOR_GREEN, COLOR_YELLOW, COLOR_RED, COLOR_BLUE, COLOR_PURPLE, COLOR_RESET, normalize_tier, API_ONLY_ENGINES
from makewand.health import get_or_update_status
from makewand.discovery import discover_available_models
from makewand.execution_contract import EXIT_FAILED, EXIT_UNVERIFIED, EXIT_USAGE_ERROR

# Query/help commands need no provider or orchestration imports. Keep these
# public wrappers so integrations and existing patches retain their API.
def run_review(*args, **kwargs):
    from makewand.orchestrator import run_review as execute
    return execute(*args, **kwargs)


def select_optimal_engine_pair(*args, **kwargs):
    from makewand.engine_selection import select_optimal_engine_pair as execute
    return execute(*args, **kwargs)


from makewand.providers.agy import execute_agy_task
from makewand.providers.claude import execute_claude_task
from makewand.providers.codex import execute_codex_task
from makewand.providers.muse import execute_muse_task
from makewand.providers.grok import execute_grok_task

DIRECT_TASK_COMMANDS = frozenset({
    "claude", "codex", "agy", "muse", "grok", "local", "aider",
    "deepseek", "qwen", "glm", "kimi", "openrouter", "siliconflow",
})


# Refresh a little before health treats the snapshot as expired.
_QUOTA_SNAPSHOT_FRESH_SECONDS = 600
TIMEOUT_DEFAULTS = {**dict.fromkeys(DIRECT_TASK_COMMANDS, 300), "review": 300}
PROMPT_COMMANDS = DIRECT_TASK_COMMANDS


def _resolve_direct_timeout(args):
    """Resolve task limits before discovery, daemon dispatch or ledger access."""
    total = args.total_timeout
    if total is not None and (not math.isfinite(total) or total <= 0):
        raise ValueError("--total-timeout must be a finite positive number")
    if args.subcommand in TIMEOUT_DEFAULTS:
        if args.timeout is None:
            args.timeout = total if total is not None else TIMEOUT_DEFAULTS[args.subcommand]
        elif total is not None:
            args.timeout = min(args.timeout, total)


def _command_wall_timeout(args):
    """Workflow budget, also used by the daemon and process observer."""
    if args.total_timeout is not None:
        return args.total_timeout
    return getattr(args, "timeout", None)


def _execute_direct_task(engine, execute, prompt, **kwargs):
    """Direct commands share typed admission, deadlines and observation, and record
    the dispatch in the usage ledger with the route (cli/api) the adapter took."""
    from makewand.execution_contract import ExecutionRequest
    from makewand import execution_runtime
    from makewand.config import get_api_policy
    from makewand.usage import observe_dispatch_route, record_engine_usage
    from makewand.workflow import remember_result
    context = execution_runtime.current_context()
    maximum = os.environ.get("MAKEWAND_MAX_MODEL_CALLS")
    request = ExecutionRequest(task_id=execution_runtime.task_id(),
        stage=context.get("stage") or "direct", engine=engine,
        tier=kwargs.get("tier", "standard"), model=kwargs.get("model"),
        readonly=kwargs.get("readonly", False), repo_trust=kwargs.get("repo_trust", "trusted"),
        api_policy=get_api_policy(), deadline_unix_ms=context.get("deadline_unix_ms"),
        timeout_ms=max(1, math.ceil(kwargs.get("timeout", 300) * 1000)),
        budget_file=os.environ.get("MAKEWAND_CALL_BUDGET_FILE"),
        max_model_calls=int(maximum) if maximum else None,
        workflow=context.get("workflow") or "direct", risk=context.get("risk"),
        prompt=prompt, cwd=kwargs.get("cwd") or os.getcwd())
    launched = False
    def callback(effective_timeout):
        nonlocal launched
        from makewand.workflow import provider_outcome
        try:
            return provider_outcome(execute(prompt, **dict(kwargs, timeout=effective_timeout)))
        finally:
            launched = execution_runtime.provider_invocation_used()
    with observe_dispatch_route() as route:
        result = remember_result(execution_runtime.execute(request, callback))
    if not launched:
        # Refused before launch (capacity, budget, quota, sandbox, policy); no provider ran.
        return result
    try:
        record_engine_usage(engine, tier=kwargs.get("tier", "standard"), success=result[0], task=prompt,
                            route=route.route, model=kwargs.get("model"))
    except Exception:
        pass
    return result


def _execution_exit_code(fallback=EXIT_FAILED):
    from makewand.workflow import last_result
    result = last_result()
    return getattr(result, "exit_code", fallback)


def build_status_json(cache: Dict[str, Any]) -> Dict[str, Any]:
    """Machine-readable status; quota numbers carry their source explicitly."""
    from makewand.config import get_all_supported_providers, get_provider_execution_mode, get_api_policy
    from makewand.health import calculate_provider_quota, is_status_stale, STATUS_STALE_SECONDS
    providers = {}
    for key in get_all_supported_providers():
        info = cache.get(key, {}) if isinstance(cache.get(key), dict) else {}
        quota = calculate_provider_quota(key, info)
        providers[key] = {
            "status": info.get("status", "unknown"),
            "reason": info.get("reason", ""),
            "mode": get_provider_execution_mode(key),
            "updated_at": info.get("updated_at", ""),
            "stale": is_status_stale(info),
            "verified": info.get("verified", True) if info.get("status") == "healthy" else None,
            "resets_at": info.get("resets_at"),
            "quota": {
                "percentage": quota.get("percentage"),
                "source": quota.get("source"),
                "source_label": quota.get("source_label"),
                "desc": quota.get("desc"),
                "updated_at": quota.get("updated_at"),
                "is_unlimited": quota.get("is_unlimited", False),
            },
        }
    from makewand.sandbox import resolve_trusted_bwrap
    from makewand.proxy import proxy_status
    return {
        "engine": "python",
        "version": __version__,
        "api_policy": get_api_policy(),
        "proxy": proxy_status(),
        "privacy_attestation": {
            "sandbox_available": bool(resolve_trusted_bwrap()),
            "local_only_supported": True,
            "offline_flags": ["--local-only", "--offline", "--provider local"],
            "repo_trust_levels": ["trusted", "untrusted"],
        },
        "quota_semantics": ("source=official 表示可关联当前账号与额度窗口的官方观测；缺少证据时余量未知。"
                            "本地调用计数不是官方剩余配额，不换算成额度百分比。"),
        "stale_after_seconds": STATUS_STALE_SECONDS,
        "providers": providers,
    }


def _refresh_official_quota(args):
    """status reads the snapshot `makewand-server quota` leaves behind, valid for 15 minutes.

    Nothing refreshes it when no route ran lately, and status then called a known quota unknown.
    """
    import subprocess
    from makewand.route_select import quota_argv
    if getattr(args, "repo_trust", "trusted") != "trusted":
        return  # the reader can run `agy models`, which an untrusted repository must not reach
    snapshot = Path.home() / ".cache" / "makewand" / "quota-snapshot.json"
    try:
        if time.time() - snapshot.stat().st_mtime < _QUOTA_SNAPSHOT_FRESH_SECONDS:
            return
    except OSError:
        pass
    argv = quota_argv()
    if argv is None:
        return
    try:
        subprocess.run([*argv, "--timeout", "10s"], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL, timeout=15, check=False)
    except (OSError, subprocess.SubprocessError):
        pass


def _status_cache(args):
    import json
    from makewand.health import ProbePolicyError
    _refresh_official_quota(args)
    try:
        return get_or_update_status(force_probe=getattr(args, "probe", False),
            repo_trust=getattr(args, "repo_trust", "trusted"), cwd=getattr(args, "cwd", None))
    except ProbePolicyError as error:
        if getattr(args, "json", False):
            print(json.dumps({"status": "UNVERIFIED", "exit_code": EXIT_UNVERIFIED, "error": str(error)}, ensure_ascii=False))
        else:
            print(str(error), file=sys.stderr)
        raise SystemExit(EXIT_UNVERIFIED)


def cmd_status(args):
    import shutil
    from makewand.config import (
        get_all_supported_providers,
        get_active_providers,
        get_provider_execution_mode,
        is_provider_enabled
    )

    if getattr(args, "json", False):
        import json
        cache = _status_cache(args)
        print(json.dumps(build_status_json(cache), ensure_ascii=False, indent=2))
        return

    print(c("\n============================================================", COLOR_BOLD))
    print(c("       Makewand Multi-Model AI 订阅与全工具拓扑看板", COLOR_BOLD + COLOR_CYAN))
    print(c("============================================================\n", COLOR_BOLD))

    from makewand.config import get_api_policy
    policy = get_api_policy()
    from makewand.proxy import proxy_status
    print("代理来源: " + {"environment": "环境变量", "config_default": "配置默认值", "none": "无"}[proxy_status()["source"]])
    print("API 费用策略: " + ("allow_paid（允许云 API 按量计费）" if policy == "allow_paid" else "subscription_only（禁止 Makewand 云 API 调用）"))
    print(c("额度说明: 缺少当前账号/模型的官方观测时余量未知；本地调用计数仅用于削峰，不是官方剩余配额。",
            COLOR_YELLOW))
    cache = _status_cache(args)

    status_badges = {
        "healthy":    c("[🟢 正常可用]", COLOR_GREEN + COLOR_BOLD),
        "limited":    c("[🔴 额度受限]", COLOR_RED + COLOR_BOLD),
        "needs_auth": c("[🔑 需要授权]", COLOR_YELLOW + COLOR_BOLD),
        "warning":    c("[🟡 状态警告]", COLOR_YELLOW + COLOR_BOLD),
        "error":      c("[❌ 连接异常]", COLOR_RED + COLOR_BOLD),
        "disabled":   c("[🚫 手动禁用]", COLOR_RED + COLOR_BOLD),
        "missing":    c("[⚪ 未就绪]", COLOR_RESET),
        "unknown":    c("[⚪ 未探测]", COLOR_RESET),
    }

    mode_badges = {
        "hybrid":       c("[双模自适应: 订阅优先+API兜底]", COLOR_CYAN + COLOR_BOLD),
        "subscription": c("[订阅模式: 消耗工具订阅额度]", COLOR_GREEN),
        "api":          c("[纯 API 模式: 按量计费]", COLOR_BLUE + COLOR_BOLD),
        "local":        c("[本地私有: 0 成本/离线/隐私]", COLOR_PURPLE + COLOR_BOLD),
        "disabled":     c("[🚫 已关闭]", COLOR_RED),
        "none":         c("[未配置]", COLOR_RESET),
    }

    from makewand.discovery import get_provider_model_tier
    codex_active_model = get_provider_model_tier("codex", "standard").get("model", "gpt-6.1-sol")
    claude_active_model = get_provider_model_tier("claude", "standard").get("model", "claude-sonnet-5")
    agy_active_model = get_provider_model_tier("agy", "standard").get("model", "gemini-3.8-flash")
    grok_active_model = get_provider_model_tier("grok", "standard").get("model", "grok-4.7")
    muse_active_model = get_provider_model_tier("muse", "standard").get("model", "muse-spark-1.3-contributor")
    try:
        from makewand.providers.local import get_default_local_model
        local_active_model = get_default_local_model()
    except Exception:
        local_active_model = "qwen2.5-coder:7b"

    display_names = {
        "agy": f"Antigravity (Google AI Pro / {agy_active_model})",
        "claude": f"Claude Code (Anthropic Subscription / {claude_active_model})",
        "codex": f"Codex CLI (OpenAI Subscription / {codex_active_model})",
        "grok": f"Grok Build CLI (xAI Subscription / {grok_active_model})",
        "muse": f"Muse Code (Meta Subscription / {muse_active_model})",
        "local": f"Local Self-Hosted (本地大模型 / Ollama / {local_active_model})",
        "aider": "Aider CLI (Pair Programmer CLI)",
        "cursor": "Cursor Agent (Cursor Subscription CLI)",
        "copilot": "GitHub Copilot (CLI / gh copilot)",
        "deepseek": "DeepSeek API (deepseek-chat / deepseek-reasoner)",
        "qwen": "Aliyun Qwen API (通义千问 / qwen2.5-coder / qwen-max)",
        "glm": "Zhipu GLM API (智谱清言 / glm-4-plus)",
        "kimi": "Moonshot Kimi API (moonshot-v1)",
        "openrouter": "OpenRouter API (Multi-model Gateway)",
        "siliconflow": "SiliconFlow API (硅基流动 / SiliconCloud)",
    }

    tool_setup_hints = {
        "aider": "已安装 (aider 0.86.2)" if shutil.which("aider") else "运行 'pip install -U aider-chat' 安装命令行",
        "cursor": "安装 Cursor 客户端并链接 cursor 命令行至 PATH",
        "copilot": "运行 'gh extension install github/gh-copilot' 或安装 copilot CLI",
        "deepseek": "设置 'export DEEPSEEK_API_KEY=...' 或写入 ~/.config/makewand/api_keys.json",
        "qwen": "设置 'export DASHSCOPE_API_KEY=...' 或写入 ~/.config/makewand/api_keys.json",
        "glm": "设置 'export ZHIPUAI_API_KEY=...' 或写入 ~/.config/makewand/api_keys.json",
        "kimi": "设置 'export MOONSHOT_API_KEY=...' 或写入 ~/.config/makewand/api_keys.json",
        "openrouter": "设置 'export OPENROUTER_API_KEY=...' 或写入 ~/.config/makewand/api_keys.json",
        "siliconflow": "设置 'export SILICONFLOW_API_KEY=...' 或写入 ~/.config/makewand/api_keys.json",
        "local": "运行 'makewand enable local' 唤醒本地 Ollama 守护服务 (http://localhost:11434)",
        "claude": "运行 'claude login' 或 'npm install -g @anthropic-ai/claude-code'",
        "codex": "运行 'npm install -g @openai/codex' 并登录",
        "agy": "配置 Google Antigravity CLI",
        "grok": "配置 Grok Build CLI",
        "muse": "配置 Muse Code CLI",
    }

    active_tools = get_active_providers()
    all_tools = get_all_supported_providers()

    # 1. Active Tools Section
    print(c(f"--- 🟢 已激活可用工具池 (Active Dynamic Pool: N={len(active_tools)}) ---", COLOR_BOLD + COLOR_GREEN))
    if active_tools:
        from datetime import datetime
        from makewand.health import calculate_provider_quota, format_quota_bar, get_reauth_hint
        from makewand.model_quota import refused_models
        from makewand.outcome_ledger import OutcomeLedger
        from makewand.route_cli import weekly_forecasts
        from makewand.route_select import load_policy
        marks = OutcomeLedger().active_marks()
        try:
            forecasts = weekly_forecasts(load_policy())
        except ValueError:
            forecasts = {}
        for key in active_tools:
            info = cache.get(key, {})
            status = info.get("status", "unknown")
            badge = status_badges.get(status, f"[{status}]")
            if status == "healthy" and info.get("verified") is False:
                badge = c("[🟡 已安装·未验证]", COLOR_YELLOW + COLOR_BOLD)
            mode = get_provider_execution_mode(key)
            mode_badge = mode_badges.get(mode, f"[{mode}]")
            name = display_names.get(key, key)
            reason = info.get("reason", "")
            resets = info.get("resets_at")

            quota_data = calculate_provider_quota(key, info)
            pct = quota_data["percentage"]
            quota_desc = quota_data["desc"]

            print(f"{badge} {mode_badge} {c(name, COLOR_BOLD)}")
            if pct is None:
                print(f"      额度指示: 未知  ({quota_desc})")
            else:
                bar = format_quota_bar(pct, width=20)
                print(f"      额度指示: {bar}  ({quota_desc})")
            if reason and reason != quota_desc and not (status == "healthy" and "运行正常" in reason and "运行正常" in quota_desc):
                print(f"      运行状态: {reason}")
            for model, why in refused_models(key, info):
                print(f"      受限模型: {model}（{why}）")
            for name, mark in marks.items():
                if name == key or name.startswith(key + ":"):
                    subject = name.partition(":")[2] or "整个工具"
                    print(f"      额度标记: {subject} 到 {datetime.fromtimestamp(mark.until):%m-%d %H:%M}（{mark.reason or '无说明'}）")
            if key in forecasts:
                print(c(f"      消耗预测: {forecasts[key]}", COLOR_YELLOW))
            if status == "needs_auth" and "重新登录" not in f"{reason}{quota_desc}":
                print(c(f"      操作提示: {get_reauth_hint(key)}", COLOR_YELLOW))
            if resets:
                print(f"      预计解封: {c(resets, COLOR_YELLOW + COLOR_BOLD)}")
            print()
    else:
        print(c("  (暂无可用的已激活工具，请查看下方待接入生态列表进行配置)\n", COLOR_YELLOW))

    # 2. Inactive / Pending Ecosystem Tools Section
    pending_tools = [p for p in all_tools if p not in active_tools and is_provider_enabled(p)]
    disabled_tools = [p for p in all_tools if not is_provider_enabled(p)]

    if pending_tools:
        print(c(f"--- ⚪ 待接入主流工具生态 (Supported Ecosystem: 提供凭据即可无缝接入) ---", COLOR_BOLD))
        for key in pending_tools:
            name = display_names.get(key, key)
            hint = tool_setup_hints.get(key, "配置对应 CLI 或 API 密钥")
            print(f"  • {c(name, COLOR_BOLD)}: {hint}")
        print()

    if disabled_tools:
        print(c(f"--- 🚫 用户手动禁用的工具 (已从调度池剔除) ---", COLOR_BOLD + COLOR_RED))
        for key in disabled_tools:
            name = display_names.get(key, key)
            print(f"  • {name} (运行 'makewand enable {key}' 重新纳入调度)")
        print()

    # 3. Dynamic Topology Recommendation
    print(c("--- 当前自适应拓扑调度推荐策略 (Dynamic Topology) ---", COLOR_BOLD))
    coders, reviewers, meta = select_optimal_engine_pair("general task", cache=cache) if active_tools else ([], [], {})
    if active_tools and not coders:
        # Never recommend an engine that cannot run: say why each one is out.
        unusable = meta.get("unusable_engines") or {}
        print(c("  ❌ 已激活的工具当前都不可用，暂无可推荐的分工 (任务不会被派发):", COLOR_RED + COLOR_BOLD))
        for engine, why in unusable.items():
            print(f"     • {engine}: {why}")
        if not unusable:
            print("     • 已激活的工具均暂无执行适配 (cursor / copilot)")
        print("     运行 'makewand probe' 立即复查；失败状态在有效期过后自动恢复为中性，额度受限在重置后自动恢复。")
    elif len(active_tools) >= 2 and not meta.get("single_tool_mode"):
        coder = coders[0]
        reviewer = reviewers[0] if reviewers else (coders[1] if len(coders) > 1 else coder)
        print(c(f"  🌟 多模型联合编排模式 (活跃工具数: N={len(active_tools)}):", COLOR_GREEN + COLOR_BOLD))
        print(f"     • 活跃工具池: {c(', '.join(active_tools), COLOR_CYAN)}")
        print(f"     • 推荐通用分工: 主力实现 [{c(coder, COLOR_BOLD)}] -> 独立跨模型红队盲审 [{c(reviewer, COLOR_BOLD)}]")
        print(f"     • 调度机制: 自动按任务类型 (代码/架构/审查/自愈) 进行领域自适应，零人工配置负担。")
        print("     • 实际派发以 makewand route 为准（按额度和成绩实时选择）。")
    elif active_tools:
        single = coders[0]
        if len(active_tools) == 1:
            print(c(f"  ⚡ 韧性单工具独立闭环模式 (活跃工具数: N=1):", COLOR_YELLOW + COLOR_BOLD))
            print(f"     • 活跃唯一工具: {c(single, COLOR_BOLD + COLOR_GREEN)}")
        else:
            print(c(f"  ⚡ 韧性单工具独立闭环模式 (活跃工具数: N={len(active_tools)}，当前仅 1 个可用):", COLOR_YELLOW + COLOR_BOLD))
            print(f"     • 当前唯一可用工具: {c(single, COLOR_BOLD + COLOR_GREEN)} (其余工具的不可用原因见上方运行状态)")
        print(f"     • 闭环机制: 由 [{single}] 独立编码，并在独立影子沙箱 (Shadow Worktree) 中进行严格对抗性自我盲审验收。")
    else:
        print(c("  ❌ 当前未检测到任何已登录或已配置凭据的 AI 工具！", COLOR_RED + COLOR_BOLD))
        print("     提示: 请至少登录一个订阅 CLI 或设置任意一个 API Key (如 export DEEPSEEK_API_KEY=...)。")
    print()

    # 4. Tool Toggle Instruction
    print(c("--- AI 工具开关与配置指令 ---", COLOR_BOLD))
    print(f"  • 禁用工具: {c('makewand disable <tool>', COLOR_CYAN)}  (例如: makewand disable local 或 makewand disable muse)")
    print(f"  • 启用工具: {c('makewand enable <tool>', COLOR_GREEN)}   (例如: makewand enable local 或 makewand enable all)")
    print(f"  • 支持的全部工具: {c(', '.join(t for t in all_tools if t not in ('cursor', 'copilot')), COLOR_YELLOW)}")
    print(f"  • 仅安装检测、暂无执行适配: {c('cursor, copilot', COLOR_YELLOW)}\n")

    # 5. Sliding window usage and burn-rate status
    try:
        from makewand.usage import get_engine_usage_stats, get_burn_rate_penalty
        u_4h = get_engine_usage_stats(window_hours=4.0)
        u_24h = get_engine_usage_stats(window_hours=24.0)
        u_7d = get_engine_usage_stats(window_hours=168.0)
        print(c("--- 本地滑动窗口用量与削峰保护看板 ---", COLOR_BOLD))
        print(f"{'模型订阅/API':<14} {'4h 调用':<10} {'24h 调用':<10} {'7d 调用':<10} {'削峰保护策略'}")
        monitored = [p for p in active_tools if p in ("claude", "codex", "grok", "agy", "muse", "local", "aider", "deepseek", "qwen")]
        for eng in monitored:
            c4 = u_4h.get(eng, {}).get("total", 0)
            c24 = u_24h.get(eng, {}).get("total", 0)
            c7d = u_7d.get(eng, {}).get("total", 0)
            pen, reason = get_burn_rate_penalty(eng)
            from makewand.usage import get_predictive_pacing_status
            pace = get_predictive_pacing_status(eng)

            if not is_provider_enabled(eng):
                status_desc = c("🚫 用户已手动禁用", COLOR_RED)
            elif pace.get("status") == "critical":
                status_desc = c(f"🔴 预测高危 ({pace.get('velocity_per_hour', 0):.1f}当量/h, 消耗{pace.get('utilization_pct', 0)}%)", COLOR_RED + COLOR_BOLD)
            elif pace.get("status") == "pacing":
                status_desc = c(f"🟡 削峰调步 ({pace.get('velocity_per_hour', 0):.1f}当量/h, 消耗{pace.get('utilization_pct', 0)}%)", COLOR_YELLOW)
            elif pen == 0.0:
                status_desc = c("🟢 额度健康平稳 (0 成本)" if eng in ("local", "aider") else "🟢 额度健康平稳", COLOR_GREEN)
            else:
                status_desc = c(f"🟡 {reason}", COLOR_YELLOW)
            print(f"{eng:<14} {c4:<10} {c24:<10} {c7d:<10} {status_desc}")
        print()
    except Exception:
        pass

def _resolve_provider_arg(raw: str, allow_all: bool) -> str:
    """Normalizes a provider name/alias or exits with an explicit usage error."""
    from makewand.config import get_all_supported_providers, normalize_provider_name
    target = (raw or "").lower().strip()
    if allow_all and target == "all":
        return "all"
    norm = normalize_provider_name(target)
    supported = get_all_supported_providers()
    if norm not in supported:
        choices = ", ".join(supported) + (", all" if allow_all else "")
        print(c(f"❌ 未知或不支持的工具名称: {raw!r} (支持: {choices}；别名如 ollama/gemini/openai/anthropic 会自动映射)", COLOR_RED), file=sys.stderr)
        sys.exit(EXIT_USAGE_ERROR)
    return norm


def _ollama_service_hint(action: str) -> str:
    return f"sudo systemctl {action} ollama"


def _manage_ollama_service(action: str) -> bool:
    """Runs a *non-interactive* sudo systemctl action and reports the real outcome."""
    import subprocess
    try:
        res = subprocess.run(["sudo", "-n", "systemctl", action, "ollama"], capture_output=True, text=True, timeout=30)
    except (OSError, subprocess.SubprocessError) as exc:
        print(c(f"  ⚠ 无法执行 '{_ollama_service_hint(action)}': {exc}", COLOR_YELLOW), file=sys.stderr)
        return False
    if res.returncode != 0:
        detail = (res.stderr or res.stdout or "").strip().splitlines()[:1]
        print(c(f"  ⚠ '{_ollama_service_hint(action)}' 失败 (exit {res.returncode}{': ' + detail[0] if detail else ''})；"
                "如需密码请手动执行该命令", COLOR_YELLOW), file=sys.stderr)
        return False
    return True


def _ollama_is_active() -> Optional[bool]:
    import subprocess
    try:
        res = subprocess.run(["systemctl", "is-active", "ollama"], capture_output=True, text=True, timeout=5)
    except (OSError, subprocess.SubprocessError):
        return None
    return res.stdout.strip() == "active"


def cmd_enable(args):
    """Enables one or all AI tool providers. Never starts system services implicitly."""
    from makewand.config import set_provider_enabled, get_all_supported_providers, get_active_providers
    target = _resolve_provider_arg(args.provider, allow_all=True)
    manage = getattr(args, "manage_service", False)
    targets = get_all_supported_providers() if target == "all" else [target]
    failed = [p for p in targets if not set_provider_enabled(p, True)]
    if failed:
        print(c(f"❌ 写入配置失败，未能启用: {', '.join(failed)} (检查 {os.environ.get('MAKEWAND_CONFIG_DIR') or '~/.config/makewand'} 是否可写)", COLOR_RED), file=sys.stderr)
        sys.exit(EXIT_FAILED)
    if target == "all":
        active = get_active_providers()
        print(c(f"✔ 已成功启用全部 AI 工具！当前动态可用池检测到 {len(active)} 个工具: {', '.join(active)}", COLOR_GREEN + COLOR_BOLD))
    else:
        print(c(f"✔ 已成功启用 AI 工具: {target} (运行 'makewand status' 查看最新状态)", COLOR_GREEN + COLOR_BOLD))
    if "local" in targets:
        active = _ollama_is_active()
        if active is False:
            if manage:
                if _manage_ollama_service("start"):
                    print(c("  ✔ 已按 --manage-service 启动本地 Ollama 服务", COLOR_GREEN))
            else:
                print(c(f"  ℹ️ 本地 Ollama 服务未运行；如需启动请执行 '{_ollama_service_hint('start')}' "
                        "(或加 --manage-service 让 makewand 以 sudo -n 代为执行)", COLOR_YELLOW))


def cmd_disable(args):
    """Disables an AI tool provider. Never stops system services implicitly."""
    from makewand.config import set_provider_enabled
    target = _resolve_provider_arg(args.provider, allow_all=False)
    if not set_provider_enabled(target, False):
        print(c(f"❌ 写入配置失败，未能禁用: {target}", COLOR_RED), file=sys.stderr)
        sys.exit(EXIT_FAILED)
    print(c(f"✔ 已成功禁用 AI 工具: {target} (Makewand 将不再向其派发任务)", COLOR_YELLOW + COLOR_BOLD))
    if target == "local":
        if getattr(args, "manage_service", False):
            if _manage_ollama_service("stop"):
                print(c("  ✔ 已按 --manage-service 停止本地 Ollama 服务", COLOR_GREEN))
        else:
            print(f"  提示: Ollama 系统服务保持原状；如需停止请执行 '{_ollama_service_hint('stop')}' (或加 --manage-service)")
    print(f"  提示: 随时可运行 'makewand enable {target}' 重新启用。")


def cmd_models(args):
    if getattr(args, "refresh", False):
        # Explicit agy refresh: 60s, ignores a fresh cache and the .attempt back-off.
        # The listing below still reads the cache and does not refresh again.
        from makewand.discovery import (
            AGY_MODELS_REFRESH_TIMEOUT_SECONDS,
            refresh_agy_models_cache,
        )
        refresh_agy_models_cache(timeout=AGY_MODELS_REFRESH_TIMEOUT_SECONDS)
    if getattr(args, "json", False):
        from makewand.health import calculate_provider_quota
        models = discover_available_models()
        models["agy"]["quota"] = calculate_provider_quota("agy")
        print(json.dumps(models, ensure_ascii=False, indent=2))
        return
    print(c("\n============================================================", COLOR_BOLD))
    print(c("       Makewand 多模型生态与动态发现矩阵 (Model Discovery)", COLOR_BOLD + COLOR_CYAN))
    print(c("============================================================\n", COLOR_BOLD))

    models = discover_available_models()

    def _model_lines(key: str, detected_src: str) -> None:
        entry = models[key]
        default_label = "当前默认" if entry.get("default_source") == "detected" else "内置默认 (未在本机检测到，仅作兜底)"
        print(f"   {default_label}: {c(entry['current_default'], COLOR_GREEN + COLOR_BOLD)}")
        available = entry.get("available") or []
        if entry.get("source") == "detected":
            print(f"   检测到版本 (来自 {detected_src}): {', '.join(available) if available else '无'}")
        elif available:
            print(f"   内置参考列表 (非检测结果，未找到 {detected_src}): {', '.join(available)}")
        else:
            print(f"   未检测到本机模型列表 ({detected_src} 不存在)")

    print(c("1. Claude Code (Anthropic 订阅):", COLOR_BOLD + COLOR_BLUE))
    _model_lines("claude", "~/.claude 模型目录缓存 / ~/.claude.json")
    print("   调用方式: --tier fast/standard/deep 自动排名；--model opus 或完整模型 ID 显式指定。")
    for tier, resolution in models["claude"].get("tier_selections", {}).items():
        print(f"   {tier}: {resolution.get('selection_reason', resolution['model'])}")
        for row in resolution.get("ranking", []):
            if "opus" in row["model"]:
                print(f"     Opus {row['model']}: {row['reason']} (score={row['score']})")
    print()

    print(c("2. Codex CLI (OpenAI 订阅):", COLOR_BOLD + COLOR_CYAN))
    _model_lines("codex", "~/.codex 模型目录缓存 / config.toml")
    print("   调用方式: 自动读取 models_cache.json 与 config.toml 动态映射档位 (--tier fast/standard/deep)。\n")

    print(c("3. Antigravity (Google AI Pro):", COLOR_BOLD + COLOR_GREEN))
    _model_lines("agy", "~/.gemini/antigravity-cli/settings.json / 官方在线模型列表")
    print("   调用方式: 自动读取 settings.json 与目录缓存映射档位 (--tier fast/standard/deep)。")
    for candidate in models["agy"].get("candidates", []):
        verification = "调用 ID 已验证" if candidate["call_id_verified"] else "调用 ID 未验证"
        eligibility = "可自动选择" if candidate["auto_select"] else "默认不选；可用 --model 显式指定"
        print(f"     {candidate['model']}: 家族={candidate['model_family'] or '未知'} · {verification} · {eligibility}")
        print(f"       来源: {candidate['source']} · 缓存时间: {candidate['cached_at'] or '未知'}")
        print(f"       工具={candidate['tool']} · 账号={candidate['account_ref'] or '未知'} · 实际模型={candidate['actual_model'] or '未知'} · 额度池={candidate['pool_ref'] or '未知'}")
    from makewand.health import calculate_provider_quota
    quota = calculate_provider_quota("agy")
    print(f"   {quota['desc']}\n")

    print(c("4. Muse Code (Meta 订阅):", COLOR_BOLD + COLOR_PURPLE))
    _model_lines("muse", "~/.local/share/muse/model-catalog / ~/.config/muse/settings.json")
    print("   调用方式: 支持动态模型识别 (--model) 与思考等级 (--reasoning-effort)。\n")

    print(c("5. Grok Build CLI (xAI 订阅):", COLOR_BOLD + COLOR_RED))
    _model_lines("grok", "~/.grok/models_cache.json")
    print("   调用方式: 读取 models_cache.json，否则使用内置默认。\n")

    print(c("6. Local Self-Hosted (本地大模型 / Ollama / vLLM):", COLOR_BOLD + COLOR_PURPLE))
    _model_lines("local", "本地 Ollama / vLLM 守护服务 (http://localhost:11434)")
    print("   自适应机制: 实时动态探测本地端点可用标签；数据 100% 离线隐私零 Token。\n")

    print(c("--- 模型选择规则 ---", COLOR_BOLD))
    print("  • 优先使用本机 CLI 缓存/配置中检测到的模型；未检测到时使用 makewand 内置的默认模型名 (硬编码兜底，可能过时)。")
    print("  • --tier fast/standard/deep 映射到上述模型或别名。")
    print("  • 可用 --model <模型名> 显式指定，原样传给底层 CLI。\n")

def cmd_quota(args):
    """Alias for status with focus on limits and reset schedule."""
    cmd_status(args)

def cmd_search(args):
    """Budgeted safe text search avoiding heavy cold archives, logs, and binaries."""
    from makewand.search import safe_search
    results = safe_search(
        args.pattern,
        root_path=args.cwd,
        max_results=args.max_results,
        max_depth=args.max_depth
    )
    if not results:
        print("未找到匹配内容。")
        return
    for r in results:
        print(f"{c(r['file'], COLOR_CYAN)}:{c(str(r['line_num']), COLOR_YELLOW)}: {r['content']}")
    print(c(f"\n共找到 {len(results)} 条匹配结果 (已自动避开冷归档、SQLite 数据库、模型与虚拟环境)。", COLOR_GREEN))

def cmd_repomap(args):
    """Generate concise repository symbol map (AST/regex extracted)."""
    from makewand.repomap import generate_repo_map
    cwd = getattr(args, "cwd", None) or os.getcwd()
    max_lines = getattr(args, "max_lines", 80)
    max_files = getattr(args, "max_files", 40)
    repomap = generate_repo_map(cwd=cwd, max_lines=max_lines, max_files=max_files)
    if getattr(args, "json", False):
        import json
        print(json.dumps({
            "cwd": cwd,
            "repomap": repomap,
            "lines": len(repomap.splitlines()) if repomap else 0
        }, ensure_ascii=False, indent=2))
    else:
        if repomap:
            print(c("\n============================================================", COLOR_BOLD))
            print(c("       Makewand 代码库全局架构感知拓扑 (Repo-Map)", COLOR_BOLD + COLOR_CYAN))
            print(c("============================================================\n", COLOR_BOLD))
            print(repomap)
            print()
        else:
            print("未在当前工作区发现有效代码符号或工作区为空。")


def cmd_sandbox(args):
    """Run command inside bubblewrap process sandbox."""
    from makewand.sandbox import run_in_sandbox
    if not args.cmd:
        print("请指定要在沙箱中运行的命令，例如: makewand sandbox python3 -m unittest")
        sys.exit(1)
    cmd = args.cmd
    if cmd and cmd[0] == "--":
        cmd = cmd[1:]
    if len(cmd) == 1:
        cmd_str = cmd[0]
        if any(c in cmd_str for c in ["|", ";", ">", "<", "&", "$", "`", "\n"]):
            cmd = ["bash", "-c", cmd_str]
        elif any(c in cmd_str for c in [" ", "\t"]) and not os.path.exists(cmd_str):
            import shlex
            try:
                cmd = shlex.split(cmd_str)
            except Exception:
                cmd = ["bash", "-c", cmd_str]
    ws = args.cwd or os.getcwd()
    print(c(f"🛡️ Makewand 沙箱执行: {' '.join(cmd)} (工作区: {ws}, 网络: {'允许' if args.allow_net else '阻断'})", COLOR_CYAN + COLOR_BOLD))
    ret, out, err, ex = run_in_sandbox(
        cmd,
        workspace=ws,
        timeout=args.timeout,
        allow_network=args.allow_net,
        stream=True
    )
    if ret != 0:
        if ex:
            sys.stderr.write(f"Sandbox Error: {ex}\n")
        sys.exit(ret if ret > 0 else 1)


def cmd_clean(args):
    import glob
    import shutil
    from pathlib import Path
    patterns = [
        "/tmp/makewand-restore-*",
        "/tmp/makewand-drill-*",
        "/tmp/makewand-agy-*",
        "/tmp/makewand-muse-*",
    ]
    if getattr(args, "all", False):
        patterns.append("/tmp/makewand-test-isolation-*")

    removed_count = 0
    freed_bytes = 0
    for pat in patterns:
        for path in glob.glob(pat):
            try:
                is_link = os.path.islink(path)
                if is_link:
                    size = 0
                    os.unlink(path)
                elif os.path.isdir(path):
                    size = sum(os.path.getsize(os.path.join(dirpath, filename))
                               for dirpath, dirnames, filenames in os.walk(path)
                               for filename in filenames)
                    shutil.rmtree(path, ignore_errors=True)
                else:
                    size = os.path.getsize(path)
                    os.remove(path)
                removed_count += 1
                freed_bytes += size
            except Exception:
                pass

    if getattr(args, "all", False):
        cache_roots = []
        xdg_cache = os.environ.get("XDG_CACHE_HOME")
        base_cache = Path(xdg_cache).expanduser() if xdg_cache else Path.home() / ".cache"
        cache_roots.append(base_cache / "makewand")
        cache_roots.append(base_cache / "trio")
        iso_root = os.environ.get("MAKEWAND_TEST_ISOLATION_ROOT")
        if iso_root:
            cache_roots.append(Path(iso_root).expanduser() / ".cache" / "makewand")
            cache_roots.append(Path(iso_root).expanduser() / ".cache" / "trio")

        for c_dir in cache_roots:
            if c_dir.is_symlink():
                try:
                    c_dir.unlink()
                    removed_count += 1
                except Exception:
                    pass
                continue
            if c_dir.exists() and c_dir.is_dir():
                for item in c_dir.iterdir():
                    try:
                        if item.is_symlink():
                            size = 0
                            item.unlink()
                        elif item.is_dir():
                            p_str = str(item)
                            size = sum(os.path.getsize(os.path.join(dirpath, filename))
                                       for dirpath, dirnames, filenames in os.walk(p_str)
                                       for filename in filenames)
                            shutil.rmtree(p_str, ignore_errors=True)
                        else:
                            size = item.stat().st_size
                            item.unlink()
                        removed_count += 1
                        freed_bytes += size
                    except Exception:
                        pass

    print(c(f"🧹 Makewand 临时文件清理完成: 已回收 {removed_count} 个临时目录/文件 (释放约 {freed_bytes / 1024:.1f} KB)。", COLOR_GREEN))
    sys.exit(0)


def _find_go_binary() -> Optional[str]:
    import shutil
    root = Path(__file__).resolve().parent.parent
    candidates = [
        root / "bin" / "makewand-server",
        root / "bin" / "makewand-server.exe",
        root / "bin" / "makewand-go",
        root / "bin" / "makewand-go.exe",
        root / "dist" / "makewand",
        shutil.which("makewand-server"),
        shutil.which("makewand-go")
    ]
    if root.name == "python" and root.parent.name == "makewand" and root.parent.parent.name == "lib":
        candidates[4:4] = [root.parents[2] / "makewand", root.parents[2] / "makewand.exe"]
    return next((str(c) for c in candidates if c and Path(c).is_file() and os.access(c, os.X_OK)), None)


def _resolve_cli_prompt(args, original_cwd: Optional[str] = None, required: bool = True) -> str:
    """
    Resolves the task prompt from positional argument, file flag (-f/-F/--file/--prompt-file),
    or non-TTY stdin pipe (e.g. `cat task.txt | makewand`).
    """
    prompt = getattr(args, "prompt", None)
    if prompt and isinstance(prompt, str) and prompt.strip():
        return prompt.strip()
    if prompt and isinstance(prompt, list):
        prompt_str = " ".join(str(p) for p in prompt).strip()
        if prompt_str:
            return prompt_str

    prompt_file = getattr(args, "prompt_file", None)
    if prompt_file:
        target_path = Path(prompt_file).expanduser()
        resolved_path = None
        if target_path.is_file():
            resolved_path = target_path
        elif not target_path.is_absolute():
            if getattr(args, "cwd", None):
                p_cwd = Path(args.cwd) / target_path
                if p_cwd.is_file():
                    resolved_path = p_cwd
            if not resolved_path and original_cwd:
                p_orig = Path(original_cwd) / target_path
                if p_orig.is_file():
                    resolved_path = p_orig

        if not resolved_path:
            print(c(f"❌ 提示词文件不存在: {prompt_file}", COLOR_RED), file=sys.stderr)
            sys.exit(EXIT_USAGE_ERROR)
        try:
            content = resolved_path.read_text(encoding="utf-8").strip()
            if not content:
                print(c(f"❌ 提示词文件内容为空: {resolved_path}", COLOR_RED), file=sys.stderr)
                sys.exit(EXIT_USAGE_ERROR)
            return content
        except Exception as e:
            print(c(f"❌ 读取提示词文件失败: {e}", COLOR_RED), file=sys.stderr)
            sys.exit(EXIT_USAGE_ERROR)

    if not sys.stdin.isatty():
        try:
            content = sys.stdin.read().strip()
            if content:
                return content
        except Exception:
            pass

    if required:
        print(c("❌ 缺少任务提示词。可直接传入参数、使用 -f/--prompt-file 指定文件，或通过管道标准输入传入。", COLOR_RED), file=sys.stderr)
        sys.exit(EXIT_USAGE_ERROR)
    return ""


def _archived_command(argv: List[str]) -> Optional[str]:
    """The archived command argv names, skipping the shared global flags before it."""
    value_flags = {"-C", "--cwd", "--repo-trust", "--approval"} | SHARED_VALUE_FLAGS | PYTHON_VALUE_FLAGS
    boolean_flags = {"--debug"} | PYTHON_BOOLEAN_FLAGS
    index = 0
    while index < len(argv):
        argument = argv[index]
        flag, equals, _ = argument.partition("=")
        if flag in value_flags:
            index += 1 if equals else 2
        elif flag in boolean_flags:
            index += 1
        elif len(argument) > 2 and argument[:2] in {"-C", "-f", "-F"}:
            index += 1
        else:
            break
    return argv[index] if index < len(argv) and argv[index] in ARCHIVED_COMMANDS else None


# Set by makewand in the environment of every provider CLI it launches.
from makewand.sandbox import PROVIDER_WORKER_ENV  # noqa: E402


def _worker_query_matches(argv: List[str], path: str, rule: Dict[str, Any]) -> bool:
    words = path.split()
    position = arguments = index = 0
    literal = False
    while index < len(argv):
        arg = argv[index]
        index += 1
        if not literal and arg == "--":
            literal = True
            continue
        if not literal and arg.startswith("-") and arg != "-":
            flag, equals, _ = arg.partition("=")
            attached = arg.startswith("-C") and len(arg) > 2
            if attached:
                flag = "-C"
            if flag not in rule["flags"]:
                return False
            if rule["flags"][flag] and not equals and not attached:
                if index >= len(argv):
                    return False
                index += 1
            continue
        if position < len(words):
            if literal or arg != words[position]:
                return False
            position += 1
        else:
            arguments += 1
    return position == len(words) and arguments <= rule["max_args"]


def _provider_worker_refusal(command: Optional[str], argv: List[str]) -> Optional[str]:
    """Guard inherited-environment misuse, not deliberate marker removal.

    Inspect literal argv before parsing/delegation: argparse accepts abbreviated
    flags, and command-looking option values must never select a safe command.
    """
    if os.environ.get(PROVIDER_WORKER_ENV) != "1":
        return None
    if len(argv) == 1 and argv[0] in WORKER_STATIC_ARGS:
        return None
    if any(_worker_query_matches(argv, path, rule) for path, rule in WORKER_QUERY_COMMANDS.items()):
        return None
    shown = f"makewand {command}" if command else "makewand"
    return (f"❌ makewand 正作为另一 makewand 任务派发的 AI 子进程运行（MAKEWAND_PROVIDER_WORKER=1），已拒绝递归或未经审计的调用 `{shown}`。"
            "仅允许白名单中的只读查询与参数；管理操作、探测、provider 启动及隐式写入均被禁止。请直接完成当前分配的任务。")


def main():
    for _stream in (sys.stdout, sys.stderr):
        if hasattr(_stream, "reconfigure"):
            try:
                _stream.reconfigure(encoding="utf-8", errors="replace")
            except Exception:
                pass
    from makewand.config import ConfigError
    try:
        return _main()
    except ConfigError as error:
        print(f"makewand: configuration error: {error}", file=sys.stderr)
        raise SystemExit(EXIT_USAGE_ERROR) from None


def _main():
    refusal = _provider_worker_refusal(None, sys.argv[1:])
    if refusal:
        print(c(refusal, COLOR_RED), file=sys.stderr)
        sys.exit(EXIT_USAGE_ERROR)
    original_cwd = os.getcwd()
    inherited_budget_file = os.environ.get("MAKEWAND_CALL_BUDGET_FILE")
    inherited_maximum = os.environ.get("MAKEWAND_MAX_MODEL_CALLS")
    archived = _archived_command(sys.argv[1:])
    if archived:
        # Without this an unknown word would reach the parser as a task prompt.
        print(f"makewand: {archived} 已归档，只保留在 tag archive/pre-trim-20261105。", file=sys.stderr)
        sys.exit(EXIT_USAGE_ERROR)
    if sys.argv[1:] == ["help"]:
        sys.argv[1:] = ["--help"]

    common_parser = argparse.ArgumentParser(add_help=False)
    common_parser.add_argument("-C", "--cwd", dest="cwd", default=None, help="Target working directory (default: current directory)")
    common_parser.add_argument("-f", "-F", "--file", "--prompt-file", dest="prompt_file", default=None, help="Read task prompt from file")
    common_parser.add_argument("--repo-trust", choices=["trusted", "untrusted"], default="trusted", help="Repository trust level: trusted or untrusted")
    common_parser.add_argument("--max-model-calls", type=int, default=None, help="Maximum attempted Makewand model tasks, including retries and reviewers")
    common_parser.add_argument("--request-id", default=None, help="Stable user-request ID shared by every CLI call; defaults to a total cap of 3")
    common_parser.add_argument("--caller", choices=["claude", "codex", "agy", "grok", "muse", "human", "automation"], default=None, help="Tool initiating this user request (metadata only)")
    common_parser.add_argument("--call-budget-file", default=None, help="Shared admission ledger for model task budgets")
    common_parser.add_argument("--effort", "--reasoning-effort", dest="effort", choices=["low", "medium", "high", "xhigh", "max", "ultra"], default=None, help="Set reasoning effort across AI models (low, medium, high, xhigh, max, ultra)")

    sub_common_parser = argparse.ArgumentParser(add_help=False)
    sub_common_parser.add_argument("-C", "--cwd", dest="cwd", default=argparse.SUPPRESS, help="Target working directory (default: current directory)")
    sub_common_parser.add_argument("-f", "-F", "--file", "--prompt-file", dest="prompt_file", default=argparse.SUPPRESS, help="Read task prompt from file")
    sub_common_parser.add_argument("--repo-trust", choices=["trusted", "untrusted"], default=argparse.SUPPRESS, help="Repository trust level: trusted or untrusted")
    sub_common_parser.add_argument("--max-model-calls", type=int, default=argparse.SUPPRESS, help="Maximum attempted Makewand model tasks")
    sub_common_parser.add_argument("--request-id", default=argparse.SUPPRESS, help="Stable shared user-request ID")
    sub_common_parser.add_argument("--caller", choices=["claude", "codex", "agy", "grok", "muse", "human", "automation"], default=argparse.SUPPRESS, help="Initiating tool (metadata only)")
    sub_common_parser.add_argument("--call-budget-file", default=argparse.SUPPRESS, help="Shared model task admission ledger")
    sub_common_parser.add_argument("--effort", "--reasoning-effort", dest="effort", choices=["low", "medium", "high", "xhigh", "max", "ultra"], default=argparse.SUPPRESS, help="Set reasoning effort across AI models (low, medium, high, xhigh, max, ultra)")

    for options, default in ((common_parser, None), (sub_common_parser, argparse.SUPPRESS)):
        options.add_argument("--risk", choices=["auto", "low", "high"],
                             default="auto" if default is None else default,
                             help="Risk of this task, recorded with the run")
        options.add_argument("--total-timeout", type=float, default=default,
                             help="Shared wall-clock deadline for every task stage, in seconds")

    parser = argparse.ArgumentParser(
        prog="makewand",
        description=f"Makewand {__version__}: dispatch work and independent reviews to the coding CLIs signed in on this machine",
        parents=[common_parser]
    )
    parser.add_argument("-v", "--version", action="version", version=f"makewand {__version__}")
    subparsers = parser.add_subparsers(dest="subcommand", help="Available subcommands")

    # models
    p_models = subparsers.add_parser("models", help="Discover and list current models across all AI ecosystems", parents=[sub_common_parser])
    p_models.add_argument("--refresh", action="store_true", default=False, help="Refresh the agy model cache now (60s timeout, ignores back-off) before listing")
    p_models.add_argument("--json", action="store_true", help="Show candidate provenance, model family, account and quota-pool metadata")

    # status
    p_status = subparsers.add_parser("status", help="Show health, official quota where it can be read, and reset times of all AIs", parents=[sub_common_parser])
    p_status.add_argument("--probe", action="store_true", help="Force immediate live probe of all CLIs (real model calls for claude/codex/muse/grok; agy makes one real call while an agy quota limit has not reset, or while a login/region failure that a real agy dispatch or an earlier real probe recorded is still live; otherwise only a version check)")
    p_status.add_argument("--json", action="store_true", default=False, help="Print machine-readable per-provider status/quota indicators")

    # probe
    p_probe = subparsers.add_parser("probe", help="Perform live probing on all AIs and update status cache", parents=[sub_common_parser])
    p_probe.add_argument("--json", action="store_true", default=False, help="Print machine-readable per-provider status/quota indicators")

    # quota
    p_quota = subparsers.add_parser("quota", help="Same as status", parents=[sub_common_parser])
    p_quota.add_argument("--probe", action="store_true", help="Force immediate live probe")
    p_quota.add_argument("--json", action="store_true", default=False, help="Print machine-readable per-provider status/quota indicators")

    # run
    # review
    p_rev = subparsers.add_parser("review", help="Review current git diff using Codex / Antigravity", parents=[sub_common_parser])
    p_rev.add_argument("--json", action="store_true", default=False, help="Output structured review verdicts in JSON format")
    p_rev.add_argument("--stream", action="store_true", default=False, help="Stream review output line-by-line")
    p_rev.add_argument("--timeout", type=int, default=None)
    p_rev.add_argument("--local-only", "--offline", dest="local_only", action="store_true", default=False, help="Strict local-only / 100%% offline mode: review diff using only local self-hosted model")

    # search (budgeted search guardrail)
    p_search = subparsers.add_parser("search", help="Budgeted fast search excluding cold archives and databases", parents=[sub_common_parser])
    p_search.add_argument("pattern", help="Regex or text pattern to search for")
    p_search.add_argument("--max-results", type=int, default=150, help="Maximum matches to return (default: 150)")
    p_search.add_argument("--max-depth", type=int, default=6, help="Maximum directory depth (default: 6)")

    # repomap (codebase architecture symbol map)
    p_repomap = subparsers.add_parser("repomap", help="Generate concise repository symbol map (AST/regex extracted)", parents=[sub_common_parser])
    p_repomap.add_argument("--max-lines", type=int, default=80, help="Max lines of repo map output (default: 80)")
    p_repomap.add_argument("--max-files", type=int, default=40, help="Max files to include in repo map (default: 40)")
    p_repomap.add_argument("--json", action="store_true", help="Output repo map in JSON format")

    # sandbox (bubblewrap process isolation)
    p_sb = subparsers.add_parser("sandbox", help="Run shell command inside bubblewrap process sandbox", parents=[sub_common_parser])
    p_sb.add_argument("--no-net", dest="allow_net", action="store_false", default=True, help="Block network inside sandbox")
    p_sb.add_argument("--timeout", type=int, default=120, help="Execution timeout in seconds")
    p_sb.add_argument("cmd", nargs=argparse.REMAINDER, help="Command to execute inside sandbox")

    p_claude = subparsers.add_parser("claude", help="Run prompt directly with Claude Code subscription", parents=[sub_common_parser])
    p_claude.add_argument("prompt", nargs="?", default=None, help="Prompt for Claude")
    p_claude.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_claude.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_claude.add_argument("--model", help="Specific model name")
    p_claude.add_argument("--stream", action="store_true", default=False)
    p_claude.add_argument("--timeout", type=int, default=None,
                             help="Per-call timeout in seconds (default: --total-timeout, otherwise 300)")
    p_claude.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_codex = subparsers.add_parser("codex", help="Run prompt directly with Codex CLI subscription", parents=[sub_common_parser])
    p_codex.add_argument("prompt", nargs="?", default=None, help="Prompt for Codex")
    p_codex.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_codex.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_codex.add_argument("--model", help="Specific model name")
    p_codex.add_argument("--stream", action="store_true", default=False)
    p_codex.add_argument("--timeout", type=int, default=None,
                             help="Per-call timeout in seconds (default: --total-timeout, otherwise 300)")
    p_codex.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_agy = subparsers.add_parser("agy", help="Run prompt directly with Antigravity CLI subscription", parents=[sub_common_parser])
    p_agy.add_argument("prompt", nargs="?", default=None, help="Prompt for Antigravity")
    p_agy.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_agy.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_agy.add_argument("--model", help="Specific model name")
    p_agy.add_argument("--stream", action="store_true", default=False)
    p_agy.add_argument("--timeout", type=int, default=None,
                             help="Per-call timeout in seconds (default: --total-timeout, otherwise 300)")
    p_agy.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_muse = subparsers.add_parser("muse", help="Run prompt directly with Muse Code subscription", parents=[sub_common_parser])
    p_muse.add_argument("prompt", nargs="?", default=None, help="Prompt for Muse Code")
    p_muse.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_muse.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_muse.add_argument("--model", help="Specific model name")
    p_muse.add_argument("--stream", action="store_true", default=False)
    p_muse.add_argument("--timeout", type=int, default=None,
                             help="Per-call timeout in seconds (default: --total-timeout, otherwise 300)")
    p_muse.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_grok = subparsers.add_parser("grok", help="Run prompt directly with Grok Build CLI (xAI subscription)", parents=[sub_common_parser])
    p_grok.add_argument("prompt", nargs="?", default=None, help="Prompt for Grok")
    p_grok.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_grok.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_grok.add_argument("--model", help="Specific model name")
    p_grok.add_argument("--stream", action="store_true", default=False)
    p_grok.add_argument("--timeout", type=int, default=None,
                             help="Per-call timeout in seconds (default: --total-timeout, otherwise 300)")
    p_grok.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_local = subparsers.add_parser("local", help="Run prompt directly with local self-hosted model (Ollama / vLLM, 0 token cost)", parents=[sub_common_parser])
    p_local.add_argument("prompt", nargs="?", default=None, help="Prompt for local model")
    p_local.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_local.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_local.add_argument("--model", help="Specific model name (e.g. gemma4:31b, llama3.2)")
    p_local.add_argument("--stream", action="store_true", default=False)
    p_local.add_argument("--timeout", type=int, default=None,
                             help="Per-call timeout in seconds (default: --total-timeout, otherwise 300)")
    p_local.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_observe = subparsers.add_parser("observe", help="Inspect all running AI sessions, classify behavior, and report makewand optimizations", parents=[sub_common_parser])
    p_observe.add_argument("--json", action="store_true", help="Output raw JSON format")
    p_observe.add_argument("--clean-hung", action="store_true", default=False,
                           help="List makewand-dispatched processes running over 30 minutes as cleanup candidates (nothing is killed without --confirm-pids)")
    p_observe.add_argument("--confirm-pids", default=None,
                           help="Comma-separated PIDs from the --clean-hung candidate list to SIGTERM (explicit confirmation)")

    # Candidate Lifecycle Subcommands
    # daemon (Resident Daemon / IPC Socket Fast-Path)
    # clean (cleanup stale /tmp restore, drill, and temporary artifact directories)
    p_clean = subparsers.add_parser("clean", help="Clean up stale /tmp restore, drill, and temporary artifact directories", parents=[sub_common_parser])
    p_clean.add_argument("--all", action="store_true", default=False, help="Clean all temporary makewand caches and artifacts")

    # route / mark / outcome (automatic dispatch bookkeeping; never call a model)
    p_route = subparsers.add_parser("route", allow_abbrev=False, help="为子任务在完整模型目录里选派活工具（只读账本和本地缓存，不调用模型）", parents=[sub_common_parser])
    p_route.add_argument("task_type", metavar="TYPE", help="子任务类型：review, implement, test, research, doc")
    p_route.add_argument("difficulty", metavar="DIFFICULTY", help="难度：mechanical, normal, hard")
    p_route.add_argument("--tier", dest="route_tier", type=int, default=None, metavar="N", help="直接指定档位（1, 2, 3），覆盖按难度推算的起始档")
    p_route.add_argument("--self", dest="self_tool", default=None, metavar="T", help="你自己是哪家工具（codex, claude, agy, grok, muse），同家候选排最后（含 agy 代理的同家族模型）")
    p_route.add_argument("--not", dest="not_tool", default=None, metavar="T", help="排除与该工具同家族的候选（含 agy 代理的同家族模型，如 claude-*、gpt-oss）")
    p_route.add_argument("--simple", action="store_true", default=False, help="只用精简三档表，不展开全部模型和推理深度")
    p_route.add_argument("--legacy", action="store_true", default=False, help="完整目录里把旧版本模型也纳入排序（与 --simple 一起用时无效）")
    p_route.add_argument("--json", action="store_true", default=False, help="输出机器可读的 JSON 结果")
    p_route.add_argument("--retry-approved", action="store_true", help="用户已批准重试，忽略本请求记录的操作失败冷却；不绕过额度、预算或暂停")

    p_mark = subparsers.add_parser("mark", allow_abbrev=False, help="记录一家工具额度受限，到期前不再派活", parents=[sub_common_parser])
    p_mark.add_argument("provider", metavar="PROVIDER", help="工具名：codex, claude, agy, grok, muse")
    p_mark.add_argument("duration", metavar="DURATION", help="受限时长，d/h/m/s 组合，如 6h、93h17m、2d、45m")
    p_mark.add_argument("--reason", default=None, help="受限原因（可选，路由跳过时会显示）")
    p_mark.add_argument("--model", dest="mark_model", default=None, help="只标记这一个模型，同一工具的其他模型照常参选")

    p_outcome = subparsers.add_parser("outcome", allow_abbrev=False, help="记录一次派活的验收成绩", parents=[sub_common_parser])
    p_outcome.add_argument("provider", metavar="PROVIDER", help="工具名：codex, claude, agy, grok, muse")
    p_outcome.add_argument("outcome_tier", metavar="TIER", type=int, help="档位（1, 2, 3）")
    p_outcome.add_argument("task_type", metavar="TYPE", help="子任务类型：review, implement, test, research, doc")
    p_outcome.add_argument("result", choices=["pass", "fail"], help="受委派工作是否通过验收；review 按审查有效性记，代码裁决用 --candidate-verdict")
    p_outcome.add_argument("--seconds", type=float, default=None, help="这次派活耗时秒数（可选）")
    p_outcome.add_argument("--calls", type=int, default=None, help="这次派活的模型调用次数（可选）")
    p_outcome.add_argument("--model", default=None, metavar="M", help="记入账本的模型名，供候选级统计（可选）")
    p_outcome.add_argument("--candidate-verdict", choices=["pass", "fail", "unverified"], help="代码裁决，独立于审查工作是否有效；仅 review 可用")
    p_outcome.add_argument("--candidate-hash", help="完整候选差异的 SHA256")
    p_outcome.add_argument("--failure-kind", choices=["startup", "auth", "quota", "timeout", "unknown", "invalid_output", "verification"], help="失败类别；候选被有效审查拒绝时不要把工具记 fail")

    p_budget = subparsers.add_parser("budget", allow_abbrev=False, help="查看或修复共享调用账本 call-budget.json 领先实际时间的时钟（不调用模型）", parents=[sub_common_parser])
    p_budget.add_argument("budget_action", metavar="ACTION", choices=["clock", "request"], help="clock：账本时钟；request：请求总调用上限与已用计数")
    p_budget.add_argument("--set-limit", type=int, help="用户已批准的新请求总上限；保留 ID 与已用计数，不能自动提高")
    p_budget.add_argument("--repair", action="store_true", default=False, help="让领先实际时间的时钟与日期桶回到实际时间（持账本锁，不退还已计的调用次数）")
    p_budget.add_argument("--json", action="store_true", default=False, help="输出机器可读的 JSON 结果")

    from makewand.config import get_all_supported_providers
    all_supported = get_all_supported_providers()

    p_enable = subparsers.add_parser("enable", help="Enable an AI tool provider", parents=[sub_common_parser])
    p_enable.add_argument("provider", help=f"Provider name or alias to enable: {', '.join(all_supported)}, all")
    p_enable.add_argument("--manage-service", action="store_true", default=False,
                          help="For local: also run 'sudo -n systemctl start ollama' (never done implicitly)")

    p_disable = subparsers.add_parser("disable", help="Disable an AI tool provider", parents=[sub_common_parser])
    p_disable.add_argument("provider", help=f"Provider name or alias to disable: {', '.join(all_supported)}")
    p_disable.add_argument("--manage-service", action="store_true", default=False,
                           help="For local: also run 'sudo -n systemctl stop ollama' (never done implicitly)")

    p_aider = subparsers.add_parser("aider", help="Run prompt directly with Aider CLI pair programmer", parents=[sub_common_parser])
    p_aider.add_argument("prompt", nargs="?", default=None, help="Prompt for Aider")
    p_aider.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_aider.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_aider.add_argument("--model", help="Specific model name")
    p_aider.add_argument("--stream", action="store_true", default=False)
    p_aider.add_argument("--timeout", type=int, default=None,
                             help="Per-call timeout in seconds (default: --total-timeout, otherwise 300)")
    p_aider.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_deepseek = subparsers.add_parser("deepseek", help="Run prompt directly with DeepSeek API", parents=[sub_common_parser])
    p_deepseek.add_argument("prompt", nargs="?", default=None, help="Prompt for DeepSeek")
    p_deepseek.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_deepseek.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_deepseek.add_argument("--model", help="Specific model name (e.g. deepseek-chat, deepseek-reasoner)")
    p_deepseek.add_argument("--stream", action="store_true", default=False)
    p_deepseek.add_argument("--timeout", type=int, default=None,
                             help="Per-call timeout in seconds (default: --total-timeout, otherwise 300)")
    p_deepseek.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_qwen = subparsers.add_parser("qwen", help="Run prompt directly with Aliyun Qwen API", parents=[sub_common_parser])
    p_qwen.add_argument("prompt", nargs="?", default=None, help="Prompt for Qwen")
    p_qwen.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_qwen.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_qwen.add_argument("--model", help="Specific model name (e.g. qwen2.5-coder-32b-instruct, qwen-max)")
    p_qwen.add_argument("--stream", action="store_true", default=False)
    p_qwen.add_argument("--timeout", type=int, default=None,
                             help="Per-call timeout in seconds (default: --total-timeout, otherwise 300)")
    p_qwen.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    for api_provider, label in (("glm", "Zhipu GLM API"), ("kimi", "Moonshot Kimi API"),
                                ("openrouter", "OpenRouter API"), ("siliconflow", "SiliconFlow API")):
        p_api = subparsers.add_parser(api_provider, help=f"Run prompt directly with {label} (text only; does not edit files)", parents=[sub_common_parser])
        p_api.add_argument("prompt", nargs="?", default=None, help=f"Prompt for {label}")
        p_api.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
        p_api.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
        p_api.add_argument("--model", help="Specific model name")
        p_api.add_argument("--stream", action="store_true", default=False)
        p_api.add_argument("--timeout", type=int, default=None,
                                 help="Per-call timeout in seconds (default: --total-timeout, otherwise 300)")
        p_api.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    for detect_only in ("cursor", "copilot"):
        p_det = subparsers.add_parser(detect_only, help=f"{detect_only}: detected only, no execution adapter yet", parents=[sub_common_parser])
        p_det.add_argument("prompt", nargs="*", help=argparse.SUPPRESS)

    # A bare word used to start the pipeline. Anything that is not a command is
    # now refused, so a mistyped command can never become a prompt for a model.
    args = parser.parse_args()
    try:
        _resolve_direct_timeout(args)
    except ValueError as error:
        parser.error(str(error))

    # A provider CLI that makewand launched (claude/codex/agy/...) can read its own
    # global instructions or skills telling it to call makewand. Refuse anything
    # that would dispatch models again so nested dispatch can never loop.
    refusal = _provider_worker_refusal(args.subcommand, sys.argv[1:])
    if refusal:
        print(c(refusal, COLOR_RED), file=sys.stderr)
        sys.exit(EXIT_USAGE_ERROR)
    if os.environ.get(PROVIDER_WORKER_ENV) == "1":
        # Audited repository queries bypass config/credential loading, inherited
        # budgets, daemon dispatch and observer initialization (all may write).
        args.cwd = os.path.abspath(args.cwd or os.getcwd())
        return {"search": cmd_search, "repomap": cmd_repomap}[args.subcommand](args)
    # Validate policy before discovery, daemon dispatch or orchestration.
    # Those paths may catch transport errors, so policy validity must already
    # be established before any provider can be started.
    from makewand.config import load_user_config
    load_user_config()
    from makewand.workflow import choose_workflow
    try:
        selected = choose_workflow("auto", args.risk)
        if hasattr(args, "timeout") and args.timeout <= 0:
            raise ValueError("--timeout must be positive")
    except ValueError as error:
        parser.error(str(error))
    if hasattr(args, "tier") and args.tier:
        args.tier = normalize_tier(args.tier)

    try:
        inherited_request = os.environ.get("MAKEWAND_TASK_ID") or None
        explicit_request = args.request_id is not None
        request_id = args.request_id if args.request_id is not None else inherited_request
        if request_id is not None:
            from makewand.execution_contract import identifier
            identifier(request_id, "request_id")
        if args.request_id is not None and inherited_request is not None and args.request_id != inherited_request:
            raise ValueError("--request-id cannot replace an inherited user request")
        if args.request_id is not None:
            os.environ["MAKEWAND_TASK_ID"] = args.request_id
        args.request_id = request_id
        args.caller = args.caller or os.environ.get("MAKEWAND_CALLER") or None
        if args.caller is not None:
            from makewand.outcome_ledger import CALLERS
            if args.caller not in CALLERS:
                raise ValueError("invalid MAKEWAND_CALLER")
            os.environ["MAKEWAND_CALLER"] = args.caller
        dispatch_subcommands = PROMPT_COMMANDS | {"review", None}
        if (explicit_request or (request_id is not None and args.subcommand in dispatch_subcommands)) and args.max_model_calls is None and inherited_maximum is None:
            args.max_model_calls = 3
            # The ledger keeps this apart from a limit somebody chose.
            os.environ["MAKEWAND_MAX_MODEL_CALLS_DEFAULTED"] = "1"
        elif args.max_model_calls is not None:
            os.environ.pop("MAKEWAND_MAX_MODEL_CALLS_DEFAULTED", None)
        inherited_limit = int(inherited_maximum) if inherited_maximum is not None else None
        if inherited_limit is not None and inherited_limit <= 0:
            raise ValueError("MAKEWAND_MAX_MODEL_CALLS must be positive")
        if args.max_model_calls is not None and args.max_model_calls <= 0:
            raise ValueError("--max-model-calls must be positive")
        parent_budget = str(Path(original_cwd, Path(inherited_budget_file).expanduser()).resolve()) if inherited_budget_file else None
        explicit_budget = str(Path(original_cwd, Path(args.call_budget_file).expanduser()).resolve()) if args.call_budget_file else None
        if parent_budget and explicit_budget and parent_budget != explicit_budget:
            raise ValueError("--call-budget-file cannot replace an inherited model task ledger")
    except (OSError, TypeError, ValueError) as error:
        parser.error(str(error))
    if args.max_model_calls is not None:
        args.max_model_calls = min(args.max_model_calls, inherited_limit) if inherited_limit is not None else args.max_model_calls
        if not explicit_budget and not parent_budget:
            from makewand.config import ensure_config_dir
            ensure_config_dir()
            from makewand.layered_budget import default_path
            explicit_budget = default_path()
            os.environ["MAKEWAND_LAYERED_BUDGET"] = "1"
        os.environ["MAKEWAND_MAX_MODEL_CALLS"] = str(args.max_model_calls)
    elif inherited_limit is not None:
        os.environ["MAKEWAND_MAX_MODEL_CALLS"] = str(inherited_limit)
    if parent_budget or explicit_budget:
        os.environ["MAKEWAND_LAYERED_BUDGET"] = "1"
        args.call_budget_file = parent_budget or explicit_budget
        os.environ["MAKEWAND_CALL_BUDGET_FILE"] = args.call_budget_file

    if args.call_budget_file:
        from makewand.execution_runtime import initialize_cli_task
        initialize_cli_task()

    # Handle directory redirection (-C / --cwd) across all commands
    if hasattr(args, "cwd") and args.cwd:
        args.cwd = os.path.abspath(args.cwd)
        try:
            os.chdir(args.cwd)
        except OSError as e:
            print(c(f"❌ 无法切换工作目录到 {args.cwd}: {e}", COLOR_RED), file=sys.stderr)
            sys.exit(EXIT_USAGE_ERROR)
    elif not getattr(args, "cwd", None):
        args.cwd = os.getcwd()

    if not args.subcommand:
        parser.print_help()
        sys.exit(0)

    # Subcommands that consume prompt text (from argument, -f file, or stdin pipe)
    if args.subcommand in PROMPT_COMMANDS:
        args.prompt = _resolve_cli_prompt(args, original_cwd=original_cwd, required=True)

    from makewand.execution_runtime import execution_context
    from makewand.workflow import remember_result
    from makewand.telemetry import stage
    remember_result(None)
    deadline = math.ceil(time.time() * 1000 + args.total_timeout * 1000) if args.total_timeout is not None else None
    workflow = "review" if args.subcommand == "review" else "direct"
    with execution_context(deadline_unix_ms=deadline, workflow=workflow, risk=selected.risk):
        # Publish the deadline after preflight/prompt reads, immediately before
        # dispatch. With no total budget, allow 30s for workflow startup/teardown.
        if args.subcommand != "sandbox":
            from makewand.observer import mark_process_as_dispatcher
            from makewand.execution_runtime import current_context
            observed_deadline = current_context().get("deadline_unix_ms")
            duration = _command_wall_timeout(args)
            if observed_deadline is None and duration is not None:
                observed_deadline = math.ceil((time.time() + duration + 30) * 1000)
            mark_process_as_dispatcher(deadline_unix_ms=observed_deadline)
        exit_signal = None
        with stage("command") as span:
            try:
                result = _dispatch_parsed_command(args)
            except SystemExit as signal:
                exit_signal = signal
                from makewand.execution_contract import STATUS_CODES
                code = signal.code if signal.code is not None else 0
                status = next((name for name, number in STATUS_CODES.items() if number == code), "UNKNOWN")
                span.finish(status=status)
                result = None
            else:
                from makewand.workflow import last_result
                outcome = last_result()
                span.finish(status=getattr(outcome, "status", "PASSED" if outcome is None or outcome[0] else "FAILED"))
        if exit_signal is not None:
            raise exit_signal
        return result


def _dispatch_parsed_command(args):

    if args.subcommand == "route":
        from makewand.route_cli import cmd_route
        cmd_route(args)
    elif args.subcommand == "mark":
        from makewand.route_cli import cmd_mark
        cmd_mark(args)
    elif args.subcommand == "outcome":
        from makewand.route_cli import cmd_outcome
        cmd_outcome(args)
    elif args.subcommand == "budget":
        from makewand.budget_cli import cmd_budget
        cmd_budget(args)
    elif args.subcommand == "models":
        cmd_models(args)
    elif args.subcommand == "status":
        cmd_status(args)
    elif args.subcommand == "probe":
        args.probe = True
        cmd_status(args)
    elif args.subcommand == "quota":
        cmd_quota(args)
    elif args.subcommand == "review":
        exit_code = run_review(
            cwd=args.cwd,
            stream=args.stream,
            timeout=args.timeout,
            effort=getattr(args, "effort", None),
            output_json=getattr(args, "json", False),
            repo_trust=getattr(args, "repo_trust", "trusted"),
            local_only=getattr(args, "local_only", False)
        )
        sys.exit(exit_code if exit_code is not None else 0)
    elif args.subcommand == "clean":
        cmd_clean(args)
    elif args.subcommand == "enable":
        cmd_enable(args)
    elif args.subcommand == "disable":
        cmd_disable(args)
    elif args.subcommand == "search":
        cmd_search(args)
    elif args.subcommand == "repomap":
        cmd_repomap(args)
    elif args.subcommand == "sandbox":
        cmd_sandbox(args)
    elif args.subcommand == "claude":
        ok, out, err = _execute_direct_task("claude", execute_claude_task, args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, effort=getattr(args, "effort", None), stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted"))
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(_execution_exit_code())
    elif args.subcommand == "codex":
        ok, out, err = _execute_direct_task("codex", execute_codex_task, args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, effort=getattr(args, "effort", None), stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted"))
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(_execution_exit_code())
    elif args.subcommand == "agy":
        ok, out, err = _execute_direct_task("agy", execute_agy_task, args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, effort=getattr(args, "effort", None), stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted"))
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(_execution_exit_code())
    elif args.subcommand == "muse":
        ok, out, err = _execute_direct_task("muse", execute_muse_task, args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, effort=getattr(args, "effort", None), stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted"))
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(_execution_exit_code())
    elif args.subcommand == "grok":
        ok, out, err = _execute_direct_task("grok", execute_grok_task, args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, effort=getattr(args, "effort", None), stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted"))
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(_execution_exit_code())
    elif args.subcommand == "local":
        from makewand.providers.local import execute_local_task
        ok, out, err = _execute_direct_task("local", execute_local_task, args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, effort=getattr(args, "effort", None), stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted"))
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(_execution_exit_code())
    elif args.subcommand == "aider":
        from makewand.providers.aider import execute_aider_task
        ok, out, err = _execute_direct_task("aider", execute_aider_task, args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted"))
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(1)
    elif args.subcommand in API_ONLY_ENGINES:
        from makewand.orchestrator import dispatch_task
        from makewand.workflow import remember_result
        ok, out, err = remember_result(dispatch_task(args.subcommand, args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, effort=getattr(args, "effort", None), stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted")))
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(_execution_exit_code())
    elif args.subcommand in ("cursor", "copilot"):
        print(c(f"❌ {args.subcommand} 目前只做安装检测，尚无执行适配器，无法直接派发任务。"
                "请改用 claude/codex/agy/grok/muse/aider 或 API provider。", COLOR_RED), file=sys.stderr)
        sys.exit(EXIT_USAGE_ERROR)
    elif args.subcommand == "observe":
        from makewand.observer import observe_all_dialogs, format_observation_markdown
        confirm_raw = getattr(args, "confirm_pids", None)
        confirm_pids = []
        if confirm_raw:
            if not getattr(args, "clean_hung", False):
                print(c("❌ --confirm-pids 只能与 --clean-hung 一起使用", COLOR_RED), file=sys.stderr)
                sys.exit(EXIT_USAGE_ERROR)
            try:
                confirm_pids = [int(p) for p in confirm_raw.replace(" ", "").split(",") if p]
            except ValueError:
                print(c(f"❌ --confirm-pids 需要逗号分隔的 PID 列表，收到: {confirm_raw}", COLOR_RED), file=sys.stderr)
                sys.exit(EXIT_USAGE_ERROR)
        rep = observe_all_dialogs(clean_hung=getattr(args, "clean_hung", False), confirm_pids=confirm_pids)
        if getattr(args, "json", False):
            import json
            print(json.dumps(rep, ensure_ascii=False, indent=2))
        else:
            print(format_observation_markdown(rep))
            if getattr(args, "clean_hung", False):
                candidates = rep.get("hung_candidates") or []
                if not candidates:
                    print(c("\n✔ 没有发现 makewand 派发且运行超过 30 分钟的进程；不会处理任何交互会话或用户自己的进程。", COLOR_GREEN))
                elif not confirm_pids:
                    pid_list = ",".join(str(cand["pid"]) for cand in candidates)
                    print(c("\n⚠ 以上为清理候选 (仅限 makewand 自己派发、无控制终端、运行超过 30 分钟)，尚未发送任何信号。", COLOR_YELLOW))
                    print(f"  确认后执行: makewand observe --clean-hung --confirm-pids {pid_list}")
            if rep.get("cleaned_pids"):
                print(c("\n🧹 已向确认的 makewand 派发进程发送 SIGTERM:", COLOR_GREEN + COLOR_BOLD))
                for cp in rep["cleaned_pids"]:
                    print(f"  • PID {cp['pid']} ({cp['comm']}, dispatch {cp['dispatch_id']})")
            skipped = sorted(set(confirm_pids) - {cp["pid"] for cp in rep.get("cleaned_pids", [])})
            if skipped:
                print(c(f"  跳过的 PID (不是当前候选或已退出): {', '.join(map(str, skipped))}", COLOR_YELLOW))

if __name__ == "__main__":
    main()
