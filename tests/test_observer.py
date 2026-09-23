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

    def test_get_external_ai_sessions(self):
        from unittest.mock import patch
        from makewand.observer import get_external_ai_sessions

        fake_tmux_out = b"pts/1\npts/2\n"
        fake_ps_out = (
            b"  PID  PPID TT       ETIME COMMAND ARGS\n"
            b" 1001   500 pts/1    00:10 agy     agy\n"
            b" 2002   600 pts/36   00:20 codex   /usr/local/bin/codex\n"
            b" 3003   700 pts/38   00:05 claude  /usr/bin/claude\n"
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
            self.assertEqual(len(ext), 2)
            ttys = {e["tty"] for e in ext}
            self.assertIn("pts/36", ttys)
            self.assertIn("pts/38", ttys)
            self.assertNotIn("pts/1", ttys)

if __name__ == "__main__":
    unittest.main()
