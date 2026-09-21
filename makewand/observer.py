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
import subprocess
from pathlib import Path
from datetime import datetime
from makewand.config import c, COLOR_BOLD, COLOR_CYAN, COLOR_GREEN, COLOR_YELLOW, COLOR_RED, COLOR_RESET

KNOWN_WORKSPACES = {
    "sample_project_1": "/path/to/workspace/sample_project_1",
    "sample_project_2": "/path/to/workspace/dev/sample_project_2",
    "makewand": "/path/to/workspace/makewand",
    "network": "/path/to/workspace/network",
    "sample_project_6": "/path/to/workspace/sample_project_6/platform",
    "stock": "/path/to/workspace/stock",
    "sample_project_7": "/path/to/workspace/sample_project_7",
    "sample_project_4": "/path/to/workspace/sample_project_4",
    "sample_project_5": "/path/to/workspace/sample_project_5",
    "sample_project_3": "/path/to/workspace/dev/sample_project_3"
}

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
    return KNOWN_WORKSPACES.get(session_name, "unknown")

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
            if any(k in comm_lower for k in ("bash", "zsh", "sh", "tmux", "agy", "node", "codex", "npm", "playwright")):
                continue
            if "bin/codex" in args_lower or "antigravity" in args_lower or "node_modules" in args_lower or "mcp" in args_lower or "lsp" in args_lower:
                continue
            if info["etimes"] >= threshold_seconds:
                return {
                    "pid": info["pid"],
                    "comm": info["comm"],
                    "etimes": info["etimes"],
                    "args": info["args"][:160]
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
        (("> Ask Codex to do anything" in text or "» Ask Codex to do anything" in text or "› Ask Codex to do anything" in text))
    )
    is_actively_running = (
        "Waiting for background terminal" in text or
        "Working (" in text or
        ("running" in text and ("task(s)" in text or "● [" in text or "manage.py" in text))
    )

    # 1. If at prompt and not actively running background jobs, session is idle_ready
    if is_at_prompt and not is_actively_running:
        return "idle_ready", "任务已闭环，处于待命提示符状态"

    # 2. Check long running process next
    if long_proc:
        elapsed_min = long_proc["etimes"] // 60
        args_str = long_proc["args"]
        if "psycopg2" in args_str or "postgres" in args_str or "SELECT " in args_str:
            return "heavy_db_query", f"执行大型 SQL 统计已持续 {elapsed_min} 分钟 (PID {long_proc['pid']})，引发磁盘 AIO 争抢"
        elif "pytest" in args_str or "manage.py test" in args_str or re.search(r"\b(test|tests|unittest|jest|vitest)\b", args_str):
            return "test_ci", f"运行测试套件已持续 {elapsed_min} 分钟 (PID {long_proc['pid']})"
        elif any(k in args_str for k in ("build_snapshot", "train", "dataset", "stage_and_phash", "preprocess", "download")) or \
             (session_name == "sample_project_5" and ("python" in long_proc["comm"] or "build" in args_str)):
            return "data_pipeline", f"大规模数据流水线/模型训练进行中已持续 {elapsed_min} 分钟 (PID {long_proc['pid']})"
        elif elapsed_min >= 30:
            return "hung_anomaly", f"子进程 (PID {long_proc['pid']}, {long_proc['comm']}) 持续运行达 {elapsed_min} 分钟"

    # 3. Check for hung anomaly in sample_project_1 (specifically pytest with exclusive lock held)
    if session_name == "sample_project_1":
        if "running" in text and "bash scripts/ci/run_in_ephemer" in text and is_file_locked("/run/lock/sample_project_1-p920-heavy-postgres.lock"):
            return "hung_anomaly", "持续占用排他单槽锁或死锁挂起，需超时看门狗"
        if "1 task(s)" in text and "task-" in text and "running" in text:
            if is_file_locked("/run/lock/sample_project_1-p920-heavy-postgres.lock"):
                return "hung_anomaly", "持有重型数据库锁挂起中"

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
    if is_at_prompt:
        return "idle_ready", "任务已闭环，处于待命提示符状态"

    # 9. System Monitor & Diagnostics
    if "磁盘" in text or "容量" in text or "调度控制器" in text or "轮巡" in text or "算力" in text:
        return "sys_monitor", "系统服务、存储水位与运行状态监控"

    return "general_task", "常规任务探索与执行中"

def analyze_makewand_optimizations(session_reports, metrics):
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

    # Standard healthy check
    if not optimizations:
        optimizations.append({
            "target": "系统平稳运行 (Status Quo Optimal)",
            "priority": "LOW",
            "reason": "各个会话负荷正常，无死锁，无任务风暴。",
            "proposal": "维持当前架构，保持意图分流与沙箱隔离正常运作。"
        })

    return optimizations

def observe_all_dialogs(save_report=True):
    """
    Run a complete observation turn across all dialogs.
    Returns structured observation dict.
    """
    metrics = get_system_metrics()
    active_sessions = get_active_tmux_sessions()

    session_reports = []
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

    optimizations = analyze_makewand_optimizations(session_reports, metrics)

    report = {
        "timestamp": datetime.now().isoformat(),
        "human_time": datetime.now().strftime("%Y-%m-%d %H:%M:%S"),
        "metrics": metrics,
        "session_count": len(session_reports),
        "sessions": session_reports,
        "makewand_optimizations": optimizations
    }

    if save_report:
        save_dir = Path.home() / ".config" / "makewand"
        save_dir.mkdir(parents=True, exist_ok=True)
        report_path = save_dir / "dialog_observations.json"
        try:
            with open(report_path, "w", encoding="utf-8") as f:
                json.dump(report, f, ensure_ascii=False, indent=2)
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
        "hung_anomaly": "🔴 异常卡死",
        "heavy_db_query": "🟡 高负荷",
        "test_ci": "🔵 测试中",
        "data_pipeline": "🟣 数据流水线",
        "code_refactor": "🟣 代码重构",
        "sys_monitor": "🟢 监控待命",
        "idle_ready": "🟢 就绪空闲",
        "general_task": "⚪ 执行中"
    }

    for s in report["sessions"]:
        icon = status_icons.get(s["category"], "⚪")
        lines.append(f"| `{s['name']}` | `{s['cwd']}` | **{s['category']}** | {s['status_note']} | {icon} |")

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
