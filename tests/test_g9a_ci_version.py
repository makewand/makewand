"""Regression tests for version consistency and the changelog (eng-delivery#9).

- scripts/check_version.sh catches every hardcoded version copy that drifts
  from makewand.__version__ and a release tag that does not match it (v3.0.1 and
  v3.0.2 were tagged while __version__ still said 3.0.0);
- CHANGELOG.md documents every 3.x release.
"""
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
CHECK_VERSION = ROOT / "scripts" / "check_version.sh"
VERSION_SURFACE = [
    "makewand/__init__.py",
    "makewand/interactive.py",
    "tests/test_interactive.py",
    "README.md",
    "site/index.html",
    "site/docs.html",
    "site/main.js",
    "internal/buildinfo/buildinfo.go",
    "scripts/install.sh",
    "scripts/install_source.py",
    "CHANGELOG.md",
]


def _source_version():
    text = (ROOT / "makewand" / "__init__.py").read_text(encoding="utf-8")
    return re.search(r'^__version__\s*=\s*"([^"]+)"', text, re.M).group(1)


def _git(*args):
    return subprocess.run(["git", "-C", str(ROOT), *args], capture_output=True, text=True)


@unittest.skipUnless(shutil.which("bash"), "bash required")
class CheckVersionTests(unittest.TestCase):
    def _run(self, *args, root=None):
        cmd = ["bash", str(CHECK_VERSION)]
        if root is not None:
            cmd += ["--root", str(root)]
        return subprocess.run(cmd + list(args), capture_output=True, text=True)

    def _fixture(self, tmp):
        root = Path(tmp)
        for rel in VERSION_SURFACE:
            src = ROOT / rel
            if src.exists():
                (root / rel).parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(src, root / rel)
        return root

    def test_repository_is_consistent(self):
        result = self._run()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_matching_release_tag_passes(self):
        result = self._run("--tag", "v" + _source_version())
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_release_tag_drift_fails(self):
        # v3.0.1 and v3.0.2 were tagged while __version__ still said 3.0.0.
        result = self._run("--tag", "v" + _source_version() + "1")
        self.assertEqual(result.returncode, 1)
        self.assertIn("does not match", result.stderr)

    def test_each_drifted_copy_is_reported(self):
        version = _source_version()
        stale = "v0.0.1"
        cases = {
            "README.md": lambda s: s.replace("v" + version, stale, 1),
            "site/index.html": lambda s: s.replace("v" + version, stale, 1),
            "site/main.js": lambda s: s.replace("v" + version, stale, 1),
            "makewand/__init__.py": lambda s: s.replace("(v" + version + ")", "(" + stale + ")", 1),
            "makewand/interactive.py": lambda s: s + f'\n_STALE_BANNER = "Makewand ({stale})"\n',
            "tests/test_interactive.py": lambda s: s + f'\n# card shows "Makewand ({stale})"\n',
            "CHANGELOG.md": lambda s: s.replace(f"## [{version}]", "## [0.0.1]"),
            "internal/buildinfo/buildinfo.go": lambda s: s.replace('Version = "dev"', f'Version = "{version}"', 1),
            "scripts/install.sh": lambda s: s.replace('exec python3 -I "$SCRIPT_ROOT/scripts/install_source.py" "$SCRIPT_ROOT"', 'echo "installation bypassed"', 1),
            "scripts/install_source.py": lambda s: s.replace('"-X github.com/makewand/makewand/internal/buildinfo.Version=" + version', '"-X github.com/makewand/makewand/internal/buildinfo.Version=0.0.1"', 1),
        }
        for rel, mutate in cases.items():
            with self.subTest(file=rel), tempfile.TemporaryDirectory() as tmp:
                root = self._fixture(tmp)
                baseline = self._run(root=root)
                self.assertEqual(baseline.returncode, 0, baseline.stdout + baseline.stderr)
                target = root / rel
                original = target.read_text(encoding="utf-8")
                mutated = mutate(original)
                self.assertNotEqual(original, mutated, f"fixture mutation did not apply to {rel}")
                target.write_text(mutated, encoding="utf-8")
                result = self._run(root=root)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertIn(rel, result.stderr)

    def test_installer_rejects_mixed_or_unchecked_engine_versions(self):
        mutations = [
            lambda s: s.replace('version = python_version[len("makewand "):]', 'version = "0.0.1"', 1),
            lambda s: s.replace('if native_version != "makewand version " + version:', 'if False:', 1),
            lambda s: s.replace('"-I", "-B", str(stage / "bin/makewand"), "--version"', '"-I", "-B", str(stage / "bin/makewand"), "--help"', 1),
        ]
        for index, mutate in enumerate(mutations):
            with self.subTest(mutation=index), tempfile.TemporaryDirectory() as tmp:
                root = self._fixture(tmp)
                target = root / "scripts/install_source.py"
                original = target.read_text(encoding="utf-8")
                mutated = mutate(original)
                self.assertNotEqual(original, mutated)
                target.write_text(mutated, encoding="utf-8")
                result = self._run(root=root)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertIn("scripts/install_source.py", result.stderr)


class ChangelogTests(unittest.TestCase):
    def test_every_3x_release_has_a_section(self):
        text = (ROOT / "CHANGELOG.md").read_text(encoding="utf-8")
        headings = set(re.findall(r"^## \[([^\]]+)\]", text, re.M))
        expected = {"Unreleased", "3.0.0", "3.0.1", "3.0.2", "3.1.0", _source_version()}
        tags = _git("tag", "--list", "v*") if shutil.which("git") else None
        if tags is not None and tags.returncode == 0:
            for tag in tags.stdout.split():
                match = re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)", tag)
                if match and int(match.group(1)) >= 3:
                    expected.add(tag[1:])
        self.assertEqual(sorted(expected - headings), [])


if __name__ == "__main__":
    unittest.main()
