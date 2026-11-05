"""
Unit tests for orchestrator logic: task tiering and defect detection.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
    from _verdicts import answer_with_nonce
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401
    from tests._verdicts import answer_with_nonce

import unittest
import tempfile
import makewand.orchestrator
from pathlib import Path
from unittest.mock import patch
from makewand.orchestrator import extract_review_verdict_dict, run_review, EXIT_PASSED, EXIT_FAILED, EXIT_UNVERIFIED
import makewand.config as makewand_config


def _own_delivery_dirs(art_base, pre_dirs, repo, test):
    """Delivery artifacts created by this test's task (matched by repo_root).

    Other test processes may share a legacy fixed artifacts path, so newest-by-mtime
    is not enough; each match is removed when the test finishes.
    """
    import json
    import shutil
    own = []
    for path in set(art_base.glob("delivery_*")) - set(pre_dirs):
        try:
            manifest = json.loads((path / "delivery_manifest.json").read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        if manifest.get("repo_root") == str(repo):
            own.append(path)
            test.addCleanup(shutil.rmtree, path, True)
    return own

class TestOrchestrator(unittest.TestCase):


    def test_extract_review_verdict_dict(self):
        # Case 1: Structured JSON pass
        res_pass = (
            "Review analysis...\n"
            "MAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}"
        )
        dict_pass = extract_review_verdict_dict(res_pass)
        self.assertTrue(dict_pass["pass"])
        self.assertEqual(dict_pass["defects"], [])

        # Case 2: Structured JSON defect
        res_defect = (
            "Review analysis...\n"
            "MAKEWAND_VERDICT: {\"pass\": false, \"defects\": [\"[P1] Deadlock in mutex.Lock()\"]}"
        )
        dict_defect = extract_review_verdict_dict(res_defect)
        self.assertFalse(dict_defect["pass"])
        self.assertEqual(dict_defect["defects"], ["[P1] Deadlock in mutex.Lock()"])

    @patch("makewand.orchestrator.get_git_diff")
    @patch("makewand.orchestrator.get_or_update_status")
    @patch("makewand.orchestrator.execute_codex_task")
    def test_run_review_json_output(self, mock_codex, mock_status, mock_diff):
        import io
        import json
        from contextlib import redirect_stdout

        mock_status.return_value = {"codex": {"status": "healthy"}}
        mock_diff.return_value = "diff --git a/test.py b/test.py\n+x = 1"
        mock_codex.side_effect = answer_with_nonce((True, "LGTM\nMAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}", None))

        buf = io.StringIO()
        with redirect_stdout(buf):
            code = run_review(cwd="/tmp", output_json=True)

        self.assertEqual(code, EXIT_PASSED)
        payload = json.loads(buf.getvalue().strip())
        self.assertTrue(payload["pass"])
        self.assertEqual(payload["engine"], "codex")
        self.assertEqual(payload["exit_code"], 0)


if __name__ == "__main__":
    unittest.main()
