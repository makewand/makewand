"""Provider account selection survives sandbox environment isolation."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from makewand.sandbox import _provider_mounts, wrap_bwrap, SandboxConfigError


class CodexHomeTests(unittest.TestCase):
    def test_only_selected_account_is_mounted_with_protected_instructions(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory) / "home"
            default, selected = home / ".codex", home / ".codex-2"
            default.mkdir(parents=True)
            selected.mkdir()
            with patch.dict(os.environ, {"CODEX_HOME": str(selected)}):
                args, roots, skips = _provider_mounts("codex", str(home), False, [str(Path(directory) / "project")])
            self.assertEqual(roots, [str(selected)])
            self.assertNotIn(str(default), args)
            for protected in ("config.toml", "AGENTS.md", "hooks", "skills"):
                target = str(selected / protected)
                index = args.index(target)
                self.assertEqual(args[index - 1], "--ro-bind")

    def test_selected_home_environment_is_preserved_only_for_codex(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            selected, workspace = root / "codex-state", root / "project"
            selected.mkdir()
            workspace.mkdir()
            with patch.dict(os.environ, {"CODEX_HOME": str(selected)}), patch("makewand.sandbox.is_bwrap_available", return_value=True):
                args = wrap_bwrap(["codex", "exec", "prompt"], workspace=str(workspace), is_provider=True, provider_name="codex")
                index = args.index("CODEX_HOME")
                self.assertEqual(args[index - 1:index + 2], ["--setenv", "CODEX_HOME", str(selected)])
                general = wrap_bwrap(["echo", "ok"], workspace=str(workspace))
                self.assertNotIn("CODEX_HOME", general)

    def test_missing_or_broad_home_fails_instead_of_switching_accounts(self):
        with tempfile.TemporaryDirectory() as directory:
            for selected in ("/", directory, str(Path(directory) / "missing")):
                with patch.dict(os.environ, {"CODEX_HOME": selected}), self.assertRaises(SandboxConfigError):
                    _provider_mounts("codex", directory, False, [str(Path(directory) / "project")])

    def test_symlinked_account_root_cannot_expose_home(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory) / "home"
            home.mkdir()
            selected = Path(directory) / "account"
            selected.symlink_to(home, target_is_directory=True)
            with patch.dict(os.environ, {"CODEX_HOME": str(selected)}), self.assertRaises(SandboxConfigError):
                _provider_mounts("codex", str(home), False, [str(Path(directory) / "project")])
