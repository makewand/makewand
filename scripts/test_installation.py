#!/usr/bin/env python3
"""Offline installer and relocatable release regressions; never touches user state."""
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
import importlib.util
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent.parent


def fake_go(extra="", version_override=None):
    """A local compiler stub that reports the version passed via ldflags."""
    prefix = "#!" + sys.executable + "\nimport sys\nprint("
    suffix = " if '--version' in sys.argv else 'native-help')\n"
    return ("#!" + sys.executable + "\nimport os, pathlib, sys\n"
            "args = sys.argv[1:]\n"
            "output = pathlib.Path(args[args.index('-o') + 1])\n"
            "version = args[args.index('-ldflags') + 1].rsplit('=', 1)[1]\n" + extra + "\n" +
            ("version = " + repr(version_override) + "\n" if version_override is not None else "") +
            "output.write_text(" + repr(prefix) + " + repr('makewand version ' + version) + " + repr(suffix) + ")\n"
            "output.chmod(0o755)\n")


def installer_module():
    spec = importlib.util.spec_from_file_location("makewand_install_source_test", ROOT / "scripts/install_source.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def find_bash():
    if os.name == "nt":
        git = shutil.which("git")
        if git:
            git_root = Path(git).resolve().parent.parent
            for sub in ("bin/bash.exe", "usr/bin/bash.exe"):
                candidate = git_root / sub
                if candidate.is_file():
                    return str(candidate)
        for p in (r"C:\Program Files\Git\bin\bash.exe", r"C:\Program Files\Git\usr\bin\bash.exe"):
            if os.path.isfile(p):
                return p
    return "bash"


class InstallationTests(unittest.TestCase):
    def test_failed_upgrade_preserves_complete_previous_version(self):
        with tempfile.TemporaryDirectory(prefix="makewand-upgrade-") as directory:
            temp = Path(directory)
            source, tools = temp / "source", temp / "tools"
            (source / "bin").mkdir(parents=True)
            (source / "scripts").mkdir()
            tools.mkdir()
            shutil.copytree(ROOT / "makewand", source / "makewand", ignore=shutil.ignore_patterns("__pycache__"))
            shutil.copy2(ROOT / "bin/makewand", source / "bin/makewand")
            for name in ("install.sh", "install_source.py"):
                shutil.copy2(ROOT / "scripts" / name, source / "scripts" / name)
            go = tools / "go"
            go.write_text(fake_go())
            go.chmod(0o755)
            env = dict(os.environ, PATH=f"{tools}{os.pathsep}{os.environ['PATH']}",
                       MAKEWAND_INSTALL_ROOT=str(temp / "installed"), MAKEWAND_BIN_DIR=str(temp / "bin"),
                       MAKEWAND_SKILLS_DIR=str(temp / "skills"), MAKEWAND_CONFIG_DIR=str(temp / "config"))
            command = [find_bash(), str(source / "scripts/install.sh")]
            subprocess.run(command, env=env, check=True, capture_output=True)
            current = temp / "installed/current"
            previous_target = current.resolve()
            launcher = temp / "bin/makewand"
            for failure in ("build", "import", "smoke", "version"):
                with self.subTest(failure=failure):
                    package = source / "makewand/cli.py"
                    old_source = package.read_text()
                    old_go = go.read_text()
                    if failure == "build":
                        go.write_text("#!/bin/sh\nexit 7\n")
                    elif failure == "import":
                        package.write_text("raise RuntimeError('broken new engine')\n")
                    elif failure == "version":
                        go.write_text(fake_go(version_override="0.0.0-wrong"))
                    else:
                        package.write_text(old_source + "\nif 'run' in sys.argv: raise RuntimeError('broken smoke')\n")
                    try:
                        failed = subprocess.run(command, env=env, capture_output=True)
                        self.assertNotEqual(failed.returncode, 0)
                        self.assertEqual(current.resolve(), previous_target)
                        for args in (("--version",), ("run", "--help"), ("status", "--help")):
                            subprocess.run([str(launcher), *args], env=env, check=True, capture_output=True)
                    finally:
                        go.write_text(old_go)
                        package.write_text(old_source)
            self.assertEqual(len(list((temp / "installed/versions").iterdir())), 1)

            # Freeze Go, Python, and embedded assets together. A compiler that
            # changes the checkout must still build the original staged input.
            (source / "cmd/makewand").mkdir(parents=True)
            (source / "cmd/makewand/main.go").write_text("package main\n// frozen Go source\n")
            (source / "router").mkdir()
            (source / "router/strategy_tables.go").write_text("package router\n//go:embed defaults.json\n")
            (source / "router/defaults.json").write_text('{"source": "frozen"}\n')
            (source / "serverui/static").mkdir(parents=True)
            (source / "serverui/ui.go").write_text("package serverui\n//go:embed static/*\n")
            (source / "serverui/static/index.html").write_text("frozen UI\n")
            (source / "makewand/VERSION").write_text("3.2.0\n")
            env["INJECT_ORIGINAL_SOURCE"] = str(source)
            go.write_text(fake_go(
                "frozen = pathlib.Path.cwd()\n"
                "assert frozen != pathlib.Path(os.environ['INJECT_ORIGINAL_SOURCE'])\n"
                "assert 'frozen Go source' in (frozen / 'cmd/makewand/main.go').read_text()\n"
                "assert (frozen / 'router/defaults.json').read_text() == '{\"source\": \"frozen\"}\\n'\n"
                "assert (frozen / 'serverui/static/index.html').read_text() == 'frozen UI\\n'\n"
                "assert (frozen / 'makewand/VERSION').read_text().strip() == '3.2.0'\n"
                "original = pathlib.Path(os.environ['INJECT_ORIGINAL_SOURCE'])\n"
                "(original / 'makewand/VERSION').write_text('9.9.9\\n')\n"
                "(original / 'cmd/makewand/main.go').write_text('changed original Go source\\n')\n"
                "(original / 'router/defaults.json').write_text('changed original asset\\n')"))
            subprocess.run(command, env=env, check=True, capture_output=True)
            frozen_target = current.resolve()
            self.assertNotEqual(frozen_target, previous_target)
            self.assertEqual((frozen_target / "makewand/VERSION").read_text().strip(), "3.2.0")
            self.assertEqual(subprocess.check_output([str(frozen_target / "bin/makewand-server"), "--version"], text=True).strip(),
                             "makewand version 3.2.0")
            self.assertEqual((temp / "installed/previous").resolve(), previous_target)
            self.assertFalse((frozen_target / ".source").exists())

            (source / "makewand/VERSION").write_text("3.3.0\n")
            go.write_text(fake_go("(pathlib.Path.cwd() / 'cmd/makewand/main.go').write_text('mutated frozen source\\n')"))
            failed = subprocess.run(command, env=env, capture_output=True)
            self.assertNotEqual(failed.returncode, 0)
            self.assertEqual(current.resolve(), frozen_target)

            # Activation failures roll back current and remove the temporary
            # wrapper. A failed rollback retains both recoverable engine sets.
            go.write_text(fake_go())
            module = installer_module()
            actual_replace, actual_link = module.os.replace, module.atomic_link
            def fail_launcher(src, dst):
                if Path(dst) == launcher:
                    raise PermissionError("injected launcher activation failure")
                return actual_replace(src, dst)
            with patch.dict(os.environ, env), patch.object(module.os, "replace", side_effect=fail_launcher):
                with self.assertRaisesRegex(PermissionError, "launcher activation"):
                    module.install(source)
            self.assertEqual(current.resolve(), frozen_target)
            self.assertFalse(list((temp / "bin").glob(".makewand.*")))

            def fail_rollback(target, destination):
                if Path(destination) == current and str(target) == str(frozen_target):
                    raise PermissionError("injected current rollback failure")
                return actual_link(target, destination)
            with patch.dict(os.environ, env), patch.object(module.os, "replace", side_effect=fail_launcher), \
                 patch.object(module, "atomic_link", side_effect=fail_rollback):
                with self.assertRaisesRegex(RuntimeError, "rollback failed.*Preserved complete new version"):
                    module.install(source)
            self.assertNotEqual(current.resolve(), frozen_target)
            self.assertTrue((current.resolve() / "bin/makewand").is_file())
            self.assertTrue((current.resolve() / "bin/makewand-server").is_file())
            self.assertEqual((temp / "installed/previous").resolve(), frozen_target)
            self.assertFalse(list((temp / "bin").glob(".makewand.*")))
            for target in (previous_target, frozen_target, current.resolve()):
                subprocess.run([sys.executable, "-I", str(target / "bin/makewand"), "run", "--help"],
                               env=env, check=True, capture_output=True)

    def test_site_upgrade_replaces_symlink_and_ignores_cwd_package(self):
        with tempfile.TemporaryDirectory(prefix="makewand-install-") as directory:
            temp = Path(directory)
            app, binaries, fakebin = temp / "app", temp / "bin", temp / "tools"
            for path in (app / ".git", app / "bin", app / "scripts", binaries, fakebin):
                path.mkdir(parents=True, exist_ok=True)
            shutil.copytree(ROOT / "makewand", app / "makewand", ignore=shutil.ignore_patterns("__pycache__"))
            shutil.copy2(ROOT / "bin" / "makewand", app / "bin" / "makewand")
            shutil.copy2(ROOT / "scripts" / "install.sh", app / "scripts" / "install.sh")
            shutil.copy2(ROOT / "scripts" / "install_source.py", app / "scripts" / "install_source.py")
            (binaries / "makewand").symlink_to(app / "bin" / "makewand")
            before = hashlib.sha256((app / "bin" / "makewand").read_bytes()).hexdigest()
            (fakebin / "git").write_text("#!/bin/sh\nexit 0\n")
            (fakebin / "go").write_text(fake_go())
            for executable in fakebin.iterdir():
                executable.chmod(0o755)
            hostile = temp / "hostile"
            (hostile / "makewand").mkdir(parents=True)
            marker = hostile / "executed"
            (hostile / "makewand" / "__init__.py").write_text(f"from pathlib import Path\nPath({str(marker)!r}).touch()\nraise RuntimeError('shadowed')\n")
            env = dict(os.environ, PATH=f"{fakebin}{os.pathsep}{os.environ['PATH']}",
                       MAKEWAND_INSTALL_ROOT=str(app), MAKEWAND_SOURCE_DIR=str(app), MAKEWAND_BIN_DIR=str(binaries),
                       MAKEWAND_SKILLS_DIR=str(temp / "skills"), MAKEWAND_CONFIG_DIR=str(temp / "config"),
                       PYTHONPATH=str(hostile))
            subprocess.run([find_bash(), str(ROOT / "site" / "install.sh")], cwd=hostile, env=env,
                           check=True, capture_output=True, text=True)
            self.assertFalse((binaries / "makewand").is_symlink())
            self.assertEqual(before, hashlib.sha256((app / "bin" / "makewand").read_bytes()).hexdigest())
            for args in (("--version",), ("--help",), ("review", "--repo-trust", "untrusted", "--help"),
                         ("-C", str(hostile), "serve", "--help"),
                         ("--max-model-calls", "1", "serve", "--help"),
                         ("serve", "--call-budget-file", str(temp / "unused-budget.json"), "--help"),
                         ("--cwd=" + str(hostile), "--repo-trust", "untrusted", "chat", "--help")):
                subprocess.run([str(binaries / "makewand"), *args], cwd=hostile, env=env,
                               check=True, capture_output=True, text=True)
            for args in (("--daemon", "serve", "--help"), ("serve", "--workflow", "race", "--help")):
                rejected = subprocess.run([str(binaries / "makewand"), *args], cwd=hostile, env=env,
                                          capture_output=True, text=True)
                self.assertEqual(rejected.returncode, 2)
                self.assertIn("cannot be honored", rejected.stderr)
            self.assertFalse(marker.exists())
            config = json.loads((temp / "config" / "config.json").read_text())
            self.assertIs(config["enabled_providers"]["local"], False)
            check = "import sys; sys.path.insert(0, sys.argv[1]); from makewand.config import is_provider_enabled; assert not is_provider_enabled('local')"
            subprocess.run(["python3", "-I", "-c", check, str(app)], cwd=hostile, env=env, check=True)

    def test_release_bundle_from_clean_extraction(self):
        with tempfile.TemporaryDirectory(prefix="makewand-release-") as directory:
            temp = Path(directory)
            bundle = temp / "staged" / "makewand"
            bundle.mkdir(parents=True)
            frozen = temp / "frozen-source"
            frozen.mkdir()
            installer = installer_module()
            expected_inputs = installer.freeze_source(ROOT, frozen)
            executable = "makewand.exe" if os.name == "nt" else "makewand"
            version = "v0.0.0-contract"
            subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags",
                            f"-X github.com/makewand/makewand/internal/buildinfo.Version={version}",
                            "-o", str(bundle / executable), "./cmd/makewand"], cwd=frozen,
                           env=dict(os.environ, GOWORK="off"), check=True)
            installer.verify_frozen_source(frozen, expected_inputs)
            (frozen / "scripts").mkdir(exist_ok=True)
            shutil.copy2(ROOT / "scripts/stage_python_engine.py", frozen / "scripts/stage_python_engine.py")
            subprocess.run(["python3", "-I", str(frozen / "scripts/stage_python_engine.py"), str(bundle), version], check=True)
            archive = shutil.make_archive(str(temp / "release"), "gztar", bundle.parent, bundle.name)
            extracted = temp / "extracted"
            import tarfile
            with tarfile.open(archive) as packed:
                if hasattr(tarfile, "data_filter"):
                    packed.extractall(extracted, filter="data")
                else:
                    packed.extractall(extracted)
            shutil.rmtree(bundle.parent)
            subprocess.run([find_bash(), str(ROOT / "scripts" / "release_contract.sh"),
                            str(extracted / "makewand" / executable), version], check=True)
            if os.name != "nt":
                # Homebrew retains both engines under libexec and exposes a bin entry.
                prefix = temp / "prefix"
                (prefix / "bin").mkdir(parents=True)
                shutil.copytree(extracted / "makewand", prefix / "libexec")
                (prefix / "bin" / "makewand").symlink_to(prefix / "libexec" / executable)
                subprocess.run([str(prefix / "bin" / "makewand"), "run", "--help"],
                               cwd=temp, check=True, capture_output=True)
            # Incomplete artifacts must fail even if the developer has another install.
            shutil.rmtree(extracted / "makewand" / "lib")
            result = subprocess.run([find_bash(), str(ROOT / "scripts" / "release_contract.sh"),
                                     str(extracted / "makewand" / executable), version], capture_output=True)
            self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main(verbosity=2)
