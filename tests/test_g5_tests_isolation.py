"""Regression tests for hermetic Python test runs (G5).

Covers the evaluation findings where running the suite outside
``scripts/test_python.py`` rewrote the developer's real state
(``~/.config/makewand/config.json`` with ``api_policy=allow_paid``,
``~/.gemini/config/trio_status.json``, candidates, autofix memory ...), probed
the host's Ollama daemon, and where gate results depended on host state.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation

import ast
import importlib
import importlib.util
import io
import json
import os
import pwd
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import unittest
import urllib.error
import urllib.request
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
import makewand.memory as memory
import makewand.usage as usage

REPO_ROOT = _isolation.REPO_ROOT
TESTS_DIR = REPO_ROOT / "tests"
REAL_HOME = Path(pwd.getpwuid(os.getuid()).pw_dir).resolve()
ISO = _isolation.activate()


def _inside(path, root):
    try:
        Path(os.path.abspath(path)).relative_to(Path(os.path.abspath(root)))
        return True
    except ValueError:
        return False


def _snapshot(root):
    """Relative path -> (size, mtime_ns, sha-ish content) for every entry under root."""
    result = {}
    for path in sorted(Path(root).rglob("*")):
        rel = str(path.relative_to(root))
        if path.is_file():
            result[rel] = (path.stat().st_size, path.stat().st_mtime_ns, path.read_bytes())
        else:
            result[rel] = ("dir",)
    return result


class TestProcessIsolation(unittest.TestCase):
    """In-process guarantees provided by tests/_isolation.py."""

    def test_home_xdg_and_makewand_state_env_point_into_isolation_root(self):
        for key in ("HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME",
                    "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "MAKEWAND_CONFIG_DIR",
                    "MAKEWAND_USAGE_FILE", "MAKEWAND_ARTIFACTS_DIR", "MAKEWAND_SHADOW_DIR"):
            with self.subTest(key=key):
                self.assertIn(key, os.environ)
                self.assertTrue(_inside(os.environ[key], ISO.root), os.environ[key])
        self.assertTrue(_inside(Path.home(), ISO.root))
        self.assertFalse(_inside(ISO.root, REAL_HOME))

    def test_config_and_derived_state_paths_are_redirected(self):
        paths = {f"config.{name}": getattr(config, name)
                 for name in _isolation.CONFIG_PATH_NAMES if hasattr(config, name)}
        paths.update({
            "memory.PATTERNS_FILE": memory.PATTERNS_FILE,
            "memory.PATTERNS_LOCK": memory.PATTERNS_LOCK,
            "usage.USAGE_WINDOW_FILE": usage.USAGE_WINDOW_FILE,
            "usage.active_file": usage._get_active_usage_file(),
            "health.STATUS_CACHE_FILE": health.STATUS_CACHE_FILE,
            "health.LEGACY_TRIO_CACHE": health.LEGACY_TRIO_CACHE,
        })
        real_state = (REAL_HOME / ".config" / "makewand", REAL_HOME / ".gemini",
                      Path("/tmp/makewand_test_usage.json"))
        for label, value in paths.items():
            with self.subTest(path=label):
                self.assertTrue(_inside(value, ISO.root), f"{label}={value}")
                for forbidden in real_state:
                    self.assertFalse(_inside(value, forbidden), f"{label}={value}")

    def test_credentials_and_policy_switches_are_scrubbed(self):
        leaked = [key for key in os.environ
                  if key.endswith(("_API_KEY", "_AUTH_TOKEN"))
                  or key == "MAKEWAND_API_POLICY"
                  or key.startswith(("MAKEWAND_ENABLE_", "MAKEWAND_DISABLE_"))]
        self.assertEqual(leaked, [])
        with patch.object(config, "load_user_config", return_value={}):
            self.assertEqual(config.get_api_policy(), "subscription_only")
            self.assertFalse(config.has_api_configured("claude"))

    def test_scrub_removes_injected_credentials_and_switches(self):
        injected = {"OPENAI_API_KEY": "sk-live", "ANTHROPIC_AUTH_TOKEN": "tok",
                    "MAKEWAND_API_POLICY": "allow_paid", "MAKEWAND_ENABLE_LOCAL": "1",
                    "MAKEWAND_DISABLE_CODEX": "1", "OLLAMA_HOST": "127.0.0.1:11434",
                    "GIT_DIR": "/somewhere/.git", "PATH": os.environ["PATH"]}
        with patch.dict(os.environ, injected):
            removed = set(_isolation._scrub_environment())
            for key in injected:
                if key != "PATH":
                    self.assertNotIn(key, os.environ)
            self.assertTrue(set(injected) - {"PATH"} <= removed)
            self.assertIn("PATH", os.environ)

    def test_ai_cli_stubs_shadow_real_binaries_and_exit_127(self):
        for name in _isolation.STUBBED_CLIS:
            with self.subTest(cli=name):
                found = shutil.which(name)
                self.assertEqual(Path(found).parent, ISO.bin_dir)
                marker = f"--g5-stub-probe-{name}"
                proc = subprocess.run([name, marker, "two words"], capture_output=True, text=True, timeout=10)
                self.assertEqual(proc.returncode, _isolation.STUB_EXIT_CODE)
                self.assertIn(f"{name} {marker} two words", _isolation.stub_calls())

    def test_local_model_endpoint_blocked_even_with_environment_cleared(self):
        from makewand.providers.local import is_local_model_available
        self.assertEqual(config.get_api_config("local")["base_url"], _isolation.UNREACHABLE_LOCAL_ENDPOINT)
        self.assertFalse(is_local_model_available(timeout=0.5)[0])
        before = len(_isolation.blocked_connections())
        # Even when a test clears the environment (default endpoint localhost:11434),
        # the host's Ollama daemon must never be reached.
        with patch.dict(os.environ, {}, clear=True), patch.object(config, "load_user_config", return_value={}):
            self.assertFalse(is_local_model_available(timeout=0.5)[0])
        with self.assertRaises(urllib.error.URLError):
            urllib.request.urlopen("http://127.0.0.1:11434/api/tags", timeout=0.5)
        blocked = _isolation.blocked_connections()[before:]
        self.assertTrue(any("11434" in line for line in blocked), blocked)

    def test_network_guard_refuses_off_host_but_allows_loopback_servers(self):
        server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.addCleanup(server.close)
        server.bind(("127.0.0.1", 0))
        server.listen(1)
        client = socket.create_connection(server.getsockname(), timeout=2)
        client.close()
        with self.assertRaises(ConnectionRefusedError):
            socket.create_connection(("192.0.2.10", 443), timeout=2)  # TEST-NET-1
        probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.addCleanup(probe.close)
        self.assertNotEqual(probe.connect_ex(("198.51.100.7", 80)), 0)

    def test_rebase_prefers_exact_and_longest_prefix(self):
        old_home, new_home = Path("/old/home"), Path("/iso/home")
        mapping = [
            (old_home / ".config" / "makewand", new_home / ".config" / "makewand"),
            (old_home / ".config" / "makewand" / "legacy_status.json", Path("/iso/legacy/trio_status.json")),
            (old_home / ".gemini", new_home / ".gemini"),
        ]
        rebase = _isolation.rebase_path
        self.assertEqual(rebase(old_home / ".config/makewand/legacy_status.json", mapping),
                         Path("/iso/legacy/trio_status.json"))
        self.assertEqual(rebase(old_home / ".config/makewand/candidates/x", mapping),
                         new_home / ".config/makewand/candidates/x")
        self.assertEqual(rebase(old_home / ".gemini/config/trio_status.json", mapping),
                         new_home / ".gemini/config/trio_status.json")
        self.assertIsNone(rebase(old_home / "dev/project", mapping))
        self.assertIsNone(rebase("not a path", mapping))

    def test_hybrid_routing_suite_never_persists_its_patched_config(self):
        """runtime-state#4 / py-reliability#2: the allow_paid+local test config was
        written into the session (or real) config.json by set_provider_enabled."""
        module = importlib.import_module(("tests." if __package__ else "") + "test_hybrid_routing")
        watched = [config.CONFIG_FILE, config.CONFIG_DIR / "candidates", config.CONFIG_DIR / "backups"]
        before = {str(p): (p.exists(), p.read_bytes() if p.is_file() else None) for p in watched}
        suite = unittest.defaultTestLoader.loadTestsFromModule(module)
        result = unittest.TextTestRunner(stream=io.StringIO(), verbosity=0).run(suite)
        self.assertTrue(result.wasSuccessful(), result.failures + result.errors)
        after = {str(p): (p.exists(), p.read_bytes() if p.is_file() else None) for p in watched}
        self.assertEqual(before, after)
        if config.CONFIG_FILE.exists():
            self.assertNotEqual(json.loads(config.CONFIG_FILE.read_text()).get("api_policy"), "allow_paid")


class TestLateActivation(unittest.TestCase):
    def test_constants_bound_before_isolation_are_rebound(self):
        """A makewand module imported before the isolation (e.g. by a test module
        without the guard) must not keep pointing at the caller's real state."""
        with tempfile.TemporaryDirectory(prefix="makewand-g5-late-") as tmp:
            fake_home = Path(tmp) / "home"
            (fake_home / ".gemini" / "config").mkdir(parents=True)
            code = (
                "import sys, json\n"
                f"sys.path[:0] = [{str(REPO_ROOT)!r}, {str(TESTS_DIR)!r}]\n"
                "import makewand.config, makewand.health, makewand.memory, makewand.usage\n"
                "import _isolation\n"
                "import makewand.config as c, makewand.health as h, makewand.memory as m, makewand.usage as u\n"
                "print(json.dumps({'root': str(_isolation.state().root), 'paths': [str(p) for p in (\n"
                "  c.CONFIG_DIR, c.CONFIG_FILE, c.CANDIDATES_DIR, c.LEGACY_TRIO_CACHE, h.STATUS_CACHE_FILE,\n"
                "  h.LEGACY_TRIO_CACHE, h.STATUS_LOCK_FILE, m.PATTERNS_FILE, u.USAGE_WINDOW_FILE)]}))\n"
            )
            env = {"HOME": str(fake_home), "PATH": os.environ["PATH"], "TMPDIR": tmp, "LANG": "C.UTF-8"}
            proc = subprocess.run([sys.executable, "-I", "-c", code], capture_output=True, text=True,
                                  env=env, cwd=tmp, timeout=60)
            self.assertEqual(proc.returncode, 0, proc.stderr)
            data = json.loads(proc.stdout.strip().splitlines()[-1])
            for value in data["paths"]:
                self.assertTrue(_inside(value, data["root"]), value)
                self.assertFalse(_inside(value, fake_home), value)
            self.assertFalse(Path(data["root"]).exists(), "isolation root must be removed at exit")


    def test_isolation_root_removed_when_runner_is_terminated(self):
        """A test run killed with SIGTERM (CI timeout, Ctrl-C in a wrapper) must not
        leave its private HOME/config tree behind in /tmp."""
        import signal
        with tempfile.TemporaryDirectory(prefix="makewand-g5-term-") as tmp:
            code = (
                "import sys, time\n"
                f"sys.path[:0] = [{str(REPO_ROOT)!r}, {str(TESTS_DIR)!r}]\n"
                "import _isolation\n"
                "print(_isolation.state().root, flush=True)\n"
                "time.sleep(60)\n"
            )
            env = {"HOME": str(Path(tmp) / "home"), "PATH": os.environ["PATH"], "TMPDIR": tmp}
            proc = subprocess.Popen([sys.executable, "-I", "-c", code], stdout=subprocess.PIPE,
                                    text=True, env=env, cwd=tmp)
            try:
                root = Path(proc.stdout.readline().strip())
                self.assertTrue(root.is_dir())
                proc.send_signal(signal.SIGTERM)
                self.assertEqual(proc.wait(timeout=30), -signal.SIGTERM)
            finally:
                proc.kill() if proc.poll() is None else None
                proc.stdout.close()
            self.assertFalse(root.exists())


class TestEntryPointsNeverTouchCallerHome(unittest.TestCase):
    """Every documented way of running the suite leaves the caller's HOME intact."""

    # hybrid: config.json/api_policy; adaptive: ~/.gemini trio_status mirror;
    # audit: ~/.aider.conf.yml, host Ollama dependency, rejected artifacts.
    MODULES = ("test_hybrid_routing", "test_adaptive_governance", "test_audit_v31_fixes")

    def run_entry(self, argv, extra_env=None):
        with tempfile.TemporaryDirectory(prefix="makewand-g5-entry-") as tmp:
            home = Path(tmp) / "home"
            private_tmp = Path(tmp) / "tmp"
            private_tmp.mkdir()
            # An existing user with Makewand + Gemini/Antigravity state.
            (home / ".config" / "makewand").mkdir(parents=True)
            (home / ".config" / "makewand" / "config.json").write_text(
                json.dumps({"api_policy": "subscription_only", "custom": 1}))
            (home / ".gemini" / "config").mkdir(parents=True)
            (home / ".gemini" / "config" / "trio_status.json").write_text('{"sentinel": true}')
            before = _snapshot(home)
            env = {"HOME": str(home), "PATH": os.environ["PATH"], "TMPDIR": str(private_tmp),
                   "LANG": "C.UTF-8",
                   # Defence in depth for pre-fix trees: never reach a host Ollama.
                   "LOCAL_MODEL_ENDPOINT": _isolation.UNREACHABLE_LOCAL_ENDPOINT,
                   **(extra_env or {})}
            proc = subprocess.run(argv, cwd=str(REPO_ROOT), env=env, capture_output=True,
                                  text=True, timeout=600)
            self.assertEqual(proc.returncode, 0, proc.stdout[-3000:] + proc.stderr[-3000:])
            self.assertEqual(_snapshot(home), before, "tests wrote into the caller's HOME")
            leftovers = [p.name for p in private_tmp.iterdir() if p.name.startswith("makewand-test-isolation-")]
            self.assertEqual(leftovers, [], "isolation root must be removed at exit")

    def test_pytest_entry(self):
        spec = importlib.util.find_spec("pytest")
        if spec is None or not spec.origin:
            self.skipTest("pytest not importable from this interpreter (e.g. python3 -I)")
        # The child gets a fresh HOME, so a user-site pytest must be passed explicitly.
        site_dir = str(Path(spec.origin).resolve().parent.parent)
        self.run_entry([sys.executable, "-m", "pytest", "-q", "-p", "no:cacheprovider"]
                       + [f"tests/{m}.py" for m in self.MODULES],
                       extra_env={"PYTHONPATH": site_dir})

    def test_unittest_module_entry(self):
        # ``python3 -m unittest tests.<module>`` from the checkout. ``tests`` is a
        # namespace package here, so an unrelated regular ``tests`` package on
        # sys.path (seen on dev hosts via .pth files) would shadow it: pin the
        # package to this checkout, then run unittest's real __main__.
        bootstrap = (
            "import runpy, sys, types\n"
            "pkg = types.ModuleType('tests')\n"
            f"pkg.__path__ = [{str(TESTS_DIR)!r}]\n"
            "sys.modules['tests'] = pkg\n"
            f"sys.path.insert(0, {str(REPO_ROOT)!r})\n"
            "sys.argv = ['python -m unittest'] + sys.argv[1:]\n"
            "runpy.run_module('unittest', run_name='__main__', alter_sys=True)\n"
        )
        self.run_entry([sys.executable, "-c", bootstrap, "-q"] + [f"tests.{m}" for m in self.MODULES])

    def test_unittest_discover_entry(self):
        # README: python3 -m unittest discover tests
        self.run_entry([sys.executable, "-m", "unittest", "discover", "-q", "tests",
                        "-p", "test_[ha][ydu]*_*.py"])

    def test_official_gate_entry(self):
        self.run_entry([sys.executable, "-I", "scripts/test_python.py"] + list(self.MODULES))


class TestGuardPresence(unittest.TestCase):
    # Owned by other remediation groups; conftest.py / scripts/test_python.py still
    # isolate them, only a direct ``python3 -m unittest`` of such a file relies on
    # its own guard (cross-group request filed).
    NOT_OWNED = {"test_multi_model_optimization.py", "test_sandbox.py"}
    OTHER_GROUP_FILE = re.compile(r"^test_g\d+[a-z]?_(?!tests_)")

    def test_every_owned_test_module_isolates_before_importing_makewand(self):
        for path in sorted(TESTS_DIR.glob("test_*.py")):
            if path.name in self.NOT_OWNED or self.OTHER_GROUP_FILE.match(path.name):
                continue
            with self.subTest(module=path.name):
                tree = ast.parse(path.read_text(encoding="utf-8"))
                guard_line = makewand_line = None
                for node in ast.walk(tree):
                    if isinstance(node, ast.Import):
                        names = [a.name for a in node.names]
                    elif isinstance(node, ast.ImportFrom):
                        names = [node.module or ""] + [f"{node.module}.{a.name}" for a in node.names]
                    else:
                        continue
                    if any(n == "_isolation" or n.endswith("._isolation") or n == "tests._isolation"
                           for n in names) and guard_line is None:
                        guard_line = node.lineno
                    if any(n == "makewand" or n.startswith("makewand.") for n in names):
                        makewand_line = node.lineno if makewand_line is None else min(makewand_line, node.lineno)
                self.assertIsNotNone(guard_line, "missing `import _isolation` guard")
                if makewand_line is not None:
                    self.assertLess(guard_line, makewand_line)


if __name__ == "__main__":
    unittest.main()
