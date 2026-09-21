"""
Unit tests for makewand.observer module.
"""

import unittest
from makewand.observer import classify_operation, analyze_makewand_optimizations

class TestObserver(unittest.TestCase):
    def test_classify_operation(self):
        # 1. Hung anomaly
        cat, note = classify_operation("sample_project_1", ["SECRET_KEY=123 bash scripts/ci/run_in_ephemer...", "running"], {"load_1m": 20})
        self.assertEqual(cat, "hung_anomaly")

        # 2. Heavy DB query storm
        cat, note = classify_operation("sample_project_7", ["● 12 task(s) running", "psycopg2 query"], {"load_1m": 25})
        self.assertEqual(cat, "heavy_db_query")

        # 3. Test & CI
        cat, note = classify_operation("sample_project_4", ["npm test", "passing 12 tests"], {"load_1m": 1.5})
        self.assertEqual(cat, "test_ci")

        # 4. Code refactor
        cat, note = classify_operation("sample_project_3", ["Edit(/path/to/file)", "git commit -m fix"], {"load_1m": 2.0})
        self.assertEqual(cat, "code_refactor")

        # 5. Idle ready
        cat, note = classify_operation("stock", ["? for shortcuts", "Gemini 3.8 Flash · high"], {"load_1m": 1.0})
        self.assertEqual(cat, "idle_ready")

        # 6. Long running process detection
        long_proc = {"pid": 594545, "comm": "python3", "etimes": 3600, "args": "psycopg2 query measurements"}
        cat, note = classify_operation("sample_project_7", ["running"], {"load_1m": 25}, long_proc=long_proc)
        self.assertEqual(cat, "heavy_db_query")
        self.assertIn("60 分钟", note)

    def test_analyze_makewand_optimizations(self):
        reports = [
            {"name": "sample_project_1", "category": "hung_anomaly", "status_note": "deadlock"},
            {
                "name": "sample_project_7",
                "category": "heavy_db_query",
                "status_note": "heavy load",
                "long_proc": {"pid": 594545, "comm": "python3", "etimes": 4800, "args": "psycopg2 query"}
            }
        ]
        metrics = {"load_1m": 22.0}
        opts = analyze_makewand_optimizations(reports, metrics)
        self.assertTrue(any(o["priority"] == "CRITICAL" for o in opts))
        self.assertTrue(any("背压" in o["proposal"] or "限流" in o["target"] for o in opts))
        self.assertTrue(any("数据库查询" in o["target"] for o in opts))

if __name__ == "__main__":
    unittest.main()
