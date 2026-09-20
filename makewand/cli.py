"""
Makewand CLI: Command-line interface and subcommand parsers.
"""

import os
import sys
import argparse
from makewand.config import (
    c,
    COLOR_BOLD,
    COLOR_CYAN,
    COLOR_GREEN,
    COLOR_YELLOW,
    COLOR_RED,
    COLOR_BLUE,
    COLOR_PURPLE,
    COLOR_RESET
)
from makewand.health import get_or_update_status
from makewand.discovery import discover_available_models
from makewand.orchestrator import run_pipeline, run_review, run_race
from makewand.providers.agy import execute_agy_task
from makewand.providers.claude import execute_claude_task
from makewand.providers.codex import execute_codex_task
from makewand.providers.muse import execute_muse_task

def cmd_status(args):
    print(c("\n============================================================", COLOR_BOLD))
    print(c("       Makewand Multi-Model AI 订阅与额度健康看板", COLOR_BOLD + COLOR_CYAN))
    print(c("============================================================\n", COLOR_BOLD))

    cache = get_or_update_status(force_probe=args.probe)

    status_badges = {
        "healthy":    c("[🟢 正常可用]", COLOR_GREEN + COLOR_BOLD),
        "limited":    c("[🔴 额度受限]", COLOR_RED + COLOR_BOLD),
        "needs_auth": c("[🔑 需要授权]", COLOR_YELLOW + COLOR_BOLD),
        "warning":    c("[🟡 状态警告]", COLOR_YELLOW + COLOR_BOLD),
        "error":      c("[❌ 连接异常]", COLOR_RED + COLOR_BOLD),
        "missing":    c("[⚪ 未就绪]", COLOR_RESET),
        "unknown":    c("[⚪ 未探测]", COLOR_RESET),
    }

    display_names = {
        "agy": "Antigravity (Google AI Pro / Gemini)",
        "claude": "Claude Code (Anthropic Subscription)",
        "codex": "Codex CLI (OpenAI Subscription / gpt-6-astra)",
        "muse": "Muse Code (Meta Subscription / Llama)"
    }

    for key in ["agy", "claude", "codex", "muse"]:
        info = cache.get(key, {})
        status = info.get("status", "unknown")
        badge = status_badges.get(status, f"[{status}]")
        name = display_names.get(key, key)
        reason = info.get("reason", "")
        resets = info.get("resets_at")

        print(f"{badge} {c(name, COLOR_BOLD)}")
        if reason:
            print(f"      说明: {reason}")
        if resets:
            print(f"      预计解封/状态: {c(resets, COLOR_YELLOW + COLOR_BOLD)}")
        print()

    # Routing recommendation
    print(c("--- 当前自适应调度推荐策略 ---", COLOR_BOLD))
    c_stat = cache.get("claude", {}).get("status")
    x_stat = cache.get("codex", {}).get("status")
    m_stat = cache.get("muse", {}).get("status")

    if c_stat == "healthy" and x_stat == "healthy":
        print(c("  🌟 黄金三角流水线: agy(规划) -> claude(编码实现) -> codex(红队盲审)", COLOR_GREEN))
    elif c_stat == "limited" and x_stat == "healthy":
        print(c("  ⚡ 降级模式 A: Claude 额度受限，由 Codex (gpt-6-astra) 接管实现，Antigravity 负责审查", COLOR_YELLOW))
    elif c_stat == "healthy" and x_stat == "limited":
        print(c("  ⚡ 降级模式 B: Codex 额度受限，由 Claude 实现代码，Antigravity 接管红队自审", COLOR_YELLOW))
    elif m_stat == "healthy":
        print(c("  ⚡ 降级模式 C: 启用 Muse Code 作为主力/辅助编码引擎，由 Antigravity 实施质检验收", COLOR_PURPLE))
    elif c_stat == "limited" and x_stat == "limited":
        print(c("  🛡️ 自愈兜底模式: 外部订阅均受限，全流程由 Antigravity (Google AI Pro) 独立闭环", COLOR_CYAN))
    else:
        print("  使用 'makewand probe' 刷新当前实时健康度。")
    print()

def cmd_models(args):
    print(c("\n============================================================", COLOR_BOLD))
    print(c("       Makewand 多模型生态与动态发现矩阵 (Model Discovery)", COLOR_BOLD + COLOR_CYAN))
    print(c("============================================================\n", COLOR_BOLD))

    models = discover_available_models()

    print(c("1. Claude Code (Anthropic 订阅):", COLOR_BOLD + COLOR_BLUE))
    print(f"   当前默认: {c(models['claude']['current_default'], COLOR_GREEN + COLOR_BOLD)}")
    print(f"   检测到版本: {', '.join(models['claude']['available']) if models['claude']['available'] else '跟随官方动态下发'}")
    print("   自适应机制: 采用动态别名与 --fallback-model，官方升级新代际即刻自动同步。\n")

    print(c("2. Codex CLI (OpenAI 订阅):", COLOR_BOLD + COLOR_CYAN))
    print(f"   当前默认: {c(models['codex']['current_default'], COLOR_GREEN + COLOR_BOLD)}")
    print(f"   检测到版本: {', '.join(models['codex']['available']) if models['codex']['available'] else '跟随官方动态下发'}")
    print("   自适应机制: 实时读取 config.toml 与模型热迁移表，支持动态推理深度。\n")

    print(c("3. Antigravity (Google AI Pro):", COLOR_BOLD + COLOR_GREEN))
    print(f"   当前默认: {c(models['agy']['current_default'], COLOR_GREEN + COLOR_BOLD)}")
    print(f"   检测到版本: {', '.join(models['agy']['available'])}")
    print("   自适应机制: 原生搭载 Gemini 3.8 全系列与动态思维推理 (effort low/medium/high)。\n")

    print(c("4. Muse Code (Meta 订阅):", COLOR_BOLD + COLOR_PURPLE))
    print(f"   当前默认: {c(models['muse']['current_default'], COLOR_GREEN + COLOR_BOLD)}")
    print(f"   预置配置: {', '.join(models['muse']['available'])}")
    print("   自适应机制: 支持 --preset 与 --reasoning-effort (low/high/ultra)，集成 OS 沙箱管控。\n")

    print(c("--- 新模型自适应与透传规则 ---", COLOR_BOLD))
    print("  ✔ 零硬编码: 默认不锁定静态模型版本号，直接调用官方推荐指针。")
    print("  ✔ 智能映射: --tier fast/standard/deep 自动根据提供商最新技术代差映射。")
    print("  ✔ 自由透传: 支持 --model <任意新模型名> 直接传递给底层 CLI，永不过时。\n")

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

def cmd_sandbox(args):
    """Run command inside bubblewrap process sandbox."""
    from makewand.sandbox import run_in_sandbox
    if not args.cmd:
        print("请指定要在沙箱中运行的命令，例如: makewand sandbox python3 -m unittest")
        sys.exit(1)
    cmd = args.cmd
    if cmd and cmd[0] == "--":
        cmd = cmd[1:]
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

def main():
    parser = argparse.ArgumentParser(
        prog="makewand",
        description="Makewand v3.0: Unified Multi-Model AI Subscription Orchestrator"
    )
    subparsers = parser.add_subparsers(dest="subcommand", help="Available subcommands")

    # models
    subparsers.add_parser("models", help="Discover and list current models across all AI ecosystems")

    # status
    p_status = subparsers.add_parser("status", help="Show health, quota limits, and reset times of all AIs")
    p_status.add_argument("--probe", action="store_true", help="Force immediate live probe of all CLIs")

    # probe
    subparsers.add_parser("probe", help="Perform live probing on all AIs and update status cache")

    # quota
    p_quota = subparsers.add_parser("quota", help="Show remaining subscription quota across providers")
    p_quota.add_argument("--probe", action="store_true", help="Force immediate live probe")

    # run
    p_run = subparsers.add_parser("run", help="Run auto-adaptive multi-model pipeline with auto-fix loop")
    p_run.add_argument("prompt", help="The task prompt to execute")
    p_run.add_argument("--cwd", help="Target working directory")
    p_run.add_argument("--tier", choices=["auto", "fast", "standard", "deep"], default="auto", help="Execution tier: fast, standard, deep")
    p_run.add_argument("--model", help="Explicit model override")
    p_run.add_argument("--no-auto-fix", dest="auto_fix", action="store_false", default=True, help="Disable review defect auto-fix loop")
    p_run.add_argument("--max-fix", type=int, default=2, help="Max auto-fix iterations (default: 2)")
    p_run.add_argument("--stream", action="store_true", default=False, help="Stream subprocess output line-by-line")
    p_run.add_argument("--timeout", type=int, default=300, help="Per-stage timeout in seconds")

    # review
    p_rev = subparsers.add_parser("review", help="Review current git diff using Codex / Antigravity")
    p_rev.add_argument("--cwd", help="Target working directory")
    p_rev.add_argument("--stream", action="store_true", default=False, help="Stream review output line-by-line")
    p_rev.add_argument("--timeout", type=int, default=300)

    # race
    p_race = subparsers.add_parser("race", help="Run prompt on two models in parallel worktrees and compare")
    p_race.add_argument("prompt", help="Prompt for race comparison")
    p_race.add_argument("--cwd", help="Target working directory")
    p_race.add_argument("--timeout", type=int, default=300)

    # search (budgeted search guardrail)
    p_search = subparsers.add_parser("search", help="Budgeted fast search excluding cold archives and databases")
    p_search.add_argument("pattern", help="Regex or text pattern to search for")
    p_search.add_argument("--cwd", help="Root directory to search (default: current directory)")
    p_search.add_argument("--max-results", type=int, default=150, help="Maximum matches to return (default: 150)")
    p_search.add_argument("--max-depth", type=int, default=6, help="Maximum directory depth (default: 6)")

    # sandbox (bubblewrap process isolation)
    p_sb = subparsers.add_parser("sandbox", help="Run shell command inside bubblewrap process sandbox")
    p_sb.add_argument("--cwd", help="Target working directory (default: current directory)")
    p_sb.add_argument("--no-net", dest="allow_net", action="store_false", default=True, help="Block network inside sandbox")
    p_sb.add_argument("--timeout", type=int, default=120, help="Execution timeout in seconds")
    p_sb.add_argument("cmd", nargs=argparse.REMAINDER, help="Command to execute inside sandbox")

    # direct runners
    p_claude = subparsers.add_parser("claude", help="Run prompt directly with Claude Code subscription")
    p_claude.add_argument("prompt", help="Prompt for Claude")
    p_claude.add_argument("--cwd", help="Working directory")
    p_claude.add_argument("--tier", choices=["fast", "standard", "deep"], default="standard")
    p_claude.add_argument("--model", help="Specific model name")
    p_claude.add_argument("--stream", action="store_true", default=False)
    p_claude.add_argument("--timeout", type=int, default=300)

    p_codex = subparsers.add_parser("codex", help="Run prompt directly with Codex CLI subscription")
    p_codex.add_argument("prompt", help="Prompt for Codex")
    p_codex.add_argument("--cwd", help="Working directory")
    p_codex.add_argument("--tier", choices=["fast", "standard", "deep"], default="standard")
    p_codex.add_argument("--model", help="Specific model name")
    p_codex.add_argument("--stream", action="store_true", default=False)
    p_codex.add_argument("--timeout", type=int, default=300)

    p_agy = subparsers.add_parser("agy", help="Run prompt directly with Antigravity CLI subscription")
    p_agy.add_argument("prompt", help="Prompt for Antigravity")
    p_agy.add_argument("--cwd", help="Working directory")
    p_agy.add_argument("--tier", choices=["fast", "standard", "deep"], default="standard")
    p_agy.add_argument("--model", help="Specific model name")
    p_agy.add_argument("--stream", action="store_true", default=False)
    p_agy.add_argument("--timeout", type=int, default=300)

    p_muse = subparsers.add_parser("muse", help="Run prompt directly with Muse Code subscription")
    p_muse.add_argument("prompt", help="Prompt for Muse Code")
    p_muse.add_argument("--cwd", help="Working directory")
    p_muse.add_argument("--tier", choices=["fast", "standard", "deep"], default="standard")
    p_muse.add_argument("--model", help="Specific model name")
    p_muse.add_argument("--stream", action="store_true", default=False)
    p_muse.add_argument("--timeout", type=int, default=300)

    args = parser.parse_args()

    if not args.subcommand:
        parser.print_help()
        sys.exit(0)

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
        ok = run_pipeline(
            args.prompt,
            cwd=args.cwd,
            tier=args.tier,
            model=args.model,
            stream=args.stream,
            auto_fix=args.auto_fix,
            max_fix=args.max_fix,
            timeout=args.timeout
        )
        if not ok:
            sys.exit(1)
    elif args.subcommand == "review":
        run_review(cwd=args.cwd, stream=args.stream, timeout=args.timeout)
    elif args.subcommand == "race":
        run_race(args.prompt, cwd=args.cwd, timeout=args.timeout)
    elif args.subcommand == "search":
        cmd_search(args)
    elif args.subcommand == "sandbox":
        cmd_sandbox(args)
    elif args.subcommand == "claude":
        ok, out, err = execute_claude_task(args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, stream=args.stream)
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(1)
    elif args.subcommand == "codex":
        ok, out, err = execute_codex_task(args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, stream=args.stream)
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(1)
    elif args.subcommand == "agy":
        ok, out, err = execute_agy_task(args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, stream=args.stream)
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(1)
    elif args.subcommand == "muse":
        ok, out, err = execute_muse_task(args.prompt, cwd=args.cwd, timeout=args.timeout, tier=args.tier, model=args.model, stream=args.stream)
        if ok and not args.stream: print(out)
        elif not ok:
            if err: sys.stderr.write(f"{err}\n")
            sys.exit(1)

if __name__ == "__main__":
    main()
