"""
Behavior tests for the Python provider-sandbox audit remediation.

H1  every provider launch without bubblewrap (read-only included) needs the
    recorded MAKEWAND_UNSAFE_HOST_EXEC authorization and is audited.
H3  the host provider state directory is never a writable bind source; a
    private per-run home receives copies of allowlisted credentials, and only
    those are written back (regular files, no symlinks, not when the host copy
    changed meanwhile).
H5  pathname sockets below node_modules / .git internals are masked.
Also: mandatory seccomp with explicit opt-out, resource-limit prefix without
--as, overflow-uid launcher acceptance only inside a user namespace, and the
MAKEWAND_PROVIDER_WORKER marker on provider launches only.

Everything is mocked: no real provider CLI is started.
"""

try:  # isolation must precede makewand imports
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import json
import os
import shutil
import socket
import stat
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

from makewand import sandbox
from makewand.sandbox import (
    PROVIDER_PROFILES,
    PROVIDER_WORKER_ENV,
    ProviderHome,
    SandboxConfigError,
    SandboxUnavailableError,
    provider_sandbox_launch,
    run_in_sandbox,
    seccomp_filter_context,
    wrap_bwrap,
)

FAKE_BWRAP = "/usr/bin/bwrap"


def _pairs(cmd, flag):
    return [(cmd[i + 1], cmd[i + 2]) for i, x in enumerate(cmd[:-2]) if x == flag]


@contextlib.contextmanager
def fake_home():
    home = Path(tempfile.mkdtemp(prefix="pysb-home-"))
    try:
        with patch.dict(os.environ, {"HOME": str(home)}):
            yield home
    finally:
        shutil.rmtree(home, ignore_errors=True)


@contextlib.contextmanager
def workspace():
    ws = Path(tempfile.mkdtemp(prefix="pysb-ws-"))
    try:
        yield ws
    finally:
        shutil.rmtree(ws, ignore_errors=True)


@contextlib.contextmanager
def recorded_host_exec_ack():
    """A valid one-time acknowledgment in the (isolated) shared config + the env request."""
    cfg_dir = sandbox._makewand_config_dir()
    cfg_dir.mkdir(parents=True, exist_ok=True)
    cfg_path = cfg_dir / "config.json"
    old = cfg_path.read_text() if cfg_path.exists() else None
    cfg_path.write_text(json.dumps({
        sandbox.ACK_VERSION_KEY: sandbox.UNSAFE_HOST_EXEC_ACK_CURRENT_VERSION,
        sandbox.ACK_AT_KEY: "2026-10-12T00:00:00Z",
        sandbox.ACK_HOST_KEY: socket.gethostname(),
    }))
    audit_path = sandbox.unsafe_host_exec_audit_path()
    if audit_path.exists():
        audit_path.unlink()
    try:
        with patch.dict(os.environ, {sandbox.UNSAFE_HOST_EXEC_ENV: "1"}):
            yield audit_path
    finally:
        if old is None:
            cfg_path.unlink()
        else:
            cfg_path.write_text(old)


# ---------------------------------------------------------------------------
# H1: unsandboxed launches need authorization, read-only included
# ---------------------------------------------------------------------------
def _run_provider(name, ws):
    """Calls the provider entry point read-only; returns (result, run_subprocess mock)."""
    common = [
        patch("makewand.config.has_subscription_configured", return_value=True),
        patch("makewand.config.has_api_configured", return_value=False),
        patch("makewand.health.load_status_cache", return_value={}),
        # aider imports run_subprocess at call time from providers.base
        patch("makewand.providers.base.run_subprocess" if name == "aider"
              else f"makewand.providers.{name}.run_subprocess", return_value=(0, "ok", "", None)),
    ]
    if name == "aider":
        common += [
            patch("makewand.providers.aider.is_provider_enabled", return_value=True),
            patch("makewand.providers.aider.is_aider_available", return_value=True),
            patch("makewand.providers.aider.get_api_config", return_value={}),
        ]
    if name == "muse":
        common += [
            patch("makewand.providers.muse.detect_muse_guard", return_value=(False, None)),
            patch("makewand.providers.muse.get_muse_executable", return_value=("muse", [])),
        ]
    with contextlib.ExitStack() as stack:
        mocks = [stack.enter_context(p) for p in common]
        run_mock = mocks[3]
        if name == "claude":
            from makewand.providers.claude import execute_claude_task as fn
        elif name == "codex":
            from makewand.providers.codex import execute_codex_task as fn
        elif name == "grok":
            from makewand.providers.grok import execute_grok_task as fn
        elif name == "agy":
            from makewand.providers.agy import execute_agy_task as fn
        elif name == "aider":
            from makewand.providers.aider import execute_aider_task as fn
        else:
            from makewand.providers.muse import execute_muse_task as fn
        result = fn("review the code", cwd=str(ws), readonly=True)
    return result, run_mock


PROVIDERS = ("claude", "codex", "grok", "agy", "aider", "muse")
PROVIDER_BIN = {"claude": "claude", "codex": "codex", "grok": "grok", "agy": "agy", "aider": "aider", "muse": "muse"}


class TestH1UnsandboxedLaunchAuthorization(unittest.TestCase):
    def setUp(self):
        sandbox._host_exec_session.update({"warned": False, "declined": False})

    def test_readonly_launch_without_bwrap_or_authorization_is_refused(self):
        for name in PROVIDERS:
            with self.subTest(provider=name), workspace() as ws, \
                    patch.dict(os.environ, {}, clear=False), \
                    patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                    patch("makewand.sandbox._agy_host_credits_disabled", return_value=True), \
                    patch("makewand.sandbox.audit_unsafe_host_exec") as audit:
                os.environ.pop(sandbox.UNSAFE_HOST_EXEC_ENV, None)
                (ok, out, err), run_mock = _run_provider(name, ws)
                self.assertFalse(ok, f"{name} ran on the host without authorization")
                self.assertIsNone(out)
                self.assertIn("沙箱", err)
                run_mock.assert_not_called()
                audit.assert_not_called()

    def test_env_request_without_recorded_ack_is_refused(self):
        # MAKEWAND_UNSAFE_HOST_EXEC=1 alone (non-interactive, no ack) never authorizes
        for name in PROVIDERS:
            with self.subTest(provider=name), workspace() as ws, \
                    patch.dict(os.environ, {sandbox.UNSAFE_HOST_EXEC_ENV: "1"}), \
                    patch("makewand.sandbox._load_shared_config", return_value=({}, None)), \
                    patch("makewand.sandbox._stdio_interactive", return_value=False), \
                    patch("makewand.sandbox.is_bwrap_available", return_value=False):
                (ok, _, _), run_mock = _run_provider(name, ws)
                self.assertFalse(ok)
                run_mock.assert_not_called()

    def test_authorized_readonly_launch_runs_and_is_audited(self):
        for name in PROVIDERS:
            with self.subTest(provider=name), workspace() as ws, recorded_host_exec_ack() as audit_path, \
                    patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                    patch("makewand.sandbox._agy_host_credits_disabled", return_value=True), \
                    patch("makewand.sandbox.audit_unsafe_host_exec", wraps=sandbox.audit_unsafe_host_exec) as audit:
                (ok, out, err), run_mock = _run_provider(name, ws)
                self.assertTrue(ok, err)
                run_mock.assert_called_once()
                cmd = list(run_mock.call_args[0][0])
                # host launch carries the provider-worker marker in its environment
                self.assertEqual(os.path.basename(cmd[0]), PROVIDER_BIN[name])
                child_env = run_mock.call_args[1]["extra_env"]
                self.assertEqual(child_env[PROVIDER_WORKER_ENV], "1")
                if name in ("codex", "claude"):
                    variable = "CODEX_HOME" if name == "codex" else "CLAUDE_CONFIG_DIR"
                    self.assertNotIn(variable, child_env)
                audit.assert_called_once()
                context, audited_cmd, _cwd, source = audit.call_args[0]
                self.assertEqual(context, f"provider:{name}")
                self.assertEqual(source, "config-ack")
                self.assertEqual(os.path.basename(str(audited_cmd[0])), PROVIDER_BIN[name])
                entries = [json.loads(line) for line in audit_path.read_text().splitlines()]
                self.assertEqual(len(entries), 1)
                self.assertEqual(entries[0]["context"], f"provider:{name}")
                self.assertEqual(entries[0]["source"], "config-ack")

    def test_untrusted_repo_never_runs_on_host_even_when_authorized(self):
        with workspace() as ws, patch("makewand.sandbox.audit_unsafe_host_exec") as audit:
            with self.assertRaises(SandboxUnavailableError):
                sandbox.authorize_unsandboxed_launch("claude", ["claude"], str(ws), True,
                                                     repo_trust="untrusted", auth_source="config-ack")
            audit.assert_not_called()


# ---------------------------------------------------------------------------
# H3: private provider home, credential write-back
# ---------------------------------------------------------------------------
def _make_roots(home, name):
    roots = []
    for rel in PROVIDER_PROFILES[name]["roots"]:
        root = home / rel
        root.mkdir(parents=True, exist_ok=True)
        roots.append(root)
    for root_rel, rel in PROVIDER_PROFILES[name].get("credentials", []):
        p = home / root_rel / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text('{"access_token": "host", "refresh_token": "r"}')
        os.chmod(p, 0o600)
    for root_rel, sub, kind, _ in PROVIDER_PROFILES[name].get("protected", [])[:2]:
        p = home / root_rel / sub
        if kind == "dir":
            p.mkdir(parents=True, exist_ok=True)
        else:
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text("x\n")
    return roots


class TestH3HostStateNeverWritable(unittest.TestCase):
    def test_host_provider_dir_never_a_writable_bind_source(self):
        for name in PROVIDER_PROFILES:
            for readonly in (True, False):
                with self.subTest(provider=name, readonly=readonly), fake_home() as home, workspace() as ws, \
                        patch("makewand.sandbox.resolve_trusted_bwrap", return_value=FAKE_BWRAP):
                    os.environ.pop("CODEX_HOME", None)
                    roots = [os.path.realpath(r) for r in _make_roots(home, name)]
                    session_home = ProviderHome(name)
                    try:
                        cmd = wrap_bwrap([name, "x"], workspace=str(ws), readonly=readonly,
                                         is_provider=True, provider_name=name, provider_home=session_home)
                        binds = _pairs(cmd, "--bind")
                        for root in roots:
                            for src, _dst in binds:
                                self.assertFalse(src == root or src.startswith(root + os.sep), (src, root))
                            # the private 0700 per-run directory is bound at the expected path
                            srcs = [src for src, dst in binds if dst == root]
                            self.assertEqual(len(srcs), 1, binds)
                            self.assertTrue(srcs[0].startswith(session_home.private_dir + os.sep))
                        self.assertEqual(stat.S_IMODE(os.stat(session_home.private_dir).st_mode), 0o700)
                        # protected entries that exist on the host are read-only binds
                        for root_rel, sub, _k, _p in PROVIDER_PROFILES[name].get("protected", [])[:2]:
                            p = str(home / root_rel / sub)
                            self.assertIn((p, p), _pairs(cmd, "--ro-bind"))
                    finally:
                        session_home.finalize(write_back=False)
                    self.assertFalse(os.path.exists(session_home.private_dir or ""))


class TestH3CredentialWriteBack(unittest.TestCase):
    def _home(self, home, provider="codex"):
        os.environ.pop("CODEX_HOME", None)
        _make_roots(home, provider)
        h = ProviderHome(provider, str(home))
        h.materialize([])
        return h

    def _private(self, h, rel):
        cred = next(c for c in h.credentials if c.rel == rel)
        return Path(cred.private_root, rel), Path(cred.host_path)

    def test_changed_allowlisted_credential_is_written_back(self):
        with fake_home() as home:
            h = self._home(home)
            priv, host = self._private(h, "auth.json")
            self.assertEqual(priv.read_text(), '{"access_token": "host", "refresh_token": "r"}')
            self.assertEqual(stat.S_IMODE(priv.stat().st_mode), 0o600)
            priv.write_text('{"access_token": "refreshed", "refresh_token": "r"}')
            h.finalize()
            self.assertEqual(host.read_text(), '{"access_token": "refreshed", "refresh_token": "r"}')
            self.assertFalse(host.is_symlink())
            self.assertEqual(stat.S_IMODE(host.stat().st_mode), 0o600)

    def test_non_allowlisted_files_never_reach_the_host(self):
        with fake_home() as home:
            h = self._home(home)
            priv_root = Path(h.credentials[0].private_root)
            (priv_root / "AGENTS.override.md").write_text("PWNED")
            (priv_root / "hooks").mkdir()
            (priv_root / "hooks" / "h.sh").write_text("evil")
            (priv_root / "sessions").mkdir()
            (priv_root / "sessions" / "s.jsonl").write_text("s")
            h.finalize()
            for rel in ("AGENTS.override.md", "hooks", "sessions"):
                self.assertFalse((home / ".codex" / rel).exists(), rel)
            self.assertFalse(priv_root.exists())

    # The symlink targets below hold valid refreshes the merge would accept, so
    # only refusing the symlink keeps the host copy.
    def test_symlinked_private_credential_is_not_followed(self):
        with fake_home() as home:
            secret = home / "host-secret"
            secret.write_text('{"access_token": "followed", "refresh_token": "r"}')
            h = self._home(home)
            priv, host = self._private(h, "auth.json")
            priv.unlink()
            priv.symlink_to(secret)
            h.finalize()
            self.assertEqual(host.read_text(), '{"access_token": "host", "refresh_token": "r"}')
            self.assertFalse(host.is_symlink())
            self.assertEqual(secret.read_text(), '{"access_token": "followed", "refresh_token": "r"}')

    def test_symlinked_private_parent_directory_is_not_followed(self):
        with fake_home() as home:
            outside = home / "outside"
            outside.mkdir()
            (outside / "antigravity-oauth-token").write_text('{"access_token": "followed", "refresh_token": "r"}')
            h = self._home(home, "agy")
            priv, host = self._private(h, "antigravity-cli/antigravity-oauth-token")
            shutil.rmtree(priv.parent)
            priv.parent.symlink_to(outside, target_is_directory=True)
            h.finalize()
            self.assertEqual(host.read_text(), '{"access_token": "host", "refresh_token": "r"}')

    def test_non_regular_private_credential_is_skipped(self):
        with fake_home() as home:
            h = self._home(home)
            priv, host = self._private(h, "auth.json")
            priv.unlink()
            priv.mkdir()
            h.finalize()
            self.assertEqual(host.read_text(), '{"access_token": "host", "refresh_token": "r"}')

    def test_write_back_skipped_when_host_copy_changed_concurrently(self):
        with fake_home() as home:
            h = self._home(home)
            priv, host = self._private(h, "auth.json")
            priv.write_text('{"access_token": "sandbox", "refresh_token": "r"}')
            host.write_text('{"access_token": "other-session", "refresh_token": "r"}')
            h.finalize()
            self.assertEqual(host.read_text(), '{"access_token": "other-session", "refresh_token": "r"}')

    def test_unchanged_credential_is_not_rewritten(self):
        with fake_home() as home:
            h = self._home(home)
            _priv, host = self._private(h, "auth.json")
            with patch("makewand.sandbox._atomic_write_host") as writer:
                h.finalize()
            writer.assert_not_called()
            self.assertEqual(host.read_text(), '{"access_token": "host", "refresh_token": "r"}')


# ---------------------------------------------------------------------------
# H5: sockets below node_modules / .git internals
# ---------------------------------------------------------------------------
class TestH5SocketMasking(unittest.TestCase):
    def test_sockets_under_node_modules_and_git_are_masked(self):
        with workspace() as ws, patch("makewand.sandbox.resolve_trusted_bwrap", return_value=FAKE_BWRAP):
            nm = ws / "node_modules" / "pkg"
            nm.mkdir(parents=True)
            gitdir = ws / ".git"
            gitdir.mkdir()
            socks = []
            try:
                paths = [nm / "d.sock", gitdir / "fsmonitor--daemon.ipc"]
                for p in paths:
                    s = socket.socket(socket.AF_UNIX)
                    s.bind(str(p))
                    socks.append(s)
                for readonly in (False, True):
                    cmd = wrap_bwrap(["true"], workspace=str(ws), readonly=readonly)
                    masked = {dst for src, dst in _pairs(cmd, "--ro-bind") if src == "/dev/null"}
                    for p in paths:
                        self.assertIn(str(p), masked, (readonly, p))
            finally:
                for s in socks:
                    s.close()


# ---------------------------------------------------------------------------
# seccomp, resource limits, launcher trust
# ---------------------------------------------------------------------------
class TestSeccompRequired(unittest.TestCase):
    def setUp(self):
        os.environ.pop(sandbox.SECCOMP_OPT_OUT_ENV, None)
        os.environ.pop(sandbox.SECCOMP_REQUIRE_ENV, None)

    def test_filter_failure_is_fatal_by_default(self):
        with patch("makewand.sandbox.generate_seccomp_bpf_filter", return_value=None):
            with self.assertRaises(SandboxConfigError):
                with seccomp_filter_context(enabled=True):
                    self.fail("must not yield without a filter")

    def test_opt_out_skips_filter(self):
        with patch.dict(os.environ, {sandbox.SECCOMP_OPT_OUT_ENV: "1"}), \
                patch("makewand.sandbox.generate_seccomp_bpf_filter", return_value=None):
            with seccomp_filter_context(enabled=True) as (fd, pass_fds):
                self.assertIsNone(fd)
                self.assertEqual(pass_fds, ())

    def test_require_overrides_opt_out(self):
        with patch.dict(os.environ, {sandbox.SECCOMP_OPT_OUT_ENV: "1", sandbox.SECCOMP_REQUIRE_ENV: "1"}), \
                patch("makewand.sandbox.generate_seccomp_bpf_filter", return_value=None):
            with self.assertRaises(SandboxConfigError):
                with seccomp_filter_context(enabled=True):
                    pass

    def _launch(self, ws):
        return provider_sandbox_launch("codex", ["codex", "exec", "x"], str(ws), readonly=True)

    def test_provider_launch_always_loads_seccomp(self):
        with fake_home(), workspace() as ws, \
                patch("makewand.sandbox.is_bwrap_available", return_value=True), \
                patch("makewand.sandbox.resolve_trusted_bwrap", return_value=FAKE_BWRAP), \
                patch("makewand.sandbox.sandbox_resource_limit_prefix", return_value=[]), \
                patch("makewand.sandbox.generate_seccomp_bpf_filter", return_value=b"\0" * 8), \
                patch("makewand.sandbox._provider_socket_preflight"):
            # This is a mount/descriptor plan test with deliberately invalid
            # filter bytes; the real socket/filter lifecycle is tested separately.
            with self._launch(ws) as launch:
                self.assertTrue(launch.sandboxed)
                fd = launch.cmd[launch.cmd.index("--seccomp") + 1]
                self.assertEqual(launch.pass_fds, (int(fd),))

    def test_provider_launch_fails_closed_without_filter_and_honours_opt_out(self):
        with fake_home(), workspace() as ws, \
                patch("makewand.sandbox.is_bwrap_available", return_value=True), \
                patch("makewand.sandbox.resolve_trusted_bwrap", return_value=FAKE_BWRAP), \
                patch("makewand.sandbox.sandbox_resource_limit_prefix", return_value=[]), \
                patch("makewand.sandbox._provider_socket_preflight"), \
                patch("makewand.sandbox.generate_seccomp_bpf_filter", return_value=None):
            with self.assertRaises(SandboxConfigError):
                with self._launch(ws):
                    self.fail("launched without seccomp")
            with patch.dict(os.environ, {sandbox.SECCOMP_OPT_OUT_ENV: "1"}):
                with self._launch(ws) as launch:
                    self.assertNotIn("--seccomp", launch.cmd)
                    self.assertEqual(launch.pass_fds, ())


class TestResourceLimitPrefix(unittest.TestCase):
    def setUp(self):
        os.environ.pop(sandbox.SANDBOX_MEMORY_MAX_ENV, None)
        os.environ.pop(sandbox.SANDBOX_TASKS_MAX_ENV, None)

    def _binary(self, available):
        return lambda name: f"/usr/bin/{name}" if name in available else None

    def test_systemd_scope_prefix(self):
        with patch("makewand.sandbox._trusted_system_binary", side_effect=self._binary({"systemd-run", "prlimit"})), \
                patch("makewand.sandbox._systemd_scope_usable", return_value=True):
            prefix = sandbox.sandbox_resource_limit_prefix()
        self.assertEqual(prefix[0], "/usr/bin/systemd-run")
        self.assertEqual(prefix[-1], "--")
        for prop in ("MemoryMax=8G", "MemorySwapMax=0", "TasksMax=512"):
            self.assertIn(prop, prefix)
        self.assertFalse(any(a.startswith("--as") for a in prefix))

    def test_prlimit_fallback_never_uses_as(self):
        with patch("makewand.sandbox._trusted_system_binary", side_effect=self._binary({"prlimit"})):
            prefix = sandbox.sandbox_resource_limit_prefix()
        self.assertEqual(prefix[0], "/usr/bin/prlimit")
        self.assertEqual(prefix[-1], "--")
        # RLIMIT_NPROC counts every thread of the uid; a relative budget broke
        # bwrap ("Creating new namespace failed") on busy hosts, so it is never set.
        self.assertFalse(any(a.startswith("--nproc") for a in prefix), prefix)
        self.assertTrue(any(a.startswith("--nofile=") for a in prefix))
        self.assertTrue(any(a.startswith("--fsize=") for a in prefix))
        self.assertFalse(any(a.startswith("--as") for a in prefix), prefix)

    @unittest.skipUnless(hasattr(os, "geteuid") and os.geteuid() != 0, "POSIX non-root permissions")
    def test_unreadable_dirs_scan_rule(self):
        # own unreadable dir: could be chmod-ed back from inside -> incomplete;
        # another user's untraversable dir: unreachable from inside -> skipped.
        with tempfile.TemporaryDirectory() as ws:
            locked = os.path.join(ws, "locked")
            os.mkdir(locked, 0o000)
            try:
                _, _, truncated = sandbox._scan_tree([ws], max_depth=None, entry_limit=1000, collect_git=True)
                self.assertTrue(truncated)
                with patch("makewand.sandbox._untraversable_foreign_dir", return_value=True):
                    _, _, truncated = sandbox._scan_tree([ws], max_depth=None, entry_limit=1000, collect_git=True)
                self.assertFalse(truncated)
            finally:
                os.chmod(locked, 0o700)

    def test_memory_limit_values_follow_the_go_contract(self):
        # 0/off/none mean no cap; 8g and 8GiB are accepted (internal/processjob).
        for value, want in (("0", "MemoryMax=infinity"), ("off", "MemoryMax=infinity"),
                            ("8g", "MemoryMax=8G"), ("8GiB", "MemoryMax=8G"), ("512M", "MemoryMax=512M")):
            with self.subTest(value=value), patch.dict(os.environ, {sandbox.SANDBOX_MEMORY_MAX_ENV: value}), \
                    patch("makewand.sandbox._trusted_system_binary", side_effect=self._binary({"systemd-run"})), \
                    patch("makewand.sandbox._systemd_scope_usable", return_value=True):
                self.assertIn(want, sandbox.sandbox_resource_limit_prefix())

    def test_invalid_limit_values_cannot_inject_properties(self):
        with patch.dict(os.environ, {sandbox.SANDBOX_MEMORY_MAX_ENV: "1G -p Delegate=yes"}), \
                patch("makewand.sandbox._trusted_system_binary", side_effect=self._binary({"systemd-run"})), \
                patch("makewand.sandbox._systemd_scope_usable", return_value=True):
            prefix = sandbox.sandbox_resource_limit_prefix()
        self.assertIn("MemoryMax=8G", prefix)
        self.assertFalse(any("Delegate" in a for a in prefix))

    def test_no_launcher_returns_empty_prefix(self):
        with patch("makewand.sandbox._trusted_system_binary", return_value=None):
            self.assertEqual(sandbox.sandbox_resource_limit_prefix(), [])


class TestOverflowUidLauncherTrust(unittest.TestCase):
    OVERFLOW = 65534

    def _check(self, uid, mode, in_userns, allow_current_user=False):
        fake = SimpleNamespace(st_uid=uid, st_mode=stat.S_IFREG | mode)
        with patch("makewand.sandbox._in_user_namespace", return_value=in_userns), \
                patch("makewand.sandbox._overflow_uid", return_value=self.OVERFLOW), \
                patch("makewand.sandbox.os.stat", return_value=fake):
            return sandbox._trusted_launcher_ok("/usr/bin/bwrap", allow_current_user)

    def test_overflow_uid_accepted_only_inside_user_namespace(self):
        if os.geteuid() == self.OVERFLOW:
            self.skipTest("running as the overflow uid")
        self.assertTrue(self._check(self.OVERFLOW, 0o755, in_userns=True))
        self.assertFalse(self._check(self.OVERFLOW, 0o755, in_userns=False))

    def test_overflow_uid_still_rejects_writable_modes(self):
        self.assertFalse(self._check(self.OVERFLOW, 0o775, in_userns=True))
        self.assertFalse(self._check(self.OVERFLOW, 0o757, in_userns=True))

    def test_root_and_current_user_rules(self):
        self.assertTrue(self._check(0, 0o755, in_userns=False))
        self.assertFalse(self._check(0, 0o777, in_userns=False))
        if os.geteuid() != 0:
            self.assertFalse(self._check(os.geteuid(), 0o755, in_userns=False, allow_current_user=False))
        self.assertTrue(self._check(os.geteuid(), 0o755, in_userns=False, allow_current_user=True))


# ---------------------------------------------------------------------------
# MAKEWAND_PROVIDER_WORKER marker
# ---------------------------------------------------------------------------
class TestProviderWorkerMarker(unittest.TestCase):
    def setUp(self):
        sandbox._host_exec_session.update({"warned": False, "declined": False})

    def _bwrap_env(self):
        return [
            patch("makewand.sandbox.is_bwrap_available", return_value=True),
            patch("makewand.sandbox.resolve_trusted_bwrap", return_value=FAKE_BWRAP),
            patch("makewand.sandbox.sandbox_resource_limit_prefix", return_value=[]),
            patch("makewand.sandbox._provider_socket_preflight"),
        ]

    def test_provider_bwrap_launch_sets_marker(self):
        with fake_home(), workspace() as ws, contextlib.ExitStack() as st:
            for p in self._bwrap_env():
                st.enter_context(p)
            with provider_sandbox_launch("claude", ["claude", "-p", "x"], str(ws), readonly=True) as launch:
                i = launch.cmd.index(PROVIDER_WORKER_ENV)
                self.assertEqual(launch.cmd[i - 1:i + 2], ["--setenv", PROVIDER_WORKER_ENV, "1"])

    def test_provider_host_launch_sets_marker(self):
        with workspace() as ws, patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                patch("makewand.sandbox._agy_host_credits_disabled", return_value=True), \
                patch("makewand.sandbox.audit_unsafe_host_exec") as audit:
            with provider_sandbox_launch("codex", ["codex", "exec", "x"], str(ws), readonly=True,
                                         host_auth_source="config-ack") as launch:
                self.assertFalse(launch.sandboxed)
                # no `env` prefix: it does not exist on Windows
                self.assertEqual(launch.cmd, ["codex", "exec", "x"])
                self.assertEqual(launch.env[PROVIDER_WORKER_ENV], "1")
                self.assertNotIn("CODEX_HOME", launch.env)
                self.assertIsNone(launch.home)
            audit.assert_called_once()

    def test_non_provider_run_in_sandbox_has_no_marker(self):
        with workspace() as ws, contextlib.ExitStack() as st:
            for p in self._bwrap_env():
                st.enter_context(p)
            run = st.enter_context(patch("makewand.sandbox.run_subprocess", return_value=(0, "", "", None)))
            run_in_sandbox(["python3", "-c", "pass"], workspace=str(ws))
            cmd = list(run.call_args[0][0])
            self.assertIn(FAKE_BWRAP, cmd)
            self.assertNotIn(PROVIDER_WORKER_ENV, cmd)
            self.assertFalse(any(PROVIDER_WORKER_ENV in str(a) for a in cmd))

    def test_non_provider_host_run_has_no_marker(self):
        with workspace() as ws, patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                patch("makewand.sandbox._agy_host_credits_disabled", return_value=True), \
                patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(True, "config-ack")), \
                patch("makewand.sandbox.audit_unsafe_host_exec") as audit, \
                patch("makewand.sandbox.run_subprocess", return_value=(0, "", "", None)) as run:
            run_in_sandbox(["python3", "-c", "pass"], workspace=str(ws))
            cmd = list(run.call_args[0][0])
            self.assertFalse(any(PROVIDER_WORKER_ENV in str(a) for a in cmd))
            audit.assert_called_once()

    def test_non_provider_host_run_without_authorization_is_blocked(self):
        with workspace() as ws, patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                patch("makewand.sandbox._agy_host_credits_disabled", return_value=True), \
                patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(False, None)), \
                patch("makewand.sandbox.run_subprocess") as run:
            code, _, _, cat = run_in_sandbox(["python3", "-c", "pass"], workspace=str(ws), readonly=True)
            self.assertEqual(code, -1)
            self.assertEqual(cat, "SandboxUnavailable")
            run.assert_not_called()


if __name__ == "__main__":
    unittest.main()
