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


def get_active_ai_processes() -> List[Dict[str, Any]]:
    """
    Scans host processes to find active AI coding tools (Claude, Codex, Antigravity, Grok, Muse, Aider, Makewand).
    Returns list of process info dictionaries.
    """
    ai_processes = []
    my_pid = os.getpid()
    try:
        out = subprocess.check_output(
            ["ps", "-eo", "pid,ppid,tty,etime,comm,args"],
            stderr=subprocess.DEVNULL,
            timeout=3
        ).decode("utf-8", errors="replace")

        for line in out.strip().splitlines()[1:]:
            parts = line.strip().split(None, 5)
            if len(parts) < 6:
                continue
            pid_s, ppid_s, tty, etime, comm, args = parts
            if not pid_s.isdigit():
                continue
            pid = int(pid_s)
            if pid == my_pid:
                continue

            comm_lower = comm.lower()
            args_lower = args.lower()

            ai_type = None
            if "codex" in comm_lower or "bin/codex" in args_lower or "@openai/codex" in args_lower:
                ai_type = "codex"
            elif "claude" in comm_lower or "bin/claude" in args_lower or "@anthropic/claude" in args_lower:
                ai_type = "claude"
            elif "agy" in comm_lower or "antigravity" in comm_lower:
                ai_type = "agy"
            elif "grok" in comm_lower or "bin/grok" in args_lower:
                ai_type = "grok"
            elif "muse" in comm_lower or "bin/muse" in args_lower or "muse-bin" in comm_lower:
                ai_type = "muse"
            elif "aider" in comm_lower or "bin/aider" in args_lower:
                ai_type = "aider"
            elif "makewand" in comm_lower or "makewand" in args_lower:
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
                "cwd": cwd
            })
    except Exception:
        pass
    return ai_processes


def detect_cross_session_collisions(target_cwd: str) -> Dict[str, Any]:
    """
    Detects concurrent AI sessions operating in the same workspace or git repository.
    Returns structured collision diagnostics.
    """
    canonical_target = os.path.realpath(target_cwd)
    repo_root = get_git_repo_toplevel(canonical_target)
    worktrees = get_git_worktrees(canonical_target) if repo_root else []

    all_panes = get_all_active_tmux_panes()
    ai_procs = get_active_ai_processes()

    # Match AI processes to tmux panes where possible
    pane_map_by_pid: Dict[int, Dict[str, Any]] = {p["pane_pid"]: p for p in all_panes}
    pane_map_by_tty: Dict[str, Dict[str, Any]] = {}
    for p in all_panes:
        t = p.get("tty", "")
        if t.startswith("/dev/"):
            t = t[5:]
        if t:
            pane_map_by_tty[t] = p

    collisions = []
    same_worktree_sessions = []
    same_repo_sessions = []

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

        # Location identification (tmux pane or external terminal)
        tty = proc.get("tty", "")
        tty_clean = tty[5:] if tty.startswith("/dev/") else tty
        pane_info = pane_map_by_pid.get(proc.get("ppid", 0)) or pane_map_by_tty.get(tty_clean)
        location = pane_info["pane_id"] if pane_info else f"TTY {tty}"

        # 1. Exact same worktree collision
        if p_cwd == canonical_target:
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
        # 2. Same Git repository root collision (different worktree or subdir)
        elif repo_root and (p_cwd == repo_root or p_cwd.startswith(repo_root + os.sep)):
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

    # Check Git lock file
    git_locked = False
    index_lock_path = None
    if repo_root:
        idx_lock = Path(repo_root) / ".git" / "index.lock"
        if idx_lock.exists():
            git_locked = True
            index_lock_path = str(idx_lock)

    has_collision = (len(same_worktree_sessions) > 0) or git_locked

    # Suggested isolated worktree command
    repo_name = Path(repo_root).name if repo_root else Path(canonical_target).name
    tag = time.strftime("%m%d_%H%M")
    suggested_wt_cmd = f"git worktree add ../{repo_name}-wt-{tag} -b feat/task-{tag}"

    return {
        "has_collision": has_collision,
        "target_cwd": canonical_target,
        "repo_root": repo_root,
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
    lines.append(c("\n╔══════════════════════════════════════════════════════════════════════════════╗", COLOR_BOLD + COLOR_RED))
    lines.append(c("║  ⚠ [Makewand 碰撞感知] 检测到多 Session 正在共享操作相同工作区！           ║", COLOR_BOLD + COLOR_RED))
    lines.append(c("╚══════════════════════════════════════════════════════════════════════════════╝", COLOR_BOLD + COLOR_RED))
    lines.append(c("• 规则铁律: 严禁多 Session 在同一工作树下并发动手，避免代码状态踩踏覆盖。", COLOR_YELLOW))

    same_wt = report.get("same_worktree_sessions", [])
    if same_wt:
        lines.append(c(f"• 冲突会话列表 (共 {len(same_wt)} 个并发会话活跃在当前目录):", COLOR_BOLD))
        for s in same_wt:
            lines.append(f"  - 进程 PID {c(str(s['pid']), COLOR_BOLD + COLOR_YELLOW)}: 工具 {c(s['ai_type'], COLOR_CYAN)} "
                         f"位于 [{c(s['location'], COLOR_PURPLE)}] (目录: {s['cwd']})")

    if report.get("git_locked"):
        lines.append(c(f"• Git 状态: 检测到活跃的 .git/index.lock ({report.get('index_lock_path')})，有写操作正在进行！", COLOR_RED + COLOR_BOLD))

    lines.append(c("\n💡 推荐隔离方案 (创建独立 worktree 分支开发):", COLOR_GREEN + COLOR_BOLD))
    lines.append(f"   {c(report.get('suggested_worktree_cmd', ''), COLOR_BOLD + COLOR_CYAN)}")
    lines.append("   (可使用 --force 或 --allow-collision 明确强制执行)\n")

    return "\n".join(lines)


def get_all_active_sessions_report() -> Dict[str, Any]:
    """Generates a complete host-wide session and worktree topology report."""
    ai_procs = get_active_ai_processes()
    panes = get_all_active_tmux_panes()
    pane_map = {p["pane_pid"]: p for p in panes}

    by_repo: Dict[str, List[Dict[str, Any]]] = {}
    for proc in ai_procs:
        cwd = proc.get("cwd", "unknown")
        repo = get_git_repo_toplevel(cwd) or cwd
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
