#!/usr/bin/env python3
"""Offline installer and relocatable release regressions; never touches user state."""
import hashlib
import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


class InstallationTests(unittest.TestCase):
    def test_site_upgrade_replaces_symlink_and_ignores_cwd_package(self):
        with tempfile.TemporaryDirectory(prefix="makewand-install-") as directory:
            temp = Path(directory)
            app, binaries, fakebin = temp / "app", temp / "bin", temp / "tools"
            for path in (app / ".git", app / "bin", app / "scripts", binaries, fakebin):
                path.mkdir(parents=True, exist_ok=True)
            shutil.copytree(ROOT / "makewand", app / "makewand", ignore=shutil.ignore_patterns("__pycache__"))
            shutil.copy2(ROOT / "bin" / "makewand", app / "bin" / "makewand")
            shutil.copy2(ROOT / "scripts" / "install.sh", app / "scripts" / "install.sh")
            (binaries / "makewand").symlink_to(app / "bin" / "makewand")
            before = hashlib.sha256((app / "bin" / "makewand").read_bytes()).hexdigest()
            (fakebin / "git").write_text("#!/bin/sh\nexit 0\n")
            (fakebin / "go").write_text(
                '#!/bin/sh\nwhile [ "$#" -gt 0 ]; do\n'
                'if [ "$1" = -o ]; then shift; output="$1"; fi\nshift\ndone\n'
                'printf "#!/bin/sh\\necho native-help\\n" > "$output"\nchmod +x "$output"\n'
            )
            for executable in fakebin.iterdir():
                executable.chmod(0o755)
            hostile = temp / "hostile"
            (hostile / "makewand").mkdir(parents=True)
            marker = hostile / "executed"
            (hostile / "makewand" / "__init__.py").write_text(f"from pathlib import Path\nPath({str(marker)!r}).touch()\nraise RuntimeError('shadowed')\n")
            env = dict(os.environ, PATH=f"{fakebin}{os.pathsep}{os.environ['PATH']}",
                       MAKEWAND_INSTALL_ROOT=str(app), MAKEWAND_BIN_DIR=str(binaries),
                       MAKEWAND_SKILLS_DIR=str(temp / "skills"), MAKEWAND_CONFIG_DIR=str(temp / "config"),
                       PYTHONPATH=str(hostile))
            subprocess.run(["bash", str(ROOT / "site" / "install.sh")], cwd=hostile, env=env,
                           check=True, capture_output=True, text=True)
            self.assertFalse((binaries / "makewand").is_symlink())
            self.assertEqual(before, hashlib.sha256((app / "bin" / "makewand").read_bytes()).hexdigest())
            for args in (("--version",), ("--help",), ("review", "--repo-trust", "untrusted", "--help")):
                subprocess.run([str(binaries / "makewand"), *args], cwd=hostile, env=env,
                               check=True, capture_output=True, text=True)
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
            executable = "makewand.exe" if os.name == "nt" else "makewand"
            version = "v0.0.0-contract"
            subprocess.run(["go", "build", "-trimpath", "-ldflags",
                            f"-X github.com/makewand/makewand/internal/buildinfo.Version={version}",
                            "-o", str(bundle / executable), "./cmd/makewand"], cwd=ROOT, check=True)
            subprocess.run(["python3", "-I", str(ROOT / "scripts" / "stage_python_engine.py"), str(bundle), version], check=True)
            archive = shutil.make_archive(str(temp / "release"), "gztar", bundle.parent, bundle.name)
            extracted = temp / "extracted"
            import tarfile
            with tarfile.open(archive) as packed:
                if hasattr(tarfile, "data_filter"):
                    packed.extractall(extracted, filter="data")
                else:
                    packed.extractall(extracted)
            shutil.rmtree(bundle.parent)
            subprocess.run(["bash", str(ROOT / "scripts" / "release_contract.sh"),
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
            result = subprocess.run(["bash", str(ROOT / "scripts" / "release_contract.sh"),
                                     str(extracted / "makewand" / executable), version], capture_output=True)
            self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main(verbosity=2)
