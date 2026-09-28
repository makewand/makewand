"""
G4 regression tests: provider credential/state directories inside the bwrap sandbox.

Findings covered:
  py-security#1, replay-0926-memory#8, replay-F01-F06-adaptive#15 — the provider
  state directory used to be bound writable as a whole, so a sandboxed model
  could overwrite ~/.claude/CLAUDE.md, drop commands/plugins/skills, replace
  ~/.grok/bin/grok, edit .codex/AGENTS.md or .gemini/settings.json and thereby
  persist behaviour into later *host* sessions.

Every test builds a throw-away fake HOME, so the real ~/.claude etc. are never
touched, and executes the real bwrap command line when bubblewrap works.
"""

import contextlib
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from makewand import sandbox
from makewand.sandbox import wrap_bwrap, is_bwrap_available

BWRAP_OK = is_bwrap_available()


@contextlib.contextmanager
def fake_home():
    home = Path(tempfile.mkdtemp(prefix="g4-fake-home-"))
    try:
        with patch.dict(os.environ, {"HOME": str(home)}):
            yield home
    finally:
        shutil.rmtree(home, ignore_errors=True)


@contextlib.contextmanager
def temp_workspace():
    ws = Path(tempfile.mkdtemp(prefix="g4-ws-"))
    try:
        yield ws
    finally:
        shutil.rmtree(ws, ignore_errors=True)


def run_payload(provider, workspace, payload, readonly=False):
    cmd = wrap_bwrap(["bash", "-c", payload], workspace=str(workspace), is_provider=True,
                     provider_name=provider, readonly=readonly, allow_network=False)
    return subprocess.run(cmd, cwd=str(workspace), capture_output=True, text=True, timeout=60)


def attempt(path_expr, label):
    """Shell snippet printing 'W <label>' when the write succeeded, 'RO <label>' otherwise."""
    return f"( {path_expr} ) 2>/dev/null && echo 'W {label}' || echo 'RO {label}'\n"


def mounts(cmd, flag):
    return [(cmd[i + 1], cmd[i + 2]) for i, x in enumerate(cmd[:-2]) if x == flag]


@unittest.skipUnless(BWRAP_OK, "bubblewrap not usable on this host")
class TestClaudeStateDirectory(unittest.TestCase):
    def _make_claude(self, home):
        c = home / ".claude"
        for d in ("commands", "agents", "skills/x", "plugins", "hooks", "projects/-proj"):
            (c / d).mkdir(parents=True, exist_ok=True)
        (c / "CLAUDE.md").write_text("orig-global\n")
        (c / "commands" / "keep.md").write_text("keep\n")
        (c / "skills" / "x" / "SKILL.md").write_text("skill\n")
        (c / "hooks" / "h.sh").write_text("#!/bin/sh\n")
        (c / "settings.json").write_text('{"model": "x"}\n')
        (c / ".credentials.json").write_text('{"tok": "OLD"}\n')
        (c / "shell-snapshots").mkdir()
        (c / "shell-snapshots" / "snapshot-bash-host.sh").write_text("echo host\n")
        (home / ".claude.json").write_text('{"mcpServers": {}}\n')
        return c

    def test_writable_task_cannot_persist_instructions_extensions_or_config(self):
        with fake_home() as home, temp_workspace() as ws:
            c = self._make_claude(home)
            payload = (
                attempt("echo PWNED > ~/.claude/CLAUDE.md", "CLAUDE.md")
                + attempt("echo x > ~/.claude/commands/pwn.md", "commands")
                + attempt("mkdir -p ~/.claude/plugins/evil && echo x > ~/.claude/plugins/evil/x.js", "plugins")
                + attempt("mkdir -p ~/.claude/agents && echo x > ~/.claude/agents/a.md", "agents")
                + attempt("echo x > ~/.claude/skills/x/SKILL.md", "skills")
                + attempt("echo x > ~/.claude/hooks/h.sh", "hooks")
                + attempt("mkdir -p ~/.claude/output-styles/o", "output-styles")
                + attempt("echo '{}' > ~/.claude/settings.json", "settings.json")
                + attempt("echo '{\"hooks\":1}' > ~/.claude/settings.local.json", "settings.local.json")
                + attempt("echo '{}' > ~/.claude.json", ".claude.json")
                + attempt("mkdir -p ~/.claude/projects/-proj/memory/z && echo x > ~/.claude/projects/-proj/memory/MEMORY.md", "memory")
                + attempt("echo '{\"tok\": \"NEW\"}' > ~/.claude/.credentials.json", "credentials")
                + attempt("echo s > ~/.claude/projects/-proj/session.jsonl", "session")
                + attempt("echo s > ws_file", "workspace")
                + "ls ~/.claude/shell-snapshots | wc -l | sed 's/^/SNAPSHOTS=/'\n"
            )
            res = run_payload("claude", ws, payload)
            out = res.stdout
            self.assertEqual(res.returncode, 0, res.stderr)
            for label in ("CLAUDE.md", "commands", "plugins", "agents", "skills", "hooks", "output-styles",
                          "settings.json", "settings.local.json", ".claude.json", "memory"):
                self.assertIn(f"RO {label}", out, f"{label} must be read-only inside the sandbox\n{out}")
            # session and credential state stay writable (user decision c)
            for label in ("credentials", "session", "workspace"):
                self.assertIn(f"W {label}", out, out)
            # host-sessions' shell snapshots are hidden behind an empty tmpfs
            self.assertIn("SNAPSHOTS=0", out)

            self.assertEqual((c / "CLAUDE.md").read_text(), "orig-global\n")
            self.assertFalse((c / "commands" / "pwn.md").exists())
            self.assertFalse((c / "plugins" / "evil").exists())
            self.assertEqual((c / "skills" / "x" / "SKILL.md").read_text(), "skill\n")
            self.assertEqual((c / "settings.json").read_text(), '{"model": "x"}\n')
            self.assertTrue((c / "shell-snapshots" / "snapshot-bash-host.sh").exists())
            # missing protected paths became empty, harmless host placeholders
            self.assertEqual(list((c / "output-styles").iterdir()), [])
            self.assertEqual((c / "settings.local.json").read_text(), "{}\n")
            self.assertEqual(list((c / "projects" / "-proj" / "memory").iterdir()), [])
            self.assertEqual((c / ".credentials.json").read_text().strip(), '{"tok": "NEW"}')

    def test_readonly_task_mounts_whole_state_root_readonly_except_state(self):
        with fake_home() as home, temp_workspace() as ws:
            c = self._make_claude(home)
            (c / "backups").mkdir()
            payload = (
                attempt("echo x > ~/.claude/new-top-level-file", "new-file")
                + attempt("echo x > ~/.claude/CLAUDE.md", "CLAUDE.md")
                + attempt("echo x > ~/.claude/projects/-proj/s.jsonl", "projects")
                + attempt("echo '{\"tok\": \"R\"}' > ~/.claude/.credentials.json", "credentials")
                + attempt("echo b > ~/.claude/backups/b.json", "backups")
                + attempt("echo s > ws_file", "workspace")
            )
            res = run_payload("claude", ws, payload, readonly=True)
            out = res.stdout
            self.assertEqual(res.returncode, 0, res.stderr)
            for label in ("new-file", "CLAUDE.md", "projects", "workspace"):
                self.assertIn(f"RO {label}", out, out)
            for label in ("credentials", "backups"):
                self.assertIn(f"W {label}", out, out)
            self.assertFalse((c / "new-top-level-file").exists())
            # read-only tasks never create host placeholders
            self.assertFalse((c / "output-styles").exists())

    def test_symlinked_protected_file_is_readonly_and_sandbox_still_starts(self):
        with fake_home() as home, temp_workspace() as ws:
            c = self._make_claude(home)
            dot = home / "dotfiles"
            dot.mkdir()
            (dot / "CLAUDE.md").write_text("dotfile\n")
            (c / "CLAUDE.md").unlink()
            (c / "CLAUDE.md").symlink_to(dot / "CLAUDE.md")
            res = run_payload("claude", ws, attempt("echo PWNED > ~/.claude/CLAUDE.md", "CLAUDE.md") + "cat ~/.claude/CLAUDE.md\n")
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertIn("RO CLAUDE.md", res.stdout)
            self.assertIn("dotfile", res.stdout)
            self.assertEqual((dot / "CLAUDE.md").read_text(), "dotfile\n")

    def test_other_provider_state_is_never_visible(self):
        with fake_home() as home, temp_workspace() as ws:
            self._make_claude(home)
            for d in (".codex", ".grok", ".gemini", ".config/muse", ".ssh"):
                (home / d).mkdir(parents=True)
                (home / d / "secret").write_text("s\n")
            payload = "".join(f"test -e ~/{d}/secret && echo 'VISIBLE {d}' || echo 'HIDDEN {d}'\n"
                              for d in (".codex", ".grok", ".gemini", ".config/muse", ".ssh"))
            res = run_payload("claude", ws, payload)
            self.assertNotIn("VISIBLE", res.stdout, res.stdout)


@unittest.skipUnless(BWRAP_OK, "bubblewrap not usable on this host")
class TestOtherProviderStateDirectories(unittest.TestCase):
    def test_grok_bin_stays_readonly_in_both_modes(self):
        for readonly in (True, False):
            with self.subTest(readonly=readonly), fake_home() as home, temp_workspace() as ws:
                g = home / ".grok"
                (g / "bin").mkdir(parents=True)
                (g / "bin" / "grok").write_text("#!/bin/sh\necho real-grok\n")
                (g / "config.toml").write_text("model = 'x'\n")
                (g / "auth.json").write_text("{}\n")
                payload = (
                    attempt("echo 'echo PWNED' > ~/.grok/bin/grok", "bin")
                    + attempt("echo x > ~/.grok/config.toml", "config")
                    + attempt("mkdir -p ~/.grok/hooks && echo x > ~/.grok/hooks/h.sh", "hooks")
                    + attempt("echo '{\"t\":1}' > ~/.grok/auth.json", "auth")
                )
                res = run_payload("grok", ws, payload, readonly=readonly)
                self.assertEqual(res.returncode, 0, res.stderr)
                for label in ("bin", "config", "hooks"):
                    self.assertIn(f"RO {label}", res.stdout, res.stdout)
                self.assertIn("W auth", res.stdout)
                self.assertEqual((g / "bin" / "grok").read_text(), "#!/bin/sh\necho real-grok\n")
                cmd = wrap_bwrap(["grok"], workspace=str(ws), is_provider=True, provider_name="grok", readonly=readonly)
                root_bind = cmd.index(str(g))
                bin_ro = [i for i, x in enumerate(cmd) if x == str(g / "bin") and cmd[i - 1] == "--ro-bind"]
                self.assertTrue(bin_ro and bin_ro[0] > root_bind, "grok bin/ ro-bind must come after the ~/.grok bind")

    def test_codex_instruction_config_paths_readonly_and_placeholders(self):
        with fake_home() as home, temp_workspace() as ws:
            x = home / ".codex"
            (x / "skills" / "s").mkdir(parents=True)
            (x / "skills" / "s" / "SKILL.md").write_text("skill\n")
            (x / "config.toml").write_text("model = 'gpt'\n")
            (x / "auth.json").write_text("{}\n")
            (x / "sessions").mkdir()
            (x / "shell_snapshots").mkdir()
            (x / "shell_snapshots" / "host.sh").write_text("echo host\n")
            payload = (
                attempt("echo x >> ~/.codex/config.toml", "config.toml")
                + attempt("echo PWNED > ~/.codex/AGENTS.md", "AGENTS.md")
                + attempt("mkdir -p ~/.codex/prompts && echo x > ~/.codex/prompts/p.md", "prompts")
                + attempt("echo x > ~/.codex/skills/s/SKILL.md", "skills")
                + attempt("mkdir -p ~/.codex/rules && echo x > ~/.codex/rules/r.rules", "rules")
                + attempt("echo '{\"t\":2}' > ~/.codex/auth.json", "auth")
                + attempt("echo s > ~/.codex/sessions/r.jsonl", "sessions")
                + "ls ~/.codex/shell_snapshots | wc -l | sed 's/^/SNAP=/'\n"
            )
            res = run_payload("codex", ws, payload)
            self.assertEqual(res.returncode, 0, res.stderr)
            for label in ("config.toml", "AGENTS.md", "prompts", "skills", "rules"):
                self.assertIn(f"RO {label}", res.stdout, res.stdout)
            for label in ("auth", "sessions"):
                self.assertIn(f"W {label}", res.stdout, res.stdout)
            self.assertIn("SNAP=0", res.stdout)
            self.assertEqual((x / "AGENTS.md").read_text(), "")
            self.assertEqual((x / "config.toml").read_text(), "model = 'gpt'\n")
            self.assertTrue((x / "shell_snapshots" / "host.sh").exists())

    def test_agy_gemini_dir_symlinked_elsewhere_is_protected_via_real_path(self):
        with fake_home() as home, temp_workspace() as ws:
            real = home / "relocated" / "gemini"
            real.mkdir(parents=True)
            (real / "settings.json").write_text('{"mcpServers": {}}\n')
            (real / "oauth_creds.json").write_text("{}\n")
            (home / ".gemini").symlink_to(real)
            payload = (
                attempt("echo '{\"mcpServers\":{\"x\":{}}}' > ~/.gemini/settings.json", "settings")
                + attempt(f"echo '{{}}' > {real}/settings.json", "settings-real-path")
                + attempt("echo PWNED > ~/.gemini/GEMINI.md", "GEMINI.md")
                + attempt("mkdir -p ~/.gemini/extensions/evil", "extensions")
                + attempt("mkdir -p ~/.gemini/commands && echo x > ~/.gemini/commands/c.toml", "commands")
                + attempt("echo '{\"t\":1}' > ~/.gemini/oauth_creds.json", "oauth")
            )
            res = run_payload("agy", ws, payload)
            self.assertEqual(res.returncode, 0, res.stderr)
            for label in ("settings", "settings-real-path", "GEMINI.md", "extensions", "commands"):
                self.assertIn(f"RO {label}", res.stdout, res.stdout)
            self.assertIn("W oauth", res.stdout)
            self.assertEqual((real / "settings.json").read_text(), '{"mcpServers": {}}\n')

    def test_muse_config_readonly_and_runtime_sockets_unreachable(self):
        with fake_home() as home, temp_workspace() as ws:
            cfg = home / ".config" / "muse"
            data = home / ".local" / "share" / "muse"
            cfg.mkdir(parents=True)
            (data / "runtime" / "muse").mkdir(parents=True)
            (cfg / "settings.json").write_text('{"schema_version": 1}\n')
            (cfg / "env").write_text("META_API_KEY=x\n")
            (cfg / "auth.json").write_text("{}\n")
            sock_path = data / "runtime" / "muse" / "ms-test.sock"
            srv = socket.socket(socket.AF_UNIX)
            srv.bind(str(sock_path))
            srv.listen(1)
            try:
                payload = (
                    attempt("echo x > ~/.config/muse/settings.json", "settings")
                    + attempt("echo 'NODE_OPTIONS=--require=/x' >> ~/.config/muse/env", "env")
                    + attempt("echo '{\"t\":1}' > ~/.config/muse/auth.json", "auth")
                    + f"test -S {sock_path} && echo SOCKET_VISIBLE || echo SOCKET_HIDDEN\n"
                )
                res = run_payload("muse", ws, payload)
            finally:
                srv.close()
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertIn("RO settings", res.stdout)
            self.assertIn("RO env", res.stdout)
            self.assertIn("W auth", res.stdout)
            self.assertIn("SOCKET_HIDDEN", res.stdout)
            self.assertEqual((cfg / "env").read_text(), "META_API_KEY=x\n")

    def test_sockets_inside_provider_state_dir_are_masked(self):
        with fake_home() as home, temp_workspace() as ws:
            x = home / ".codex"
            (x / "custom-daemon").mkdir(parents=True)
            sock_path = x / "custom-daemon" / "ctl.sock"
            srv = socket.socket(socket.AF_UNIX)
            srv.bind(str(sock_path))
            srv.listen(1)
            code = (
                "import socket,sys\n"
                "s=socket.socket(socket.AF_UNIX)\n"
                f"try:\n s.connect({str(sock_path)!r}); print('CONNECTED')\n"
                "except Exception as e:\n print('BLOCKED', type(e).__name__)\n"
            )
            try:
                cmd = wrap_bwrap([sys.executable, "-c", code], workspace=str(ws), is_provider=True,
                                 provider_name="codex", allow_network=False)
                res = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
            finally:
                srv.close()
            self.assertIn("BLOCKED", res.stdout, res.stdout + res.stderr)


class TestProviderMountArgv(unittest.TestCase):
    """Argument-level checks that do not need a working bubblewrap."""

    def test_missing_provider_root_is_not_created_or_mounted(self):
        with fake_home() as home, temp_workspace() as ws:
            cmd = wrap_bwrap(["claude", "-p", "x"], workspace=str(ws), is_provider=True, provider_name="claude")
            self.assertFalse((home / ".claude").exists())
            self.assertNotIn(str(home / ".claude"), cmd)

    def test_non_provider_commands_never_mount_provider_state(self):
        with fake_home() as home, temp_workspace() as ws:
            (home / ".claude").mkdir()
            cmd = wrap_bwrap(["claude", "-p", "x"], workspace=str(ws), is_provider=False)
            self.assertNotIn(str(home / ".claude"), cmd)
            self.assertFalse((home / ".claude" / "CLAUDE.md").exists())

    def test_placeholder_failure_fails_closed(self):
        with fake_home() as home, temp_workspace() as ws:
            (home / ".claude").mkdir()
            with patch("makewand.sandbox.os.open", side_effect=PermissionError("denied")):
                with self.assertRaises(sandbox.SandboxConfigError):
                    wrap_bwrap(["claude"], workspace=str(ws), is_provider=True, provider_name="claude")


if __name__ == "__main__":
    unittest.main()
