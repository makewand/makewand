"""
Makewand Interactive Console / REPL.
Allows users to launch Makewand simply by typing `makewand` without any arguments,
matching the UX of claude, codex, and agy.
"""

import os
import sys
import atexit
from pathlib import Path

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
    COLOR_RESET,
    ensure_config_dir
)
from makewand.health import load_status_cache, get_or_update_status
from makewand.discovery import discover_available_models
from makewand.orchestrator import (
    run_pipeline,
    run_review,
    run_race,
    is_identity_or_chit_chat,
    get_identity_message
)
from makewand.search import safe_search
from makewand.sandbox import run_in_sandbox

BANNER = rf"""{COLOR_CYAN}{COLOR_BOLD}
   __  ___      __                              __
  /  |/  /___ _/ /_____ _      ______ _____  ____/ /
 / /|_/ / __ `/ //_/ _ \ | /| / / __ `/ __ \/ __  / 
/ /  / / /_/ / ,< /  __/ |/ |/ / /_/ / / / / /_/ /  
/_/  /_/\__,_/_/|_|\___/|__/|__/\__,_/_/ /_/\__,_/   v3.1{COLOR_RESET}

  ✨ {COLOR_BOLD}零成本多模型 AI 订阅与全生态编程工具统一调度中枢{COLOR_RESET}
  统合调度: {COLOR_GREEN}AGY (Google){COLOR_RESET} · {COLOR_BLUE}Claude Code{COLOR_RESET} · {COLOR_CYAN}Codex (OpenAI){COLOR_RESET} · {COLOR_RED}Grok (xAI){COLOR_RESET} · {COLOR_PURPLE}Muse (Meta){COLOR_RESET} · {COLOR_YELLOW}Aider/API/Local{COLOR_RESET}
"""

SLASH_COMMANDS = [
    "/help", "/?",
    "/status", "/quota",
    "/probe",
    "/models",
    "/review",
    "/race",
    "/search",
    "/sandbox",
    "/observe",
    "/tier",
    "/clear",
    "/exit", "/quit"
]

def setup_readline():
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

    atexit.register(lambda: readline.write_history_file(hist_file) if os.path.exists(hist_dir) else None)

    def completer(text, state):
        options = [cmd for cmd in SLASH_COMMANDS if cmd.startswith(text)]
        if state < len(options):
            return options[state]
        return None

    readline.set_completer(completer)
    readline.parse_and_bind("tab: complete")

def print_status_bar():
    from makewand.health import calculate_provider_quota
    cache = load_status_cache()
    def fmt(name, key):
        info = cache.get(key, {})
        st = info.get("status", "unknown")
        quota = calculate_provider_quota(key, info)
        pct = quota["percentage"]
        if st == "healthy":
            return f"{COLOR_GREEN}🟢 {name} {pct}%{COLOR_RESET}"
        elif st == "limited":
            return f"{COLOR_RED}🔴 {name} 0% (限流){COLOR_RESET}"
        elif st == "needs_auth":
            return f"{COLOR_YELLOW}🔑 {name} (需授权){COLOR_RESET}"
        else:
            return f"⚪ {name}"

    bar = f"  状态: {fmt('AGY', 'agy')} | {fmt('Claude', 'claude')} | {fmt('Codex', 'codex')} | {fmt('Grok', 'grok')} | {fmt('Muse', 'muse')}"
    print(bar)

def print_help_menu():
    print(f"\n{COLOR_BOLD}【Makewand 交互模式内置指令】{COLOR_RESET}")
    print(f"  {COLOR_CYAN}/status, /quota{COLOR_RESET}      查看五大模型订阅健康度与额度看板")
    print(f"  {COLOR_CYAN}/probe{COLOR_RESET}              强制对本机已安装的 AI CLI 发起实时探活")
    print(f"  {COLOR_CYAN}/models{COLOR_RESET}             查看动态探测到的各大模型版本")
    print(f"  {COLOR_CYAN}/review{COLOR_RESET}             对当前工作区未提交的 git diff 进行独立红队审查")
    print(f"  {COLOR_CYAN}/race <任务>{COLOR_RESET}        在隔离临时沙箱中并发派发两组模型竞速比拼")
    print(f"  {COLOR_CYAN}/search <关键字>{COLOR_RESET}    安全带预算搜索代码，避开数据库与冷归档")
    print(f"  {COLOR_CYAN}/sandbox <命令>{COLOR_RESET}     在 Bubblewrap 物理沙箱中运行命令")
    print(f"  {COLOR_CYAN}/tier <auto|fast|standard|deep>{COLOR_RESET} 切换当前任务推理档位")
    print(f"  {COLOR_CYAN}/clear{COLOR_RESET}              清屏")
    print(f"  {COLOR_CYAN}/exit, /quit{COLOR_RESET}        退出交互会话 (快捷键: Ctrl+D)\n")
    print(f"  {COLOR_YELLOW}直接输入自然语言需求，即可自动触发全链路跨模型编码、审查与自愈！{COLOR_RESET}\n")

def start_interactive_session(repo_trust: str = "trusted"):
    setup_readline()
    print(BANNER)
    cwd = os.getcwd()
    print(f"  {COLOR_BOLD}工作目录:{COLOR_RESET} {cwd}")
    if repo_trust == "untrusted":
        print(c("  🛡️ 仓库信任级别: UNTRUSTED (已强制启用 Bubblewrap 全隔离与严格只读挂载)", COLOR_YELLOW))
    print_status_bar()
    print(f"\n  输入您的任务或提问直接开始；输入 {COLOR_CYAN}/help{COLOR_RESET} 查看内置快捷指令。按 {COLOR_YELLOW}Ctrl+C{COLOR_RESET} 取消当前输入，{COLOR_YELLOW}Ctrl+D{COLOR_RESET} 退出。\n")

    current_tier = "auto"

    while True:
        try:
            # Clean, minimalist prompt matching Claude Code and Codex CLI
            prompt_str = f"\001{COLOR_CYAN}{COLOR_BOLD}\002>\001{COLOR_RESET}\002 "
            user_input = input(prompt_str).strip()
        except KeyboardInterrupt:
            print("\n")
            continue
        except EOFError:
            print(f"\n{COLOR_CYAN}退出 Makewand 会话。再见！{COLOR_RESET}")
            break

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
            print(BANNER)
            print_status_bar()
            continue

        elif lower.startswith("/tier"):
            parts = user_input.split(maxsplit=1)
            if len(parts) > 1 and parts[1].strip() in ("auto", "fast", "standard", "deep"):
                current_tier = parts[1].strip()
                print(c(f"✔ 推理档位已切换为: {current_tier}", COLOR_GREEN))
            else:
                print(c(f"当前推理档位: {current_tier} (可选: auto, fast, standard, deep)", COLOR_YELLOW))
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

        # Check for identity queries or greetings to respond conversationally
        if is_identity_or_chit_chat(user_input):
            print(f"\n{get_identity_message()}\n")
            continue

        # Regular natural language prompt -> run orchestrator pipeline!
        try:
            run_pipeline(
                user_input,
                cwd=cwd,
                tier=current_tier,
                stream=True,
                auto_fix=True,
                repo_trust=repo_trust
            )
        except KeyboardInterrupt:
            print(c("\n⚠ 任务已被用户中断 (Ctrl+C)。", COLOR_YELLOW))
        except Exception as e:
            print(c(f"\n❌ 执行遇到异常: {e}", COLOR_RED))
        print("\n")
