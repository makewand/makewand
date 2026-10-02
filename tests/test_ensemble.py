"""Unit tests for makewand/ensemble.py and ensemble CLI commands.

Covers:
- resolve_ensemble_providers (filtering, defaults, aliases, local-only, fallbacks)
- extract_summary_bullet (verdicts, defects, keywords, fallbacks, length limits)
- format_ensemble_matrix (Markdown table formatting, statuses, summaries, links)
- run_ensemble (concurrent multi-model dispatch, output_dir file writing, matrix generation)
- CLI ensemble and clean commands
"""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import sys
import tempfile
import unittest
from unittest.mock import patch, MagicMock

from makewand.ensemble import (
    FRONTIER_ENSEMBLE_PROVIDERS,
    resolve_ensemble_providers,
    extract_summary_bullet,
    format_ensemble_matrix,
    run_ensemble,
)
from makewand.cli import cmd_clean


class TestEnsemble(unittest.TestCase):
    """Test suite for multi-model ensemble orchestrator."""

    def test_resolve_ensemble_providers_local_only(self):
        providers = resolve_ensemble_providers(local_only=True)
        self.assertEqual(providers, ["local"])

    @patch("makewand.ensemble.is_provider_enabled", return_value=True)
    @patch("makewand.ensemble.has_subscription_configured", return_value=True)
    @patch("makewand.ensemble.has_api_configured", return_value=False)
    @patch("makewand.health.load_status_cache", return_value={})
    def test_resolve_ensemble_providers_default(self, mock_cache, mock_api, mock_sub, mock_enabled):
        providers = resolve_ensemble_providers()
        self.assertEqual(providers, FRONTIER_ENSEMBLE_PROVIDERS)

    @patch("makewand.ensemble.is_provider_enabled", return_value=True)
    @patch("makewand.ensemble.has_subscription_configured", return_value=True)
    @patch("makewand.ensemble.has_api_configured", return_value=False)
    @patch("makewand.health.load_status_cache", return_value={})
    def test_resolve_ensemble_providers_custom_and_aliases(self, mock_cache, mock_api, mock_sub, mock_enabled):
        # Comma-separated string
        p1 = resolve_ensemble_providers("claude, codex")
        self.assertEqual(p1, ["claude", "codex"])

        # List of strings
        p2 = resolve_ensemble_providers(["muse", "grok"])
        self.assertEqual(p2, ["muse", "grok"])

        # 'all' alias
        p3 = resolve_ensemble_providers("all")
        self.assertEqual(p3, FRONTIER_ENSEMBLE_PROVIDERS)

    @patch("makewand.ensemble.is_provider_enabled")
    @patch("makewand.ensemble.has_subscription_configured")
    @patch("makewand.ensemble.has_api_configured", return_value=False)
    @patch("makewand.health.load_status_cache")
    def test_resolve_ensemble_providers_skips_disabled_and_limited(
        self, mock_cache, mock_api, mock_sub, mock_enabled
    ):
        mock_enabled.side_effect = lambda p: p != "claude"  # Claude disabled
        mock_sub.side_effect = lambda p: True
        mock_cache.return_value = {"codex": {"status": "limited"}}  # Codex limited

        providers = resolve_ensemble_providers(["claude", "codex", "agy"])
        self.assertEqual(providers, ["agy"])

    @patch("makewand.ensemble.is_provider_enabled", return_value=False)
    @patch("makewand.ensemble.has_subscription_configured", return_value=False)
    @patch("makewand.ensemble.has_api_configured", return_value=False)
    @patch("makewand.health.load_status_cache", return_value={})
    def test_resolve_ensemble_providers_fallback(self, mock_cache, mock_api, mock_sub, mock_enabled):
        # All disabled and unconfigured -> falls back to local
        providers = resolve_ensemble_providers()
        self.assertEqual(providers, ["local"])

    def test_extract_summary_bullet_empty(self):
        self.assertEqual(extract_summary_bullet(""), "无输出")
        self.assertEqual(extract_summary_bullet("   \n\t  "), "无输出")
        self.assertEqual(extract_summary_bullet(None), "无输出")

    def test_extract_summary_bullet_verdict(self):
        text_with_defects = (
            "MAKEWAND_VERDICT: "
            '{"verdict": "REJECT", "pass": false, "defects": ["Memory leak in loop", "Missing lock"]}\n'
        )
        bullet = extract_summary_bullet(text_with_defects)
        self.assertIn("发现缺陷 (2项)", bullet)
        self.assertIn("Memory leak in loop", bullet)

        text_pass = (
            "MAKEWAND_VERDICT: "
            '{"verdict": "PASS", "pass": true, "defects": []}\n'
        )
        bullet_pass = extract_summary_bullet(text_pass)
        self.assertIn("评审通过", bullet_pass)

    def test_extract_summary_bullet_keywords(self):
        text_accept = "# 审稿报告\n\n### 审稿结论\nAccept as is with high priority.\n"
        self.assertIn("审稿结论", extract_summary_bullet(text_accept))

        text_lgtm = "LGTM! The patch is clean and adheres to all invariants.\n"
        self.assertEqual(extract_summary_bullet(text_lgtm), "LGTM! The patch is clean and adheres to all invariants.")

    def test_extract_summary_bullet_fallback_and_truncation(self):
        text_fallback = "# Heading 1\n## Heading 2\nThis is a detailed analysis sentence of the system architecture.\n"
        bullet = extract_summary_bullet(text_fallback)
        self.assertEqual(bullet, "This is a detailed analysis sentence of the system architecture.")

        long_line = "A" * 300
        truncated = extract_summary_bullet(long_line, max_len=50)
        self.assertEqual(len(truncated), 50)

    def test_format_ensemble_matrix(self):
        results = {
            "claude": {
                "ok": True,
                "duration": 4.5,
                "model_display": "CLAUDE (claude-sonnet-5-5)",
                "summary": "评审通过 (Pass: True)",
                "output_file": "/tmp/review/review_claude.md",
                "output": "Claude detailed review output text...",
            },
            "codex": {
                "ok": False,
                "duration": 2.1,
                "model_display": "CODEX (gpt-6.1-sol)",
                "summary": "连接超时",
                "output_file": None,
                "error": "Timeout after 300s",
            }
        }
        matrix = format_ensemble_matrix(results, effort="high", tier="deep")
        self.assertIn("多模型智囊团联合评审汇总矩阵", matrix)
        self.assertIn("| **CLAUDE (claude-sonnet-5-5)** | ✅ 成功 | 4.5s | 评审通过 (Pass: True) | `/tmp/review/review_claude.md` |", matrix)
        self.assertIn("| **CODEX (gpt-6.1-sol)** | ❌ 失败 | 2.1s | 连接超时 | - |", matrix)
        self.assertIn("#### 专家: CLAUDE (claude-sonnet-5-5)", matrix)
        self.assertIn("Claude detailed review output text...", matrix)
        self.assertIn("#### 专家: CODEX (gpt-6.1-sol)", matrix)
        self.assertIn("Timeout after 300s", matrix)

    @patch("makewand.ensemble.resolve_ensemble_providers", return_value=["claude", "codex"])
    @patch("makewand.orchestrator.dispatch_task")
    def test_run_ensemble_basic(self, mock_dispatch, mock_resolve):
        def _fake_dispatch(prov, prompt, **kwargs):
            self.assertTrue(kwargs.get("readonly"))
            self.assertEqual(kwargs.get("effort"), "high")
            if prov == "claude":
                return True, "### 审稿结论: Accept as is.\nAll good.", None
            return True, "LGTM, no defects found.", None

        mock_dispatch.side_effect = _fake_dispatch

        with tempfile.TemporaryDirectory() as td:
            res = run_ensemble(
                prompt="Review paper draft",
                providers="claude,codex",
                cwd=td,
                output_dir=td,
                prefix="nature_round1",
                effort="high",
            )
            self.assertTrue(res["ok"])
            self.assertTrue(res["all_succeeded"])
            self.assertEqual(res["providers"], ["claude", "codex"])

            # Verify files written
            claude_file = os.path.join(td, "nature_round1_claude.md")
            codex_file = os.path.join(td, "nature_round1_codex.md")
            matrix_file = os.path.join(td, "nature_round1_matrix.md")

            self.assertTrue(os.path.isfile(claude_file))
            self.assertTrue(os.path.isfile(codex_file))
            self.assertTrue(os.path.isfile(matrix_file))

            with open(claude_file, "r", encoding="utf-8") as f:
                content = f.read()
                self.assertIn("Accept as is", content)

            with open(matrix_file, "r", encoding="utf-8") as f:
                matrix_content = f.read()
                self.assertIn("多模型智囊团联合评审汇总矩阵", matrix_content)

    @patch("makewand.ensemble.resolve_ensemble_providers", return_value=[])
    def test_run_ensemble_no_providers(self, mock_resolve):
        res = run_ensemble("Prompt", providers="none")
        self.assertFalse(res["ok"])
        self.assertIn("未找到任何可用的 AI 评审引擎", res["error"])

    def test_cmd_clean(self):
        with tempfile.TemporaryDirectory(prefix="makewand-restore-test-") as td1:
            dummy_file = os.path.join(td1, "state.json")
            with open(dummy_file, "w") as f:
                f.write('{"test": true}')

            # Mock glob to return td1
            with patch("glob.glob", side_effect=lambda pat: [td1] if "restore" in pat else []):
                args = MagicMock()
                with self.assertRaises(SystemExit) as cm:
                    cmd_clean(args)
                self.assertEqual(cm.exception.code, 0)
                # Should have removed td1
                self.assertFalse(os.path.exists(dummy_file))


if __name__ == "__main__":
    unittest.main()
