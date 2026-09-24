"""
Unit tests for makewand.observer module.
"""

import unittest
from makewand.observer import classify_operation, analyze_makewand_optimizations

class TestObserver(unittest.TestCase):
    def test_classify_operation(self):
        # 1. Hung anomaly
        cat, note = classify_operation("session_ci", ["SECRET_KEY=123 bash scripts/ci/run_in_ephemer...", "running"], {"load_1m": 20})
        self.assertEqual(cat, "hung_anomaly")

        # 2. Heavy DB query storm
        cat, note = classify_operation("session_db", ["● 12 task(s) running", "psycopg2 query"], {"load_1m": 25})
        self.assertEqual(cat, "heavy_db_query")

        # 3. Test & CI
        cat, note = classify_operation("session_web", ["npm test", "passing 12 tests"], {"load_1m": 1.5})
        self.assertEqual(cat, "test_ci")

        # 4. Code refactor
        cat, note = classify_operation("session_dev", ["Edit(/path/to/file)", "git commit -m fix"], {"load_1m": 2.0})
        self.assertEqual(cat, "code_refactor")

        # 5. Idle ready
        cat, note = classify_operation("session_chat", ["? for shortcuts", "Gemini 3.8 Flash · high"], {"load_1m": 1.0})
        self.assertEqual(cat, "idle_ready")

        # 6. Long running process detection
        long_proc = {"pid": 594545, "comm": "python3", "etimes": 3600, "args": "psycopg2 query measurements"}
        cat, note = classify_operation("session_db", ["running"], {"load_1m": 25}, long_proc=long_proc)
        self.assertEqual(cat, "heavy_db_query")
        self.assertIn("60 分钟", note)

        # 7. Data pipeline classification
        pipeline_proc = {"pid": 1791793, "comm": "python3", "etimes": 14760, "args": "python3 build_snapshot.py --lane all"}
        cat, note = classify_operation("session_ml_pipeline", ["● [15:53:48] STOP_AFTER=2 ./run_big_v0021.sh running"], {"load_1m": 16.0}, long_proc=pipeline_proc)
        self.assertEqual(cat, "data_pipeline")
        self.assertIn("数据流水线", note)

        # 8. Idle ready overrides background daemon
        cat, note = classify_operation("session_codex", ["» Ask Codex to do anything", "Worked for 2h 2m 44s · done 1:35 PM"], {"load_1m": 16.0})
        self.assertEqual(cat, "idle_ready")

        # 9. Quota exhausted detection at prompt
        cat, note = classify_operation("session_exhausted", [
            "Weekly limit: [░░░░░░░░░░░░░░░░░░░░] 0% left (resets 07:49 on 28 Sep)",
            "› Ask Codex to do anything"
        ], {"load_1m": 1.0})
        self.assertEqual(cat, "quota_exhausted")
        self.assertIn("0% left", note)

        # 10. Server / dashboard daemon classification (not hung anomaly)
        server_proc = {"pid": 3328609, "comm": "python3", "etimes": 2100, "args": "python3 scripts/serve_retail_interactive_dashboard.py --port 8765"}
        cat, note = classify_operation("session_dashboard", ["Serving HTTP on 0.0.0.0 port 8765 ..."], {"load_1m": 2.0}, long_proc=server_proc)
        self.assertEqual(cat, "server_daemon")
        self.assertIn("后台服务/仪表盘", note)

    def test_analyze_makewand_optimizations(self):
        reports = [
            {"name": "backend_service", "category": "hung_anomaly", "status_note": "deadlock"},
            {
                "name": "data_analytics",
                "category": "heavy_db_query",
                "status_note": "heavy load",
                "long_proc": {"pid": 594545, "comm": "python3", "etimes": 4800, "args": "psycopg2 query"}
            },
            {
                "name": "model_pipeline",
                "category": "data_pipeline",
                "status_note": "pipeline running"
            },
            {
                "name": "whereifish",
                "category": "quota_exhausted",
                "status_note": "0% left"
            }
        ]
        metrics = {"load_1m": 22.0}
        ext_sessions = [
            {"tty": "pts/36", "ai_type": "codex", "pid": 3867843, "cwd": "/mock/projects/service_app", "etime": "10:00"}
        ]
        opts = analyze_makewand_optimizations(reports, metrics, external_sessions=ext_sessions)
        self.assertTrue(any(o["priority"] == "CRITICAL" for o in opts))
        self.assertTrue(any("背压" in o["proposal"] or "限流" in o["target"] for o in opts))
        self.assertTrue(any("数据库查询" in o["target"] for o in opts))
        self.assertTrue(any("亲和调度" in o["target"] for o in opts))
        self.assertTrue(any("防踩踏守卫" in o["target"] for o in opts))
        self.assertTrue(any("额度枯竭" in o["target"] for o in opts))

    def test_get_external_ai_sessions(self):
        from unittest.mock import patch
        from makewand.observer import get_external_ai_sessions

        fake_tmux_out = b"pts/1\npts/2\n"
        fake_ps_out = (
            b"  PID  PPID TT       ETIME COMMAND ARGS\n"
            b" 1001   500 pts/1    00:10 agy     agy\n"
            b" 2002   600 pts/36   00:20 codex   /usr/local/bin/codex\n"
            b" 3003   700 pts/38   00:05 claude  /usr/bin/claude\n"
            b" 4004   800 pts/40   00:15 grok    /home/user/.grok/bin/grok\n"
            b" 5005   900 pts/42   00:25 muse    /home/user/.local/bin/muse\n"
        )

        with patch("subprocess.check_output") as mock_run, \
             patch("os.readlink", return_value="/tmp/test_ws"):
            def check_output_side_effect(cmd, **kwargs):
                if cmd[0] == "tmux":
                    return fake_tmux_out
                elif cmd[0] == "ps":
                    return fake_ps_out
                return b""

            mock_run.side_effect = check_output_side_effect
            ext = get_external_ai_sessions()
            self.assertEqual(len(ext), 4)
            ttys = {e["tty"] for e in ext}
            ai_types = {e["ai_type"] for e in ext}
            self.assertIn("pts/36", ttys)
            self.assertIn("pts/38", ttys)
            self.assertIn("pts/40", ttys)
            self.assertIn("pts/42", ttys)
            self.assertIn("grok", ai_types)
            self.assertIn("muse", ai_types)
            self.assertNotIn("pts/1", ttys)

    def test_is_session_holding_file_lock(self):
        from unittest.mock import patch
        from makewand.observer import is_session_holding_file_lock

        with patch("os.path.exists", return_value=True), \
             patch("subprocess.check_output") as mock_sub:
            def sub_side_effect(cmd, **kwargs):
                if cmd[0] == "fuser":
                    return b"5001 5002\n"
                elif cmd[0] == "tmux":
                    return b"1000\n"
                elif cmd[0] == "ps":
                    return b"  PID  PPID\n 1000   100\n 2000  1000\n 5001  2000\n 9000   100\n"
                return b""

            mock_sub.side_effect = sub_side_effect
            # Session holding lock (5001 is descendant of 1000)
            self.assertTrue(is_session_holding_file_lock("active_session", "/run/lock/test.lock"))

            def sub_side_effect_unrelated(cmd, **kwargs):
                if cmd[0] == "fuser":
                    return b"8888\n"
                elif cmd[0] == "tmux":
                    return b"1000\n"
                elif cmd[0] == "ps":
                    return b"  PID  PPID\n 1000   100\n 2000  1000\n"
                return b""

            mock_sub.side_effect = sub_side_effect_unrelated
            # Unrelated session does not hold lock
            self.assertFalse(is_session_holding_file_lock("other_session", "/run/lock/test.lock"))

if __name__ == "__main__":
    unittest.main()
