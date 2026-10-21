"""Hermetic state isolation shared by every Python test entry point.

``tests/conftest.py`` (pytest), ``scripts/test_python.py`` (the official gate)
and each test module (for direct ``python3 -m unittest ...`` runs) import this
module before anything from ``makewand`` is imported.  Importing it activates
the isolation once per process:

* ``HOME``, ``XDG_*``, ``MAKEWAND_CONFIG_DIR``, ``MAKEWAND_USAGE_FILE``,
  ``MAKEWAND_ARTIFACTS_DIR``, ``MAKEWAND_SHADOW_DIR`` and
  ``MAKEWAND_EXECUTION_EVIDENCE_DIR`` point into a private temporary root that
  is removed when the process exits (provider evidence would otherwise default
  to the user cache, which on Windows is ``LocalAppData``, not under ``HOME``);
* the path constants of ``makewand.config`` (and copies of them already bound in
  other loaded ``makewand`` modules) are rewritten into that root;
* provider credentials and policy switches (``*_API_KEY``, ``*_AUTH_TOKEN``,
  ``MAKEWAND_*`` such as ``MAKEWAND_API_POLICY``/``MAKEWAND_ENABLE_*``/
  ``MAKEWAND_DISABLE_*``, local model settings) are removed from the environment;
* a stub directory is prepended to ``PATH``: every AI CLI stub records its argv
  and exits 127, so a test can never start a real, quota-consuming agent;
* proxy settings (``HTTP_PROXY``, ``HTTPS_PROXY``, ``ALL_PROXY``, ``NO_PROXY``
  and their lower-case forms) are removed: a loopback proxy would carry a
  request on to the real endpoint;
* the local model endpoint points to an unreachable port, and in-process name
  resolution of non-local hosts and connections to the Ollama port, to any
  non-loopback address or to a loopback port that no socket of this process
  has bound (a test's own fake server) fail with ``IsolationNetworkError``,
  which is logged, echoed to stderr and summarised at the end of a pytest run.

The real ``~/.config/makewand``, ``~/.gemini`` and fixed ``/tmp/makewand-*``
paths are therefore never touched by tests, whichever entry point is used.
"""

from __future__ import annotations

import atexit
import errno
import ipaddress
import os
import shutil
import signal
import socket
import stat
import sys
import tempfile
import weakref
from pathlib import Path
from types import SimpleNamespace
from typing import Dict, Iterable, List, Optional, Tuple

REPO_ROOT = Path(__file__).resolve().parent.parent

ROOT_ENV = "MAKEWAND_TEST_ISOLATION_ROOT"
# Optional: append stub invocations to this file as well (kept after exit, for audits).
STUB_LOG_ENV = "MAKEWAND_TEST_STUB_LOG"
_KEEP_MAKEWAND_ENV = {ROOT_ENV, STUB_LOG_ENV}

# Every agent CLI Makewand can start, plus the helpers probed for Copilot/Cursor.
STUBBED_CLIS = ("claude", "codex", "agy", "gemini", "grok", "muse", "aider", "ollama",
                "cursor", "copilot", "gh")
STUB_EXIT_CODE = 127

# Discard port on loopback: refused immediately, never an Ollama daemon.
UNREACHABLE_LOCAL_ENDPOINT = "http://127.0.0.1:9/v1"
BLOCKED_PORTS = frozenset({11434})
LEGACY_ARTIFACTS_ROOT = Path("/tmp/makewand-artifacts")

_SCRUB_SUFFIXES = ("_API_KEY", "_AUTH_TOKEN", "_BASE_URL", "_MODEL")
_SCRUB_PREFIXES = ("MAKEWAND_", "LOCAL_MODEL_", "OLLAMA_", "CODEX_")
# Proxy settings, compared upper-case: urllib and the provider CLIs honour both
# cases. Every other "*_PROXY" variable (NODE_USE_ENV_PROXY, npm_config_proxy's
# https twin, PIP_PROXY, GLOBAL_AGENT_HTTP_PROXY) goes too.
PROXY_ENV_VARS = ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "FTP_PROXY", "SOCKS_PROXY")
_LOCAL_HOST_NAMES = ("localhost", "ip6-localhost", "ip6-loopback", "localhost.localdomain")
# Repository redirection variables (set e.g. inside git hooks) would make the
# fixture repositories created by tests operate on the caller's repository.
_SCRUB_GIT = ("GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
              "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE",
              "GIT_PREFIX", "GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_PARAMETERS",
              "GIT_CONFIG_COUNT")

# makewand.config path constants redirected into the isolation root.
CONFIG_PATH_NAMES = ("CONFIG_DIR", "CONFIG_FILE", "API_KEYS_FILE", "STATUS_CACHE_FILE",
                     "CANDIDATES_DIR", "BACKUPS_DIR", "LEGACY_TRIO_CACHE",
                     "ARTIFACTS_DIR", "SHADOW_WORKTREES_DIR")

_STATE: Optional[SimpleNamespace] = None


def _inside(path, root) -> bool:
    try:
        Path(os.path.abspath(path)).relative_to(Path(os.path.abspath(root)))
        return True
    except ValueError:
        return False


def _scrub_proxies() -> List[str]:
    removed = []
    for key in list(os.environ):
        if key.upper() in PROXY_ENV_VARS or key.upper().endswith("_PROXY"):
            os.environ.pop(key, None)
            removed.append(key)
    return removed


def _scrub_environment() -> List[str]:
    removed = _scrub_proxies()
    for key in list(os.environ):
        if key in _KEEP_MAKEWAND_ENV:
            continue
        if (key.endswith(_SCRUB_SUFFIXES) or key.startswith(_SCRUB_PREFIXES)
                or key in _SCRUB_GIT or key.startswith(("GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"))):
            os.environ.pop(key, None)
            removed.append(key)
    return removed


def _write_stubs(bin_dir: Path, log_file: Path) -> None:
    bin_dir.mkdir(parents=True, exist_ok=True)
    for name in STUBBED_CLIS:
        if os.name == "nt":
            # PATHEXT lookup must find our fixture before any installed real
            # agent. Never expand caller argv in cmd.exe: shell metacharacters
            # in a test prompt must remain inert. Windows logs the CLI name.
            script = bin_dir / (name + ".cmd")
            script.write_text(
                "@echo off\n"
                f'>>"{log_file}" echo {name}\n'
                f"echo makewand test stub: {name} is disabled during tests 1>&2\n"
                f"exit /b {STUB_EXIT_CODE}\n",
                encoding="utf-8",
            )
            continue
        script = bin_dir / name
        script.write_text(
            "#!/bin/sh\n"
            "# makewand test isolation: real AI CLIs never run during tests.\n"
            f"for log in '{log_file}' \"${{{STUB_LOG_ENV}:-}}\"; do\n"
            "  [ -n \"$log\" ] || continue\n"
            f"  {{ printf '%s' '{name}'; for arg in \"$@\"; do printf ' %s' \"$arg\"; done; printf '\\n'; }} >> \"$log\" 2>/dev/null\n"
            "done\n"
            f"echo 'makewand test stub: {name} is disabled during tests' >&2\n"
            f"exit {STUB_EXIT_CODE}\n",
            encoding="utf-8",
        )
        script.chmod(0o755)


class IsolationNetworkError(ConnectionRefusedError):
    """A connection the test isolation refused: tests never reach the network."""


class IsolationResolutionError(socket.gaierror):
    """A host name the test isolation refused to resolve: tests never reach the network."""


# Sockets of this process bound to an inet port (a test's own fake servers), with their port.
_BOUND_SOCKETS: "weakref.WeakKeyDictionary" = weakref.WeakKeyDictionary()


def _owned_ports() -> set:
    return {port for sock, port in list(_BOUND_SOCKETS.items()) if sock.fileno() != -1}


def _is_local_name(host: str) -> bool:
    name = host.lower().rstrip(".")
    return name in _LOCAL_HOST_NAMES or name.endswith(".localhost")


def _address_block_reason(family, address) -> Optional[str]:
    if family not in (socket.AF_INET, getattr(socket, "AF_INET6", None)):
        return None
    if not isinstance(address, tuple) or len(address) < 2:
        return None
    host, port = str(address[0]), address[1]
    if port in BLOCKED_PORTS:
        return f"local model port {port}"
    if not _is_local_name(host):
        try:
            ip = ipaddress.ip_address(host.split("%", 1)[0])
        except ValueError:
            return f"unresolved host {host!r}"
        if not ip.is_loopback:
            return f"non-loopback address {host}"
    if port not in _owned_ports():
        # A loopback proxy (this host exports one) would carry the request on.
        return f"loopback port {port} that no socket of this test process has bound"
    return None


def _resolution_block_reason(host) -> Optional[str]:
    if host is None:
        return None
    name = host.decode("ascii", "replace") if isinstance(host, (bytes, bytearray)) else str(host)
    if not name or _is_local_name(name) or name.lower() == socket.gethostname().lower():
        return None
    try:
        ipaddress.ip_address(name.split("%", 1)[0])
        return None  # an address literal: connect() decides
    except ValueError:
        return f"name resolution of non-local host {name!r}"


def _install_network_guard(log_file: Path) -> None:
    """Refuse in-process name resolution and connections that could leave the test.

    Allowed: loopback ports bound by a socket of this process (a test's own
    server). Refused, logged and echoed to stderr: the Ollama port, every other
    loopback port (a local proxy), non-loopback addresses and non-local names.
    """
    if getattr(socket.socket, "_makewand_isolation_guard", False):
        return
    real_connect = socket.socket.connect
    real_connect_ex = socket.socket.connect_ex
    real_bind = socket.socket.bind
    real_getaddrinfo = socket.getaddrinfo

    def _refuse(target, reason):
        try:
            with open(log_file, "a", encoding="utf-8") as handle:
                handle.write(f"{target!r} {reason}\n")
        except OSError:
            pass
        stream = sys.__stderr__
        if stream is not None:
            try:
                stream.write(f"makewand test isolation: blocked {target!r}: {reason}\n")
                stream.flush()
            except (OSError, ValueError):
                pass

    def bind(sock, address):
        result = real_bind(sock, address)
        if sock.family in (socket.AF_INET, getattr(socket, "AF_INET6", None)):
            try:
                _BOUND_SOCKETS[sock] = sock.getsockname()[1]
            except (OSError, TypeError, IndexError):
                pass
        return result

    def connect(sock, address):
        reason = _address_block_reason(sock.family, address)
        if reason:
            _refuse(address, reason)
            raise IsolationNetworkError(errno.ECONNREFUSED, f"blocked by makewand test isolation: {reason}")
        return real_connect(sock, address)

    def connect_ex(sock, address):
        reason = _address_block_reason(sock.family, address)
        if reason:
            _refuse(address, reason)
            return errno.ECONNREFUSED
        return real_connect_ex(sock, address)

    def getaddrinfo(host, *args, **kwargs):
        reason = _resolution_block_reason(host)
        if reason:
            _refuse(host, reason)
            raise IsolationResolutionError(socket.EAI_NONAME, f"blocked by makewand test isolation: {reason}")
        return real_getaddrinfo(host, *args, **kwargs)

    socket.socket.bind = bind
    socket.socket.connect = connect
    socket.socket.connect_ex = connect_ex
    socket.getaddrinfo = getaddrinfo
    socket.socket._makewand_isolation_guard = True


def rebase_path(value, mapping: Iterable[Tuple[Path, Path]]) -> Optional[Path]:
    """Move ``value`` from an old location to its new one.

    Exact matches win; otherwise the longest matching old directory prefix is used.
    """
    if not isinstance(value, Path):
        return None
    mapping = list(mapping)
    for old, new in mapping:
        if value == old:
            return new
    best = None
    for old, new in mapping:
        try:
            rel = value.relative_to(old)
        except ValueError:
            continue
        if best is None or len(old.parts) > len(best[0].parts):
            best = (old, new / rel)
    return best[1] if best else None


def rebind_module_paths(modules, mapping: List[Tuple[Path, Path]], root: Path) -> List[str]:
    """Rewrite module-level ``Path`` constants that still point at old state locations.

    Paths inside the isolation root or inside the repository (code, data files)
    are never touched.
    """
    changed = []
    for module in modules:
        for attr, value in list(vars(module).items()):
            if not isinstance(value, Path) or _inside(value, root) or _inside(value, REPO_ROOT):
                continue
            new = rebase_path(value, mapping)
            if new is not None and new != value:
                setattr(module, attr, new)
                changed.append(f"{module.__name__}.{attr}")
    return changed


def _makewand_modules():
    return [m for name, m in list(sys.modules.items())
            if m is not None and (name == "makewand" or name.startswith("makewand."))]


def _redirect_makewand(state: SimpleNamespace, previous_home: Path) -> None:
    import makewand.config as config

    old: Dict[str, Path] = {n: getattr(config, n) for n in CONFIG_PATH_NAMES if hasattr(config, n)}
    new: Dict[str, Path] = {
        "CONFIG_DIR": state.config_dir,
        "CONFIG_FILE": state.config_dir / "config.json",
        "API_KEYS_FILE": state.config_dir / "api_keys.json",
        "STATUS_CACHE_FILE": state.config_dir / "status.json",
        "CANDIDATES_DIR": state.config_dir / "candidates",
        "BACKUPS_DIR": state.config_dir / "backups",
        "LEGACY_TRIO_CACHE": state.legacy_trio_cache,
        "ARTIFACTS_DIR": state.artifacts_dir,
        "SHADOW_WORKTREES_DIR": state.shadow_dir,
    }
    mapping: List[Tuple[Path, Path]] = []
    for name, value in old.items():
        setattr(config, name, new[name])
        mapping.append((Path(value), new[name]))
    # Per-user state directories under the previous HOME (never the whole HOME:
    # the repository or site-packages may live there).
    mapping += [
        (previous_home / ".config" / "makewand", state.config_dir),
        (previous_home / ".local" / "state" / "makewand", state.state_dir),
        (previous_home / ".gemini", state.home / ".gemini"),
    ]
    rebind_module_paths(_makewand_modules(), mapping, state.root)

    # Fail closed: never run tests against state outside the isolation root.
    for name in old:
        value = getattr(config, name)
        if not _inside(value, state.root):
            raise RuntimeError(f"makewand test isolation failed: config.{name}={value}")


def _cleanup(root: Path, owner_pid: int) -> None:
    if os.getpid() != owner_pid or not root.exists():
        return

    def _retry(func, path, _exc):
        try:
            os.chmod(os.path.dirname(path), stat.S_IRWXU)
            if not os.path.islink(path):
                os.chmod(path, stat.S_IRWXU)
            func(path)
        except OSError:
            pass

    handler = {"onexc": _retry} if sys.version_info >= (3, 12) else {"onerror": _retry}
    shutil.rmtree(root, **handler)


def _install_sigterm_cleanup(root: Path, owner_pid: int) -> None:
    """Remove the isolation root when the runner is terminated (atexit does not run)."""
    try:
        if signal.getsignal(signal.SIGTERM) is not signal.SIG_DFL:
            return

        def _on_sigterm(signum, _frame):
            _cleanup(root, owner_pid)
            signal.signal(signum, signal.SIG_DFL)
            os.kill(os.getpid(), signum)

        signal.signal(signal.SIGTERM, _on_sigterm)
    except ValueError:  # not in the main thread
        pass


def activate() -> SimpleNamespace:
    """Isolate this process (idempotent); returns the isolation description."""
    global _STATE
    if _STATE is not None:
        return _STATE

    inherited = os.environ.get(ROOT_ENV)
    previous_home = Path(os.path.expanduser("~")).resolve()
    if inherited and Path(inherited).is_dir() and _inside(os.environ.get("HOME", ""), inherited):
        # Another copy of this module (e.g. ``tests._isolation``) or a parent test
        # process already isolated the environment: reuse it, never clean it up.
        root, owner = Path(inherited), False
    else:
        root, owner = Path(tempfile.mkdtemp(prefix="makewand-test-isolation-")).resolve(), True

    home = root / "home"
    state = SimpleNamespace(
        root=root,
        home=home,
        config_dir=home / ".config" / "makewand",
        state_dir=home / ".local" / "state" / "makewand",
        artifacts_dir=home / ".local" / "state" / "makewand" / "artifacts",
        shadow_dir=home / ".local" / "state" / "makewand" / "shadow-worktrees",
        legacy_trio_cache=root / "legacy" / "trio_status.json",
        usage_file=root / "usage.json",
        evidence_dir=root / "execution-evidence",
        bin_dir=root / "bin",
        stub_log=root / "stub_calls.log",
        network_log=root / "network_blocked.log",
        runtime_dir=root / "run",
        owner=owner,
        scrubbed_env=[],
    )

    try:
        import pwd
        real_user_home = Path(pwd.getpwuid(os.getuid()).pw_dir).resolve()
    except Exception:
        real_user_home = previous_home
    user_base = os.environ.get("PYTHONUSERBASE") or str(real_user_home / ".local")

    if owner:
        state.scrubbed_env = _scrub_environment()
        for path in (home, state.runtime_dir):
            path.mkdir(parents=True, exist_ok=True)
        state.runtime_dir.chmod(0o700)
        _write_stubs(state.bin_dir, state.stub_log)
        os.environ.update({
            ROOT_ENV: str(root),
            "HOME": str(home),
            "PYTHONUSERBASE": user_base,
            "XDG_CONFIG_HOME": str(home / ".config"),
            "XDG_STATE_HOME": str(home / ".local" / "state"),
            "XDG_CACHE_HOME": str(home / ".cache"),
            "XDG_DATA_HOME": str(home / ".local" / "share"),
            "XDG_RUNTIME_DIR": str(state.runtime_dir),
            "MAKEWAND_CONFIG_DIR": str(state.config_dir),
            "MAKEWAND_USAGE_FILE": str(state.usage_file),
            "MAKEWAND_EXECUTION_EVIDENCE_DIR": str(state.evidence_dir),
            "MAKEWAND_ARTIFACTS_DIR": str(state.artifacts_dir),
            "MAKEWAND_SHADOW_DIR": str(state.shadow_dir),
            "LOCAL_MODEL_ENDPOINT": UNREACHABLE_LOCAL_ENDPOINT,
            "OLLAMA_HOST": UNREACHABLE_LOCAL_ENDPOINT.rsplit("/", 1)[0],
            "PATH": str(state.bin_dir) + os.pathsep + os.environ.get("PATH", os.defpath),
        })
        atexit.register(_cleanup, root, os.getpid())
        _install_sigterm_cleanup(root, os.getpid())
    else:
        # A child started with proxy settings of its own must not inherit a way out either.
        state.scrubbed_env = _scrub_proxies()
        if "PYTHONUSERBASE" not in os.environ:
            os.environ["PYTHONUSERBASE"] = user_base
        if str(state.bin_dir) not in os.environ.get("PATH", "").split(os.pathsep):
            os.environ["PATH"] = str(state.bin_dir) + os.pathsep + os.environ.get("PATH", os.defpath)

    _install_network_guard(state.network_log)
    if str(REPO_ROOT) not in sys.path:
        sys.path.insert(0, str(REPO_ROOT))
    _STATE = state
    _redirect_makewand(state, previous_home)
    return state


def state() -> SimpleNamespace:
    return activate()


def artifacts_root() -> Path:
    """Where the orchestrator writes delivery/rejected artifacts in this tree.

    Uses ``makewand.config.ARTIFACTS_DIR`` when the code base defines it
    (private per-user state, redirected into the isolation root); older trees
    still write to the fixed ``/tmp/makewand-artifacts`` location.
    """
    import makewand.config as config
    value = getattr(config, "ARTIFACTS_DIR", None)
    return Path(value) if value else LEGACY_ARTIFACTS_ROOT


def stub_calls() -> List[str]:
    try:
        return activate().stub_log.read_text(encoding="utf-8").splitlines()
    except FileNotFoundError:
        return []


def blocked_connections() -> List[str]:
    try:
        return activate().network_log.read_text(encoding="utf-8").splitlines()
    except FileNotFoundError:
        return []


activate()
