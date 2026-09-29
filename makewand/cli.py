"""
Makewand CLI: Command-line interface and subcommand parsers.
"""

import os
import sys
import argparse
from pathlib import Path
from typing import List, Optional, Dict, Any
from makewand import __version__
from makewand.config import (
    c,
    COLOR_BOLD,
    COLOR_CYAN,
    COLOR_GREEN,
    COLOR_YELLOW,
    COLOR_RED,
    COLOR_BLUE,
    COLOR_PURPLE,
    COLOR_RESET,
    normalize_tier,
    tier_to_go_mode,
)
from makewand.health import get_or_update_status
from makewand.discovery import discover_available_models
from makewand.orchestrator import (
    run_pipeline,
    run_review,
    run_race,
    select_optimal_engine_pair,
    EXIT_PASSED,
    EXIT_FAILED,
    EXIT_UNVERIFIED,
    EXIT_USAGE_ERROR,
    EXIT_APPLY_CONFLICT,
)
from makewand.providers.agy import execute_agy_task
from makewand.providers.claude import execute_claude_task
from makewand.providers.codex import execute_codex_task
from makewand.providers.muse import execute_muse_task
from makewand.providers.grok import execute_grok_task

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
            },
        }
    return {
        "engine": "python",
        "version": __version__,
        "api_policy": get_api_policy(),
        "quota_semantics": ("Python 入口的额度数值除 source=official (CLI 输出中的官方百分比) 外，均来自本地调用计数估算"
                            "或健康状态推定，不是官方 5 小时/每周剩余额度；官方额度读取仅在 Go 组件 'makewand-server quota' 中实现"),
        "stale_after_seconds": STATUS_STALE_SECONDS,
        "providers": providers,
    }


def cmd_status(args):
    import shutil
    from makewand.config import (
        ALL_SUPPORTED_PROVIDERS,
        get_all_supported_providers,
        get_active_providers,
        get_provider_execution_mode,
        is_provider_enabled,
        normalize_provider_name
    )

    if getattr(args, "json", False):
        import json
        cache = get_or_update_status(force_probe=getattr(args, "probe", False))
        print(json.dumps(build_status_json(cache), ensure_ascii=False, indent=2))
        return

    print(c("\n============================================================", COLOR_BOLD))
    print(c("       Makewand Multi-Model AI 订阅与全工具拓扑看板", COLOR_BOLD + COLOR_CYAN))
    print(c("============================================================\n", COLOR_BOLD))

    from makewand.config import get_api_policy
    policy = get_api_policy()
    print("API 费用策略: " + ("allow_paid（允许云 API 按量计费）" if policy == "allow_paid" else "subscription_only（禁止 Makewand 云 API 调用）"))
    print(c("额度说明: 除标注“官方报告”外，下方额度条为本地调用计数估算或由健康状态推定，不是官方剩余配额；"
            "官方 5 小时/每周额度读取仅在 Go 组件 'makewand-server quota' 中实现。", COLOR_YELLOW))
    cache = get_or_update_status(force_probe=getattr(args, "probe", False))

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

    display_names = {
        "agy": "Antigravity (Google AI Pro / Gemini 3.8)",
        "claude": "Claude Code (Anthropic Subscription)",
        "codex": "Codex CLI (OpenAI Subscription / gpt-6-astra)",
        "grok": "Grok Build CLI (xAI Subscription / grok-4.7)",
        "muse": "Muse Code (Meta Subscription / Llama 4)",
        "aider": "Aider CLI (Pair Programmer CLI)",
        "cursor": "Cursor Agent (Cursor Subscription CLI)",
        "copilot": "GitHub Copilot (CLI / gh copilot)",
        "deepseek": "DeepSeek API (deepseek-chat / deepseek-reasoner)",
        "qwen": "Aliyun Qwen API (通义千问 / qwen2.5-coder / qwen-max)",
        "glm": "Zhipu GLM API (智谱清言 / glm-4-plus)",
        "kimi": "Moonshot Kimi API (moonshot-v1)",
        "openrouter": "OpenRouter API (Multi-model Gateway)",
        "siliconflow": "SiliconFlow API (硅基流动 / SiliconCloud)",
        "local": "Local Self-Hosted (本地大模型 / Ollama / vLLM)"
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
        from makewand.health import calculate_provider_quota, format_quota_bar, get_reauth_hint
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
                print(f"      额度指示: 未探测  ({quota_desc})")
            else:
                bar = format_quota_bar(pct, width=20)
                print(f"      额度指示: {bar}  ({quota_desc})")
            if reason and reason != quota_desc and not (status == "healthy" and "运行正常" in reason and "运行正常" in quota_desc):
                print(f"      运行状态: {reason}")
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
    if len(active_tools) >= 2:
        coders, reviewers, meta = select_optimal_engine_pair("general task")
        coder = coders[0] if coders else active_tools[0]
        reviewer = reviewers[0] if reviewers else (coders[1] if len(coders) > 1 else coder)
        print(c(f"  🌟 多模型联合编排模式 (活跃工具数: N={len(active_tools)}):", COLOR_GREEN + COLOR_BOLD))
        print(f"     • 活跃工具池: {c(', '.join(active_tools), COLOR_CYAN)}")
        print(f"     • 推荐通用分工: 主力实现 [{c(coder, COLOR_BOLD)}] -> 独立跨模型红队盲审 [{c(reviewer, COLOR_BOLD)}]")
        print(f"     • 调度机制: 自动按任务类型 (代码/架构/审查/自愈) 进行领域自适应，零人工配置负担。")
    elif len(active_tools) == 1:
        single = active_tools[0]
        print(c(f"  ⚡ 韧性单工具独立闭环模式 (活跃工具数: N=1):", COLOR_YELLOW + COLOR_BOLD))
        print(f"     • 活跃唯一工具: {c(single, COLOR_BOLD + COLOR_GREEN)}")
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
    print(c(f"✔ 已成功禁用 AI 工具: {target} (Makewand 调度流水线将不再向其派发任务)", COLOR_YELLOW + COLOR_BOLD))
    if target == "local":
        if getattr(args, "manage_service", False):
            if _manage_ollama_service("stop"):
                print(c("  ✔ 已按 --manage-service 停止本地 Ollama 服务", COLOR_GREEN))
        else:
            print(f"  提示: Ollama 系统服务保持原状；如需停止请执行 '{_ollama_service_hint('stop')}' (或加 --manage-service)")
    print(f"  提示: 随时可运行 'makewand enable {target}' 重新启用。")


def cmd_models(args):
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
    print("   调用方式: 按档位传别名 (fable/sonnet/haiku)，具体版本由 Claude Code 解析。\n")

    print(c("2. Codex CLI (OpenAI 订阅):", COLOR_BOLD + COLOR_CYAN))
    _model_lines("codex", "~/.codex/config.toml")
    print("   调用方式: 读取 config.toml 中的 model，否则使用内置默认。\n")

    print(c("3. Antigravity (Google AI Pro):", COLOR_BOLD + COLOR_GREEN))
    _model_lines("agy", "agy 模型缓存 (当前未实现检测)")
    print("   调用方式: 使用内置默认模型名与 --effort；未做在线模型发现。\n")

    print(c("4. Muse Code (Meta 订阅):", COLOR_BOLD + COLOR_PURPLE))
    _model_lines("muse", "~/.config/muse/settings.json")
    print("   调用方式: 支持 --preset 与 --reasoning-effort。\n")

    print(c("5. Grok Build CLI (xAI 订阅):", COLOR_BOLD + COLOR_RED))
    _model_lines("grok", "~/.grok/models_cache.json")
    print("   调用方式: 读取 models_cache.json，否则使用内置默认。\n")

    print(c("6. Local Self-Hosted (本地大模型 / Ollama / vLLM):", COLOR_BOLD + COLOR_PURPLE))
    try:
        from makewand.providers.local import is_local_model_available, get_default_local_model, list_local_models
        avail, _, _ = is_local_model_available()
        if avail:
            print(f"   当前默认: {c(get_default_local_model(), COLOR_GREEN + COLOR_BOLD)}")
            print(f"   检测到可用模型: {', '.join(list_local_models())}")
            print("   自适应机制: 使用配置的本地模型端点；数据去向取决于该端点配置。\n")
        else:
            print("   状态: 未检测到本地 Ollama / vLLM 服务 (http://localhost:11434 未响应)\n")
    except Exception as e:
        print(f"   状态: 检测异常 ({e})\n")

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

def cmd_plan(args):
    """Decompose complex goals into DAG tasks and execute topologically."""
    import json
    from makewand.orchestrator import decompose_task_to_dag, execute_task_dag
    cwd = getattr(args, "cwd", None) or os.getcwd()
    prompt = args.prompt
    tier = getattr(args, "tier", "deep")
    repo_trust = getattr(args, "repo_trust", "trusted")

    dag = decompose_task_to_dag(prompt, cwd=cwd, tier=tier, local_only=getattr(args, "local_only", False))

    if getattr(args, "json", False):
        print(json.dumps(dag.to_dict(), indent=2, ensure_ascii=False))
    else:
        dag.render_terminal()

    if getattr(args, "execute", False):
        print(c("🚀 [Makewand DAG Engine] 正在拓扑推进执行流水线...", COLOR_BOLD + COLOR_GREEN))
        ok, summary, stage_results = execute_task_dag(
            dag,
            cwd=cwd,
            tier=tier,
            repo_trust=repo_trust,
            tiered=getattr(args, "tiered", False),
            architect_engine=getattr(args, "architect", None),
            worker_engine=getattr(args, "worker", None),
            local_only=getattr(args, "local_only", False),
        )
        if not ok:
            print(c(f"\n❌ [Makewand DAG Engine] 流水线执行未完全通过: {summary}", COLOR_BOLD + COLOR_RED))
            sys.exit(1)
        print(c(f"\n✔ [Makewand DAG Engine] 全部 DAG 拓扑阶段均已高质量交付验收！", COLOR_BOLD + COLOR_GREEN))
        sys.exit(0)

def cmd_aci(args):
    """LLM-dedicated Agent-Computer Interface commands (SWE-agent inspired)."""
    from makewand.aci import view_window, search_code
    cwd = getattr(args, "cwd", None) or os.getcwd()
    if args.action == "view":
        print(view_window(args.target, line_number=args.line, cwd=cwd))
    elif args.action in ("search", "grep"):
        print(search_code(args.target, cwd=cwd))

def cmd_mcp(args):
    """Model Context Protocol commands."""
    import json
    from makewand.mcp import MCPClient
    cwd = getattr(args, "cwd", None) or os.getcwd()
    client = MCPClient(args.server_cmd, cwd=cwd)
    ok, msg = client.initialize()
    if not ok:
        print(c(f"❌ MCP 初始化失败: {msg}", COLOR_RED + COLOR_BOLD))
        sys.exit(1)
    try:
        if args.action == "list":
            tools = client.list_tools()
            server_name = client.server_info.get("name", "unknown")
            server_ver = client.server_info.get("version", "")
            print(c(f"✔ 成功连接 MCP 服务端: {server_name} v{server_ver}", COLOR_GREEN + COLOR_BOLD))
            print(f"发现可用工具 ({len(tools)} 个):\n")
            for t in tools:
                print(f"  • {c(t.get('name', ''), COLOR_BOLD + COLOR_CYAN)}: {t.get('description', '')}")
        elif args.action == "call":
            if not getattr(args, "tool", None):
                print(c("❌ 请指定要调用的工具名称: --tool <name>", COLOR_RED))
                sys.exit(1)
            raw_args = {}
            if getattr(args, "args", None):
                try:
                    raw_args = json.loads(args.args)
                except Exception as e:
                    print(c(f"❌ 参数 JSON 解析失败: {e}", COLOR_RED))
                    sys.exit(1)
            res = client.call_tool(args.tool, raw_args)
            print(json.dumps(res, indent=2, ensure_ascii=False))
    finally:
        client.close()

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

def cmd_candidates(args):
    from makewand.candidate import CandidateManager
    races = CandidateManager.list_races()
    if not races:
        print("当前没有任何封存的候选工作区。通过 'makewand race <任务>' 发起竞速即可产生候选。")
        return
    print(c("\n============================================================", COLOR_BOLD))
    print(c("             Makewand 竞速候选工作区列表", COLOR_BOLD + COLOR_CYAN))
    print(c("============================================================\n", COLOR_BOLD))
    for r in races:
        r_id = r.get("race_id")
        created = r.get("created_at", "")[:19]
        prompt = r.get("prompt", "")
        winner = r.get("winner") or "未决"
        a_mod = r.get("candidates", {}).get("A", {}).get("model", "A")
        b_mod = r.get("candidates", {}).get("B", {}).get("model", "B")
        print(f"• ID: {c(r_id, COLOR_YELLOW + COLOR_BOLD)}  时间: {created}")
        print(f"  任务: {prompt}")
        print(f"  对决: 选手 A ({a_mod}) vs 选手 B ({b_mod}) | 推荐胜出: {c(winner, COLOR_GREEN + COLOR_BOLD)}")
        print(f"  操作: makewand inspect {r_id} | makewand apply {r_id} | makewand discard {r_id}\n")

def cmd_inspect(args):
    from makewand.candidate import CandidateManager
    race = CandidateManager.get_race(args.race_id)
    if not race:
        print(c(f"未找到候选记录: {args.race_id or '最新'}", COLOR_RED))
        sys.exit(EXIT_USAGE_ERROR)

    print(c("\n============================================================", COLOR_BOLD))
    print(c(f"         Makewand 候选方案详情 [{race.get('race_id')}]", COLOR_BOLD + COLOR_CYAN))
    print(c("============================================================\n", COLOR_BOLD))
    print(f"原始任务: {c(race.get('prompt', ''), COLOR_BOLD)}")
    print(f"工作目录: {race.get('base_cwd')}")
    print(f"创建时间: {race.get('created_at', '')[:19]}")
    winner = race.get("winner")
    if winner:
        print(f"主裁推荐: {c(f'选手 {winner}', COLOR_GREEN + COLOR_BOLD)}")
    print()

    cand = (args.candidate.upper() if args.candidate else None)
    cand_a = race.get("candidates", {}).get("A", {})
    cand_b = race.get("candidates", {}).get("B", {})

    if cand == "A":
        pars_a = cand_a.get("parsimony", {})
        if pars_a:
            print(c(f"--- 选手 A ({cand_a.get('model')}) 补丁精简度: {pars_a.get('summary', '')} ---", COLOR_CYAN + COLOR_BOLD))
        print(c(f"--- 选手 A ({cand_a.get('model')}) 改动详情 (git diff) ---", COLOR_CYAN + COLOR_BOLD))
        diff = cand_a.get("diff", "")
        print(diff if diff else "无有效代码变更")
    elif cand == "B":
        pars_b = cand_b.get("parsimony", {})
        if pars_b:
            print(c(f"--- 选手 B ({cand_b.get('model')}) 补丁精简度: {pars_b.get('summary', '')} ---", COLOR_BLUE + COLOR_BOLD))
        print(c(f"--- 选手 B ({cand_b.get('model')}) 改动详情 (git diff) ---", COLOR_BLUE + COLOR_BOLD))
        diff = cand_b.get("diff", "")
        print(diff if diff else "无有效代码变更")
    else:
        print(c("--- 两位选手表现对比 ---", COLOR_BOLD))
        pars_a = cand_a.get("parsimony", {})
        pars_b = cand_b.get("parsimony", {})
        pars_info_a = f", 精简度={pars_a.get('parsimony_ratio', 1.0):.2f}" if pars_a else ""
        pars_info_b = f", 精简度={pars_b.get('parsimony_ratio', 1.0):.2f}" if pars_b else ""
        print(f"选手 A [{cand_a.get('model')}]: 耗时={cand_a.get('duration')}s, Diff大小={len(cand_a.get('diff', ''))} 字节{pars_info_a}, 状态={'成功' if cand_a.get('success') else '失败'}")
        print(f"选手 B [{cand_b.get('model')}]: 耗时={cand_b.get('duration')}s, Diff大小={len(cand_b.get('diff', ''))} 字节{pars_info_b}, 状态={'成功' if cand_b.get('success') else '失败'}")
        print()
        if race.get("judge_report"):
            print(c("--- 裁判裁决报告 ---", COLOR_BOLD))
            print(race.get("judge_report").strip())
        print(f"\n提示: 使用 'makewand inspect {race.get('race_id')} --candidate A|B' 查看完整代码差异。")

def cmd_apply(args):
    from makewand.candidate import CandidateManager
    ok, files, msg = CandidateManager.apply_candidate(
        race_id=args.race_id,
        candidate_label=args.candidate,
        dry_run=args.dry_run,
        force=args.force
    )
    if not ok:
        print(c(f"❌ {msg}", COLOR_RED + COLOR_BOLD))
        if "冲突" in msg:
            sys.exit(EXIT_APPLY_CONFLICT)
        sys.exit(EXIT_FAILED)

    print(c(f"✔ {msg}", COLOR_GREEN + COLOR_BOLD))
    if files:
        for f in files:
            print(f"  • {f}")
    sys.exit(EXIT_PASSED)

def cmd_discard(args):
    from makewand.candidate import CandidateManager
    ok, msg = CandidateManager.discard_race(race_id=args.race_id, all_races=args.all)
    if ok:
        print(c(f"✔ {msg}", COLOR_GREEN))
    else:
        print(c(f"❌ {msg}", COLOR_RED))

def _normalize_version(text: str) -> str:
    return text.strip().lstrip("vV")


def read_go_binary_version(bin_path: str, timeout: float = 5.0) -> Optional[str]:
    """Returns the version a Go makewand binary reports via --version (cobra format)."""
    import re as _re
    import subprocess
    try:
        res = subprocess.run([bin_path, "--version"], capture_output=True, text=True, timeout=timeout)
    except (OSError, subprocess.SubprocessError):
        return None
    match = _re.search(r"\bversion\s+(\S+)", f"{res.stdout}\n{res.stderr}")
    return match.group(1) if match else None


def check_go_python_version(bin_path: str) -> Optional[str]:
    """
    Compares the Go component's version with this Python engine's version.
    Returns a warning string on mismatch/unknown, None when they agree.
    Set MAKEWAND_SKIP_VERSION_CHECK=1 to skip.
    """
    if os.environ.get("MAKEWAND_SKIP_VERSION_CHECK") == "1":
        return None
    go_version = read_go_binary_version(bin_path)
    if go_version is None:
        return (f"⚠ 无法读取 Go 组件版本 ({bin_path} --version)，无法确认其与 Python 引擎 {__version__} 一致；"
                "建议重新运行 scripts/install.sh")
    if _normalize_version(go_version).startswith("dev"):
        return (f"⚠ Go 组件 {bin_path} 为未标记版本的开发构建 ({go_version})，无法确认与 Python 引擎 {__version__} 一致；"
                "发行安装请运行 scripts/install.sh")
    if _normalize_version(go_version) != _normalize_version(__version__):
        return (f"⚠ 版本不一致：Go 组件 {go_version} ({bin_path}) ≠ Python 引擎 {__version__}；"
                "两套引擎可能行为不一致，请重新运行 scripts/install.sh 以同步")
    return None


def _find_go_binary() -> Optional[str]:
    import shutil
    root = Path(__file__).resolve().parent.parent
    candidates = [
        root / "bin" / "makewand-server",
        root / "bin" / "makewand-go",
        root / "dist" / "makewand",
        shutil.which("makewand-server"),
        shutil.which("makewand-go")
    ]
    return next((str(c) for c in candidates if c and Path(c).is_file() and os.access(c, os.X_OK)), None)


def _build_dev_go_binary() -> Optional[str]:
    """MAKEWAND_DEV=1 only: build the source tree into a cache binary (no `go run` in the user's cwd)."""
    import shutil
    import subprocess
    root = Path(__file__).resolve().parent.parent
    if not shutil.which("go") or not (root / "cmd" / "makewand").is_dir():
        return None
    cache_root = Path(os.environ.get("XDG_CACHE_HOME") or (Path.home() / ".cache")) / "makewand"
    try:
        cache_root.mkdir(parents=True, exist_ok=True)
    except OSError:
        return None
    target = cache_root / "makewand-dev"
    print(c(f"ℹ️ MAKEWAND_DEV=1：从源码构建 Go 组件 ({root}) ...", COLOR_YELLOW), file=sys.stderr)
    build = subprocess.run(
        ["go", "build", "-ldflags", f"-X github.com/makewand/makewand/internal/buildinfo.Version={__version__}",
         "-o", str(target), "./cmd/makewand"],
        cwd=str(root),
    )
    return str(target) if build.returncode == 0 and target.is_file() else None


def delegate_to_go_server(args_list: List[str]):
    """
    Delegates server/TUI commands to the compiled Go makewand binary.

    Installed use never compiles or `go run`s the source tree: that path used to
    run with cwd = the makewand source root, so relative paths (e.g. `chat .`)
    silently pointed at the wrong directory. Developers can opt in with
    MAKEWAND_DEV=1, which builds a cache binary and runs it in the user's cwd.
    """
    import subprocess
    bin_path = _find_go_binary()
    if not bin_path and os.environ.get("MAKEWAND_DEV") == "1":
        bin_path = _build_dev_go_binary()

    if bin_path:
        warning = check_go_python_version(bin_path)
        if warning:
            print(c(warning, COLOR_YELLOW), file=sys.stderr)
        ret = subprocess.run([bin_path] + args_list)
        sys.exit(ret.returncode)

    print(f"❌ 命令 '{args_list[0]}' 为 Makewand 服务端/远程扩展组件，需要 Go 编译产物支持。", file=sys.stderr)
    print("   请重新运行 scripts/install.sh，或在源码根目录运行: go build -o bin/makewand-server ./cmd/makewand", file=sys.stderr)
    print("   (开发者可设置 MAKEWAND_DEV=1 让 makewand 自动从源码构建)", file=sys.stderr)
    sys.exit(1)

def main():
    GO_SUBCOMMANDS = {
        "serve", "chat", "new", "preview", "doctor", "setup", "token", "audit", "usage", "user", "state"
    }
    if len(sys.argv) > 1 and sys.argv[1] in GO_SUBCOMMANDS:
        delegate_to_go_server(sys.argv[1:])

    common_parser = argparse.ArgumentParser(add_help=False)
    common_parser.add_argument("--repo-trust", choices=["trusted", "untrusted"], default="trusted", help="Repository trust level: trusted or untrusted")

    sub_common_parser = argparse.ArgumentParser(add_help=False)
    sub_common_parser.add_argument("--repo-trust", choices=["trusted", "untrusted"], default=argparse.SUPPRESS, help="Repository trust level: trusted or untrusted")

    parser = argparse.ArgumentParser(
        prog="makewand",
        description="Makewand v3.1: Unified Multi-Model AI Subscription & Universal Tool Orchestrator",
        parents=[common_parser]
    )
    parser.add_argument("-v", "--version", action="version", version=f"makewand {__version__}")
    subparsers = parser.add_subparsers(dest="subcommand", help="Available subcommands")

    # models
    subparsers.add_parser("models", help="Discover and list current models across all AI ecosystems", parents=[sub_common_parser])

    # status
    p_status = subparsers.add_parser("status", help="Show health, quota indicators (local estimates), and reset times of all AIs", parents=[sub_common_parser])
    p_status.add_argument("--probe", action="store_true", help="Force immediate live probe of all CLIs (real model calls for claude/codex/muse/grok)")
    p_status.add_argument("--json", action="store_true", default=False, help="Print machine-readable per-provider status/quota indicators")

    # probe
    subparsers.add_parser("probe", help="Perform live probing on all AIs and update status cache", parents=[sub_common_parser])

    # quota
    p_quota = subparsers.add_parser("quota", help="Show quota indicators (Python: local call-count estimates, not official quota)", parents=[sub_common_parser])
    p_quota.add_argument("--probe", action="store_true", help="Force immediate live probe")
    p_quota.add_argument("--json", action="store_true", default=False, help="Print machine-readable per-provider status/quota indicators")

    # run
    p_run = subparsers.add_parser("run", help="Run auto-adaptive multi-model pipeline with auto-fix loop", parents=[sub_common_parser])
    p_run.add_argument("prompt", help="The task prompt to execute")
    p_run.add_argument("--cwd", help="Target working directory")
    p_run.add_argument("--tier", choices=["auto", "fast", "standard", "deep", "balanced", "power"], default="auto", help="Execution tier: fast, standard (balanced), deep (power)")
    p_run.add_argument("--mode", dest="tier", choices=["auto", "fast", "standard", "deep", "balanced", "power"], help="Alias for --tier: fast, balanced, power")
    p_run.add_argument("--model", help="Explicit model override")
    p_run.add_argument("--no-auto-fix", dest="auto_fix", action="store_false", default=True, help="Disable review defect auto-fix loop")
    p_run.add_argument("--max-fix", type=int, default=2, help="Maximum auto-fix iterations (default: 2)")
    p_run.add_argument("--stream", action="store_true", default=False, help="Stream subprocess output line-by-line")
    p_run.add_argument("--boost", action="store_true", default=False, help="Force boost/overclock mode: bypass soft burn rate penalty and allocate highest reasoning power")
    p_run.add_argument("--local-only", "--offline", dest="local_only", action="store_true", default=False, help="Strict local-only / 100%% offline mode: use local self-hosted models for both coding and review")
    p_run.add_argument("--provider", dest="provider", default=None, help="Explicit primary provider override (e.g. deepseek, qwen, local, claude, codex, agy, grok, muse)")

    # review
    p_rev = subparsers.add_parser("review", help="Review current git diff using Codex / Antigravity", parents=[sub_common_parser])
    p_rev.add_argument("--cwd", help="Target working directory")
    p_rev.add_argument("--json", action="store_true", default=False, help="Output structured review verdicts in JSON format")
    p_rev.add_argument("--stream", action="store_true", default=False, help="Stream review output line-by-line")
    p_rev.add_argument("--timeout", type=int, default=300)
    p_rev.add_argument("--local-only", "--offline", dest="local_only", action="store_true", default=False, help="Strict local-only / 100%% offline mode: review diff using only local self-hosted model")

    # race
    p_race = subparsers.add_parser("race", help="Run prompt on two models in parallel worktrees and compare", parents=[sub_common_parser])
    p_race.add_argument("prompt", help="Prompt for race comparison")
    p_race.add_argument("--cwd", help="Target working directory")
    p_race.add_argument("--timeout", type=int, default=300)

    # search (budgeted search guardrail)
    p_search = subparsers.add_parser("search", help="Budgeted fast search excluding cold archives and databases", parents=[sub_common_parser])
    p_search.add_argument("pattern", help="Regex or text pattern to search for")
    p_search.add_argument("--cwd", help="Root directory to search (default: current directory)")
    p_search.add_argument("--max-results", type=int, default=150, help="Maximum matches to return (default: 150)")
    p_search.add_argument("--max-depth", type=int, default=6, help="Maximum directory depth (default: 6)")

    # repomap (codebase architecture symbol map)
    p_repomap = subparsers.add_parser("repomap", help="Generate concise repository symbol map (AST/regex extracted)", parents=[sub_common_parser])
    p_repomap.add_argument("--cwd", help="Root directory to map (default: current directory)")
    p_repomap.add_argument("--max-lines", type=int, default=80, help="Max lines of repo map output (default: 80)")
    p_repomap.add_argument("--max-files", type=int, default=40, help="Max files to include in repo map (default: 40)")
    p_repomap.add_argument("--json", action="store_true", help="Output repo map in JSON format")

    # plan (multi-agent DAG task decomposition pipeline, inspired by OmO Ultrawork)
    p_plan = subparsers.add_parser("plan", help="Decompose complex goals into DAG tasks and execute topologically", parents=[sub_common_parser])
    p_plan.add_argument("prompt", help="High-level engineering goal to decompose")
    p_plan.add_argument("--cwd", help="Target working directory")
    p_plan.add_argument("--tier", choices=["auto", "fast", "standard", "deep", "balanced", "power"], default="deep")
    p_plan.add_argument("--execute", action="store_true", default=False, help="Execute decomposed DAG tasks topologically with verification gates")
    p_plan.add_argument("--json", action="store_true", default=False, help="Output plan in JSON format")
    p_plan.add_argument("--local-only", "--offline", dest="local_only", action="store_true", default=False)
    p_plan.add_argument("--tiered", action="store_true", default=False, help="Enable Architect-Worker tiered dispatch (Architect for design/audit, Worker for implementation)")
    p_plan.add_argument("--architect", default=None, help="Engine for Architect role (e.g. claude, codex, agy)")
    p_plan.add_argument("--worker", default=None, help="Engine for Worker role (e.g. local, deepseek, qwen)")

    # aci (LLM-dedicated Agent-Computer Interface, inspired by SWE-agent)
    p_aci = subparsers.add_parser("aci", help="LLM-dedicated Agent-Computer Interface (SWE-agent inspired)", parents=[sub_common_parser])
    p_aci.add_argument("action", choices=["view", "search", "grep"], help="ACI action to perform")
    p_aci.add_argument("target", help="Filepath for view, or search term for search/grep")
    p_aci.add_argument("line", nargs="?", type=int, default=1, help="Line number for view window (default: 1)")
    p_aci.add_argument("--cwd", help="Working directory")

    # mcp (Model Context Protocol client integration, inspired by Claude Code)
    p_mcp = subparsers.add_parser("mcp", help="Model Context Protocol (MCP) server integration", parents=[sub_common_parser])
    p_mcp.add_argument("action", choices=["list", "call"], help="MCP action: list tools, or call a tool")
    p_mcp.add_argument("--tool", help="Tool name for call action")
    p_mcp.add_argument("--args", help="JSON string arguments for tool call")
    p_mcp.add_argument("--cwd", help="Working directory")
    p_mcp.add_argument("server_cmd", nargs="+", help="Command to launch MCP server (e.g. npx -y @modelcontextprotocol/server-...)")

    # sandbox (bubblewrap process isolation)
    p_sb = subparsers.add_parser("sandbox", help="Run shell command inside bubblewrap process sandbox", parents=[sub_common_parser])
    p_sb.add_argument("--cwd", help="Target working directory (default: current directory)")
    p_sb.add_argument("--no-net", dest="allow_net", action="store_false", default=True, help="Block network inside sandbox")
    p_sb.add_argument("--timeout", type=int, default=120, help="Execution timeout in seconds")
    p_sb.add_argument("cmd", nargs=argparse.REMAINDER, help="Command to execute inside sandbox")

    p_claude = subparsers.add_parser("claude", help="Run prompt directly with Claude Code subscription", parents=[sub_common_parser])
    p_claude.add_argument("prompt", help="Prompt for Claude")
    p_claude.add_argument("--cwd", help="Working directory")
    p_claude.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_claude.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_claude.add_argument("--model", help="Specific model name")
    p_claude.add_argument("--stream", action="store_true", default=False)
    p_claude.add_argument("--timeout", type=int, default=300)
    p_claude.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_codex = subparsers.add_parser("codex", help="Run prompt directly with Codex CLI subscription", parents=[sub_common_parser])
    p_codex.add_argument("prompt", help="Prompt for Codex")
    p_codex.add_argument("--cwd", help="Working directory")
    p_codex.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_codex.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_codex.add_argument("--model", help="Specific model name")
    p_codex.add_argument("--stream", action="store_true", default=False)
    p_codex.add_argument("--timeout", type=int, default=300)
    p_codex.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_agy = subparsers.add_parser("agy", help="Run prompt directly with Antigravity CLI subscription", parents=[sub_common_parser])
    p_agy.add_argument("prompt", help="Prompt for Antigravity")
    p_agy.add_argument("--cwd", help="Working directory")
    p_agy.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_agy.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_agy.add_argument("--model", help="Specific model name")
    p_agy.add_argument("--stream", action="store_true", default=False)
    p_agy.add_argument("--timeout", type=int, default=300)
    p_agy.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_muse = subparsers.add_parser("muse", help="Run prompt directly with Muse Code subscription", parents=[sub_common_parser])
    p_muse.add_argument("prompt", help="Prompt for Muse Code")
    p_muse.add_argument("--cwd", help="Working directory")
    p_muse.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_muse.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_muse.add_argument("--model", help="Specific model name")
    p_muse.add_argument("--stream", action="store_true", default=False)
    p_muse.add_argument("--timeout", type=int, default=300)
    p_muse.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_grok = subparsers.add_parser("grok", help="Run prompt directly with Grok Build CLI (xAI subscription)", parents=[sub_common_parser])
    p_grok.add_argument("prompt", help="Prompt for Grok")
    p_grok.add_argument("--cwd", help="Working directory")
    p_grok.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_grok.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_grok.add_argument("--model", help="Specific model name")
    p_grok.add_argument("--stream", action="store_true", default=False)
    p_grok.add_argument("--timeout", type=int, default=300)
    p_grok.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_local = subparsers.add_parser("local", help="Run prompt directly with local self-hosted model (Ollama / vLLM, 0 token cost)", parents=[sub_common_parser])
    p_local.add_argument("prompt", help="Prompt for local model")
    p_local.add_argument("--cwd", help="Working directory")
    p_local.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_local.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_local.add_argument("--model", help="Specific model name (e.g. gemma4:31b, llama3.2)")
    p_local.add_argument("--stream", action="store_true", default=False)
    p_local.add_argument("--timeout", type=int, default=300)
    p_local.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_observe = subparsers.add_parser("observe", help="Inspect all running AI sessions, classify behavior, and report makewand optimizations", parents=[sub_common_parser])
    p_observe.add_argument("--json", action="store_true", help="Output raw JSON format")
    p_observe.add_argument("--clean-hung", action="store_true", default=False,
                           help="List makewand-dispatched processes running over 30 minutes as cleanup candidates (nothing is killed without --confirm-pids)")
    p_observe.add_argument("--confirm-pids", default=None,
                           help="Comma-separated PIDs from the --clean-hung candidate list to SIGTERM (explicit confirmation)")

    # Candidate Lifecycle Subcommands
    p_cands = subparsers.add_parser("candidates", help="List all pending multi-model race candidate workspaces", parents=[sub_common_parser])

    p_inspect = subparsers.add_parser("inspect", help="Inspect race candidate diffs and referee verdicts", parents=[sub_common_parser])
    p_inspect.add_argument("race_id", nargs="?", default=None, help="Race ID (defaults to latest)")
    p_inspect.add_argument("--candidate", choices=["A", "B", "a", "b"], default=None, help="Inspect specific candidate (A or B)")

    p_apply = subparsers.add_parser("apply", help="Safely apply a race candidate solution to current workspace with conflict checks", parents=[sub_common_parser])
    p_apply.add_argument("race_id", nargs="?", default=None, help="Race ID (defaults to latest)")
    p_apply.add_argument("--candidate", choices=["A", "B", "a", "b"], default=None, help="Candidate to apply (A or B, defaults to winner)")
    p_apply.add_argument("--dry-run", action="store_true", default=False, help="Simulate apply and show changed files without touching disk")
    p_apply.add_argument("--force", action="store_true", default=False, help="Force overwrite even if local workspace has conflicts")

    p_discard = subparsers.add_parser("discard", help="Discard saved race candidate workspaces", parents=[sub_common_parser])
    p_discard.add_argument("race_id", nargs="?", default=None, help="Race ID to discard (defaults to latest)")
    p_discard.add_argument("--all", action="store_true", default=False, help="Discard all candidate workspaces")

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
    p_aider.add_argument("prompt", help="Prompt for Aider")
    p_aider.add_argument("--cwd", help="Working directory")
    p_aider.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_aider.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_aider.add_argument("--model", help="Specific model name")
    p_aider.add_argument("--stream", action="store_true", default=False)
    p_aider.add_argument("--timeout", type=int, default=300)
    p_aider.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_deepseek = subparsers.add_parser("deepseek", help="Run prompt directly with DeepSeek API", parents=[sub_common_parser])
    p_deepseek.add_argument("prompt", help="Prompt for DeepSeek")
    p_deepseek.add_argument("--cwd", help="Working directory")
    p_deepseek.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_deepseek.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_deepseek.add_argument("--model", help="Specific model name (e.g. deepseek-chat, deepseek-reasoner)")
    p_deepseek.add_argument("--stream", action="store_true", default=False)
    p_deepseek.add_argument("--timeout", type=int, default=300)
    p_deepseek.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_qwen = subparsers.add_parser("qwen", help="Run prompt directly with Aliyun Qwen API", parents=[sub_common_parser])
    p_qwen.add_argument("prompt", help="Prompt for Qwen")
    p_qwen.add_argument("--cwd", help="Working directory")
    p_qwen.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
    p_qwen.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
    p_qwen.add_argument("--model", help="Specific model name (e.g. qwen2.5-coder-32b-instruct, qwen-max)")
    p_qwen.add_argument("--stream", action="store_true", default=False)
    p_qwen.add_argument("--timeout", type=int, default=300)
    p_qwen.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    for api_provider, label in (("glm", "Zhipu GLM API"), ("kimi", "Moonshot Kimi API"),
                                ("openrouter", "OpenRouter API"), ("siliconflow", "SiliconFlow API")):
        p_api = subparsers.add_parser(api_provider, help=f"Run prompt directly with {label} (text only; does not edit files)", parents=[sub_common_parser])
        p_api.add_argument("prompt", help=f"Prompt for {label}")
        p_api.add_argument("--cwd", help="Working directory")
        p_api.add_argument("--tier", choices=["fast", "standard", "deep", "balanced", "power"], default="standard")
        p_api.add_argument("--mode", dest="tier", choices=["fast", "standard", "deep", "balanced", "power"], help="Alias for --tier")
        p_api.add_argument("--model", help="Specific model name")
        p_api.add_argument("--stream", action="store_true", default=False)
        p_api.add_argument("--timeout", type=int, default=300)
        p_api.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    for detect_only in ("cursor", "copilot"):
        p_det = subparsers.add_parser(detect_only, help=f"{detect_only}: detected only, no execution adapter yet", parents=[sub_common_parser])
        p_det.add_argument("prompt", nargs="*", help=argparse.SUPPRESS)

    known_subcommands = {
        "models", "status", "probe", "quota", "run", "review", "race", "search", "sandbox",
        "claude", "codex", "agy", "grok", "muse", "local", "aider", "deepseek", "qwen", "glm", "kimi",
        "openrouter", "siliconflow", "cursor", "copilot",
        "observe", "candidates", "inspect", "apply", "discard",
        "enable", "disable", "repomap", "plan", "aci", "mcp"
    }
    # If user invokes `makewand "do something"` or `makewand --repo-trust untrusted "do something"`, automatically route to `makewand run ...`
    is_auto_routed_run = False
    has_subcmd = any(arg in known_subcommands for arg in sys.argv[1:])
    if not has_subcmd and len(sys.argv) > 1:
        idx = 1
        while idx < len(sys.argv):
            arg = sys.argv[idx]
            if arg in ("--repo-trust", "-t"):
                idx += 2
            elif arg.startswith("-"):
                idx += 1
            else:
                sys.argv.insert(idx, "run")
                is_auto_routed_run = True
                break

    args = parser.parse_args()
    if hasattr(args, "tier") and args.tier:
        args.tier = normalize_tier(args.tier)

    # Mark this process as a makewand dispatcher: every provider CLI, sandbox and
    # test process it spawns inherits MAKEWAND_DISPATCH_ID, which is how
    # `observe --clean-hung` tells makewand's own processes apart from the
    # user's sessions. `sandbox` runs a user-chosen command, so it is not marked.
    if args.subcommand != "sandbox":
        from makewand.observer import mark_process_as_dispatcher
        mark_process_as_dispatcher()

    if not args.subcommand:
        from makewand.interactive import start_interactive_session
        start_interactive_session(repo_trust=getattr(args, "repo_trust", "trusted"))
        sys.exit(0)

    if hasattr(args, "cwd"):
        args.cwd = os.path.abspath(args.cwd) if args.cwd else os.getcwd()

    if args.subcommand == "models":
        cmd_models(args)
    elif args.subcommand == "status":
        cmd_status(args)
    elif args.subcommand == "probe":
        args.probe = True
        cmd_status(args)
    elif args.subcommand == "quota":
        cmd_quota(args)
    elif args.subcommand == "run":
        # Explicit `makewand run <prompt>` indicates the user intended code execution, but natural language
        # entrypoint `makewand "<prompt>"` must allow intent classification (e.g. explain, identity, review)
        force_code = not is_auto_routed_run
        repo_trust = getattr(args, "repo_trust", "trusted")
        ok = run_pipeline(
            args.prompt,
            cwd=args.cwd,
            tier=normalize_tier(args.tier),
            model=args.model,
            stream=args.stream,
            auto_fix=args.auto_fix,
            max_fix=getattr(args, "max_fix", 2),
            timeout=args.timeout,
            force_code=force_code,
            repo_trust=repo_trust,
            boost=getattr(args, "boost", False),
            forced_engine=getattr(args, "provider", None),
            local_only=getattr(args, "local_only", False)
        )
        if not ok:
            sys.exit(EXIT_FAILED)
        sys.exit(EXIT_PASSED)
    elif args.subcommand == "review":
        exit_code = run_review(
            cwd=args.cwd,
            stream=args.stream,
            timeout=args.timeout,
            output_json=getattr(args, "json", False),
            repo_trust=getattr(args, "repo_trust", "trusted"),
            local_only=getattr(args, "local_only", False)
        )
        sys.exit(exit_code if exit_code is not None else 0)
    elif args.subcommand == "race":
        exit_code = run_race(args.prompt, cwd=args.cwd, timeout=args.timeout, repo_trust=getattr(args, "repo_trust", "trusted"))
        sys.exit(exit_code if exit_code is not None else 0)
    elif args.subcommand == "candidates":
        cmd_candidates(args)
    elif args.subcommand == "inspect":
        cmd_inspect(args)
    elif args.subcommand == "apply":
        cmd_apply(args)
    elif args.subcommand == "discard":
        cmd_discard(args)
    elif args.subcommand == "enable":
        cmd_enable(args)
    elif args.subcommand == "disable":
        cmd_disable(args)
    elif args.subcommand == "search":
        cmd_search(args)
    elif args.subcommand == "repomap":
        cmd_repomap(args)
    elif args.subcommand == "plan":
        cmd_plan(args)
    elif args.subcommand == "aci":
        cmd_aci(args)
    elif args.subcommand == "mcp":
        cmd_mcp(args)
    elif args.subcommand == "sandbox":
        cmd_sandbox(args)
    elif args.subcommand == "claude":
        ok, out, err = execute_claude_task(args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted"))
        try:
            from makewand.usage import record_engine_usage
            record_engine_usage("claude", tier=getattr(args, "tier", "standard"), success=ok, task=args.prompt)
        except Exception:
            pass
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(1)
    elif args.subcommand == "codex":
        ok, out, err = execute_codex_task(args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted"))
        try:
            from makewand.usage import record_engine_usage
            record_engine_usage("codex", tier=getattr(args, "tier", "standard"), success=ok, task=args.prompt)
        except Exception:
            pass
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(1)
    elif args.subcommand == "agy":
        ok, out, err = execute_agy_task(args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted"))
        try:
            from makewand.usage import record_engine_usage
            record_engine_usage("agy", tier=getattr(args, "tier", "standard"), success=ok, task=args.prompt)
        except Exception:
            pass
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(1)
    elif args.subcommand == "muse":
        ok, out, err = execute_muse_task(args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted"))
        try:
            from makewand.usage import record_engine_usage
            record_engine_usage("muse", tier=getattr(args, "tier", "standard"), success=ok, task=args.prompt)
        except Exception:
            pass
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(1)
    elif args.subcommand == "grok":
        ok, out, err = execute_grok_task(args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted"))
        try:
            from makewand.usage import record_engine_usage
            record_engine_usage("grok", tier=getattr(args, "tier", "standard"), success=ok, task=args.prompt)
        except Exception:
            pass
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(1)
    elif args.subcommand == "local":
        from makewand.providers.local import execute_local_task
        ok, out, err = execute_local_task(args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted"))
        try:
            from makewand.usage import record_engine_usage
            record_engine_usage("local", tier=getattr(args, "tier", "standard"), success=ok, task=args.prompt)
        except Exception:
            pass
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(1)
    elif args.subcommand == "aider":
        from makewand.providers.aider import execute_aider_task
        ok, out, err = execute_aider_task(args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted"))
        try:
            from makewand.usage import record_engine_usage
            record_engine_usage("aider", tier=getattr(args, "tier", "standard"), success=ok, task=args.prompt)
        except Exception:
            pass
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(1)
    elif args.subcommand in ("deepseek", "qwen", "glm", "kimi", "openrouter", "siliconflow"):
        from makewand.orchestrator import dispatch_task
        ok, out, err = dispatch_task(args.subcommand, args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, stream=args.stream, readonly=getattr(args, "readonly", False), repo_trust=getattr(args, "repo_trust", "trusted"))
        try:
            from makewand.usage import record_engine_usage
            record_engine_usage(args.subcommand, tier=getattr(args, "tier", "standard"), success=ok, task=args.prompt)
        except Exception:
            pass
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(1)
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
