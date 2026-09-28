"""
G4 regression tests: MAKEWAND_UNSAFE_HOST_EXEC in the Python engine (arch-product#2).

SECURITY.md promises that the environment variable alone never enables host
execution: a one-time, host-bound acknowledgment is required, non-interactive
runs without it are refused and every host execution is audited. The Python
run_in_sandbox() used to run commands directly on the host as soon as the
variable was "1". The acknowledgment is shared with the Go engine
(cmd/makewand/hostexec.go, internal/config): same config.json keys, same
risk-statement version, same audit file.
"""

import contextlib
import io
import json
import os
import re
import socket
import stat
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import makewand.config as mw_config
from makewand import sandbox
from makewand.sandbox import run_in_sandbox

REPO_ROOT = Path(__file__).resolve().parent.parent


class HostExecCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.cfg_dir = Path(self._tmp.name) / "config"
        self.ws = Path(self._tmp.name) / "ws"
        self.ws.mkdir()
        self.marker = self.ws / "host-exec-ran"
        self._patches = [
            patch.object(mw_config, "CONFIG_DIR", self.cfg_dir),
            patch("makewand.sandbox.is_bwrap_available", return_value=False),
            patch.dict(sandbox._host_exec_session, {"warned": False, "declined": False}),
        ]
        for p in self._patches:
            p.start()
        self.addCleanup(self._cleanup)

    def _cleanup(self):
        for p in reversed(self._patches):
            p.stop()
        self._tmp.cleanup()

    @contextlib.contextmanager
    def env(self, value="1"):
        with patch.dict(os.environ, {"MAKEWAND_UNSAFE_HOST_EXEC": value} if value is not None else {}):
            if value is None:
                os.environ.pop("MAKEWAND_UNSAFE_HOST_EXEC", None)
            yield

    def write_config(self, data):
        self.cfg_dir.mkdir(parents=True, exist_ok=True)
        (self.cfg_dir / "config.json").write_text(json.dumps(data), encoding="utf-8")

    def valid_ack(self, **overrides):
        data = {
            "language": "zh",
            "unsafe_host_exec_ack_version": sandbox.UNSAFE_HOST_EXEC_ACK_CURRENT_VERSION,
            "unsafe_host_exec_ack_at": "2026-09-28T00:00:00Z",
            "unsafe_host_exec_ack_host": socket.gethostname(),
        }
        data.update(overrides)
        return data

    def run_marker(self, interactive=False, answer=""):
        stdin = io.StringIO(answer)
        with patch("makewand.sandbox._stdio_interactive", return_value=interactive), \
                patch("sys.stdin", stdin), patch("sys.stderr", io.StringIO()) as err:
            ret, out, stderr, category = run_in_sandbox(
                ["bash", "-c", f"echo ran > {self.marker}"], workspace=str(self.ws))
        return ret, category, err.getvalue()

    def audit_lines(self):
        path = self.cfg_dir / sandbox.UNSAFE_HOST_EXEC_AUDIT_FILE
        if not path.exists():
            return []
        return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]


class TestUnsafeHostExecGate(HostExecCase):
    def test_env_var_alone_non_interactive_is_refused(self):
        with self.env("1"):
            ret, category, err = self.run_marker(interactive=False)
        self.assertEqual(category, "SandboxUnavailable")
        self.assertNotEqual(ret, 0)
        self.assertFalse(self.marker.exists(), "host execution must not happen without acknowledgment")
        self.assertEqual(self.audit_lines(), [])
        self.assertIn("尚未完成一次性宿主执行确认", err)

    def test_valid_ack_executes_and_audits_every_run(self):
        self.write_config(self.valid_ack())
        with self.env("1"):
            for _ in range(2):
                ret, category, _ = self.run_marker()
                self.assertEqual(ret, 0, category)
        self.assertTrue(self.marker.exists())
        lines = self.audit_lines()
        self.assertEqual(len(lines), 2)
        self.assertEqual(list(lines[0].keys()), ["time", "context", "command", "args", "dir", "source"])
        self.assertEqual(lines[0]["command"], "bash")
        self.assertEqual(lines[0]["args"][0], "-c")
        self.assertEqual(lines[0]["dir"], str(self.ws))
        self.assertEqual(lines[0]["source"], "config-ack")
        mode = stat.S_IMODE((self.cfg_dir / sandbox.UNSAFE_HOST_EXEC_AUDIT_FILE).stat().st_mode)
        self.assertEqual(mode, 0o600)

    def test_ack_bound_to_other_host_or_old_version_is_refused(self):
        for bad in (
            {"unsafe_host_exec_ack_host": "some-other-machine"},
            {"unsafe_host_exec_ack_host": ""},
            {"unsafe_host_exec_ack_version": 0},
            {"unsafe_host_exec_ack_version": "1"},
            {"unsafe_host_exec_ack_version": True},
        ):
            with self.subTest(bad=bad):
                self.write_config(self.valid_ack(**bad))
                with self.env("1"):
                    ret, category, _ = self.run_marker(interactive=False)
                self.assertEqual(category, "SandboxUnavailable")
                self.assertFalse(self.marker.exists())

    def test_ack_without_env_var_is_not_enough(self):
        self.write_config(self.valid_ack())
        with self.env(None):
            ret, category, _ = self.run_marker()
        self.assertEqual(category, "SandboxUnavailable")
        self.assertFalse(self.marker.exists())

    def test_interactive_yes_records_go_compatible_ack(self):
        self.write_config({"language": "zh", "python_only_key": [1, 2]})
        with self.env("1"):
            ret, category, err = self.run_marker(interactive=True, answer="YES\n")
        self.assertEqual(ret, 0, err)
        self.assertTrue(self.marker.exists())
        cfg = json.loads((self.cfg_dir / "config.json").read_text(encoding="utf-8"))
        self.assertEqual(cfg["python_only_key"], [1, 2])
        self.assertEqual(cfg["language"], "zh")
        self.assertEqual(cfg["unsafe_host_exec_ack_version"], sandbox.UNSAFE_HOST_EXEC_ACK_CURRENT_VERSION)
        self.assertEqual(cfg["unsafe_host_exec_ack_host"], socket.gethostname())
        self.assertRegex(cfg["unsafe_host_exec_ack_at"], r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$")
        self.assertEqual(stat.S_IMODE((self.cfg_dir / "config.json").stat().st_mode), 0o600)
        self.assertEqual(self.audit_lines()[0]["source"], "interactive-ack")

    def test_interactive_decline_refuses_and_does_not_reprompt(self):
        self.write_config({"language": "zh"})
        before = (self.cfg_dir / "config.json").read_text(encoding="utf-8")
        with self.env("1"):
            ret, category, err = self.run_marker(interactive=True, answer="no\n")
            self.assertEqual(category, "SandboxUnavailable")
            ret2, category2, err2 = self.run_marker(interactive=True, answer="yes\n")
        self.assertEqual(category2, "SandboxUnavailable")
        self.assertNotIn("输入 \"yes\"", err2)
        self.assertFalse(self.marker.exists())
        self.assertEqual((self.cfg_dir / "config.json").read_text(encoding="utf-8"), before)

    def test_corrupt_config_fails_closed_and_is_left_untouched(self):
        self.cfg_dir.mkdir(parents=True)
        (self.cfg_dir / "config.json").write_text("{not json", encoding="utf-8")
        with self.env("1"):
            ret, category, err = self.run_marker(interactive=True, answer="yes\n")
        self.assertEqual(category, "SandboxUnavailable")
        self.assertFalse(self.marker.exists())
        self.assertEqual((self.cfg_dir / "config.json").read_text(encoding="utf-8"), "{not json")
        self.assertIn("fail closed", err)

    def test_gate_helper_matches_run_in_sandbox(self):
        with self.env("1"), patch("makewand.sandbox._stdio_interactive", return_value=False), \
                patch("sys.stderr", io.StringIO()):
            self.assertFalse(sandbox.is_unsafe_host_exec_authorized())
            self.write_config(self.valid_ack())
            self.assertTrue(sandbox.is_unsafe_host_exec_authorized())


class TestGoAlignment(unittest.TestCase):
    """The acknowledgment and audit log must stay interchangeable with the Go engine."""

    def test_keys_version_and_audit_file_match_go_sources(self):
        cfg_go = (REPO_ROOT / "internal" / "config" / "config.go").read_text(encoding="utf-8")
        hostexec_go = (REPO_ROOT / "cmd" / "makewand" / "hostexec.go").read_text(encoding="utf-8")
        for key in (sandbox.ACK_VERSION_KEY, sandbox.ACK_AT_KEY, sandbox.ACK_HOST_KEY):
            self.assertIn(f'json:"{key},omitempty"', cfg_go)
        m = re.search(r"UnsafeHostExecAckCurrentVersion\s*=\s*(\d+)", cfg_go)
        self.assertIsNotNone(m)
        self.assertEqual(int(m.group(1)), sandbox.UNSAFE_HOST_EXEC_ACK_CURRENT_VERSION)
        m = re.search(r'unsafeHostExecAuditFile\s*=\s*"([^"]+)"', hostexec_go)
        self.assertEqual(m.group(1), sandbox.UNSAFE_HOST_EXEC_AUDIT_FILE)
        for field in ("time", "context", "command", "args", "dir", "source"):
            self.assertRegex(hostexec_go, rf'json:"{field}(,omitempty)?"')

    def test_audit_file_lives_in_config_dir(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(mw_config, "CONFIG_DIR", Path(tmp)):
            self.assertEqual(sandbox.unsafe_host_exec_audit_path(), Path(tmp) / "unsafe_exec_audit.jsonl")


if __name__ == "__main__":
    unittest.main()
