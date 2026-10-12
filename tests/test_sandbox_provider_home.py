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
from makewand.sandbox import _provider_mounts, wrap_bwrap, SandboxConfigError, ProviderHome


class CodexHomeTests(unittest.TestCase):
    def test_only_selected_account_is_mounted_with_protected_instructions(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory) / "home"
            default, selected = home / ".codex", home / ".codex-2"
            default.mkdir(parents=True)
            selected.mkdir()
            (default / "auth.json").write_text('{"account": "default"}')
            (selected / "auth.json").write_text('{"account": "selected"}')
            for name in ("config.toml", "AGENTS.md"):
                (selected / name).write_text("x\n")
            for name in ("hooks", "skills"):
                (selected / name).mkdir()
            with patch.dict(os.environ, {"CODEX_HOME": str(selected)}):
                home_obj = ProviderHome("codex", str(home))
                try:
                    args = home_obj.materialize([str(Path(directory) / "project")])
                    roots = list(home_obj.state_roots)
                    self.assertEqual(roots, [os.path.realpath(selected)])
                    self.assertNotIn(str(default), args)
                    # the selected account root is backed by a private 0700 copy, never bound itself
                    index = args.index(str(selected))
                    self.assertEqual(args[index - 2], "--bind")
                    private_root = args[index - 1]
                    self.assertTrue(private_root.startswith(home_obj.private_dir + os.sep))
                    self.assertEqual(os.stat(home_obj.private_dir).st_mode & 0o777, 0o700)
                    self.assertNotIn(("--bind", str(selected)), list(zip(args, args[1:])))
                    # only the selected account's credential was copied
                    self.assertEqual(Path(private_root, "auth.json").read_text(), '{"account": "selected"}')
                    for protected in ("config.toml", "AGENTS.md", "hooks", "skills"):
                        target = str(selected / protected)
                        index = args.index(target)
                        self.assertEqual(args[index - 1:index + 2], ["--ro-bind", target, target])
                finally:
                    home_obj.finalize(write_back=False)
            self.assertFalse(os.path.exists(home_obj.private_dir or private_root))

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
