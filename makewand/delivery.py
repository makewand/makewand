"""Standalone, read-only destination checks for exported Git deliveries.

The same standard-library implementation runs at export and inside the generated
script. Git administration is excluded from the content scan; repository marker
identities, HEAD commits and symbolic refs are bound separately. This is a
preflight/postimage check, not a lock against external editors or a transaction.
"""

import base64
import hashlib
import json
import math
import os
import stat
import subprocess
import sys
import time
from pathlib import Path


MAX_ENTRIES = 100000
MAX_BYTES = 512 * 1024 * 1024
MAX_SECONDS = 10
MAX_METADATA_BYTES = 32 * 1024 * 1024
SNAPSHOT_LIMIT_CAPS = {"max_entries": 1000000, "max_bytes": 16 * 1024 * 1024 * 1024,
                       "max_seconds": 300}


def snapshot_limits(limits=None):
    """Validate an explicit policy, or read initial user limits exactly once.

    Later checks pass the frozen policy rather than consulting the environment.
    The SDK separately supplies its remaining execution deadline.
    """
    defaults = {"max_entries": MAX_ENTRIES, "max_bytes": MAX_BYTES, "max_seconds": MAX_SECONDS}
    if limits is None:
        limits = {name: os.environ.get("MAKEWAND_DELIVERY_" + name.upper(), value)
                  for name, value in defaults.items()}
    if not isinstance(limits, dict) or set(limits) != set(defaults):
        raise ValueError("Delivery snapshot limits require max_entries, max_bytes and max_seconds")
    result = {}
    for name, value in limits.items():
        try:
            if isinstance(value, bool):
                raise ValueError
            number = float(value) if name == "max_seconds" else int(value)
            if name != "max_seconds" and str(number) != str(value):
                raise ValueError
            if not math.isfinite(number) or not 0 < number <= SNAPSHOT_LIMIT_CAPS[name]:
                raise ValueError
        except (TypeError, ValueError, OverflowError):
            raise ValueError(f"Delivery {name} must be positive and at most {SNAPSHOT_LIMIT_CAPS[name]}") from None
        result[name] = number
    return result


def _frozen_limits(frozen):
    # Older snapshots retain their original defaults, regardless of current env.
    return snapshot_limits(frozen.get("snapshot_limits", {
        "max_entries": MAX_ENTRIES, "max_bytes": MAX_BYTES, "max_seconds": MAX_SECONDS}))


class _SnapshotBudget:
    def __init__(self, limits, timeout=None):
        self.limits = limits
        self.seconds = limits["max_seconds"] if timeout is None else min(limits["max_seconds"], timeout)
        self.started = time.monotonic()
        self.entries = 0
        self.bytes = 0

    def fail(self, name, detail=""):
        elapsed = time.monotonic() - self.started
        raise OSError(f"Delivery destination snapshot exceeded {name} limit "
                      f"({self.limits[name]}; entries={self.entries}, bytes={self.bytes}, "
                      f"elapsed={elapsed:.3f}s, effective_seconds={self.seconds:g})" + detail)

    def remaining(self):
        remaining = self.seconds - (time.monotonic() - self.started)
        if remaining <= 0:
            self.fail("max_seconds")
        if self.entries > self.limits["max_entries"]:
            self.fail("max_entries")
        if self.bytes > self.limits["max_bytes"]:
            self.fail("max_bytes")
        return remaining

    def entry(self):
        self.entries += 1
        self.remaining()

    def file_size(self, size):
        if self.bytes + size > self.limits["max_bytes"]:
            self.fail("max_bytes", f"; next_file_bytes={size}")


def _entry_record(directory, name, before, relative, budget):
    """Read one payload through its pinned parent; never follow a link."""
    mode = stat.S_IMODE(before.st_mode)
    if stat.S_ISREG(before.st_mode):
        budget.file_size(before.st_size)
        descriptor = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
        try:
            if _signature(os.fstat(descriptor)) != _signature(before):
                raise OSError("Delivery file changed: " + relative)
            digest = hashlib.sha256()
            while True:
                budget.remaining()
                chunk = os.read(descriptor, 65536)
                if not chunk:
                    break
                budget.bytes += len(chunk)
                budget.remaining()
                digest.update(chunk)
            if _signature(os.fstat(descriptor)) != _signature(before):
                raise OSError("Delivery file changed while hashing: " + relative)
            return ["file", digest.hexdigest(), mode]
        finally:
            os.close(descriptor)
    if stat.S_ISDIR(before.st_mode):
        return ["dir", mode]
    if stat.S_ISLNK(before.st_mode):
        return ["link", os.readlink(name, dir_fd=directory), mode]
    return ["special", stat.S_IFMT(before.st_mode), mode, before.st_rdev]


def _signature(info):
    return (info.st_dev, info.st_ino, info.st_mode, info.st_size,
            info.st_mtime_ns, info.st_ctime_ns)


def _open_root(path):
    """Pin every real directory component, refusing symlinked ancestors."""
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    descriptor = os.open("/", flags)
    try:
        for component in path.split("/"):
            if component:
                child = os.open(component, flags, dir_fd=descriptor)
                os.close(descriptor)
                descriptor = child
        return descriptor
    except BaseException:
        os.close(descriptor)
        raise


def _git_environment():
    # A caller's Git redirection variables must never redirect these checks.
    env = {key: value for key, value in os.environ.items()
           if not key.startswith("GIT_")}
    env.update(GIT_OPTIONAL_LOCKS="0", GIT_CONFIG_GLOBAL=os.devnull,
               GIT_CONFIG_SYSTEM=os.devnull)
    return env


def _git_command(path):
    return ["git", "--no-replace-objects", "-c", "core.fsmonitor=",
            "-c", "core.hooksPath=/dev/null", "-c", "core.attributesFile=/dev/null",
            "-c", "core.autocrlf=false", "-c", "core.pager=cat",
            "-C", path, "--work-tree=" + path]


def check_delivery_patch(path, patch, timeout=MAX_SECONDS):
    result = subprocess.run(_git_command(path) + ["apply", "--check", "--binary", os.fspath(patch)],
                            env=_git_environment(), capture_output=True, timeout=timeout)
    if result.returncode:
        raise OSError("exported patch does not match the frozen destination baseline "
                      f"(exit {result.returncode}): " + os.fsdecode(result.stderr).strip())


def _git_state(path, budget):
    command = _git_command(path)
    env = _git_environment()
    def run(arguments):
        try:
            return subprocess.run(command + arguments, env=env, capture_output=True, timeout=budget())
        except subprocess.TimeoutExpired:
            budget()  # Report the effective snapshot/deadline limit and counts.
            raise
    symbolic = run(["symbolic-ref", "-q", "HEAD"])
    if symbolic.returncode not in (0, 1):
        raise OSError("Cannot inspect delivery repository HEAD: " + path)
    head = run(["rev-parse", "--verify", "HEAD"])
    if head.returncode and symbolic.returncode:
        raise OSError("Cannot inspect delivery repository commit: " + path)
    return [head.stdout.decode("ascii").strip() if head.returncode == 0 else None,
            os.fsdecode(symbolic.stdout).strip() if symbolic.returncode == 0 else None]


def capture_delivery_baseline(workspace, timeout=None, *, limits=None):
    """Freeze all destination entries, including ignored files and empty dirs."""
    if (os.name != "posix" or not hasattr(os, "O_NOFOLLOW")
            or os.open not in os.supports_dir_fd):
        raise OSError("Delivery checks require POSIX no-follow directory handles")
    root = os.path.abspath(os.fspath(workspace))
    entries, repositories = {}, {}
    limits = snapshot_limits(limits)
    budget = _SnapshotBudget(limits, timeout)

    def scan(directory, prefix):
        before_directory = os.fstat(directory)
        for name in sorted(os.listdir(directory)):
            budget.entry()
            relative = prefix + name
            before = os.stat(name, dir_fd=directory, follow_symlinks=False)
            mode = stat.S_IMODE(before.st_mode)
            if name == ".git":
                if not (stat.S_ISDIR(before.st_mode) or stat.S_ISREG(before.st_mode)):
                    raise OSError("Delivery repository marker must be a real file or directory")
                repo = prefix.rstrip("/")
                repositories[repo] = {
                    "marker": [before.st_dev, before.st_ino, stat.S_IFMT(before.st_mode)],
                    "head": _git_state(os.path.join(root, repo), budget.remaining),
                }
            elif stat.S_ISDIR(before.st_mode):
                child = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                                dir_fd=directory)
                try:
                    if _signature(os.fstat(child)) != _signature(before):
                        raise OSError("Delivery directory changed: " + relative)
                    entries[relative] = ["dir", mode]
                    scan(child, relative + "/")
                finally:
                    os.close(child)
            else:
                entries[relative] = _entry_record(directory, name, before, relative, budget)
            after = os.stat(name, dir_fd=directory, follow_symlinks=False)
            if name == ".git":
                if (after.st_dev, after.st_ino, after.st_mode) != (before.st_dev, before.st_ino, before.st_mode):
                    raise OSError("Delivery repository marker changed: " + relative)
            elif _signature(after) != _signature(before):
                raise OSError("Delivery entry changed during inspection: " + relative)
        if _signature(os.fstat(directory)) != _signature(before_directory):
            raise OSError("Delivery directory changed during inspection: " + prefix)

    descriptor = _open_root(root)
    try:
        info = os.fstat(descriptor)
        scan(descriptor, "")
        budget.remaining()
        reopened = _open_root(root)
        try:
            current = os.fstat(reopened)
            if (current.st_dev, current.st_ino) != (info.st_dev, info.st_ino):
                raise OSError("Delivery destination directory identity changed")
        finally:
            os.close(reopened)
        return {"schema": 1, "root": root,
                "root_identity": [info.st_dev, info.st_ino, stat.S_IMODE(info.st_mode)],
                "entries": entries, "repositories": repositories, "snapshot_limits": limits}
    finally:
        os.close(descriptor)


def check_delivery_state(frozen, changes=None, *, only_changes=False, timeout=None):
    """Check the full preimage, or the patch postimage over that preimage.

    Git patches encode regular-file permissions as executable/non-executable.
    Changed files use that permission contract; unchanged entries retain their
    exact modes. Only parent directories of patch paths may be created/removed.
    """
    current = capture_delivery_baseline(frozen["root"], timeout=timeout, limits=_frozen_limits(frozen))
    return _check_delivery_snapshot(frozen, current, changes, only_changes=only_changes)


def _check_delivery_snapshot(frozen, current, changes, *, only_changes=False):
    if current["root_identity"] != frozen["root_identity"]:
        raise OSError("Delivery destination directory identity or mode changed")
    if current["repositories"] != frozen["repositories"]:
        raise OSError("Delivery repository HEAD or marker identity changed")
    expected = dict(frozen["entries"])
    actual = dict(current["entries"])
    if changes is not None:
        parent_dirs = set()
        for name, record in changes.items():
            parts = name.split("/")
            parent_dirs.update("/".join(parts[:index]) for index in range(1, len(parts)))
            if record is None:
                expected.pop(name, None)
            elif record[0] == "file":
                expected[name] = ["git_file", record[1], bool(record[2] & 0o100)]
            else:
                expected[name] = list(record)
        for name in parent_dirs:
            before_dir, after_dir = expected.get(name), actual.get(name)
            if (before_dir and after_dir and before_dir[0] == after_dir[0] == "dir"
                    and before_dir != after_dir):
                raise OSError("Delivery parent directory permissions changed: " + repr(name))
            if expected.get(name, [None])[0] == "dir":
                expected.pop(name)
            if actual.get(name, [None])[0] == "dir":
                actual.pop(name)
        for name, record in expected.items():
            if record[0] == "git_file" and actual.get(name, [None])[0] == "file":
                real = actual[name]
                actual[name] = ["git_file", real[1], bool(real[2] & 0o100)]
    paths = set(changes) if only_changes else expected.keys() | actual.keys()
    for name in sorted(paths):
        if expected.get(name) != actual.get(name):
            raise OSError("Delivery destination content, permissions or entry set changed: " + repr(name))
    return True


def _patch_ancestors(changes):
    ancestors = {""}
    for name in changes:
        if not isinstance(name, str) or "\0" in name or name.startswith("/"):
            raise ValueError("Invalid delivery patch path")
        parts = name.split("/")
        if any(part in ("", ".", "..", ".git") for part in parts):
            raise ValueError("Invalid delivery patch path: " + repr(name))
        ancestors.update("/".join(parts[:index]) for index in range(1, len(parts)))
    return ancestors


def _capture_patch_state(frozen, changes):
    """Inspect only patch payloads and their ancestry, without listing siblings."""
    root = frozen["root"]
    budget = _SnapshotBudget(_frozen_limits(frozen))
    ancestors = _patch_ancestors(changes)
    root_fd = _open_root(root)
    result = {"root": root, "entries": {}, "ancestors": {}, "repositories": {}}

    def open_directory(relative):
        descriptor = os.dup(root_fd)
        try:
            for name in relative.split("/") if relative else ():
                budget.remaining()
                before = os.stat(name, dir_fd=descriptor, follow_symlinks=False)
                if not stat.S_ISDIR(before.st_mode):
                    raise OSError("Delivery patch ancestor must be a real directory: " + relative)
                child = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
                if _signature(os.fstat(child)) != _signature(before):
                    os.close(child)
                    raise OSError("Delivery patch ancestor changed: " + relative)
                os.close(descriptor)
                descriptor = child
            return descriptor
        except BaseException:
            os.close(descriptor)
            raise

    def directory_identity(info):
        return [info.st_dev, info.st_ino, stat.S_IMODE(info.st_mode)]

    try:
        result["root_identity"] = directory_identity(os.fstat(root_fd))
        if result["root_identity"] != frozen["root_identity"]:
            raise OSError("Delivery destination directory identity or mode changed")
        for relative in sorted(ancestors):
            budget.entry()
            try:
                descriptor = open_directory(relative)
            except FileNotFoundError:
                result["ancestors"][relative] = None
                result["repositories"][relative] = None
                continue
            try:
                result["ancestors"][relative] = directory_identity(os.fstat(descriptor))
                try:
                    marker = os.stat(".git", dir_fd=descriptor, follow_symlinks=False)
                except FileNotFoundError:
                    result["repositories"][relative] = None
                else:
                    if not (stat.S_ISDIR(marker.st_mode) or stat.S_ISREG(marker.st_mode)):
                        raise OSError("Delivery repository marker must be a real file or directory")
                    result["repositories"][relative] = {
                        "marker": [marker.st_dev, marker.st_ino, stat.S_IFMT(marker.st_mode)],
                        "head": _git_state(os.path.join(root, relative), budget.remaining),
                    }
                    after = os.stat(".git", dir_fd=descriptor, follow_symlinks=False)
                    if (after.st_dev, after.st_ino, after.st_mode) != (marker.st_dev, marker.st_ino, marker.st_mode):
                        raise OSError("Delivery repository marker changed: " + relative)
            finally:
                os.close(descriptor)
        for relative in sorted(changes):
            budget.entry()
            parent, _, name = relative.rpartition("/")
            try:
                descriptor = open_directory(parent)
            except FileNotFoundError:
                result["entries"][relative] = None
                continue
            try:
                try:
                    before = os.stat(name, dir_fd=descriptor, follow_symlinks=False)
                except FileNotFoundError:
                    result["entries"][relative] = None
                    continue
                result["entries"][relative] = _entry_record(descriptor, name, before, relative, budget)
                if _signature(os.stat(name, dir_fd=descriptor, follow_symlinks=False)) != _signature(before):
                    raise OSError("Delivery entry changed during inspection: " + relative)
            finally:
                os.close(descriptor)
        # Reopen ancestry to ensure each inspected descriptor still names the
        # same directories. Unrelated sibling contents are deliberately unread.
        for relative, identity in result["ancestors"].items():
            try:
                descriptor = open_directory(relative)
            except FileNotFoundError:
                if identity is not None:
                    raise OSError("Delivery patch ancestor disappeared: " + relative)
                continue
            try:
                if directory_identity(os.fstat(descriptor)) != identity:
                    raise OSError("Delivery patch ancestor identity or mode changed: " + relative)
            finally:
                os.close(descriptor)
        reopened = _open_root(root)
        try:
            if directory_identity(os.fstat(reopened)) != result["root_identity"]:
                raise OSError("Delivery destination directory identity or mode changed")
        finally:
            os.close(reopened)
        budget.remaining()
        return result
    finally:
        os.close(root_fd)


def _check_patch_postimage(frozen, changes, current):
    for relative, repository in current["repositories"].items():
        if repository != frozen["repositories"].get(relative):
            raise OSError("Delivery repository HEAD or marker identity changed: " + repr(relative))
    for relative, identity in current["ancestors"].items():
        before = frozen["entries"].get(relative)
        if before and before[0] == "dir" and identity is not None and identity[2] != before[1]:
            raise OSError("Delivery parent directory permissions changed: " + repr(relative))
    for relative, expected in changes.items():
        actual = current["entries"][relative]
        if expected is not None and expected[0] == "file" and actual is not None and actual[0] == "file":
            matches = actual[1] == expected[1] and bool(actual[2] & 0o100) == bool(expected[2] & 0o100)
        else:
            matches = actual == expected
        if not matches:
            raise OSError("Delivery destination content, permissions or entry set changed: " + repr(relative))


def _open_state_file(path, flags, mode=0o600):
    absolute = os.path.abspath(os.fspath(path))
    parent, name = os.path.split(absolute)
    directory = _open_root(parent)
    try:
        return os.open(name, flags | os.O_NOFOLLOW | os.O_NONBLOCK, mode, dir_fd=directory)
    finally:
        os.close(directory)


def write_delivery_checkpoint(frozen, changes, path):
    """Record the actual full modes of just this successfully applied patch."""
    current = _capture_patch_state(frozen, changes)
    _check_patch_postimage(frozen, changes, current)
    encoded = json.dumps(current, ensure_ascii=True).encode()
    if len(encoded) > MAX_METADATA_BYTES:
        raise OSError("Delivery rollback checkpoint exceeded its size limit")
    descriptor = _open_state_file(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL)
    with os.fdopen(descriptor, "wb") as stream:
        stream.write(encoded)
        stream.flush()
        os.fsync(stream.fileno())


def check_delivery_checkpoint(frozen, changes, path):
    """Refuse reversal over edited payloads; untouched files are not its scope."""
    try:
        descriptor = _open_state_file(path, os.O_RDONLY)
    except FileNotFoundError:
        # Git's executable bit cannot establish the full mode the command left.
        # Missing evidence must never authorize reversing an external chmod.
        raise OSError("Applied patch postimage was not captured; preserve it for manual inspection") from None
    with os.fdopen(descriptor, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > MAX_METADATA_BYTES:
            raise OSError("Invalid delivery rollback checkpoint")
        checkpoint = json.load(stream)
    if (checkpoint["root"] != frozen["root"]
            or checkpoint["root_identity"] != frozen["root_identity"]
            or set(checkpoint["entries"]) != set(changes)
            or set(checkpoint["ancestors"]) != _patch_ancestors(changes)
            or checkpoint["repositories"] != {name: frozen["repositories"].get(name)
                                               for name in _patch_ancestors(changes)}):
        raise OSError("Delivery rollback checkpoint differs from the approved patch")
    _check_patch_postimage(frozen, changes, checkpoint)
    current = _capture_patch_state(frozen, changes)
    if current["root_identity"] != checkpoint["root_identity"] or current["repositories"] != checkpoint["repositories"]:
        raise OSError("Delivery rollback destination identity or HEAD changed")
    if current["ancestors"] != checkpoint["ancestors"]:
        raise OSError("Delivery rollback ancestor identity or mode changed")
    for name, expected in checkpoint["entries"].items():
        if current["entries"].get(name) != expected:
            raise OSError("Delivery rollback conflicts with an external content or mode edit: " + repr(name))
    return True


def _shell_check(frozen, changes, operation, arguments="", failure=""):
    encoded = json.dumps({"baseline": frozen, "changes": changes}, ensure_ascii=True).encode()
    if len(encoded) > MAX_METADATA_BYTES:
        raise OSError("Delivery destination metadata exceeded its size limit")
    payload = base64.b64encode(encoded).decode("ascii")
    source = Path(__file__).read_text(encoding="utf-8")
    clause = " || " + failure if failure else ""
    return ("python3 -I -" + arguments + " <<'MAKEWAND_DELIVERY_STATE'" + clause + "\n"
            + source + "\nfrozen = json.loads(base64.b64decode(" + repr(payload) + "))\n"
            + "try:\n    " + operation + "\n"
            + "except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:\n"
            + "    print('Delivery destination check failed: ' + str(error), file=sys.stderr)\n"
            + "    sys.exit(1)\nMAKEWAND_DELIVERY_STATE\n")


def delivery_shell_guard(frozen, changes=None):
    """Embed this exact implementation; the exported script needs no SDK."""
    return _shell_check(frozen, changes, "check_delivery_state(frozen['baseline'], frozen['changes'])",
                        failure='exit "$?"' if changes is None else "rollback")


def delivery_shell_checkpoint(frozen, changes, state_argument):
    return _shell_check(frozen, changes,
                        "write_delivery_checkpoint(frozen['baseline'], frozen['changes'], sys.argv[1])",
                        arguments=" " + state_argument)


def delivery_shell_rollback_guard(frozen, changes, state_argument):
    return _shell_check(frozen, changes,
                        "check_delivery_checkpoint(frozen['baseline'], frozen['changes'], sys.argv[1])",
                        arguments=" " + state_argument)


def _run_shared_delivery(binding, arguments):
    operation = arguments[0]
    frozen = binding["baseline"]
    if operation == "preflight":
        return check_delivery_state(frozen)
    if operation == "postflight":
        return check_delivery_state(frozen, binding["changes"])
    changes = binding["groups"][arguments[1]]
    if operation == "checkpoint":
        return write_delivery_checkpoint(frozen, changes, arguments[2])
    if operation == "rollback":
        return check_delivery_checkpoint(frozen, changes, arguments[2])
    raise ValueError("Unknown delivery check operation")


def delivery_shell_setup(frozen, changes, groups):
    """Install one private, hash-bound helper and payload for the whole script."""
    source = Path(__file__).read_bytes()
    encoded = json.dumps({"baseline": frozen, "changes": changes, "groups": groups}, ensure_ascii=True).encode()
    if len(encoded) > MAX_METADATA_BYTES or len(source) > MAX_METADATA_BYTES:
        raise OSError("Delivery destination metadata exceeded its size limit")
    source_hash = hashlib.sha256(source).hexdigest()
    binding_hash = hashlib.sha256(encoded).hexdigest()
    return (
        'DELIVERY_STATE_DIR="$(mktemp -d /tmp/makewand-delivery-state.XXXXXXXX)"\n'
        'trap \'rm -rf -- "$DELIVERY_STATE_DIR"\' EXIT\n'
        'python3 -I - "$DELIVERY_STATE_DIR" <<\'MAKEWAND_DELIVERY_HELPER\'\n'
        'import base64, os\n'
        'directory = os.open(os.sys.argv[1], os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)\n'
        'try:\n'
        '    for name, data in ' + repr([
            ("helper.py", base64.b64encode(source).decode("ascii")),
            ("binding.json", base64.b64encode(encoded).decode("ascii"))]) + ':\n'
        '        descriptor = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=directory)\n'
        '        with os.fdopen(descriptor, "wb") as stream:\n'
        '            stream.write(base64.b64decode(data))\n'
        '            stream.flush()\n'
        '            os.fsync(stream.fileno())\n'
        'finally:\n    os.close(directory)\n'
        'MAKEWAND_DELIVERY_HELPER\n'
        'delivery_check() {\n'
        'python3 -I - "$DELIVERY_STATE_DIR" "$@" <<\'MAKEWAND_DELIVERY_RUNNER\'\n'
        'import hashlib, json, os, stat, sys\n'
        'try:\n'
        '    directory = os.open(sys.argv[1], os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)\n'
        '    def read(name, expected):\n'
        '        descriptor = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)\n'
        '        with os.fdopen(descriptor, "rb") as stream:\n'
        '            info = os.fstat(stream.fileno())\n'
        f'            if not stat.S_ISREG(info.st_mode) or info.st_size > {MAX_METADATA_BYTES}:\n'
        '                raise OSError("Invalid private delivery helper")\n'
        f'            data = stream.read({MAX_METADATA_BYTES + 1})\n'
        '        if hashlib.sha256(data).hexdigest() != expected:\n'
        '            raise OSError("Private delivery helper or binding changed")\n'
        '        return data\n'
        '    try:\n'
        '        source = read("helper.py", ' + repr(source_hash) + ')\n'
        '        binding = json.loads(read("binding.json", ' + repr(binding_hash) + '))\n'
        '    finally:\n        os.close(directory)\n'
        '    namespace = {"__name__": "makewand_standalone_delivery"}\n'
        '    exec(compile(source, "<makewand delivery helper>", "exec"), namespace)\n'
        '    namespace["_run_shared_delivery"](binding, sys.argv[2:])\n'
        'except Exception as error:\n'
        '    print("Delivery destination check failed: " + str(error), file=sys.stderr)\n'
        '    sys.exit(1)\n'
        'MAKEWAND_DELIVERY_RUNNER\n}\n'
    )
