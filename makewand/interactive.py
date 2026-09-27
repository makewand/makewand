"""
Makewand Interactive Console / REPL.
Fully redesigned to imitate Claude Code's interface and interaction style:
- Clean, compact rounded welcome card (no noisy 15-line ASCII art)
- Multi-turn conversation context memory (maintains session history across turns)
- Dual-mode intelligent routing (conversational Q&A vs multi-model engineering pipeline)
- Claude Code-style action bullets (● Action) and live status indicators
- Comprehensive slash commands (/help, /status, /diff, /review, /compact, /clear, /race, etc.)
- Tab autocompletion for slash commands and file paths
"""

import os
import sys
import shutil
import atexit
import subprocess
from pathlib import Path
from typing import List, Dict, Any, Optional

try:
    import readline
except ImportError:
    readline = None

from makewand.config import (
    c,
    COLOR_BOLD,
    COLOR_CYAN,
    COLOR_GREEN,
    COLOR_YELLOW,
    COLOR_RED,
    COLOR_BLUE,
    COLOR_PURPLE,
    COLOR_GRAY,
    COLOR_RESET,
)
from makewand.markdown import (
    render_terminal_markdown,
    display_width,
    pad_display,
)
from makewand.health import (
    load_status_cache,
    get_or_update_status,
    calculate_provider_quota,
)
from makewand.discovery import discover_available_models
from makewand.orchestrator import (
    run_pipeline,
    run_review,
    run_race,
    classify_prompt_intent,
    select_optimal_engine_pair,
    dispatch_task,
    is_identity_or_chit_chat,
    get_identity_message,
)
from makewand.search import safe_search
from makewand.sandbox import run_in_sandbox
from makewand.git_helper import get_git_diff

SLASH_COMMANDS = [
    "/help", "/?",
    "/status", "/quota",
    "/diff",
    "/review",
    "/compact",
    "/clear",
    "/models",
    "/probe",
    "/race",
    "/search",
    "/sandbox",
    "/observe",
    "/tier",
    "/model", "/provider",
    "/chat",
    "/run",
    "/multiline", "/paste",
    "/exit", "/quit",
]


def get_git_branch(cwd: str) -> Optional[str]:
    """Retrieves current git branch name if cwd is inside a git worktree."""
    try:
        res = subprocess.run(
            ["git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD"],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=2,
        )
        if res.returncode == 0:
            branch = res.stdout.strip()
            return branch if branch else None
    except Exception:
        pass
    return None


def format_short_path(path_str: str) -> str:
    """Shortens home directory path with ~ for compact display."""
    try:
        home = str(Path.home())
        if path_str.startswith(home):
            return "~" + path_str[len(home):]
    except Exception:
        pass
    return path_str


def get_model_status_badges(cache: Optional[Dict[str, Any]] = None) -> str:
    """Generates compact status badges for the top providers."""
    if cache is None:
        cache = load_status_cache()

    providers = [
        ("AGY", "agy"),
        ("Claude", "claude"),
        ("Codex", "codex"),
        ("Grok", "grok"),
        ("Muse", "muse"),
    ]
    badges = []
    for name, key in providers:
        info = cache.get(key, {})
        st = info.get("status", "unknown")
        quota = calculate_provider_quota(key, info)
        pct = quota.get("percentage", 100)
        if st == "healthy":
            badges.append(f"{COLOR_GREEN}🟢 {name} {pct}%{COLOR_RESET}")
        elif st == "limited":
            badges.append(f"{COLOR_RED}🔴 {name} 0%{COLOR_RESET}")
        elif st == "needs_auth":
            badges.append(f"{COLOR_YELLOW}🔑 {name}{COLOR_RESET}")
        else:
            badges.append(f"{COLOR_GRAY}⚪ {name}{COLOR_RESET}")
    return " · ".join(badges[:3])


def render_welcome_card(cwd: str, repo_trust: str = "trusted", width: int = 66) -> str:
    """
    Renders an elegant rounded-corner welcome card matching Claude Code's startup UX.
    Uses precise display_width alignment to ensure box borders never drift.
    """
    branch = get_git_branch(cwd)
    branch_str = f" ({c(branch, COLOR_CYAN)})" if branch else ""
    short_cwd = format_short_path(cwd)
    badges = get_model_status_badges()

    lines = [
        f"{COLOR_BOLD}{COLOR_CYAN}🪄 Makewand (v3.1.0){COLOR_RESET}",
        f"多模型智能调度 · {short_cwd}{branch_str}",
        f"模型状态: {badges}",
    ]

    if repo_trust == "untrusted":
        lines.append(f"{COLOR_YELLOW}🛡️ 仓库模式: UNTRUSTED (已启用 Bubblewrap 物理沙箱严格只读){COLOR_RESET}")

    lines.append("")
    lines.append(f"输入自然语言直接对话或开发，输入 {COLOR_CYAN}/help{COLOR_RESET} 查看可用指令")

    out = []
    inner_width = width - 4
    out.append(f"{COLOR_GRAY}╭" + ("─" * (width - 2)) + f"╮{COLOR_RESET}")
    for l in lines:
        padded = pad_display(l, inner_width)
        out.append(f"{COLOR_GRAY}│{COLOR_RESET} {padded} {COLOR_GRAY}│{COLOR_RESET}")
    out.append(f"{COLOR_GRAY}╰" + ("─" * (width - 2)) + f"╯{COLOR_RESET}")
    return "\n".join(out)


def print_help_menu():
    """Prints a structured slash command reference aligned with Claude Code."""
    commands = [
        ("/help, /?", "显示所有可用指令与交互指南"),
        ("/status, /quota", "查看五大模型健康状态、配额百分比与限流重置时间"),
        ("/diff", "高亮查看当前工作区所有未提交的 git 改动"),
        ("/review", "对当前 git diff 触发独立跨模型红队代码审查"),
        ("/compact", "压缩/精炼当前长对话历史上下文，节省模型 Token"),
        ("/clear", "清屏并重置当前交互会话的上下文记忆"),
        ("/models", "查看动态探测发现的各厂商最新旗舰模型版本"),
        ("/probe", "强制向本机已安装的各大 AI CLI 发起实时探活"),
        ("/race <任务>", "在隔离临时沙箱中并发派发双模型竞速对比与裁判裁决"),
        ("/search <关键字>", "安全带预算搜索代码，避开 SQLite、大文件与虚拟环境"),
        ("/sandbox <命令>", "在 Bubblewrap 物理沙箱中运行指定 shell 命令"),
        ("/observe", "全局巡检跨终端与 tmux 中的活跃 AI 会话"),
        ("/tier <档位>", "切换推理档位 (auto, fast, standard, deep)"),
        ("/model <模型>", "临时锁定指定模型引擎 (auto, claude, codex, grok, agy, muse...)"),
        ("/run <任务>", "显式强制启动全套多模型编码、测试门禁与红队自愈流水线"),
        ("/chat <提问>", "显式以技术问答/分析模式咨询主力大模型"),
        ("/multiline", "开启多行长文本粘贴模式 (输入 EOF 或 Ctrl+D 提交)"),
        ("/exit, /quit", "退出交互会话 (快捷键: Ctrl+D)"),
    ]

    print(f"\n{COLOR_BOLD}Commands:{COLOR_RESET}")
    max_cmd_len = max(len(cmd) for cmd, _ in commands)
    for cmd, desc in commands:
        cmd_padded = cmd.ljust(max_cmd_len + 2)
        print(f"  {COLOR_CYAN}{cmd_padded}{COLOR_RESET}{COLOR_GRAY}{desc}{COLOR_RESET}")
    print(f"\n  {COLOR_GRAY}提示: 直接输入自然语言即可开始。支持行尾 '\\' 或 \"\"\" 换行延续。{COLOR_RESET}\n")


def show_git_diff(cwd: str):
    """Shows git diff of working directory with syntax coloring."""
    diff_text = get_git_diff(cwd)
    if not diff_text or not diff_text.strip():
        print(c("\n✔ 工作区干净，没有未提交的代码改动。\n", COLOR_GREEN))
        return

    print(c(f"\n─── 未提交改动 (Git Diff) ───", COLOR_BOLD + COLOR_CYAN))
    for line in diff_text.splitlines():
        if line.startswith("diff --git") or line.startswith("index "):
            print(f"{COLOR_BOLD}{line}{COLOR_RESET}")
        elif line.startswith("---") or line.startswith("+++"):
            print(f"{COLOR_BOLD}{line}{COLOR_RESET}")
        elif line.startswith("@@"):
            print(f"{COLOR_CYAN}{line}{COLOR_RESET}")
        elif line.startswith("+"):
            print(f"{COLOR_GREEN}{line}{COLOR_RESET}")
        elif line.startswith("-"):
            print(f"{COLOR_RED}{line}{COLOR_RESET}")
        else:
            print(line)
    print(c("────────────────────────────\n", COLOR_GRAY))


def handle_conversational_turn(
    user_input: str,
    conversation_history: List[Dict[str, str]],
    cwd: str,
    tier: str = "standard",
    repo_trust: str = "trusted",
    stream: bool = True,
    forced_engine: Optional[str] = None,
):
    """
    Handles a conversational query/explanation/analysis turn.
    Maintains session history across turns to emulate Claude Code conversational context.
    """
    # Build augmented prompt with recent conversation history
    context_prefix = ""
    if conversation_history:
        recent_turns = conversation_history[-6:]  # Last 3 turns
        formatted_history = []
        for turn in recent_turns:
            role = "用户" if turn["role"] == "user" else "AI助手"
            c_text = turn["content"]
            if len(c_text) > 1500:
                c_text = c_text[:1500] + "... [历史截断]"
            formatted_history.append(f"{role}: {c_text}")
        context_prefix = (
            "【前序会话上下文】\n"
            + "\n".join(formatted_history)
            + "\n\n【用户最新问题】\n"
        )

    full_prompt = context_prefix + user_input if context_prefix else user_input

    if forced_engine and forced_engine != "auto":
        primary = forced_engine
        sorted_engines = [forced_engine]
    else:
        cache = get_or_update_status(force_probe=False)
        available_coders, _, route_meta = select_optimal_engine_pair(user_input, tier=tier, cache=cache)
        primary = route_meta.get("primary_coder") or (available_coders[0] if available_coders else "agy")
        sorted_engines = available_coders if available_coders else [primary, "agy", "claude", "codex", "local"]
        if primary not in sorted_engines:
            sorted_engines.insert(0, primary)

    print(f"{COLOR_GRAY}● [{primary.upper()}] 正在思考...{COLOR_RESET}\n")

    response_text = None
    for eng in sorted_engines:
        try:
            ok, out, err = dispatch_task(
                eng,
                full_prompt,
                cwd=cwd,
                timeout=180,
                tier=tier,
                stream=False,
                readonly=True,
                repo_trust=repo_trust,
            )
            if ok and out and out.strip():
                response_text = out.strip()
                break
        except Exception:
            continue

    if response_text:
        rendered = render_terminal_markdown(response_text)
        print(rendered)
        print()
        # Save turn to history
        conversation_history.append({"role": "user", "content": user_input})
        conversation_history.append({"role": "assistant", "content": response_text})
    else:
        print(c("⚠ 未能获取模型回答，请检查模型状态（/status）或网络连接。", COLOR_YELLOW))
        print()


def setup_readline():
    """Initializes readline with history file and tab completers."""
    if readline is None:
        return

    hist_dir = Path.home() / ".config" / "makewand"
    hist_dir.mkdir(parents=True, exist_ok=True)
    hist_file = str(hist_dir / "history")

    try:
        if os.path.exists(hist_file):
            readline.read_history_file(hist_file)
    except Exception:
        pass

    atexit.register(
        lambda: readline.write_history_file(hist_file)
        if os.path.exists(hist_dir)
        else None
    )

    import glob

    def completer(text, state):
        try:
            line = readline.get_line_buffer()
        except Exception:
            line = text

        # If line starts with /, complete slash commands
        if line.lstrip().startswith("/") and " " not in line.lstrip():
            options = [cmd for cmd in SLASH_COMMANDS if cmd.startswith(text)]
        else:
            # File and directory path auto-completion
            try:
                expanded = os.path.expanduser(text)
                raw_matches = glob.glob(expanded + "*")
                options = []
                for m in raw_matches:
                    if os.path.isdir(m):
                        options.append(m + "/")
                    else:
                        options.append(m)
            except Exception:
                options = []

        if state < len(options):
            return options[state]
        return None

    readline.set_completer(completer)
    try:
        readline.set_completer_delims(" \t\n`~!@#$%^&*()=+[{]}\\|;:\'\",<>?")
    except Exception:
        pass
    readline.parse_and_bind("tab: complete")


def start_interactive_session(repo_trust: str = "trusted"):
    """
    Main interactive loop mimicking Claude Code's terminal UX.
    """
    setup_readline()
    cwd = os.getcwd()

    # Display clean Claude Code-style card
    print(render_welcome_card(cwd, repo_trust=repo_trust))
    print()

    current_tier = "auto"
    current_engine = "auto"
    conversation_history: List[Dict[str, str]] = []

    while True:
        try:
            # Minimalist prompt matching Claude Code: bold cyan '>'
            if sys.stdin.isatty():
                prompt_str = f"\001{COLOR_CYAN}{COLOR_BOLD}\002>\001{COLOR_RESET}\002 "
            else:
                prompt_str = "> "
            first_line = input(prompt_str)
        except KeyboardInterrupt:
            print("\n")
            continue
        except EOFError:
            print(f"\n{COLOR_CYAN}退出 Makewand 会话。再见！{COLOR_RESET}")
            break

        lines = [first_line]
        # Clean continuation prompt matching Claude Code: 2 spaces
        cont_prompt = "  "

        # Handle line continuation via trailing backslash or unclosed triple quotes
        while True:
            cur_full = "\n".join(lines)
            last = lines[-1].rstrip()
            if last.endswith("\\"):
                lines[-1] = last[:-1]
                try:
                    lines.append(input(cont_prompt))
                    continue
                except (KeyboardInterrupt, EOFError):
                    break
            if (cur_full.count('"""') % 2 == 1) or (cur_full.count("'''") % 2 == 1):
                try:
                    lines.append(input(cont_prompt))
                    continue
                except (KeyboardInterrupt, EOFError):
                    break
            break

        user_input = "\n".join(lines).strip()
        if not user_input:
            continue

        # Handle Slash Commands
        lower = user_input.lower()
        if lower in ("/exit", "/quit", "exit", "quit"):
            print(f"{COLOR_CYAN}退出 Makewand 会话。再见！{COLOR_RESET}")
            break

        elif lower in ("/help", "/?", "help"):
            print_help_menu()
            continue

        elif lower in ("/status", "/quota"):
            from makewand.cli import cmd_status

            class DummyArgs:
                probe = False

            cmd_status(DummyArgs())
            continue

        elif lower == "/diff":
            show_git_diff(cwd)
            continue

        elif lower == "/compact":
            if not conversation_history:
                print(c("当前会话暂无历史上下文，无需压缩。", COLOR_GRAY))
            else:
                before_count = len(conversation_history)
                # Keep only the last 2 turns (4 messages)
                conversation_history = conversation_history[-4:]
                print(c(f"✔ 会话上下文已压缩 (从 {before_count} 条消息精炼为 {len(conversation_history)} 条)。", COLOR_GREEN))
            continue

        elif lower == "/probe":
            from makewand.cli import cmd_status

            class DummyArgs:
                probe = True

            cmd_status(DummyArgs())
            continue

        elif lower == "/models":
            from makewand.cli import cmd_models

            cmd_models(None)
            continue

        elif lower == "/review":
            run_review(cwd=cwd, stream=True, repo_trust=repo_trust)
            continue

        elif lower == "/clear":
            os.system("clear" if os.name == "posix" else "cls")
            conversation_history.clear()
            print(render_welcome_card(cwd, repo_trust=repo_trust))
            print()
            continue

        elif lower.startswith("/tier"):
            parts = user_input.split(maxsplit=1)
            if len(parts) > 1 and parts[1].strip() in ("auto", "fast", "standard", "deep"):
                current_tier = parts[1].strip()
                print(c(f"✔ 推理档位已切换为: {current_tier}", COLOR_GREEN))
            else:
                print(c(f"当前推理档位: {current_tier} (可选: auto, fast, standard, deep)", COLOR_YELLOW))
            continue

        elif lower.startswith(("/model", "/provider")):
            parts = user_input.split(maxsplit=1)
            from makewand.config import get_active_providers
            all_known = list(dict.fromkeys(["auto", "claude", "codex", "grok", "agy", "muse", "local", "aider"] + get_active_providers()))
            if len(parts) > 1 and parts[1].strip():
                target_m = parts[1].strip().lower()
                if target_m in all_known:
                    current_engine = target_m
                    print(c(f"✔ 交互会话已指定锁定引擎: {current_engine.upper() if current_engine != 'auto' else '自动智能路由'}", COLOR_GREEN))
                else:
                    print(c(f"未知引擎 '{target_m}'。可用选项: {', '.join(all_known)}", COLOR_YELLOW))
            else:
                desc = current_engine.upper() if current_engine != "auto" else "自动智能路由 (auto)"
                print(c(f"当前锁定引擎: {desc} (运行 '/model <engine>' 或 '/model auto' 切换)", COLOR_YELLOW))
            continue

        elif lower.startswith("/race"):
            parts = user_input.split(maxsplit=1)
            if len(parts) > 1 and parts[1].strip():
                run_race(parts[1].strip(), cwd=cwd, repo_trust=repo_trust)
            else:
                print(c("用法: /race <待比拼的任务或算法实现>", COLOR_YELLOW))
            continue

        elif lower.startswith("/search"):
            parts = user_input.split(maxsplit=1)
            if len(parts) > 1 and parts[1].strip():
                results = safe_search(parts[1].strip(), root_path=cwd)
                if not results:
                    print("未找到匹配内容。")
                else:
                    for r in results:
                        print(f"{c(r['file'], COLOR_CYAN)}:{c(str(r['line_num']), COLOR_YELLOW)}: {r['content']}")
                    print(c(f"\n共找到 {len(results)} 条匹配结果 (已避开冷归档、SQLite 与虚拟环境)。", COLOR_GREEN))
            else:
                print(c("用法: /search <关键字或正则>", COLOR_YELLOW))
            continue

        elif lower.startswith("/sandbox"):
            parts = user_input.split(maxsplit=1)
            if len(parts) > 1 and parts[1].strip():
                raw_cmd = parts[1].strip()
                if any(op in raw_cmd for op in ["|", ";", ">", "<", "&", "$", "`"]):
                    cmd_parts = ["bash", "-c", raw_cmd]
                else:
                    import shlex

                    try:
                        cmd_parts = shlex.split(raw_cmd)
                    except Exception:
                        cmd_parts = ["bash", "-c", raw_cmd]
                print(c(f"🛡️ 沙箱执行: {' '.join(cmd_parts)}", COLOR_CYAN))
                run_in_sandbox(cmd_parts, workspace=cwd, stream=True)
            else:
                print(c("用法: /sandbox <shell 命令>", COLOR_YELLOW))
            continue

        elif lower == "/observe":
            from makewand.observer import observe_all_dialogs, format_observation_markdown

            rep = observe_all_dialogs()
            print("\n" + format_observation_markdown(rep) + "\n")
            continue

        elif lower in ("/multiline", "/paste"):
            print(c("【多行输入模式】已开启：请在此输入或粘贴长文本，输入完毕后单独输入 'EOF' 或按 Ctrl+D 提交：", COLOR_CYAN))
            paste_lines = []
            while True:
                try:
                    pl = input(f"\001{COLOR_GRAY}\002... \001{COLOR_RESET}\002 ")
                    if pl.strip() == "EOF":
                        break
                    paste_lines.append(pl)
                except EOFError:
                    print()
                    break
                except KeyboardInterrupt:
                    print("\n" + c("已取消多行输入。", COLOR_YELLOW))
                    paste_lines = []
                    break
            user_input = "\n".join(paste_lines).strip()
            if not user_input:
                continue

        # Force chat command: /chat <query>
        if lower.startswith("/chat "):
            query = user_input[6:].strip()
            if query:
                handle_conversational_turn(query, conversation_history, cwd, current_tier, repo_trust)
            continue

        # Force pipeline command: /run <task>
        if lower.startswith("/run "):
            task = user_input[5:].strip()
            if task:
                try:
                    run_pipeline(
                        task,
                        cwd=cwd,
                        tier=current_tier,
                        stream=True,
                        auto_fix=True,
                        repo_trust=repo_trust,
                        forced_engine=current_engine if current_engine != "auto" else None,
                    )
                except KeyboardInterrupt:
                    print(c("\n⚠ 任务已被用户中断 (Ctrl+C)。", COLOR_YELLOW))
                except Exception as e:
                    print(c(f"\n❌ 执行遇到异常: {e}", COLOR_RED))
                print()
            continue

        # Check for identity queries or greetings to respond conversationally
        if is_identity_or_chit_chat(user_input):
            print("\n" + render_terminal_markdown(get_identity_message()) + "\n")
            conversation_history.append({"role": "user", "content": user_input})
            conversation_history.append({"role": "assistant", "content": get_identity_message()})
            continue

        # Smart Intent Classification:
        # If user is asking an explanatory/analysis question rather than modifying code,
        # run Claude Code-style conversational turn instead of the heavy CI/CD deployment pipeline!
        intent = classify_prompt_intent(user_input)

        if intent in ("identity", "explain"):
            handle_conversational_turn(
                user_input,
                conversation_history,
                cwd,
                tier=current_tier,
                repo_trust=repo_trust,
                forced_engine=current_engine if current_engine != "auto" else None,
            )
            continue

        if intent == "review":
            run_review(cwd=cwd, stream=True, user_prompt=user_input, repo_trust=repo_trust)
            continue

        # Code modification / engineering task -> execute multi-model engineering pipeline!
        try:
            success = run_pipeline(
                user_input,
                cwd=cwd,
                tier=current_tier,
                forced_engine=current_engine if current_engine != "auto" else None,
                stream=True,
                auto_fix=True,
                repo_trust=repo_trust,
            )
            # Record summary in history
            conversation_history.append({"role": "user", "content": user_input})
            conversation_history.append({
                "role": "assistant",
                "content": f"代码修改与验证流水线已执行 ({'成功通过' if success else '未完全通过'})。",
            })
        except KeyboardInterrupt:
            print(c("\n⚠ 任务已被用户中断 (Ctrl+C)。", COLOR_YELLOW))
        except Exception as e:
            print(c(f"\n❌ 执行遇到异常: {e}", COLOR_RED))
        print()
