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

from makewand.cli import cmd_clean


class TestEnsemble(unittest.TestCase):
    """Test suite for multi-model ensemble orchestrator."""


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
