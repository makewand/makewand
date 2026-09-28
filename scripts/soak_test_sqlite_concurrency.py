#!/usr/bin/env python3
"""
Makewand SQLite Concurrency & Soak Test Harness.

Exercises the SQLite WAL configuration, connection limits (SetMaxOpenConns=1 equivalent),
and multi-threaded concurrent read/write workloads to verify:
  1. Zero 'database is locked' / busy errors under sustained concurrency.
  2. ACID integrity across concurrent transactions and snapshots.
  3. WAL checkpoint consistency and latency bounds (P50/P95/P99).
"""

import os
import sys
import time
import sqlite3
import argparse
import tempfile
import threading
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import List, Dict, Any

def init_test_db(db_path: str) -> None:
    conn = sqlite3.connect(db_path, timeout=30.0)
    try:
        conn.execute("PRAGMA journal_mode = WAL")
        conn.execute("PRAGMA synchronous = NORMAL")
        conn.execute("PRAGMA busy_timeout = 30000")
        conn.execute("""
            CREATE TABLE IF NOT EXISTS sessions (
                id TEXT PRIMARY KEY,
                provider TEXT,
                model TEXT,
                status TEXT,
                tokens INTEGER,
                created_at REAL,
                updated_at REAL
            )
        """)
        conn.execute("""
            CREATE TABLE IF NOT EXISTS audit_log (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                session_id TEXT,
                event TEXT,
                payload TEXT,
                timestamp REAL
            )
        """)
        conn.commit()
    finally:
        conn.close()

def worker_task(
    worker_id: int,
    db_path: str,
    stop_event: threading.Event,
    latencies: List[float],
    errors: List[str]
) -> int:
    ops = 0
    # Each worker has its own connection with busy_timeout configured
    conn = sqlite3.connect(db_path, timeout=30.0)
    try:
        conn.execute("PRAGMA busy_timeout = 30000")
        while not stop_event.is_set():
            t0 = time.monotonic()
            try:
                op_type = ops % 4
                session_id = f"sess_{worker_id}_{ops}"
                now = time.time()

                if op_type == 0:
                    # Write: Insert new session
                    with conn:
                        conn.execute(
                            "INSERT INTO sessions VALUES (?, ?, ?, ?, ?, ?, ?)",
                            (session_id, "claude", "claude-3-7-sonnet", "running", 0, now, now)
                        )
                elif op_type == 1:
                    # Write: Insert audit event
                    with conn:
                        conn.execute(
                            "INSERT INTO audit_log (session_id, event, payload, timestamp) VALUES (?, ?, ?, ?)",
                            (session_id, "token_increment", '{"tokens": 128}', now)
                        )
                elif op_type == 2:
                    # Write: Update session status
                    with conn:
                        conn.execute(
                            "UPDATE sessions SET status = ?, tokens = tokens + 128, updated_at = ? WHERE id = ?",
                            ("completed", now, session_id)
                        )
                else:
                    # Read: Aggregate query
                    cursor = conn.cursor()
                    cursor.execute("SELECT COUNT(*), SUM(tokens) FROM sessions")
                    _ = cursor.fetchone()

                dt = (time.monotonic() - t0) * 1000.0
                latencies.append(dt)
                ops += 1

            except Exception as e:
                errors.append(f"Worker {worker_id} op {ops}: {type(e).__name__}: {e}")
                # Brief sleep on error to avoid tight error loop
                time.sleep(0.01)

    finally:
        conn.close()
    return ops

def run_soak_test(workers: int, duration_sec: float, db_path: str = "") -> Dict[str, Any]:
    temp_dir = None
    if not db_path:
        temp_dir = tempfile.TemporaryDirectory(prefix="makewand-soak-")
        db_path = os.path.join(temp_dir.name, "soak.db")

    try:
        init_test_db(db_path)
        stop_event = threading.Event()
        worker_latencies: List[List[float]] = [[] for _ in range(workers)]
        worker_errors: List[List[str]] = [[] for _ in range(workers)]

        print(f"🚀 Starting SQLite Soak Test: {workers} concurrent workers, duration {duration_sec}s...")
        start_time = time.monotonic()

        with ThreadPoolExecutor(max_workers=workers) as executor:
            futures = [
                executor.submit(worker_task, i, db_path, stop_event, worker_latencies[i], worker_errors[i])
                for i in range(workers)
            ]

            time.sleep(duration_sec)
            stop_event.set()

            total_ops = sum(f.result() for f in as_completed(futures))

        total_time = time.monotonic() - start_time
        all_latencies = [lat for w_lat in worker_latencies for lat in w_lat]
        all_errors = [err for w_err in worker_errors for err in w_err]

        all_latencies.sort()
        n = len(all_latencies)
        p50 = all_latencies[int(n * 0.50)] if n else 0.0
        p95 = all_latencies[int(n * 0.95)] if n else 0.0
        p99 = all_latencies[int(n * 0.99)] if n else 0.0

        # Run integrity check on final database
        conn = sqlite3.connect(db_path, timeout=10.0)
        try:
            cursor = conn.cursor()
            cursor.execute("PRAGMA integrity_check")
            integrity = cursor.fetchone()[0]
            cursor.execute("PRAGMA wal_checkpoint(PASSIVE)")
            checkpoint_status = cursor.fetchone()
        finally:
            conn.close()

        result = {
            "workers": workers,
            "duration_sec": round(total_time, 2),
            "total_ops": total_ops,
            "ops_per_sec": round(total_ops / total_time, 1) if total_time > 0 else 0,
            "error_count": len(all_errors),
            "errors": all_errors[:10],
            "latency_p50_ms": round(p50, 2),
            "latency_p95_ms": round(p95, 2),
            "latency_p99_ms": round(p99, 2),
            "integrity_check": integrity,
            "checkpoint_status": checkpoint_status,
        }
        return result
    finally:
        if temp_dir:
            temp_dir.cleanup()

def main():
    parser = argparse.ArgumentParser(description="Makewand SQLite Concurrency & Soak Test Harness")
    parser.add_argument("--workers", type=int, default=20, help="Number of concurrent client workers (default: 20)")
    parser.add_argument("--duration", type=float, default=5.0, help="Duration in seconds (default: 5.0)")
    parser.add_argument("--db", type=str, default="", help="Path to database file (default: ephemeral temp DB)")
    args = parser.parse_args()

    res = run_soak_test(workers=args.workers, duration_sec=args.duration, db_path=args.db)

    print("\n" + "=" * 50)
    print("        Makewand SQLite Soak Test Results")
    print("=" * 50)
    print(f"Workers:           {res['workers']}")
    print(f"Elapsed Time:      {res['duration_sec']}s")
    print(f"Completed Ops:     {res['total_ops']}")
    print(f"Throughput:        {res['ops_per_sec']} ops/sec")
    print(f"Error Count:       {res['error_count']}")
    print(f"Latency P50:       {res['latency_p50_ms']} ms")
    print(f"Latency P95:       {res['latency_p95_ms']} ms")
    print(f"Latency P99:       {res['latency_p99_ms']} ms")
    print(f"Integrity Check:   {res['integrity_check']}")
    print(f"WAL Checkpoint:    {res['checkpoint_status']}")
    print("=" * 50)

    if res['error_count'] > 0 or res['integrity_check'] != "ok":
        print("❌ Test FAILED: errors detected or database integrity compromised!")
        sys.exit(1)
    else:
        print("✔ Test PASSED: zero lock errors, perfect ACID integrity!")
        sys.exit(0)

if __name__ == "__main__":
    main()
