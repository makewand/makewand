#!/usr/bin/env python3
"""Freeze matching source inputs, validate both engines, then switch versions."""
import hashlib
import os
import re
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
import uuid
from pathlib import Path

# Source installation uses Bash and POSIX symlinks; native Windows uses bundles.
EXCLUDED = {".git", ".makewand", "__pycache__", ".venv", "venv", "node_modules",
            "benchmarks", "dist", "build", ".cache", ".pytest_cache"}
GO_INPUT_SUFFIXES = {".go", ".s", ".S", ".c", ".h", ".cc", ".cpp", ".cxx", ".m", ".mm", ".f", ".F", ".for", ".f90", ".syso"}
SEMVER = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?\Z")


def atomic_link(target, destination):
    temporary = destination.with_name("." + destination.name + "." + uuid.uuid4().hex)
    try:
        temporary.symlink_to(target)
        os.replace(temporary, destination)
    finally:
        temporary.unlink(missing_ok=True)


def _files(directory, exclude=True, strict=True):
    if directory.is_symlink():
        raise OSError(f"source directory must not be a symlink: {directory}")
    for root, directories, filenames in os.walk(directory, followlinks=False):
        for name in list(directories):
            child = Path(root) / name
            if exclude and (name in EXCLUDED or name.startswith(".")):
                directories.remove(name)
            elif child.is_symlink():
                if strict:
                    raise OSError(f"source directory must not be a symlink: {child}")
                directories.remove(name)
        for name in sorted(filenames):
            path = Path(root) / name
            if name.endswith((".pyc", ".pyo")):
                continue
            if path.is_symlink() or not stat.S_ISREG(path.lstat().st_mode):
                if strict or path.suffix in GO_INPUT_SUFFIXES:
                    raise OSError(f"source input must be a regular file: {path}")
                continue
            yield path


def source_inputs(source):
    """Select Python, Go, and every asset referenced by go:embed."""
    paths = set(_files(source / "makewand"))
    paths.add(source / "bin/makewand")
    for name in ("go.mod", "go.sum", "LICENSE", "vendor/modules.txt"):
        if (source / name).is_file():
            paths.add(source / name)
    go_files = []
    for entry in sorted(source.iterdir()):
        if entry.name in EXCLUDED or entry.name.startswith(".") or entry.is_symlink():
            continue
        candidates = _files(entry, strict=False) if entry.is_dir() else [entry]
        for path in candidates:
            if path.suffix in GO_INPUT_SUFFIXES:
                paths.add(path)
                if path.suffix == ".go":
                    go_files.append(path)
    for go_file in go_files:
        for match in re.finditer(r"^\s*//go:embed\s+(.+)$", go_file.read_text(encoding="utf-8"), re.MULTILINE):
            for pattern in shlex.split(match.group(1)):
                pattern = pattern.removeprefix("all:")
                if Path(pattern).is_absolute() or ".." in Path(pattern).parts:
                    raise OSError(f"invalid go:embed source pattern: {pattern}")
                matches = list(go_file.parent.glob(pattern))
                if not matches:
                    raise OSError(f"missing go:embed input: {go_file}: {pattern}")
                for path in matches:
                    if path.is_symlink():
                        raise OSError(f"go:embed input must not be a symlink: {path}")
                    if path.is_dir():
                        paths.update(_files(path, exclude=False))
                    else:
                        paths.add(path)
    skills = source / "skills/makewand-orchestrator"
    if skills.is_dir():
        paths.update(_files(skills))
    return sorted(paths)


def source_record(path):
    with os.fdopen(os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)), "rb") as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode):
            raise OSError(f"source input must be a regular file: {path}")
        data = handle.read()
    return {"sha256": hashlib.sha256(data).hexdigest(), "mode": stat.S_IMODE(info.st_mode)}, data


def freeze_source(source, destination):
    paths = source_inputs(source)
    expected = {path.relative_to(source).as_posix(): source_record(path)[0] for path in paths}
    for relative, record in expected.items():
        current, data = source_record(source / relative)
        if current != record:
            raise OSError(f"source changed while staging: {relative}")
        target = destination / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        with target.open("wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        target.chmod(record["mode"])
    after = {path.relative_to(source).as_posix(): source_record(path)[0] for path in source_inputs(source)}
    if after != expected:
        raise OSError("source inputs changed while establishing the frozen installation")
    return expected


def verify_frozen_source(source, expected):
    actual = {path.relative_to(source).as_posix(): source_record(path)[0] for path in source_inputs(source)}
    if actual != expected:
        raise OSError("build changed the frozen source inputs")


def _install_locked(source, root, binaries):
    versions = root / "versions"
    versions.mkdir(mode=0o700, parents=True, exist_ok=True)
    binaries.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=".staging-", dir=versions))
    current = root / "current"
    temporary = None
    activated = False
    retain_for_recovery = False
    try:
        if os.path.lexists(current) and not current.is_symlink():
            raise RuntimeError("current must be a symlink; existing installation preserved")
        frozen = stage / ".source"
        frozen.mkdir()
        expected = freeze_source(source, frozen)
        shutil.copytree(frozen / "makewand", stage / "makewand")
        (stage / "bin").mkdir()
        shutil.copy2(frozen / "bin/makewand", stage / "bin/makewand")
        if (frozen / "LICENSE").exists():
            shutil.copy2(frozen / "LICENSE", stage / "LICENSE")
        env = dict(os.environ, MAKEWAND_CONFIG_DIR=str(stage / ".smoke-config"),
                   MAKEWAND_API_POLICY="subscription_only", MAKEWAND_NO_DAEMON="1", MAKEWAND_DAEMON="0")
        env.pop("MAKEWAND_HOME", None)
        env.pop("MAKEWAND_SKIP_VERSION_CHECK", None)
        python_version = subprocess.check_output(
            [sys.executable, "-I", "-B", str(stage / "bin/makewand"), "--version"], cwd=stage, env=env, text=True).strip()
        if not python_version.startswith("makewand ") or not SEMVER.fullmatch(python_version[len("makewand "):]):
            raise RuntimeError(f"invalid Python engine version: {python_version!r}")
        version = python_version[len("makewand "):]
        native = stage / "bin/makewand-server"
        build_env = dict(os.environ, GOWORK="off")
        subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags",
                        "-X github.com/makewand/makewand/internal/buildinfo.Version=" + version,
                        "-o", str(native), "./cmd/makewand"], cwd=frozen, env=build_env, check=True)
        verify_frozen_source(frozen, expected)
        native.chmod(0o755)
        native_version = subprocess.check_output([str(native), "--version"], cwd=stage, env=env, text=True).strip()
        if native_version != "makewand version " + version:
            raise RuntimeError(f"engine version mismatch: {native_version!r} / {python_version!r}")
        for args in (("--version",), ("run", "--help"), ("serve", "--help"),
                     ("daemon", "--help"), ("sessions", "--help")):
            subprocess.run([sys.executable, "-I", "-B", str(stage / "bin/makewand"), *args],
                           cwd=stage, env=env, check=True, stdout=subprocess.DEVNULL)
        shutil.rmtree(stage / ".smoke-config", ignore_errors=True)
        skills = frozen / "skills/makewand-orchestrator"
        if skills.is_dir():
            shutil.copytree(skills, stage / "skills/makewand-orchestrator")
        shutil.rmtree(frozen)
        destination = versions / (version + "-" + uuid.uuid4().hex[:12])
        os.replace(stage, destination)
        stage = destination
        wrapper = "#!/usr/bin/env bash\nset -euo pipefail\nexec python3 -I " + shlex.quote(str(current / "bin/makewand")) + ' "$@"\n'
        fd, temporary_name = tempfile.mkstemp(prefix=".makewand.", dir=binaries)
        temporary = Path(temporary_name)
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            stream.write(wrapper)
            stream.flush()
            os.fsync(stream.fileno())
        temporary.chmod(0o755)
        previous = os.readlink(current) if current.is_symlink() else None
        if previous is not None:
            # Preserve a recovery pointer before making any visible change.
            atomic_link(previous, root / "previous")
        atomic_link(str(destination), current)
        try:
            os.replace(temporary, binaries / "makewand")
        except BaseException as activation_error:
            try:
                if previous is not None:
                    atomic_link(previous, current)
                else:
                    current.unlink(missing_ok=True)
            except BaseException as rollback_error:
                retain_for_recovery = True
                raise RuntimeError(
                    f"launcher activation failed: {activation_error}; current rollback failed: {rollback_error}. "
                    f"Preserved complete new version at {destination}; prior version at {previous!r}, "
                    f"recovery pointer {root / 'previous'}") from activation_error
            raise
        activated = True
        try:
            atomic_link("makewand", binaries / "trio")
            skills = destination / "skills/makewand-orchestrator"
            if skills.is_dir():
                skills_root = Path(os.environ.get("MAKEWAND_SKILLS_DIR", str(Path.home() / ".gemini/config/skills")))
                for name in ("makewand-orchestrator", "trio-orchestrator"):
                    shutil.copytree(skills, skills_root / name, dirs_exist_ok=True)
        except OSError as error:
            print(f"Installed engines; optional alias/skill integration failed: {error}", file=sys.stderr)
        print(f"Installed Makewand {version} in {destination}; current switched atomically.")
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)
        # Preserve a complete version whenever an exceptional switch may have
        # left current pointing to it, even if activation was interrupted.
        if not activated and not retain_for_recovery:
            try:
                retain_for_recovery = current.is_symlink() and current.resolve() == stage.resolve()
            except OSError:
                retain_for_recovery = True
            if retain_for_recovery:
                print(f"Preserved installation version for recovery: {stage}", file=sys.stderr)
        if not activated and not retain_for_recovery:
            shutil.rmtree(stage, ignore_errors=True)


def install(source):
    if os.name != "posix":
        raise RuntimeError("source installation requires POSIX; use a native release bundle or WSL2")
    import fcntl
    source = Path(source).resolve()
    root = Path(os.environ.get("MAKEWAND_INSTALL_ROOT", str(Path.home() / ".local/share/makewand"))).expanduser().resolve()
    binaries = Path(os.environ.get("MAKEWAND_BIN_DIR", str(Path.home() / ".local/bin"))).expanduser().resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    with (root / "install.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            _install_locked(source, root, binaries)
        finally:
            fcntl.flock(lock, fcntl.LOCK_UN)


if __name__ == "__main__":
    install(sys.argv[1])
