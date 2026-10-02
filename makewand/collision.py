"""
Makewand Cross-Session Collision Detector & Host-wide Worktree Awareness.

Enforces the P920 multi-session isolation rule:
"严禁多 Session 共享同一棵工作树动手：
 站主在多终端可能并行开启 Claude、Codex 或多个 Antigravity 会话。
 操作同一个仓库前，必须先只读探测（git status、git worktree list、ps 进程检查）。
 并发需求一律使用独立 worktree（如 git worktree add），禁止在同一未提交状态上互相踩踏。"
"""

import os
import sys
import json
import time
import subprocess
from pathlib import Path
from typing import Dict, List, Optional, Any, Set, Tuple

from makewand.config import (
    c,
    COLOR_BOLD,
    COLOR_CYAN,
    COLOR_GREEN,
    COLOR_YELLOW,
    COLOR_RED,
    COLOR_PURPLE,
    COLOR_RESET,
)
from makewand.git_helper import run_git_cmd


def get_git_repo_toplevel(cwd: str) -> Optional[str]:
    """Returns canonical root directory of the git repo containing cwd, or None."""
    code, out, _ = run_git_cmd(["git", "rev-parse", "--show-toplevel"], cwd=cwd)
    if code == 0 and out.strip():
        try:
            return os.path.realpath(out.strip())
        except Exception:
            return out.strip()
    return None


def get_git_common_dir(cwd: str) -> Optional[str]:
    """Returns canonical common git directory of the git repo containing cwd, or None."""
    code, out, _ = run_git_cmd(["git", "rev-parse", "--git-common-dir"], cwd=cwd)
    if code == 0 and out.strip():
        raw = out.strip()
        if not os.path.isabs(raw):
            raw = os.path.join(cwd, raw)
        try:
            return os.path.realpath(raw)
        except Exception:
            return os.path.abspath(raw)
    return None



def get_git_worktrees(cwd: str) -> List[Dict[str, str]]:
    """Returns list of worktrees registered in this git repository."""
    code, out, _ = run_git_cmd(["git", "worktree", "list", "--porcelain"], cwd=cwd)
    worktrees = []
    if code == 0 and out.strip():
        current_wt: Dict[str, str] = {}
        for line in out.strip().splitlines():
            line = line.strip()
            if not line:
                if current_wt:
                    worktrees.append(current_wt)
                    current_wt = {}
                continue
            if line.startswith("worktree "):
                current_wt["path"] = os.path.realpath(line[9:].strip())
            elif line.startswith("HEAD "):
                current_wt["head"] = line[5:].strip()
            elif line.startswith("branch "):
                current_wt["branch"] = line[7:].strip()
            elif line == "bare":
                current_wt["bare"] = "true"
        if current_wt:
            worktrees.append(current_wt)
    return worktrees


def get_all_active_tmux_panes() -> List[Dict[str, Any]]:
    """Discovers all active tmux panes and their current directory, PID, and TTY."""
    panes = []
    try:
        out = subprocess.check_output(
            ["tmux", "list-panes", "-a", "-F", "#{session_name}:#{window_index}.#{pane_index} #{pane_pid} #{pane_tty} #{pane_current_path}"],
            stderr=subprocess.DEVNULL,
            timeout=2
        ).decode("utf-8", errors="replace")
        for line in out.strip().splitlines():
            parts = line.strip().split(None, 3)
            if len(parts) >= 4:
                pane_id, pid_s, tty, path = parts
                if pid_s.isdigit():
                    panes.append({
                        "pane_id": pane_id,
                        "session_name": pane_id.split(":")[0],
                        "pane_pid": int(pid_s),
                        "tty": tty,
                        "path": os.path.realpath(path) if os.path.exists(path) else path
                    })
    except Exception:
        pass
    return panes


EXCLUDED_NON_AI_COMMANDS: Set[str] = {
    "tmux", "screen", "git", "grep", "egrep", "fgrep", "rg", "find", "fd",
    "ls", "vim", "nvim", "nano", "vi", "emacs", "cat", "less", "more", "man",
    "ssh", "rsync", "tail", "head", "strace", "lsof", "gdb", "watch", "which",
    "whereis", "awk", "sed", "cut", "sort", "uniq", "diff", "colordiff", "patch",
    "make", "ninja", "gcc", "g++", "clang", "cargo", "go", "npm", "yarn", "pnpm",
    "bun", "pip", "pip3", "pytest", "python-m-pytest", "node-gyp",
}


def is_ai_daemon_process(comm_lower: str, args_lower: str) -> bool:
    """Returns True if the process is a background support daemon (app-server, pid-updater, lsp, etc.)."""
    if "app-server" in args_lower or "app-server-daemon" in args_lower:
        return True
    if "--managed-daemon" in args_lower or "pid-update-loop" in args_lower:
        return True
    if comm_lower.startswith("codex-code-mode") or "codex-code-mode-host" in args_lower:
        return True
    if "language-server" in comm_lower or "language-server" in args_lower:
        return True
    return False


def get_process_parent_map() -> Dict[int, int]:
    """Discovers host process parent-child relationships mapping PID -> PPID."""
    parent_map: Dict[int, int] = {}
    try:
        out = subprocess.check_output(
            ["ps", "-eo", "pid,ppid"],
            stderr=subprocess.DEVNULL,
            timeout=3
        ).decode("utf-8", errors="replace")
        for line in out.strip().splitlines()[1:]:
            parts = line.strip().split()
            if len(parts) >= 2 and parts[0].isdigit() and parts[1].isdigit():
                parent_map[int(parts[0])] = int(parts[1])
    except Exception:
        pass
    return parent_map


def find_pane_for_process(
    proc: Dict[str, Any],
    pane_map_by_pid: Dict[int, Dict[str, Any]],
    pane_map_by_tty: Dict[str, Dict[str, Any]],
    parent_map: Optional[Dict[int, int]] = None
) -> Optional[Dict[str, Any]]:
    """Traces parent PIDs up the process tree to match the tmux pane shell PID."""
    ppid = proc.get("ppid", 0)
    pid = proc.get("pid", 0)

    # 1. Direct match on PPID or PID
    if ppid and ppid in pane_map_by_pid:
        return pane_map_by_pid[ppid]
    if pid and pid in pane_map_by_pid:
        return pane_map_by_pid[pid]

    # 2. Trace ancestor chain up to root (e.g. bash -> node -> codex)
    if parent_map:
        curr = ppid or pid
        visited = set()
        while curr and curr > 1 and curr not in visited:
            visited.add(curr)
            if curr in pane_map_by_pid:
                return pane_map_by_pid[curr]
            curr = parent_map.get(curr, 0)

    # 3. Fallback: match by TTY
    tty = proc.get("tty", "")
    tty_clean = tty[5:] if tty.startswith("/dev/") else tty
    if tty_clean and tty_clean in pane_map_by_tty:
        return pane_map_by_tty[tty_clean]

    return None


def get_active_ai_processes() -> List[Dict[str, Any]]:
    """
    Scans host processes to find active AI coding tools (Claude, Codex, Antigravity, Grok, Muse, Aider, Makewand).
    Filters out zombie/defunct processes, background daemons, and excludes non-AI wrapper commands.
    Returns list of process info dictionaries.
    """
    ai_processes = []
    my_pid = os.getpid()
    try:
        out = subprocess.check_output(
            ["ps", "-eo", "pid,ppid,tty,etime,state,comm,args"],
            stderr=subprocess.DEVNULL,
            timeout=3
        ).decode("utf-8", errors="replace")

        for line in out.strip().splitlines()[1:]:
            parts = line.strip().split(None, 6)
            if len(parts) < 7:
                continue
            pid_s, ppid_s, tty, etime, state, comm, args = parts
            if not pid_s.isdigit():
                continue
            pid = int(pid_s)
            if pid == my_pid:
                continue

            # Skip zombie/defunct processes
            if state.upper().startswith("Z") or "defunct" in args.lower() or "defunct" in comm.lower():
                continue

            comm_clean = comm.strip().rstrip(":")
            comm_lower = comm_clean.lower()
            comm_base = comm_clean.split(":")[0].strip().lower()
            args_lower = args.lower()

            # Ignore common non-AI commands even if an AI engine name appears in arguments
            if comm_lower in EXCLUDED_NON_AI_COMMANDS or comm_base in EXCLUDED_NON_AI_COMMANDS:
                continue

            # Ignore background helper daemons (app-server, pid-updater, lsp)
            if is_ai_daemon_process(comm_lower, args_lower):
                continue

            ai_type = None
            if comm_lower in ("codex", "codex-cli", "codex-bin") or comm_lower.startswith("codex-") or "bin/codex" in args_lower or "@openai/codex" in args_lower:
                ai_type = "codex"
            elif comm_lower in ("claude", "claude-code") or comm_lower.startswith("claude-") or "bin/claude" in args_lower or "@anthropic/claude" in args_lower:
                ai_type = "claude"
            elif comm_lower in ("agy", "antigravity", "antigravity-cli") or comm_lower.startswith(("agy-", "antigravity-")) or "bin/agy" in args_lower or "bin/antigravity" in args_lower:
                ai_type = "agy"
            elif comm_lower in ("grok", "grok-cli") or "bin/grok" in args_lower:
                ai_type = "grok"
            elif comm_lower in ("muse", "muse-bin") or comm_lower.startswith("muse-bin") or "bin/muse" in args_lower:
                ai_type = "muse"
            elif comm_lower in ("aider", "aider-chat") or "bin/aider" in args_lower or "-m aider" in args_lower:
                ai_type = "aider"
            elif comm_lower == "makewand" or comm_lower.startswith("makewand-"):
                ai_type = "makewand"
            elif comm_lower in ("python", "python3") or comm_lower.startswith("python3.") or comm_lower in ("sh", "bash", "zsh"):
                # Only match when executing the makewand package or entry point
                if any(pat in args_lower for pat in ["bin/makewand", "-m makewand", "makewand/cli.py", "makewand/__main__.py"]):
                    ai_type = "makewand"
                elif args_lower.split() and (args_lower.split()[0].endswith("/makewand") or args_lower.split()[0] == "makewand"):
                    ai_type = "makewand"

            if not ai_type:
                continue

            cwd = "unknown"
            try:
                raw_cwd = os.readlink(f"/proc/{pid}/cwd")
                cwd = os.path.realpath(raw_cwd)
            except Exception:
                pass

            ai_processes.append({
                "pid": pid,
                "ppid": int(ppid_s) if ppid_s.isdigit() else 0,
                "ai_type": ai_type,
                "comm": comm,
                "args": args[:120],
                "tty": tty,
                "etime": etime,
                "state": state,
                "cwd": cwd
            })
    except Exception:
        pass

    # Deduplicate wrapper launcher processes if the child engine process is active
    if len(ai_processes) > 1:
        child_pids = {p["pid"] for p in ai_processes}
        wrapper_pids = set()
        for p in ai_processes:
            parent_pid = p.get("ppid", 0)
            if parent_pid in child_pids:
                parent_proc = next((x for x in ai_processes if x["pid"] == parent_pid), None)
                if parent_proc and parent_proc["ai_type"] == p["ai_type"]:
                    wrapper_pids.add(parent_pid)
        if wrapper_pids:
            ai_processes = [p for p in ai_processes if p["pid"] not in wrapper_pids]

    return ai_processes


def detect_cross_session_collisions(target_cwd: str) -> Dict[str, Any]:
    """
    Detects concurrent AI sessions operating in the same workspace or git repository.
    Recognizes cross-worktree sessions via git-common-dir and registered worktrees.
    Returns structured collision diagnostics.
    """
    canonical_target = os.path.realpath(target_cwd)
    if os.path.isfile(canonical_target):
        canonical_target = os.path.dirname(canonical_target)

    repo_root = get_git_repo_toplevel(canonical_target)
    git_common_dir = get_git_common_dir(canonical_target) if repo_root else None
    worktrees = get_git_worktrees(canonical_target) if repo_root else []

    all_panes = get_all_active_tmux_panes()
    ai_procs = get_active_ai_processes()
    parent_map = get_process_parent_map()

    # Match AI processes to tmux panes where possible
    pane_map_by_pid: Dict[int, Dict[str, Any]] = {p["pane_pid"]: p for p in all_panes}
    pane_map_by_tty: Dict[str, Dict[str, Any]] = {}
    for p in all_panes:
        t = p.get("tty", "")
        if t.startswith("/dev/"):
            t = t[5:]
        if t:
            pane_map_by_tty[t] = p

    # Set of all registered worktree paths
    all_worktree_paths: Set[str] = set()
    for w in worktrees:
        wp = w.get("path")
        if wp:
            all_worktree_paths.add(os.path.realpath(wp))
    if repo_root:
        all_worktree_paths.add(repo_root)

    collisions = []
    same_worktree_sessions = []
    same_repo_sessions = []
    p_common_cache: Dict[str, Optional[str]] = {}

    # Exclude ancestors of this process to avoid self-collision warnings
    from makewand.observer import _own_ancestors
    own_ancestors = _own_ancestors()

    for proc in ai_procs:
        pid = proc["pid"]
        if pid in own_ancestors:
            continue
        p_cwd = proc.get("cwd")
        if not p_cwd or p_cwd == "unknown":
            continue

        pane_info = find_pane_for_process(proc, pane_map_by_pid, pane_map_by_tty, parent_map)
        tty = proc.get("tty", "")
        location = pane_info["pane_id"] if pane_info else f"TTY {tty}"

        # 1. Same worktree collision check
        is_same_worktree = False
        if repo_root:
            if p_cwd == canonical_target or p_cwd == repo_root or p_cwd.startswith(repo_root + os.sep):
                is_same_worktree = True
                # Disambiguate if p_cwd actually belongs to another registered worktree
                for other_wt in all_worktree_paths:
                    if other_wt != repo_root and (p_cwd == other_wt or p_cwd.startswith(other_wt + os.sep)):
                        if len(other_wt) > len(repo_root):
                            is_same_worktree = False
                            break
        else:
            is_same_worktree = (p_cwd == canonical_target) or p_cwd.startswith(canonical_target + os.sep)

        if is_same_worktree:
            item = {
                "pid": pid,
                "ai_type": proc["ai_type"],
                "comm": proc["comm"],
                "location": location,
                "tty": tty,
                "cwd": p_cwd,
                "collision_type": "same_worktree",
                "risk": "HIGH",
            }
            same_worktree_sessions.append(item)
            collisions.append(item)
        else:
            # 2. Cross-worktree / same repository concurrent session check
            is_same_repo = False
            for wt in all_worktree_paths:
                if p_cwd == wt or p_cwd.startswith(wt + os.sep):
                    is_same_repo = True
                    break

            if not is_same_repo and git_common_dir and os.path.exists(p_cwd):
                if p_cwd not in p_common_cache:
                    p_common_cache[p_cwd] = get_git_common_dir(p_cwd)
                p_common = p_common_cache[p_cwd]
                if p_common and p_common == git_common_dir:
                    is_same_repo = True

            if is_same_repo:
                item = {
                    "pid": pid,
                    "ai_type": proc["ai_type"],
                    "comm": proc["comm"],
                    "location": location,
                    "tty": tty,
                    "cwd": p_cwd,
                    "collision_type": "same_repo_concurrent",
                    "risk": "MEDIUM",
                }
                same_repo_sessions.append(item)
                collisions.append(item)

    # Check Git lock file for the specific worktree target
    git_locked = False
    index_lock_path = None
    if repo_root:
        candidate_locks = []
        wt_git = Path(repo_root) / ".git"
        if wt_git.is_file():
            # Linked worktree: index is located inside this worktree's specific gitdir
            try:
                content = wt_git.read_text().strip()
                if content.startswith("gitdir:"):
                    gdir = content[7:].strip()
                    if not os.path.isabs(gdir):
                        gdir = os.path.join(repo_root, gdir)
                    candidate_locks.append(Path(gdir) / "index.lock")
            except Exception:
                pass
        elif wt_git.is_dir():
            # Primary repository worktree: index is located directly in .git
            candidate_locks.append(wt_git / "index.lock")
        elif git_common_dir and os.path.realpath(repo_root) == os.path.realpath(str(Path(git_common_dir).parent)):
            candidate_locks.append(Path(git_common_dir) / "index.lock")

        for lock in candidate_locks:
            if lock.exists():
                git_locked = True
                index_lock_path = str(lock)
                break

    has_collision = (len(same_worktree_sessions) > 0) or git_locked

    # Suggested isolated worktree command
    repo_name = Path(repo_root).name if repo_root else Path(canonical_target).name
    tag = time.strftime("%m%d_%H%M")
    suggested_wt_cmd = f"git worktree add ../{repo_name}-wt-{tag} -b feat/task-{tag}"

    return {
        "has_collision": has_collision,
        "target_cwd": canonical_target,
        "repo_root": repo_root,
        "git_common_dir": git_common_dir,
        "worktrees": [w.get("path") for w in worktrees],
        "collisions": collisions,
        "same_worktree_sessions": same_worktree_sessions,
        "same_repo_sessions": same_repo_sessions,
        "git_locked": git_locked,
        "index_lock_path": index_lock_path,
        "suggested_worktree_cmd": suggested_wt_cmd,
    }


def format_collision_warning(report: Dict[str, Any]) -> str:
    """Formats human-readable collision warning string adhering to P920 isolation rules."""
    lines = []
    same_wt = report.get("same_worktree_sessions", [])
    same_repo = report.get("same_repo_sessions", [])
    git_locked = report.get("git_locked", False)

    if same_wt or git_locked:
        lines.append(c("\n╔══════════════════════════════════════════════════════════════════════════════╗", COLOR_BOLD + COLOR_RED))
        lines.append(c("║  ⚠ [Makewand 碰撞感知] 检测到多 Session 正在共享操作相同工作区！           ║", COLOR_BOLD + COLOR_RED))
        lines.append(c("╚══════════════════════════════════════════════════════════════════════════════╝", COLOR_BOLD + COLOR_RED))
        lines.append(c("• 规则铁律: 严禁多 Session 在同一工作树下并发动手，避免代码状态踩踏覆盖。", COLOR_YELLOW))
    else:
        lines.append(c("\n╔══════════════════════════════════════════════════════════════════════════════╗", COLOR_BOLD + COLOR_CYAN))
        lines.append(c("║  ℹ [Makewand 仓库感知] 检测到同仓库其他独立 Worktree 正在并发运行           ║", COLOR_BOLD + COLOR_CYAN))
        lines.append(c("╚══════════════════════════════════════════════════════════════════════════════╝", COLOR_BOLD + COLOR_CYAN))
        lines.append(c("• 工作树隔离状态良好: 当前工作区与外部工作树独立，请注意避免跨分支合并/推送冲突。", COLOR_GREEN))

    if same_wt:
        lines.append(c(f"• 冲突会话列表 (共 {len(same_wt)} 个并发会话活跃在当前工作区):", COLOR_BOLD))
        for s in same_wt:
            lines.append(f"  - 进程 PID {c(str(s['pid']), COLOR_BOLD + COLOR_YELLOW)}: 工具 {c(s['ai_type'], COLOR_CYAN)} "
                         f"位于 [{c(s['location'], COLOR_PURPLE)}] (目录: {s['cwd']})")

    if same_repo:
        lines.append(c(f"• 关联 Worktree 会话 (共 {len(same_repo)} 个会话在同仓库其他独立 worktree 运行):", COLOR_CYAN))
        for s in same_repo:
            lines.append(f"  - 进程 PID {c(str(s['pid']), COLOR_YELLOW)}: 工具 {c(s['ai_type'], COLOR_CYAN)} "
                         f"位于 [{c(s['location'], COLOR_PURPLE)}] (目录: {s['cwd']})")

    if report.get("git_locked"):
        lines.append(c(f"• Git 状态: 检测到活跃的 .git/index.lock ({report.get('index_lock_path')})，有写操作正在进行！", COLOR_RED + COLOR_BOLD))

    if same_wt:
        lines.append(c("\n💡 推荐隔离方案 (创建独立 worktree 分支开发):", COLOR_GREEN + COLOR_BOLD))
        lines.append(f"   {c(report.get('suggested_worktree_cmd', ''), COLOR_BOLD + COLOR_CYAN)}")
        lines.append("   (可使用 --force 或 --allow-collision 明确强制执行)\n")
    else:
        lines.append("")

    return "\n".join(lines)


def get_all_active_sessions_report() -> Dict[str, Any]:
    """Generates a complete host-wide session and worktree topology report."""
    ai_procs = get_active_ai_processes()
    panes = get_all_active_tmux_panes()

    common_dir_cache: Dict[str, Optional[str]] = {}
    worktrees_cache: Dict[str, List[Dict[str, str]]] = {}
    toplevel_cache: Dict[str, Optional[str]] = {}

    by_repo: Dict[str, List[Dict[str, Any]]] = {}
    for proc in ai_procs:
        cwd = proc.get("cwd", "unknown")
        repo = None
        if cwd and cwd != "unknown" and os.path.exists(cwd):
            if cwd not in common_dir_cache:
                common_dir_cache[cwd] = get_git_common_dir(cwd)
            common = common_dir_cache[cwd]
            if common:
                if cwd not in worktrees_cache:
                    worktrees_cache[cwd] = get_git_worktrees(cwd)
                wts = worktrees_cache[cwd]
                if wts and wts[0].get("path"):
                    repo = wts[0]["path"]
                else:
                    if cwd not in toplevel_cache:
                        toplevel_cache[cwd] = get_git_repo_toplevel(cwd)
                    repo = toplevel_cache[cwd] or common
            else:
                if cwd not in toplevel_cache:
                    toplevel_cache[cwd] = get_git_repo_toplevel(cwd)
                repo = toplevel_cache[cwd] or cwd
        else:
            repo = cwd

        if repo not in by_repo:
            by_repo[repo] = []
        by_repo[repo].append(proc)

    return {
        "timestamp": time.time(),
        "total_active_sessions": len(ai_procs),
        "total_tmux_panes": len(panes),
        "active_processes": ai_procs,
        "sessions_by_repo": by_repo
    }

