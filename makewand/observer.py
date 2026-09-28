"""
Makewand Session & Dialog Observer.
Monitors all running AI sessions (AGY / Codex / Claude / Tmux) across workspaces,
classifies operational patterns, detects deadlocks/anomalies, and derives
actionable optimization proposals for Makewand.
"""

import os
import re
import sys
import json
import time
import uuid
import signal
import subprocess
from pathlib import Path
from datetime import datetime
from typing import Dict, List, Optional, Any, Iterable
from makewand.config import (
    CONFIG_DIR,
    ensure_config_dir,
    c,
    COLOR_BOLD,
    COLOR_CYAN,
    COLOR_GREEN,
    COLOR_YELLOW,
    COLOR_RED,
    COLOR_RESET
)

# ---------------------------------------------------------------------------
# Ownership of processes for `observe --clean-hung`.
#
# makewand marks itself as a dispatcher at CLI start (MAKEWAND_DISPATCH_ID in
# os.environ), so every process it spawns inherits the marker in its initial
# environment (/proc/<pid>/environ). Sandboxed children run under bwrap
# --clearenv, but the bwrap process itself carries the marker. Only such marked,
# terminal-less (setsid) processes are ever cleanup candidates; interactive
# sessions, editors, shells, ssh, and the user's own claude/codex/grok/muse/agy
# sessions never carry the marker or own a controlling terminal.
# ---------------------------------------------------------------------------
DISPATCH_ENV_VAR = "MAKEWAND_DISPATCH_ID"
HUNG_MIN_AGE_SECONDS = 1800
PROTECTED_COMMS = frozenset({
    # shells
    "bash", "zsh", "sh", "dash", "fish", "ksh", "tcsh", "csh", "nu", "xonsh",
    # editors / pagers / interactive tools
    "vim", "nvim", "vi", "view", "emacs", "emacsclient", "nano", "micro", "hx", "helix", "kak",
    "less", "more", "man", "top", "htop", "btop", "watch", "tail", "psql", "mysql", "sqlite3",
    # remote sessions and multiplexers
    "ssh", "sshd", "mosh", "mosh-client", "mosh-server", "tmux", "tmux: server", "screen",
    # system
    "systemd", "init",
})


def new_dispatch_id() -> str:
    return f"{os.getpid()}-{uuid.uuid4().hex[:12]}"


def mark_process_as_dispatcher() -> str:
    """Give this makewand process a fresh dispatch id inherited by its children."""
    dispatch_id = new_dispatch_id()
    os.environ[DISPATCH_ENV_VAR] = dispatch_id
    return dispatch_id


def _read_proc_environ(pid: int) -> Optional[Dict[str, str]]:
    try:
        raw = Path(f"/proc/{pid}/environ").read_bytes()
    except OSError:
        return None
    env: Dict[str, str] = {}
    for item in raw.split(b"\0"):
        if b"=" in item:
            k, _, v = item.partition(b"=")
            env[k.decode("utf-8", "replace")] = v.decode("utf-8", "replace")
    return env


def _read_proc_stat(pid: int) -> Optional[Dict[str, Any]]:
    try:
        raw = Path(f"/proc/{pid}/stat").read_text(encoding="utf-8", errors="replace")
    except OSError:
        return None
    try:
        lpar, rpar = raw.index("("), raw.rindex(")")
        comm = raw[lpar + 1:rpar]
        fields = raw[rpar + 2:].split()
        return {
            "pid": pid,
            "comm": comm,
            "ppid": int(fields[1]),
            "pgrp": int(fields[2]),
            "tty_nr": int(fields[4]),
            "starttime": int(fields[19]),
        }
    except (ValueError, IndexError):
        return None


def _read_proc_cmdline(pid: int) -> str:
    try:
        raw = Path(f"/proc/{pid}/cmdline").read_bytes()
    except OSError:
        return ""
    return raw.replace(b"\0", b" ").decode("utf-8", "replace").strip()


def _process_age_seconds(starttime_ticks: int) -> Optional[float]:
    try:
        ticks = os.sysconf("SC_CLK_TCK")
        uptime = float(Path("/proc/uptime").read_text().split()[0])
        return max(0.0, uptime - starttime_ticks / float(ticks))
    except (OSError, ValueError, IndexError):
        return None


def _list_pids() -> Iterable[int]:
    try:
        return [int(name) for name in os.listdir("/proc") if name.isdigit()]
    except OSError:
        return []


def get_dispatch_id(pid: int) -> Optional[str]:
    env = _read_proc_environ(pid)
    return env.get(DISPATCH_ENV_VAR) if env else None


def _own_ancestors() -> set:
    seen = set()
    pid = os.getpid()
    while pid > 1 and pid not in seen:
        seen.add(pid)
        stat = _read_proc_stat(pid)
        if not stat:
            break
        pid = stat["ppid"]
    return seen


def find_makewand_dispatched_processes(min_age_seconds: int = HUNG_MIN_AGE_SECONDS) -> List[Dict[str, Any]]:
    """
    Lists processes that makewand itself dispatched and that have outlived
    `min_age_seconds`: they carry MAKEWAND_DISPATCH_ID, have no controlling
    terminal, are not shells/editors/remote sessions, and are the top of their
    dispatched tree. Nothing is signalled here.
    """
    stats: Dict[int, Dict[str, Any]] = {}
    marks: Dict[int, str] = {}
    for pid in _list_pids():
        stat = _read_proc_stat(pid)
        if not stat:
            continue
        stats[pid] = stat
        mark = get_dispatch_id(pid)
        if mark:
            marks[pid] = mark

    protected = _own_ancestors()
    own_mark = os.environ.get(DISPATCH_ENV_VAR)
    candidates = []
    for pid, mark in sorted(marks.items()):
        stat = stats[pid]
        if pid in protected or mark == own_mark:
            continue
        if marks.get(stat["ppid"]) == mark:
            continue  # not the top of this dispatched tree
        if stat["tty_nr"] != 0:
            continue  # attached to a terminal: interactive, never ours to kill
        comm = stat["comm"]
        if comm.lower() in PROTECTED_COMMS:
            continue
        age = _process_age_seconds(stat["starttime"])
        if age is None or age < min_age_seconds:
            continue
        dispatcher_pid = None
        head = mark.split("-", 1)[0]
        if head.isdigit():
            dispatcher_pid = int(head)
        candidates.append({
            "pid": pid,
            "pgid": stat["pgrp"],
            "comm": comm,
            "args": _read_proc_cmdline(pid)[:160],
            "etimes": int(age),
            "starttime": stat["starttime"],
            "dispatch_id": mark,
            "dispatcher_pid": dispatcher_pid,
            "dispatcher_alive": bool(dispatcher_pid and dispatcher_pid in stats),
        })
    return candidates


def terminate_dispatched_processes(candidates: List[Dict[str, Any]], confirm_pids: Iterable[int]) -> List[Dict[str, Any]]:
    """
    Sends SIGTERM only to candidates whose PID the user explicitly confirmed,
    after re-checking that the PID still belongs to the same dispatched process.
    """
    confirmed = {int(p) for p in confirm_pids}
    cleaned = []
    for cand in candidates:
        pid = cand["pid"]
        if pid not in confirmed:
            continue
        stat = _read_proc_stat(pid)
        if not stat or stat["starttime"] != cand["starttime"] or get_dispatch_id(pid) != cand["dispatch_id"]:
            continue  # exited or PID reused
        try:
            if stat["pgrp"] == pid:
                os.killpg(pid, signal.SIGTERM)
            else:
                os.kill(pid, signal.SIGTERM)
        except OSError:
            continue
        cleaned.append({"pid": pid, "comm": cand["comm"], "dispatch_id": cand["dispatch_id"]})
    return cleaned


def get_known_workspaces() -> Dict[str, str]:
    """
    Retrieves user-configured workspace directories from ~/.config/makewand/workspaces.json.
    Allows pure dynamic detection or user-local overrides without hardcoding in open-source repository.
    """
    ensure_config_dir()
    ws_file = CONFIG_DIR / "workspaces.json"
    if ws_file.exists():
        try:
            with open(ws_file, "r", encoding="utf-8") as f:
                data = json.load(f)
                if isinstance(data, dict):
                    return {str(k): str(v) for k, v in data.items()}
        except Exception:
            pass
    return {}

def get_session_cwd(session_name: str) -> str:
    """Retrieve the current working directory for a tmux session dynamically."""
    try:
        out = subprocess.check_output(
            ["tmux", "display-message", "-p", "-t", session_name, "#{pane_current_path}"],
            stderr=subprocess.DEVNULL,
            timeout=2
        ).decode("utf-8").strip()
        if out and os.path.exists(out):
            return out
    except Exception:
        pass
    return get_known_workspaces().get(session_name, "unknown")

def get_system_metrics():
    """Collect load average and memory stats."""
    load1, load5, load15 = os.getloadavg()
    mem_total_gb = 0.0
    mem_avail_gb = 0.0
    try:
        with open("/proc/meminfo", "r") as f:
            lines = f.readlines()
        m = {}
        for line in lines:
            parts = line.split(":")
            if len(parts) == 2:
                m[parts[0].strip()] = int(parts[1].strip().split()[0])
        mem_total_gb = m.get("MemTotal", 0) / (1024 * 1024)
        mem_avail_gb = m.get("MemAvailable", 0) / (1024 * 1024)
    except Exception:
        pass

    return {
        "load_1m": round(load1, 2),
        "load_5m": round(load5, 2),
        "load_15m": round(load15, 2),
        "mem_total_gb": round(mem_total_gb, 1),
        "mem_avail_gb": round(mem_avail_gb, 1),
    }

def get_active_tmux_sessions():
    """Discover active tmux sessions."""
    sessions = []
    try:
        out = subprocess.check_output(["tmux", "list-sessions", "-F", "#{session_name}"], stderr=subprocess.DEVNULL).decode("utf-8")
        for line in out.strip().splitlines():
            s = line.strip()
            if s:
                sessions.append(s)
    except Exception:
        pass
    return sessions

def get_external_ai_sessions():
    """
    Discover active AI CLI sessions (Codex, Claude, AGY) running on external terminals
    outside of tmux (e.g. direct SSH or desktop terminal pts/...).
    """
    external_sessions = []
    tmux_ttys = set()
    try:
        out = subprocess.check_output(
            ["tmux", "list-panes", "-a", "-F", "#{pane_tty}"],
            stderr=subprocess.DEVNULL,
            timeout=1
        ).decode("utf-8")
        for line in out.strip().splitlines():
            t = line.strip()
            if t:
                if t.startswith("/dev/"):
                    t = t[5:]
                tmux_ttys.add(t)
    except Exception:
        pass

    try:
        out = subprocess.check_output(
            ["ps", "-eo", "pid,ppid,tty,etime,comm,args"],
            stderr=subprocess.DEVNULL,
            timeout=2
        ).decode("utf-8", errors="replace")
        sessions_by_tty = {}
        my_pid = os.getpid()

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
            if tty in ("?", "-", "") or tty in tmux_ttys:
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

            if not ai_type:
                continue

            cwd = "unknown"
            try:
                cwd = os.readlink(f"/proc/{pid}/cwd")
            except Exception:
                pass

            if tty not in sessions_by_tty:
                sessions_by_tty[tty] = {
                    "tty": tty,
                    "pid": pid,
                    "cwd": cwd,
                    "ai_type": ai_type,
                    "etime": etime,
                    "comm": comm,
                    "args": args[:120]
                }
            else:
                if "node" not in comm and "codex-code-mode" not in comm:
                    sessions_by_tty[tty]["comm"] = comm
                    sessions_by_tty[tty]["pid"] = pid

        for tty, info in sorted(sessions_by_tty.items()):
            external_sessions.append(info)
    except Exception:
        pass

    return external_sessions

def capture_session_pane(session_name, lines_count=25):
    """Capture the visible text from the tmux session's pane."""
    try:
        out = subprocess.check_output(
            ["tmux", "capture-pane", "-p", "-t", session_name],
            stderr=subprocess.DEVNULL
        ).decode("utf-8", errors="replace")
        lines = [line.rstrip() for line in out.strip().splitlines() if line.strip()]
        return lines[-lines_count:] if lines else []
    except Exception:
        return []

def is_file_locked(lock_path):
    """Check if a file lock is actively held by a process using non-blocking flock."""
    if not os.path.exists(lock_path):
        return False
    try:
        res = subprocess.run(["flock", "-n", lock_path, "true"], capture_output=True, timeout=1)
        return res.returncode != 0
    except Exception:
        return False

def is_session_holding_file_lock(session_name: str, lock_path: str) -> bool:
    """Check if the given tmux session or any of its descendant processes holds the lock file."""
    if not os.path.exists(lock_path):
        return False
    try:
        out = subprocess.check_output(["fuser", lock_path], stderr=subprocess.DEVNULL, timeout=1).decode("utf-8")
        holder_pids = set(int(p) for p in out.strip().split() if p.isdigit())
        if not holder_pids:
            return False

        pane_pid_s = subprocess.check_output(
            ["tmux", "display-message", "-p", "-t", session_name, "#{pane_pid}"],
            stderr=subprocess.DEVNULL,
            timeout=1
        ).decode("utf-8").strip()
        if not pane_pid_s or not pane_pid_s.isdigit():
            return False

        pane_pid = int(pane_pid_s)
        if pane_pid in holder_pids:
            return True

        ps_out = subprocess.check_output(
            ["ps", "-eo", "pid,ppid"],
            stderr=subprocess.DEVNULL,
            timeout=1
        ).decode("utf-8")
        tree = {}
        for line in ps_out.strip().splitlines()[1:]:
            parts = line.strip().split()
            if len(parts) >= 2 and parts[0].isdigit() and parts[1].isdigit():
                tree[int(parts[0])] = int(parts[1])

        queue = [pane_pid]
        descendants = set()
        while queue:
            curr = queue.pop(0)
            for child, parent in tree.items():
                if parent == curr and child not in descendants:
                    descendants.add(child)
                    queue.append(child)

        return bool(holder_pids & descendants)
    except Exception:
        return False

def get_session_long_running_process(session_name: str, threshold_seconds: int = 900):
    """
    Detect if the tmux session has descendant processes executing longer than threshold_seconds.
    """
    try:
        pane_pid = subprocess.check_output(
            ["tmux", "display-message", "-p", "-t", session_name, "#{pane_pid}"],
            stderr=subprocess.DEVNULL,
            timeout=1
        ).decode("utf-8").strip()
        if not pane_pid or not pane_pid.isdigit():
            return None

        out = subprocess.check_output(
            ["ps", "-eo", "pid,ppid,etimes,comm,args"],
            stderr=subprocess.DEVNULL,
            timeout=2
        ).decode("utf-8", errors="replace")

        lines = out.strip().splitlines()
        if not lines:
            return None

        procs = {}
        for line in lines[1:]:
            parts = line.strip().split(None, 4)
            if len(parts) >= 5:
                pid_s, ppid_s, et_s, comm, args = parts
                if pid_s.isdigit() and ppid_s.isdigit() and et_s.isdigit():
                    procs[int(pid_s)] = {
                        "pid": int(pid_s),
                        "ppid": int(ppid_s),
                        "etimes": int(et_s),
                        "comm": comm,
                        "args": args
                    }

        target_root = int(pane_pid)
        descendants = set()
        queue = [target_root]
        while queue:
            curr = queue.pop(0)
            for p, info in procs.items():
                if info["ppid"] == curr and p not in descendants:
                    descendants.add(p)
                    queue.append(p)

        for p in descendants:
            info = procs.get(p)
            if not info:
                continue
            comm_lower = info["comm"].lower()
            args_lower = info["args"].lower()
            if comm_lower in ("zsh", "sh", "dash", "tmux", "agy", "node", "codex", "claude", "grok", "muse", "npm", "playwright"):
                continue
            if comm_lower == "bash":
                if not any(k in args_lower for k in ("while", "until", "for", ".sh", "eval", "python", "curl", "grep", "sleep")):
                    continue
            if "bin/codex" in args_lower or "antigravity" in args_lower or "node_modules" in args_lower or "mcp" in args_lower or "lsp" in args_lower:
                continue
            if info["etimes"] >= threshold_seconds:
                return {
                    "pid": info["pid"],
                    "comm": info["comm"],
                    "etimes": info["etimes"],
                    "args": info["args"][:160],
                    "makewand_dispatched": bool(get_dispatch_id(info["pid"])),
                }
    except Exception:
        pass
    return None

def classify_operation(session_name, lines, metrics, long_proc=None):
    """
    Classify the operational pattern of an AI session:
    - hung_anomaly (deadlock or stuck > 10m)
    - heavy_db_query (Postgres / SQL aggregation)
    - test_ci (pytest, npm test, unittest)
    - data_pipeline (dataset snapshot, model training, feature extraction)
    - code_refactor (edit, write, read files)
    - sys_monitor (system status, disk, controllers)
    - idle_ready (waiting for user prompt)
    """
    text = " \n ".join(lines)

    # Check prompt and active running indicators
    is_at_prompt = (
        ("? for shortcuts" in text and ("Gemini" in text or "esc to cancel" not in text)) or
        ("Keyboard: ↑/↓ Navigate" in text and "Switch Tab" in text) or
        (("> Ask Codex to do anything" in text or "» Ask Codex to do anything" in text or "› Ask Codex to do anything" in text)) or
        ("Gemini" in text and ("\n>" in text or "\n >" in text or "artifacts" in text or "/artifact" in text))
    )
    is_actively_running = (
        "Waiting for background terminal" in text or
        "Working (" in text or
        ("running" in text and ("task(s)" in text or "● [" in text or "manage.py" in text))
    )

    # Check for quota exhaustion indicators
    is_quota_exhausted = (
        ("Weekly limit:" in text and "0% left" in text) or
        "You've reached your limit" in text or
        "Credit balance is too low" in text or
        "Usage limit reached" in text or
        "rate_limit_exceeded" in text or
        "quota exceeded" in text
    )
    if is_quota_exhausted and is_at_prompt:
        reset_hint = ""
        match = re.search(r"resets\s+([0-9:]+\s+on\s+[0-9a-zA-Z]+|[0-9a-zA-Z\s:]+)", text)
        if match:
            reset_hint = f" (重置时间: {match.group(1).strip()})"
        return "quota_exhausted", f"底层模型配额已 100% 耗尽 (0% left){reset_hint}，建议由 AGY/Grok 接管"

    # 1. If at prompt and not actively running background jobs, session is idle_ready
    if is_at_prompt and not is_actively_running:
        return "idle_ready", "任务已闭环，处于待命提示符状态"

    # 2. Check long running process next
    if long_proc:
        elapsed_min = long_proc["etimes"] // 60
        args_str = long_proc["args"]
        if "psycopg2" in args_str or "postgres" in args_str or "SELECT " in args_str:
            return "heavy_db_query", f"执行大型 SQL 统计已持续 {elapsed_min} 分钟 (PID {long_proc['pid']})，引发磁盘 AIO 争抢"
        elif any(k in args_str for k in ("gh run watch", "gh pr watch", "gh run view", "gh workflow", "gh pr checks", "tea pr")) or ("gh " in args_str and "--watch" in args_str):
            return "test_ci", f"远程 CI/GitHub Actions 构建监控中已持续 {elapsed_min} 分钟 (PID {long_proc['pid']})"
        elif "pytest" in args_str or "manage.py test" in args_str or re.search(r"\b(test|tests|unittest|jest|vitest)\b", args_str):
            return "test_ci", f"运行测试套件已持续 {elapsed_min} 分钟 (PID {long_proc['pid']})"
        elif any(k in args_str for k in ("build_snapshot", "train", "dataset", "stage_and_phash", "preprocess", "download", "pipeline")) or \
             ("pipeline" in session_name and ("python" in long_proc["comm"] or "build" in args_str)):
            return "data_pipeline", f"大规模数据流水线/模型训练进行中已持续 {elapsed_min} 分钟 (PID {long_proc['pid']})"
        elif any(k in args_str for k in ("serve", "server", "uvicorn", "gunicorn", "dashboard", "http.server", "flask", "fastapi", "streamlit", "gradio", "webpack", "vite")) or \
             any(k in long_proc["comm"].lower() for k in ("uvicorn", "gunicorn", "caddy", "nginx")):
            return "server_daemon", f"后台服务/仪表盘运行中已持续 {elapsed_min} 分钟 (PID {long_proc['pid']})"
        elif elapsed_min >= 30 and long_proc.get("makewand_dispatched"):
            return "hung_anomaly", f"makewand 派发的子进程 (PID {long_proc['pid']}, {long_proc['comm']}) 持续运行达 {elapsed_min} 分钟，疑似挂起"
        elif elapsed_min >= 30:
            return "long_running", f"长时运行进程 (PID {long_proc['pid']}, {long_proc['comm']}, {elapsed_min} 分钟)；非 makewand 派发，仅观察不处理"

    # 3. Check for hung anomaly (specifically CI script or exclusive lock held)
    if "running" in text and any(k in text for k in ("run_in_ephemer", "exclusive_lock", "heavy_lock")):
        return "hung_anomaly", "持续占用排他单槽锁或死锁挂起，需超时看门狗"
    if "1 task(s)" in text and "task-" in text and "running" in text:
        try:
            for lpath in Path("/run/lock").glob("*-heavy-*.lock"):
                if is_session_holding_file_lock(session_name, str(lpath)):
                    return "hung_anomaly", f"持有重型数据库锁 ({lpath.name}) 挂起中"
        except Exception:
            pass

    # Makewand orchestration / scheduling
    if session_name == "makewand" and ("observe" in text or "schedule" in text or "orchestrat" in text):
        return "sys_monitor", "跨会话定时巡检调度与中枢监控"

    # 4. Heavy DB query storm
    if "task(s)" in text and re.search(r"(\d+)\s+task\(s\)", text):
        match = re.search(r"(\d+)\s+task\(s\)", text)
        if match and int(match.group(1)) >= 4:
            return "heavy_db_query", f"并发高负荷任务风暴 ({match.group(1)} 个并行任务)，导致系统负载飙升"

    if "psycopg2" in text or "SELECT " in text or "FROM measurements" in text:
        return "heavy_db_query", "执行大型关系库/大表数据聚合与统计"

    # 5. Test & CI
    if "pytest" in text or "npm test" in text or "npm run test" in text or "python manage.py test" in text or "test_feedback" in text:
        return "test_ci", "自动化单元与端到端测试验证中"

    # 6. Data pipeline
    if "build_snapshot" in text or "train_hierarchical" in text or "run_big_v" in text:
        return "data_pipeline", "大规模数据快照构建与训练批处理中"

    # 7. Code Refactor & Modification
    if "Edit(" in text or "Write(" in text or "git commit" in text or "git merge" in text:
        return "code_refactor", "多文件代码改写、逻辑重构与 Git 状态收敛"

    # 8. Idle / Ready fallback
    if is_at_prompt and not is_actively_running:
        return "idle_ready", "任务已闭环，处于待命提示符状态"

    # 9. System Monitor & Diagnostics
    if "磁盘" in text or "容量" in text or "调度控制器" in text or "轮巡" in text or "算力" in text:
        return "sys_monitor", "系统服务、存储水位与运行状态监控"

    return "general_task", "常规任务探索与执行中"

def analyze_makewand_optimizations(session_reports, metrics, external_sessions=None):
    """Derive global optimization recommendations for Makewand based on observed session patterns."""
    optimizations = []

    # Check for long-running heavy processes or stuck DB queries
    long_query_sessions = [
        s for s in session_reports
        if s.get("long_proc") and (
            "psycopg2" in s["long_proc"].get("args", "") or
            "SELECT " in s["long_proc"].get("args", "") or
            s["category"] == "heavy_db_query"
        )
    ]
    for s in long_query_sessions:
        lp = s["long_proc"]
        elapsed_m = lp["etimes"] // 60
        if elapsed_m >= 15:
            optimizations.append({
                "target": "长时间高负荷数据库查询优化 (Long-Running DB Watchdog)",
                "priority": "HIGH" if elapsed_m < 60 else "CRITICAL",
                "reason": f"监测到会话 [{s['name']}] 中的子进程 (PID {lp['pid']}, {lp['comm']}) 已持续运行 {elapsed_m} 分钟，引发系统磁盘 AIO 争抢。",
                "proposal": f"建议对会话 [{s['name']}] 涉及的慢 SQL (如 measurements 表模糊匹配) 建立专用索引或限制扫描区间，必要时执行取消以释放宿主机 I/O。"
            })

    # Check if load is high (> 15)
    if metrics["load_1m"] > 15:
        optimizations.append({
            "target": "并发任务调度限流 (Concurrency Throttling)",
            "priority": "HIGH",
            "reason": f"当前主机 1 分钟平均负载达到 {metrics['load_1m']}，主要由大型 SQL 聚合与并发测试引起。",
            "proposal": "在 Makewand 流水线与沙箱运行器中，增加基于 load average 的动态背压保护：当系统负载超过 12 时，自动将并发任务数限制为 1~2。"
        })

    # Check for data pipeline sessions
    data_pipeline_sessions = [s for s in session_reports if s["category"] == "data_pipeline"]
    if data_pipeline_sessions and metrics["load_1m"] > 10:
        names = ", ".join(s["name"] for s in data_pipeline_sessions)
        optimizations.append({
            "target": "计算密集型流水线 I/O 与 CPU 亲和调度 (Compute Pipeline Affinity)",
            "priority": "MEDIUM",
            "reason": f"会话 [{names}] 正在执行多小时级的大规模数据快照构建或模型训练。",
            "proposal": "调度批处理数据作业时自动附加 ionice -c2 -n7 与 nice -n 10，避免长时间特征计算抢占交互式会话与测试 Runner 的响应能力。"
        })

    # Check for hung anomaly
    hung_sessions = [s for s in session_reports if s["category"] == "hung_anomaly"]
    if hung_sessions:
        names = ", ".join(s["name"] for s in hung_sessions)
        optimizations.append({
            "target": "执行超时熔断机制 (Process Timeout Watchdog)",
            "priority": "CRITICAL",
            "reason": f"监测到会话 [{names}] 存在长时间挂死或排他锁死锁现象，单核消耗 100%。",
            "proposal": "在 Makewand 的 sandbox 与 provider 进程调度器中强化强制超时机制（Default Timeout: 300s），超时触发 SIGKILL 并自动释放宿主锁。"
        })

    # Check for code refactor / test_ci
    active_coders = [s for s in session_reports if s["category"] in ("code_refactor", "test_ci")]
    if active_coders:
        names = ", ".join(s["name"] for s in active_coders)
        optimizations.append({
            "target": "多模型红队盲审与并发竞速赋能 (Makewand Race / Review)",
            "priority": "MEDIUM",
            "reason": f"会话 [{names}] 正在进行高频代码修改与用例调试。",
            "proposal": "可针对重构瓶颈直接调用 `makewand review` 或 `makewand race` 派发独立工作树比拼，利用 Codex (gpt-6-astra) 的算法能力加速单测攻坚与边界排查。"
        })

    # Check for quota exhausted sessions
    quota_exhausted_sessions = [s for s in session_reports if s["category"] == "quota_exhausted"]
    if quota_exhausted_sessions:
        names = ", ".join(s["name"] for s in quota_exhausted_sessions)
        optimizations.append({
            "target": "会话额度枯竭无缝接管 (Quota Exhaustion Handover)",
            "priority": "HIGH",
            "reason": f"监测到会话 [{names}] 的底层模型配额已彻底耗尽 (0% left)，直接输入将被限流阻断。",
            "proposal": f"建议在该工作区改用 `makewand run ... --tier deep`，无缝分流至 Google AI Pro (Gemini 3.8) 或 Grok 4.7 顶格算力接续开发。"
        })

    # Check for active external sessions
    if external_sessions:
        ext_cwds = {e["cwd"] for e in external_sessions if e.get("cwd") and e["cwd"] != "unknown"}
        optimizations.append({
            "target": "多 Session 目录隔离防踩踏守卫 (Session Isolation Guard)",
            "priority": "LOW",
            "reason": f"检测到宿主机存在 {len(external_sessions)} 个外部独立终端正在操作工作目录 ({', '.join(sorted(ext_cwds))})。",
            "proposal": "Makewand 派发流水线与沙箱竞速时已启用工作树防冲突探测，避免跨终端踩踏。"
        })

    # Standard healthy check
    if not optimizations:
        optimizations.append({
            "target": "系统平稳运行 (Status Quo Optimal)",
            "priority": "LOW",
            "reason": "各个会话负荷正常，无死锁，无任务风暴。",
            "proposal": "维持当前架构，保持意图分流与沙箱隔离正常运作。"
        })

    return optimizations

def observe_all_dialogs(save_report=True, clean_hung=False, confirm_pids: Optional[Iterable[int]] = None):
    """
    Run a complete observation turn across all dialogs.
    Returns structured observation dict.

    clean_hung only *lists* makewand-dispatched processes older than 30 minutes
    (report["hung_candidates"]). A signal is sent only to PIDs the caller also
    passes in confirm_pids (the CLI's explicit --confirm-pids argument).
    """
    metrics = get_system_metrics()
    active_sessions = get_active_tmux_sessions()
    external_sessions = get_external_ai_sessions()

    session_reports = []
    cleaned_pids = []
    hung_candidates: List[Dict[str, Any]] = []
    for s in active_sessions:
        cwd = get_session_cwd(s)
        lines = capture_session_pane(s, lines_count=20)
        long_proc = get_session_long_running_process(s, threshold_seconds=900)
        cat, note = classify_operation(s, lines, metrics, long_proc=long_proc)

        session_reports.append({
            "name": s,
            "cwd": cwd,
            "category": cat,
            "status_note": note,
            "long_proc": long_proc,
            "sample_lines": lines[-5:] if lines else []
        })

    if clean_hung:
        hung_candidates = find_makewand_dispatched_processes(min_age_seconds=HUNG_MIN_AGE_SECONDS)
        if confirm_pids:
            cleaned_pids = terminate_dispatched_processes(hung_candidates, confirm_pids)

    optimizations = analyze_makewand_optimizations(session_reports, metrics, external_sessions=external_sessions)

    report = {
        "timestamp": datetime.now().isoformat(),
        "human_time": datetime.now().strftime("%Y-%m-%d %H:%M:%S"),
        "metrics": metrics,
        "session_count": len(session_reports),
        "sessions": session_reports,
        "external_sessions": external_sessions,
        "external_count": len(external_sessions),
        "makewand_optimizations": optimizations,
        "hung_candidates": hung_candidates,
        "cleaned_pids": cleaned_pids
    }

    if save_report:
        from makewand import config as _config
        save_dir = Path(_config.CONFIG_DIR)
        try:
            save_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
            report_path = save_dir / "dialog_observations.json"
            # The report contains captured terminal text: owner-only.
            fd = os.open(report_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                json.dump(report, f, ensure_ascii=False, indent=2)
            os.chmod(report_path, 0o600)
        except Exception:
            pass

    return report

def format_observation_markdown(report):
    """Format the observation report as clean GitHub Markdown."""
    m = report["metrics"]
    lines = []
    lines.append(f"### 🕒 Makewand 跨会话运行态势与分型巡检汇报 ({report['human_time']})")
    lines.append("")
    lines.append(f"**系统资源负荷**：1m 负载: `{m['load_1m']}` | 5m 负载: `{m['load_5m']}` | 15m 负载: `{m['load_15m']}` | 内存可用: `{m['mem_avail_gb']} GB / {m['mem_total_gb']} GB`")
    lines.append("")
    lines.append("| 会话名称 | 工作区路径 | 操作分型 (Category) | 运行态势与细节 | 状态 |")
    lines.append("|---|---|---|---|:---:|")

    status_icons = {
        "hung_anomaly": "🔴 疑似挂起",
        "long_running": "🟡 长时运行",
        "heavy_db_query": "🟡 高负荷",
        "quota_exhausted": "⚠️ 额度已见底",
        "test_ci": "🔵 测试中",
        "data_pipeline": "🟣 数据流水线",
        "server_daemon": "🟢 服务运行中",
        "code_refactor": "🟣 代码重构",
        "sys_monitor": "🟢 监控待命",
        "idle_ready": "🟢 就绪空闲",
        "general_task": "⚪ 执行中"
    }

    for s in report["sessions"]:
        icon = status_icons.get(s["category"], "⚪")
        lines.append(f"| `{s['name']}` | `{s['cwd']}` | **{s['category']}** | {s['status_note']} | {icon} |")

    if report.get("external_sessions"):
        lines.append("")
        lines.append("### 🖥️ 外部独立终端活跃 AI 交互会话 (External Sessions)")
        lines.append("")
        lines.append("| 终端 TTY | AI 引擎 | 进程 PID | 当前工作目录 (Active Workspace) | 运行耗时 | 状态 |")
        lines.append("|---|---|---|---|---|:---:|")
        for ext in report["external_sessions"]:
            lines.append(f"| `{ext['tty']}` | **{ext['ai_type']}** | `{ext['pid']}` | `{ext['cwd']}` | {ext['etime']} | 🟢 活跃交互 |")

    if report.get("hung_candidates"):
        lines.append("")
        lines.append("### ⏱️ makewand 派发且运行超过 30 分钟的进程 (清理候选，尚未处理)")
        lines.append("")
        lines.append("| PID | 进程 | 运行时长 | 派发者 PID | 派发者存活 | 命令 |")
        lines.append("|---|---|---|---|:---:|---|")
        for cand in report["hung_candidates"]:
            alive = "是" if cand.get("dispatcher_alive") else "否"
            lines.append(f"| `{cand['pid']}` | {cand['comm']} | {cand['etimes'] // 60} 分钟 | `{cand.get('dispatcher_pid')}` | {alive} | `{cand.get('args', '')}` |")

    lines.append("")
    lines.append("#### 🛠️ Makewand 针对性优化研判与建议：")
    for opt in report["makewand_optimizations"]:
        p_color = "**[CRITICAL]**" if opt["priority"] == "CRITICAL" else ("**[HIGH]**" if opt["priority"] == "HIGH" else f"[{opt['priority']}]")
        lines.append(f"- {p_color} **{opt['target']}**")
        lines.append(f"  - **研判依据**：{opt['reason']}")
        lines.append(f"  - **改进建议**：{opt['proposal']}")

    return "\n".join(lines)

if __name__ == "__main__":
    rep = observe_all_dialogs()
    print(format_observation_markdown(rep))
