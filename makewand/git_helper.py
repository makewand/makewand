"""
Git workspace resilience and isolated worktree management.
"""

import os
import shutil
import subprocess
from pathlib import Path
from makewand.config import c, COLOR_YELLOW

def run_git_cmd(cmd, cwd=None):
    try:
        res = subprocess.run(
            cmd,
            shell=True if isinstance(cmd, str) else False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            cwd=cwd,
            timeout=30
        )
        return res.returncode, res.stdout, res.stderr
    except Exception as e:
        return -1, "", str(e)

def ensure_git_worktree(cwd: str) -> bool:
    """
    Ensures cwd is inside a git repository.
    If not, automatically initializes a lightweight shadow git tracking tree
    so diff extraction and red-team audits work seamlessly without manual git init.
    Protects root system directories (/, /tmp, ~) from accidental init.
    """
    if not cwd:
        cwd = os.getcwd()
    resolved = Path(cwd).resolve()
    if resolved in [Path("/"), Path("/tmp"), Path.home()]:
        return False

    code, _, _ = run_git_cmd("git rev-parse --is-inside-work-tree", cwd=cwd)
    if code != 0:
        print(c("[Makewand Git] 检测到当前目录尚未初始化 Git，自动建立影子 Git 跟踪树...", COLOR_YELLOW))
        run_git_cmd("git init && git config user.name 'Makewand' && git config user.email 'makewand@local'", cwd=cwd)
        run_git_cmd("git add -A", cwd=cwd)
        run_git_cmd("git commit -m 'Makewand baseline snapshot' --allow-empty", cwd=cwd)
        return True
    return False

def get_git_diff(cwd: str) -> str:
    """
    Extracts git diff for the workspace, including newly added, modified, and deleted files.
    Auto-inits shadow git with baseline commit if necessary.
    """
    if not cwd:
        cwd = os.getcwd()
    ensure_git_worktree(cwd)
    run_git_cmd("git add -A --intent-to-add 2>/dev/null || git add -N . 2>/dev/null || true", cwd=cwd)
    code, diff_out, _ = run_git_cmd("git diff HEAD", cwd=cwd)
    if code != 0 or not diff_out or not diff_out.strip():
        code, diff_out, _ = run_git_cmd("git diff", cwd=cwd)
    return diff_out.strip() if diff_out else ""

def clone_isolated_worktree(src_dir: str, target_dir: Path):
    """
    Safely copies/clones workspace into an isolated directory for race or testing,
    skipping system sockets, fifos, .git, and cache directories.
    """
    target_dir.mkdir(parents=True, exist_ok=True)
    resolved = Path(src_dir).resolve()
    if resolved not in [Path("/"), Path("/tmp"), Path.home()]:
        for item in resolved.glob("*"):
            if item.name not in [".git", "__pycache__", ".pytest_cache"]:
                try:
                    if item.is_dir() and not item.is_symlink():
                        shutil.copytree(item, target_dir / item.name, dirs_exist_ok=True, ignore_dangling_symlinks=True)
                    elif item.is_file() and not item.is_socket():
                        shutil.copy2(item, target_dir / item.name)
                except Exception:
                    pass

    # Initialize isolated git baseline in target_dir so all existing files are committed
    run_git_cmd("git init && git config user.name 'Makewand' && git config user.email 'makewand@local'", cwd=str(target_dir))
    run_git_cmd("git add -A", cwd=str(target_dir))
    run_git_cmd("git commit -m 'Makewand isolated baseline' --allow-empty", cwd=str(target_dir))

def get_active_interactive_working_trees():
    """
    Returns a mapping of canonical working directory paths to session details
    for all active interactive AI sessions (tmux panes + external terminals).
    """
    active_trees = {}
    try:
        from makewand.observer import get_external_ai_sessions, get_active_tmux_sessions, get_session_cwd
        # 1. Tmux sessions
        for s in get_active_tmux_sessions():
            cwd = get_session_cwd(s)
            if cwd and os.path.exists(cwd):
                canon = str(Path(cwd).resolve())
                active_trees[canon] = {
                    "source": "tmux",
                    "session_name": s,
                    "cwd": canon
                }

        # 2. External AI sessions
        for ext in get_external_ai_sessions():
            cwd = ext.get("cwd")
            if cwd and os.path.exists(cwd):
                canon = str(Path(cwd).resolve())
                if canon not in active_trees:
                    active_trees[canon] = {
                        "source": "external_terminal",
                        "tty": ext.get("tty"),
                        "pid": ext.get("pid"),
                        "ai_type": ext.get("ai_type"),
                        "cwd": canon
                    }
    except Exception:
        pass
    return active_trees

def check_working_tree_isolation(target_dir: str):
    """
    Checks if target_dir overlaps with any active interactive AI session.
    Returns (is_safe, conflict_warning_message).
    """
    if not target_dir:
        return True, None
    try:
        target_path = Path(target_dir).resolve()
        active = get_active_interactive_working_trees()
        for active_cwd, info in active.items():
            active_path = Path(active_cwd).resolve()
            if target_path == active_path or active_path in target_path.parents:
                src_desc = f"tmux 会话 [{info['session_name']}]" if info.get("source") == "tmux" else f"外部独立终端 [{info.get('tty')} · PID {info.get('pid')} · {info.get('ai_type')}]"
                return False, f"工作区 {target_dir} 正由活跃交互会话 ({src_desc}) 操作中"
    except Exception:
        pass
    return True, None
