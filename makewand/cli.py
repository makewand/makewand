"""
Makewand CLI: Command-line interface and subcommand parsers.
"""

import os
import sys
import argparse
from pathlib import Path
from typing import List, Optional, Dict, Any
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

    print(c("\n============================================================", COLOR_BOLD))
    print(c("       Makewand Multi-Model AI 订阅与全工具拓扑看板", COLOR_BOLD + COLOR_CYAN))
    print(c("============================================================\n", COLOR_BOLD))

    cache = get_or_update_status(force_probe=args.probe)

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
        "subscription": c("[订阅模式: 0 Token 成本]", COLOR_GREEN),
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
        for key in active_tools:
            info = cache.get(key, {})
            status = info.get("status", "unknown")
            badge = status_badges.get(status, f"[{status}]")
            mode = get_provider_execution_mode(key)
            mode_badge = mode_badges.get(mode, f"[{mode}]")
            name = display_names.get(key, key)
            reason = info.get("reason", "")
            resets = info.get("resets_at")

            print(f"{badge} {mode_badge} {c(name, COLOR_BOLD)}")
            if reason:
                print(f"      说明: {reason}")
            if resets:
                print(f"      预计解封/状态: {c(resets, COLOR_YELLOW + COLOR_BOLD)}")
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
    print(f"  • 支持的全部工具: {c(', '.join(all_tools), COLOR_YELLOW)}\n")

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
            if not is_provider_enabled(eng):
                status_desc = c("🚫 用户已手动禁用", COLOR_RED)
            elif pen == 0.0:
                status_desc = c("🟢 额度健康平稳 (0 成本)" if eng in ("local", "aider") else "🟢 额度健康平稳", COLOR_GREEN)
            else:
                status_desc = c(f"🟡 {reason}", COLOR_YELLOW)
            print(f"{eng:<14} {c4:<10} {c24:<10} {c7d:<10} {status_desc}")
        print()
    except Exception:
        pass

def cmd_enable(args):
    """Enables one or all AI tool providers."""
    from makewand.config import set_provider_enabled, get_all_supported_providers, normalize_provider_name, get_active_providers
    import subprocess
    target = args.provider.lower().strip()
    all_supported = get_all_supported_providers()
    if target == "all":
        for p in all_supported:
            set_provider_enabled(p, True)
        try:
            res = subprocess.run(["systemctl", "is-active", "ollama"], capture_output=True, text=True)
            if res.stdout.strip() != "active":
                subprocess.run(["sudo", "systemctl", "start", "ollama"], capture_output=True)
        except Exception:
            pass
        active = get_active_providers()
        print(c(f"✔ 已成功启用全部 AI 工具！当前动态可用池检测到 {len(active)} 个工具: {', '.join(active)}", COLOR_GREEN + COLOR_BOLD))
    else:
        norm = normalize_provider_name(target)
        ok = set_provider_enabled(norm, True)
        if ok:
            print(c(f"✔ 已成功启用 AI 工具: {norm} (运行 'makewand status' 查看最新状态)", COLOR_GREEN + COLOR_BOLD))
            if norm == "local":
                try:
                    res = subprocess.run(["systemctl", "is-active", "ollama"], capture_output=True, text=True)
                    if res.stdout.strip() != "active":
                        subprocess.run(["sudo", "systemctl", "start", "ollama"], capture_output=True)
                        print(c("  ✔ 已自动唤醒本地 Ollama 守护服务 (http://localhost:11434)", COLOR_GREEN))
                except Exception:
                    pass
        else:
            supported = ", ".join(all_supported) + ", all"
            print(c(f"❌ 未知或不支持的工具名称: {target} (支持: {supported})", COLOR_RED), file=sys.stderr)
            sys.exit(1)

def cmd_disable(args):
    """Disables an AI tool provider."""
    from makewand.config import set_provider_enabled, get_all_supported_providers, normalize_provider_name
    import subprocess
    target = args.provider.lower().strip()
    norm = normalize_provider_name(target)
    ok = set_provider_enabled(norm, False)
    if ok:
        print(c(f"✔ 已成功禁用 AI 工具: {norm} (Makewand 调度流水线将不再向其派发任务)", COLOR_YELLOW + COLOR_BOLD))
        if norm == "local":
            try:
                subprocess.run(["sudo", "systemctl", "stop", "ollama"], capture_output=True)
                print(c("  ✔ 已自动停止本地 Ollama 后台服务并彻底释放内存", COLOR_GREEN))
            except Exception:
                pass
        print(f"  提示: 随时可运行 'makewand enable {norm}' 重新启用。")
    else:
        supported = ", ".join(get_all_supported_providers())
        print(c(f"❌ 未知或不支持的工具名称: {target} (支持: {supported})", COLOR_RED), file=sys.stderr)
        sys.exit(1)


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

    print(c("5. Grok Build CLI (xAI 订阅):", COLOR_BOLD + COLOR_RED))
    print(f"   当前默认: {c(models['grok']['current_default'], COLOR_GREEN + COLOR_BOLD)}")
    print(f"   检测到版本: {', '.join(models['grok']['available']) if models['grok']['available'] else '跟随官方动态下发'}")
    print("   自适应机制: 读取 models_cache.json，支持 --reasoning-effort (low/medium/high/xhigh) 与多代模型。\n")

    print(c("6. Local Self-Hosted (本地大模型 / Ollama / vLLM):", COLOR_BOLD + COLOR_PURPLE))
    try:
        from makewand.providers.local import is_local_model_available, get_default_local_model, list_local_models
        if is_local_model_available():
            print(f"   当前默认: {c(get_default_local_model(), COLOR_GREEN + COLOR_BOLD)}")
            print(f"   检测到可用模型: {', '.join(list_local_models())}")
            print("   自适应机制: 本地 GPU 离线执行，0 Token 外部调用成本，数据 100% 本地安全私有。\n")
        else:
            print("   状态: 未检测到本地 Ollama / vLLM 服务 (http://localhost:11434 未响应)\n")
    except Exception as e:
        print(f"   状态: 检测异常 ({e})\n")

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
        print(c(f"--- 选手 A ({cand_a.get('model')}) 改动详情 (git diff) ---", COLOR_CYAN + COLOR_BOLD))
        diff = cand_a.get("diff", "")
        print(diff if diff else "无有效代码变更")
    elif cand == "B":
        print(c(f"--- 选手 B ({cand_b.get('model')}) 改动详情 (git diff) ---", COLOR_BLUE + COLOR_BOLD))
        diff = cand_b.get("diff", "")
        print(diff if diff else "无有效代码变更")
    else:
        print(c("--- 两位选手表现对比 ---", COLOR_BOLD))
        print(f"选手 A [{cand_a.get('model')}]: 耗时={cand_a.get('duration')}s, Diff大小={len(cand_a.get('diff', ''))} 字节, 状态={'成功' if cand_a.get('success') else '失败'}")
        print(f"选手 B [{cand_b.get('model')}]: 耗时={cand_b.get('duration')}s, Diff大小={len(cand_b.get('diff', ''))} 字节, 状态={'成功' if cand_b.get('success') else '失败'}")
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

def delegate_to_go_server(args_list: List[str]):
    """Delegates server/TUI commands to compiled Go makewand binary or source."""
    import shutil
    import subprocess
    candidates = [
        Path(__file__).resolve().parent.parent / "bin" / "makewand-server",
        Path(__file__).resolve().parent.parent / "bin" / "makewand-go",
        Path(__file__).resolve().parent.parent / "dist" / "makewand",
        shutil.which("makewand-server"),
        shutil.which("makewand-go")
    ]
    bin_path = next((str(c) for c in candidates if c and Path(c).is_file() and os.access(c, os.X_OK)), None)
    if not bin_path and shutil.which("go"):
        cmd_dir = Path(__file__).resolve().parent.parent / "cmd" / "makewand"
        if cmd_dir.is_dir():
            cmd = ["go", "run", "./cmd/makewand"] + args_list
            ret = subprocess.run(cmd, cwd=str(Path(__file__).resolve().parent.parent))
            sys.exit(ret.returncode)

    if bin_path:
        ret = subprocess.run([bin_path] + args_list)
        sys.exit(ret.returncode)

    print(f"❌ 命令 '{args_list[0]}' 为 Makewand 服务端/远程扩展组件，需要 Go 编译产物支持。")
    print("   请在项目根目录运行: go build -o bin/makewand-server ./cmd/makewand")
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
    parser.add_argument("-v", "--version", action="version", version="makewand 3.1.0")
    subparsers = parser.add_subparsers(dest="subcommand", help="Available subcommands")

    # models
    subparsers.add_parser("models", help="Discover and list current models across all AI ecosystems", parents=[sub_common_parser])

    # status
    p_status = subparsers.add_parser("status", help="Show health, quota limits, and reset times of all AIs", parents=[sub_common_parser])
    p_status.add_argument("--probe", action="store_true", help="Force immediate live probe of all CLIs")

    # probe
    subparsers.add_parser("probe", help="Perform live probing on all AIs and update status cache", parents=[sub_common_parser])

    # quota
    p_quota = subparsers.add_parser("quota", help="Show remaining subscription quota across providers", parents=[sub_common_parser])
    p_quota.add_argument("--probe", action="store_true", help="Force immediate live probe")

    # run
    p_run = subparsers.add_parser("run", help="Run auto-adaptive multi-model pipeline with auto-fix loop", parents=[sub_common_parser])
    p_run.add_argument("prompt", help="The task prompt to execute")
    p_run.add_argument("--cwd", help="Target working directory")
    p_run.add_argument("--tier", choices=["auto", "fast", "standard", "deep"], default="auto", help="Execution tier: fast, standard, deep")
    p_run.add_argument("--model", help="Explicit model override")
    p_run.add_argument("--no-auto-fix", dest="auto_fix", action="store_false", default=True, help="Disable review defect auto-fix loop")
    p_run.add_argument("--stream", action="store_true", default=False, help="Stream subprocess output line-by-line")
    p_run.add_argument("--timeout", type=int, default=300, help="Per-stage timeout in seconds")
    p_run.add_argument("--boost", action="store_true", default=False, help="Force boost/overclock mode: bypass soft burn rate penalty and allocate highest reasoning power")

    # review
    p_rev = subparsers.add_parser("review", help="Review current git diff using Codex / Antigravity", parents=[sub_common_parser])
    p_rev.add_argument("--cwd", help="Target working directory")
    p_rev.add_argument("--json", action="store_true", default=False, help="Output structured review verdicts in JSON format")
    p_rev.add_argument("--stream", action="store_true", default=False, help="Stream review output line-by-line")
    p_rev.add_argument("--timeout", type=int, default=300)

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

    # sandbox (bubblewrap process isolation)
    p_sb = subparsers.add_parser("sandbox", help="Run shell command inside bubblewrap process sandbox", parents=[sub_common_parser])
    p_sb.add_argument("--cwd", help="Target working directory (default: current directory)")
    p_sb.add_argument("--no-net", dest="allow_net", action="store_false", default=True, help="Block network inside sandbox")
    p_sb.add_argument("--timeout", type=int, default=120, help="Execution timeout in seconds")
    p_sb.add_argument("cmd", nargs=argparse.REMAINDER, help="Command to execute inside sandbox")

    p_claude = subparsers.add_parser("claude", help="Run prompt directly with Claude Code subscription", parents=[sub_common_parser])
    p_claude.add_argument("prompt", help="Prompt for Claude")
    p_claude.add_argument("--cwd", help="Working directory")
    p_claude.add_argument("--tier", choices=["fast", "standard", "deep"], default="standard")
    p_claude.add_argument("--model", help="Specific model name")
    p_claude.add_argument("--stream", action="store_true", default=False)
    p_claude.add_argument("--timeout", type=int, default=300)
    p_claude.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_codex = subparsers.add_parser("codex", help="Run prompt directly with Codex CLI subscription", parents=[sub_common_parser])
    p_codex.add_argument("prompt", help="Prompt for Codex")
    p_codex.add_argument("--cwd", help="Working directory")
    p_codex.add_argument("--tier", choices=["fast", "standard", "deep"], default="standard")
    p_codex.add_argument("--model", help="Specific model name")
    p_codex.add_argument("--stream", action="store_true", default=False)
    p_codex.add_argument("--timeout", type=int, default=300)
    p_codex.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_agy = subparsers.add_parser("agy", help="Run prompt directly with Antigravity CLI subscription", parents=[sub_common_parser])
    p_agy.add_argument("prompt", help="Prompt for Antigravity")
    p_agy.add_argument("--cwd", help="Working directory")
    p_agy.add_argument("--tier", choices=["fast", "standard", "deep"], default="standard")
    p_agy.add_argument("--model", help="Specific model name")
    p_agy.add_argument("--stream", action="store_true", default=False)
    p_agy.add_argument("--timeout", type=int, default=300)
    p_agy.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_muse = subparsers.add_parser("muse", help="Run prompt directly with Muse Code subscription", parents=[sub_common_parser])
    p_muse.add_argument("prompt", help="Prompt for Muse Code")
    p_muse.add_argument("--cwd", help="Working directory")
    p_muse.add_argument("--tier", choices=["fast", "standard", "deep"], default="standard")
    p_muse.add_argument("--model", help="Specific model name")
    p_muse.add_argument("--stream", action="store_true", default=False)
    p_muse.add_argument("--timeout", type=int, default=300)
    p_muse.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_grok = subparsers.add_parser("grok", help="Run prompt directly with Grok Build CLI (xAI subscription)", parents=[sub_common_parser])
    p_grok.add_argument("prompt", help="Prompt for Grok")
    p_grok.add_argument("--cwd", help="Working directory")
    p_grok.add_argument("--tier", choices=["fast", "standard", "deep"], default="standard")
    p_grok.add_argument("--model", help="Specific model name")
    p_grok.add_argument("--stream", action="store_true", default=False)
    p_grok.add_argument("--timeout", type=int, default=300)
    p_grok.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_local = subparsers.add_parser("local", help="Run prompt directly with local self-hosted model (Ollama / vLLM, 0 token cost)", parents=[sub_common_parser])
    p_local.add_argument("prompt", help="Prompt for local model")
    p_local.add_argument("--cwd", help="Working directory")
    p_local.add_argument("--tier", choices=["fast", "standard", "deep"], default="standard")
    p_local.add_argument("--model", help="Specific model name (e.g. gemma4:31b, llama3.2)")
    p_local.add_argument("--stream", action="store_true", default=False)
    p_local.add_argument("--timeout", type=int, default=300)
    p_local.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_observe = subparsers.add_parser("observe", help="Inspect all running AI sessions, classify behavior, and report makewand optimizations", parents=[sub_common_parser])
    p_observe.add_argument("--json", action="store_true", help="Output raw JSON format")
    p_observe.add_argument("--clean-hung", action="store_true", default=False, help="Automatically terminate confirmed hung zombie/network processes exceeding 30 minutes")

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
    p_enable.add_argument("provider", choices=all_supported + ["all"], help="Provider name to enable")

    p_disable = subparsers.add_parser("disable", help="Disable an AI tool provider", parents=[sub_common_parser])
    p_disable.add_argument("provider", choices=all_supported, help="Provider name to disable")

    p_aider = subparsers.add_parser("aider", help="Run prompt directly with Aider CLI pair programmer", parents=[sub_common_parser])
    p_aider.add_argument("prompt", help="Prompt for Aider")
    p_aider.add_argument("--cwd", help="Working directory")
    p_aider.add_argument("--tier", choices=["fast", "standard", "deep"], default="standard")
    p_aider.add_argument("--model", help="Specific model name")
    p_aider.add_argument("--stream", action="store_true", default=False)
    p_aider.add_argument("--timeout", type=int, default=300)
    p_aider.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_deepseek = subparsers.add_parser("deepseek", help="Run prompt directly with DeepSeek API", parents=[sub_common_parser])
    p_deepseek.add_argument("prompt", help="Prompt for DeepSeek")
    p_deepseek.add_argument("--cwd", help="Working directory")
    p_deepseek.add_argument("--tier", choices=["fast", "standard", "deep"], default="standard")
    p_deepseek.add_argument("--model", help="Specific model name (e.g. deepseek-chat, deepseek-reasoner)")
    p_deepseek.add_argument("--stream", action="store_true", default=False)
    p_deepseek.add_argument("--timeout", type=int, default=300)
    p_deepseek.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    p_qwen = subparsers.add_parser("qwen", help="Run prompt directly with Aliyun Qwen API", parents=[sub_common_parser])
    p_qwen.add_argument("prompt", help="Prompt for Qwen")
    p_qwen.add_argument("--cwd", help="Working directory")
    p_qwen.add_argument("--tier", choices=["fast", "standard", "deep"], default="standard")
    p_qwen.add_argument("--model", help="Specific model name (e.g. qwen2.5-coder-32b-instruct, qwen-max)")
    p_qwen.add_argument("--stream", action="store_true", default=False)
    p_qwen.add_argument("--timeout", type=int, default=300)
    p_qwen.add_argument("--readonly", action="store_true", default=False, help="Enforce read-only analysis without modifications")

    known_subcommands = {
        "models", "status", "probe", "quota", "run", "review", "race", "search", "sandbox",
        "claude", "codex", "agy", "grok", "muse", "local", "aider", "deepseek", "qwen", "glm", "kimi",
        "openrouter", "siliconflow", "cursor", "copilot",
        "observe", "candidates", "inspect", "apply", "discard",
        "enable", "disable"
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
            tier=args.tier,
            model=args.model,
            stream=args.stream,
            auto_fix=args.auto_fix,
            max_fix=args.max_fix,
            timeout=args.timeout,
            force_code=force_code,
            repo_trust=repo_trust,
            boost=getattr(args, "boost", False)
        )
        if not ok:
            sys.exit(EXIT_FAILED)
        sys.exit(EXIT_PASSED)
    elif args.subcommand == "review":
        exit_code = run_review(cwd=args.cwd, stream=args.stream, timeout=args.timeout, output_json=getattr(args, "json", False), repo_trust=getattr(args, "repo_trust", "trusted"))
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
    elif args.subcommand == "observe":
        from makewand.observer import observe_all_dialogs, format_observation_markdown
        rep = observe_all_dialogs(clean_hung=getattr(args, "clean_hung", False))
        if getattr(args, "json", False):
            import json
            print(json.dumps(rep, ensure_ascii=False, indent=2))
        else:
            print(format_observation_markdown(rep))
            if rep.get("cleaned_pids"):
                print(c("\n🧹 已成功清理僵尸挂死进程:", COLOR_GREEN + COLOR_BOLD))
                for cp in rep["cleaned_pids"]:
                    print(f"  • 会话 [{cp['session']}], PID {cp['pid']} ({cp['comm']})")

if __name__ == "__main__":
    main()
