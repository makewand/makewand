"""Regression tests for local build gates and repository hygiene (eng-delivery#10, #11, #12).

- the Makefile offers CI-equivalent `lint` / `vuln` targets pinned to the go.mod
  toolchain (a newer local Go made prebuilt analyzers panic), and
  `make prelaunch` runs what docs/PRELAUNCH.md promises;
- .golangci.yml carries no exclusions for paths that do not exist;
- .gitignore changes never hide tracked files and cover generated artifacts.
"""
import re
import shutil
import subprocess
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def _go_toolchain():
    gomod = (ROOT / "go.mod").read_text(encoding="utf-8")
    toolchain = re.search(r"^toolchain\s+(\S+)", gomod, re.M)
    if toolchain:
        return toolchain.group(1)
    return "go" + re.search(r"^go\s+(\S+)", gomod, re.M).group(1)


def _git(*args):
    return subprocess.run(["git", "-C", str(ROOT), *args], capture_output=True, text=True)


@unittest.skipUnless(shutil.which("make"), "make required")
class MakefileTests(unittest.TestCase):
    def _dry_run(self, *targets):
        result = subprocess.run(["make", "-n", "-C", str(ROOT), *targets],
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout

    def test_lint_uses_go_mod_toolchain(self):
        self.assertIn(f"GOTOOLCHAIN={_go_toolchain()} golangci-lint run ./...", self._dry_run("lint"))

    def test_lint_version_matches_ci_pin(self):
        ci = (ROOT / ".github" / "workflows" / "ci.yml").read_text(encoding="utf-8")
        pinned = re.search(r"golangci-lint-action@[^\n]+\n(?:.*\n)*?\s+version: (v[0-9.]+)", ci).group(1)
        self.assertIn(f"GOLANGCI_LINT_VERSION ?= {pinned}", (ROOT / "Makefile").read_text(encoding="utf-8"))

    def test_vuln_uses_go_mod_toolchain(self):
        self.assertIn(f"GOTOOLCHAIN={_go_toolchain()} govulncheck ./...", self._dry_run("vuln"))

    def test_test_target_runs_release_tooling_self_tests(self):
        out = self._dry_run("test")
        self.assertIn("bash scripts/test_check_secrets.sh", out)
        self.assertIn("bash scripts/check_version.sh", out)

    def test_prelaunch_runs_what_the_docs_promise(self):
        out = self._dry_run("prelaunch")
        for command in ("./scripts/check_secrets.sh", "bash scripts/test_check_secrets.sh",
                        "bash scripts/check_version.sh", "bash -n", "gofmt -l",
                        "golangci-lint run ./...", "bash ./scripts/prelaunch_gate.sh",
                        "scripts/test_race.sh", "govulncheck ./..."):
            with self.subTest(command=command):
                self.assertIn(command, out)
        gate = (ROOT / "scripts" / "prelaunch_gate.sh").read_text(encoding="utf-8")
        self.assertIn("doctor --strict", gate)
        docs = (ROOT / "docs" / "PRELAUNCH.md").read_text(encoding="utf-8")
        for mention in ("prelaunch_gate.sh", "doctor --strict", "make lint", "make vuln",
                        "check_secrets.sh", "check_version.sh", "test_race.sh", "MAKEWAND_LIVE_SMOKE"):
            with self.subTest(doc_mention=mention):
                self.assertIn(mention, docs)
        # The old checklist promised a vet scope that `make prelaunch` never ran.
        self.assertNotIn("go vet ./cmd/... ./internal/... ./router", docs)


class RepositoryHygieneTests(unittest.TestCase):
    def test_golangci_exclusions_point_at_existing_paths(self):
        text = (ROOT / ".golangci.yml").read_text(encoding="utf-8")
        entries = []
        for block in re.finditer(r"(?m)^(\s*)paths:\s*\n((?:\1\s+- .*\n)+)", text):
            entries += [line.split("- ", 1)[1].strip() for line in block.group(2).splitlines()]
        self.assertTrue(entries, "expected at least one exclusion path")
        for entry in entries:
            literal = entry.strip("^$").rstrip("/")
            with self.subTest(entry=entry):
                self.assertTrue((ROOT / literal).exists(), f"{entry} excludes a path that does not exist")

    @unittest.skipUnless(shutil.which("git") and (ROOT / ".git").exists(), "git checkout required")
    def test_gitignore_does_not_hide_tracked_files(self):
        result = _git("ls-files", "-ci", "--exclude-standard")
        self.assertEqual(result.returncode, 0, result.stderr)
        # benchmarks/results/go.mod is a deliberately tracked module stub that
        # keeps generated benchmark projects out of `go ./...`.
        self.assertEqual(sorted(set(result.stdout.split()) - {"benchmarks/results/go.mod"}), [])

    @unittest.skipUnless(shutil.which("git") and (ROOT / ".git").exists(), "git checkout required")
    def test_generated_artifacts_are_ignored(self):
        for path in ("build/makewand", "output/playwright/preview.png", "bin/makewand-server",
                     "cmd/makewand/makewand.test", "coverage.out", ".vercel/project.json"):
            with self.subTest(path=path):
                self.assertEqual(_git("check-ignore", "-q", "--no-index", path).returncode, 0)


if __name__ == "__main__":
    unittest.main()
