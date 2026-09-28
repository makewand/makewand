"""Deployment-file regressions for the makewand server (G8 findings).

These checks keep deploy/ and the server docs consistent with the code they
describe. They only read repository files.
"""
import re
import shlex
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DEPLOY = ROOT / "deploy"
DOCS = ROOT / "docs"


def _unit_settings(path):
    settings = {}
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith(("#", ";", "[")) or "=" not in line:
            continue
        key, value = line.split("=", 1)
        settings.setdefault(key.strip(), []).append(value.strip())
    return settings


def _within(path, roots):
    path = Path(path)
    return any(path == Path(root) or Path(root) in path.parents for root in roots)


class SystemdUnitTests(unittest.TestCase):
    """go-server#7: files the server rewrites must be writable under the unit's sandbox."""

    def setUp(self):
        self.unit = _unit_settings(DEPLOY / "systemd.makewand.service")
        writable = []
        for entry in self.unit.get("ReadWritePaths", []):
            writable.extend(p.lstrip("-") for p in entry.split())
        for entry in self.unit.get("StateDirectory", []):
            writable.extend("/var/lib/" + name for name in entry.split())
        self.writable = writable
        self.argv = shlex.split(self.unit["ExecStart"][0])

    def _flag(self, name):
        self.assertIn(name, self.argv, f"ExecStart lacks {name}")
        return self.argv[self.argv.index(name) + 1]

    def test_protect_system_strict_is_kept(self):
        self.assertEqual(self.unit.get("ProtectSystem"), ["strict"])
        self.assertEqual(self.unit.get("ProtectHome"), ["true"])

    def test_auth_config_is_writable(self):
        auth_config = self._flag("--auth-config")
        self.assertFalse(auth_config.startswith("/etc/"), auth_config)
        self.assertTrue(_within(auth_config, self.writable), f"{auth_config} not under {self.writable}")

    def test_data_dir_home_and_config_dir_are_writable(self):
        self.assertTrue(_within(self._flag("--data-dir"), self.writable))
        env = dict(item.split("=", 1) for item in self.unit.get("Environment", []))
        for key in ("HOME", "MAKEWAND_CONFIG_DIR"):
            self.assertIn(key, env, f"unit must set {key}: ProtectHome hides /home")
            self.assertTrue(_within(env[key], self.writable), f"{key}={env[key]}")

    def test_deploy_guide_matches_unit(self):
        guide = (DOCS / "DEPLOY_PRODUCTION.md").read_text(encoding="utf-8")
        self.assertIn("/var/lib/makewand/server_auth.json", guide)
        self.assertNotIn("- `/etc/makewand/server_auth.json`", guide)


if __name__ == "__main__":
    unittest.main()
