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


EXCLUDED_NON_AI_COMMANDS: Set[str] = {
    "tmux", "screen", "git", "grep", "egrep", "fgrep", "rg", "find", "fd",
    "ls", "vim", "nvim", "nano", "vi", "emacs", "cat", "less", "more", "man",
    "ssh", "rsync", "tail", "head", "strace", "lsof", "gdb", "watch", "which",
    "whereis", "awk", "sed", "cut", "sort", "uniq", "diff", "colordiff", "patch",
    "make", "ninja", "gcc", "g++", "clang", "cargo", "go", "npm", "yarn", "pnpm",
    "bun", "pip", "pip3", "pytest", "python-m-pytest", "node-gyp",
}


