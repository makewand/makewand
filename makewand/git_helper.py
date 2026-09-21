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
