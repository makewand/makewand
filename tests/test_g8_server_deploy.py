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
        self.assertNotEqual(env["HOME"], self._flag("--data-dir"), "HOME and --data-dir must not share the exact same root")

    def test_deploy_guide_matches_unit(self):
        guide = (DOCS / "DEPLOY_PRODUCTION.md").read_text(encoding="utf-8")
        self.assertIn("/var/lib/makewand/server_auth.json", guide)
        self.assertNotIn("- `/etc/makewand/server_auth.json`", guide)


def _version_tuple(text):
    return tuple(int(part) for part in text.split("."))


class ContainerDeploymentTests(unittest.TestCase):
    """go-server#4, go-server#12, eng-delivery#8: the documented Compose path must boot."""

    def test_dockerfile_go_version_satisfies_go_mod(self):
        go_mod = (ROOT / "go.mod").read_text(encoding="utf-8")
        required = re.search(r"^go (\d+\.\d+(?:\.\d+)?)\s*$", go_mod, re.M).group(1)
        dockerfile = (DEPLOY / "Dockerfile").read_text(encoding="utf-8")
        image = re.search(r"^FROM golang:(\d+\.\d+(?:\.\d+)?)\S*\s+AS build", dockerfile, re.M)
        self.assertIsNotNone(image, "build stage must pin a golang:<version> image")
        self.assertGreaterEqual(_version_tuple(image.group(1)), _version_tuple(required))
        self.assertRegex(dockerfile, r"(?m)^ENV GOTOOLCHAIN=local$")

    def test_image_is_documented_as_server_only(self):
        dockerfile = (DEPLOY / "Dockerfile").read_text(encoding="utf-8")
        guide = (DOCS / "DEPLOY_PRODUCTION.md").read_text(encoding="utf-8")
        self.assertIn("Server-only image", dockerfile)
        self.assertNotIn("python3", re.findall(r"(?m)^RUN apt-get.*$", dockerfile)[0])
        self.assertIn("**server-only**", guide)
        self.assertIn("no Python runtime", guide)

    def test_compose_opts_into_paid_api_with_explanation(self):
        compose = (DEPLOY / "docker-compose.yml").read_text(encoding="utf-8")
        match = re.search(r"(?m)^\s+MAKEWAND_API_POLICY:\s*(\S+)\s*$", compose)
        self.assertIsNotNone(match, "compose must set MAKEWAND_API_POLICY for the API-key-only image")
        self.assertIn("allow_paid", match.group(1))
        self.assertIn("PAID API OPT-IN", compose)
        guide = (DOCS / "DEPLOY_PRODUCTION.md").read_text(encoding="utf-8")
        env_example = guide[guide.index("Example `deploy/.env`"):]
        self.assertIn("MAKEWAND_API_POLICY=allow_paid", env_example)
        self.assertIn("subscription_only", guide)


class ServerDocsTests(unittest.TestCase):
    """go-server#5, go-server#12: server docs must match the code and not mislead operators."""

    def setUp(self):
        self.alpha = (DOCS / "SERVER_ALPHA.md").read_text(encoding="utf-8")

    def test_troubleshooting_backup_uses_state_backup(self):
        # A copy command on a state.db path (the old advice was
        # `cp ~/.config/makewand/server/state.db{,.backup}`) loses WAL data.
        self.assertIsNone(re.search(r"cp\s+[~/$][^`\s]*state\.db", self.alpha), "plain cp of a live WAL database loses data")
        troubleshooting = self.alpha[self.alpha.index("### Database errors"):]
        self.assertIn("makewand state backup", troubleshooting.split("###")[1])

    def test_no_single_threaded_claim(self):
        self.assertNotRegex(self.alpha.lower(), r"single[- ]threaded")
        self.assertIn("concurrently", self.alpha)

    def test_every_server_environment_variable_is_documented(self):
        serve = (ROOT / "cmd" / "makewand" / "serve.go").read_text(encoding="utf-8")
        names = set(re.findall(r'os\.(?:Getenv|LookupEnv)\("(MAKEWAND_SERVER_[A-Z_]+)"\)', serve))
        self.assertGreaterEqual(len(names), 7, names)
        missing = sorted(name for name in names if f"`{name}`" not in self.alpha)
        self.assertEqual(missing, [], "undocumented serve environment variables")

    def test_registration_and_proxy_flags_are_documented(self):
        for flag in ("--registration-per-ip-limit", "--registration-global-limit", "--registration-window",
                     "--registration-concurrency", "--trusted-proxy"):
            self.assertIn(flag, self.alpha)
        self.assertIn("from the right", self.alpha)

    def test_cloudflare_exposure_states_risks_and_prerequisites(self):
        guide = (DOCS / "CLOUDFLARE_WEBSITE_DEPLOYMENT.md").read_text(encoding="utf-8")
        for needle in ("风险说明", "前置条件", "Cloudflare Access", "--trusted-proxy 127.0.0.1", "SERVER_ALPHA", "--enable-registration"):
            self.assertIn(needle, guide)
        tunnel = (DEPLOY / "cloudflare-tunnel.makewand.yml").read_text(encoding="utf-8")
        self.assertIn("WARNING", tunnel)
        self.assertIn("--trusted-proxy 127.0.0.1", tunnel)
        self.assertNotIn("noTLSVerify", tunnel)
        self.assertNotIn("safely to the public", tunnel)


if __name__ == "__main__":
    unittest.main()
