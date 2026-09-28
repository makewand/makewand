"""Regression tests for the GitHub Actions workflows (eng-delivery#1, #2, #10, #11).

The workflows cannot run locally, so these tests pin the properties whose
absence broke delivery: the bubblewrap user-namespace fix-up must run before any
sandbox test, every job needs a timeout, the release notes must install the
prebuilt archive, binaries are stamped with the bare semantic version, and the
release-metadata maintenance workflow must be manual, dry-run by default and
free of hardcoded accounts. Stdlib only (PyYAML is used when available).
"""
import json
import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
WORKFLOWS = ROOT / ".github" / "workflows"
CI = WORKFLOWS / "ci.yml"
RELEASE = WORKFLOWS / "release.yml"
NORMALIZE = WORKFLOWS / "normalize-release-metadata.yml"


def _read(path):
    return path.read_text(encoding="utf-8")


def _job_blocks(text):
    """Return {job_name: block_text} for top-level jobs (2-space indented keys)."""
    lines = text.splitlines()
    try:
        start = next(i for i, line in enumerate(lines) if line.rstrip() == "jobs:")
    except StopIteration:
        return {}
    jobs, name, buf = {}, None, []
    for line in lines[start + 1:]:
        match = re.match(r"^  ([A-Za-z0-9_-]+):\s*$", line)
        if match:
            if name:
                jobs[name] = "\n".join(buf)
            name, buf = match.group(1), []
        elif line and not line.startswith(" ") and not line.startswith("#"):
            break
        else:
            buf.append(line)
    if name:
        jobs[name] = "\n".join(buf)
    return jobs


def _step_run(text, step_name):
    """Return the dedented `run: |` script of the step called step_name."""
    lines = text.splitlines()
    idx = next(i for i, line in enumerate(lines) if line.strip() == f"- name: {step_name}")
    step_indent = len(lines[idx]) - len(lines[idx].lstrip())
    for j in range(idx + 1, len(lines)):
        line = lines[j]
        indent = len(line) - len(line.lstrip())
        if line.strip() and indent <= step_indent:
            break
        if line.strip() == "run: |":
            body_indent = indent + 2
            body = []
            for k in range(j + 1, len(lines)):
                raw = lines[k]
                if raw.strip() and (len(raw) - len(raw.lstrip())) < body_indent:
                    break
                body.append(raw[body_indent:] if raw.strip() else "")
            return "\n".join(body) + "\n"
    raise AssertionError(f"step {step_name!r} has no run block")


def _script_block(text):
    lines = text.splitlines()
    idx = next(i for i, line in enumerate(lines) if line.strip() == "script: |")
    body_indent = len(lines[idx]) - len(lines[idx].lstrip()) + 2
    body = []
    for raw in lines[idx + 1:]:
        if raw.strip() and (len(raw) - len(raw.lstrip())) < body_indent:
            break
        body.append(raw[body_indent:] if raw.strip() else "")
    return "\n".join(body)


class WorkflowStructureTests(unittest.TestCase):
    def test_every_job_has_a_timeout(self):
        for path in sorted(WORKFLOWS.glob("*.yml")):
            jobs = _job_blocks(_read(path))
            self.assertTrue(jobs, f"{path.name}: no jobs parsed")
            for name, block in jobs.items():
                with self.subTest(workflow=path.name, job=name):
                    self.assertRegex(block, r"(?m)^    timeout-minutes: [1-9][0-9]*\s*$")

    def test_workflows_parse_as_yaml_when_pyyaml_is_available(self):
        try:
            import yaml  # noqa: PLC0415
        except ImportError:
            self.skipTest("PyYAML not installed")
        for path in sorted(WORKFLOWS.glob("*.yml")):
            with self.subTest(workflow=path.name):
                self.assertIsInstance(yaml.safe_load(_read(path)), dict)

    def _assert_userns_fixup_precedes_sandbox(self, path):
        text = _read(path)
        install = text.index("apt-get install -y expect bubblewrap")
        sysctl = text.index("sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 || true")
        diag = text.index("bwrap --unshare-net --ro-bind / / true")
        gate = text.index('MAKEWAND_REQUIRE_BWRAP: "1"')
        self.assertLess(install, sysctl, "sysctl must run after bubblewrap is installed")
        self.assertLess(sysctl, diag, "diagnostic must run after the sysctl fix-up")
        self.assertLess(diag, gate, "diagnostic must run before the live sandbox gate")
        for marker in ("scripts/test_gate.sh", "scripts/test_python.py"):
            self.assertLess(sysctl, text.index(marker), f"{marker} uses bwrap; fix-up must precede it")
        diag_script = _step_run(text, "Diagnose bubblewrap sandbox")
        self.assertIn("::error", diag_script)
        self.assertIn("apparmor_restrict_unprivileged_userns", diag_script)
        self.assertRegex(diag_script, r"(?m)^exit 1\s*$")

    def test_ci_relaxes_userns_restriction_before_bwrap(self):
        self._assert_userns_fixup_precedes_sandbox(CI)

    def test_release_relaxes_userns_restriction_before_bwrap(self):
        self._assert_userns_fixup_precedes_sandbox(RELEASE)

    @unittest.skipUnless(shutil.which("bash"), "bash required")
    def test_bwrap_diagnostic_fails_readably_when_bwrap_is_broken(self):
        script = _step_run(_read(CI), "Diagnose bubblewrap sandbox")
        with tempfile.TemporaryDirectory() as tmp:
            fake = Path(tmp) / "bwrap"
            fake.write_text(
                "#!/bin/sh\n"
                'if [ "$1" = --version ]; then echo "bubblewrap 0.9.0"; exit 0; fi\n'
                'echo "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted" >&2\n'
                "exit 1\n",
                encoding="utf-8",
            )
            fake.chmod(0o755)
            env = dict(os.environ, PATH=f"{tmp}{os.pathsep}{os.environ.get('PATH', '')}")
            result = subprocess.run(["bash", "-c", script], env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 1)
        self.assertIn("::error title=bubblewrap sandbox unavailable::", result.stdout)
        self.assertIn("RTM_NEWADDR", result.stdout)

    def test_hard_bwrap_gate_is_kept(self):
        for path in (CI, RELEASE):
            with self.subTest(workflow=path.name):
                text = _read(path)
                self.assertIn('MAKEWAND_REQUIRE_BWRAP: "1"', text)
                self.assertIn("TestEvaluateCandidateFiles_LiveBwrapIsolation", text)

    def test_ci_runs_scanner_self_test_and_version_check(self):
        text = _read(CI)
        self.assertIn("./scripts/check_secrets.sh", text)
        self.assertIn("bash scripts/test_check_secrets.sh", text)
        self.assertIn("bash scripts/check_version.sh", text)


class ReleaseWorkflowTests(unittest.TestCase):
    def test_release_requires_tag_to_match_source_version(self):
        test_job = _job_blocks(_read(RELEASE))["test"]
        self.assertIn('bash scripts/check_version.sh --tag "${GITHUB_REF_NAME}"', test_job)
        self.assertIn("./scripts/check_secrets.sh", test_job)

    def test_binaries_report_bare_semver_like_source_installs(self):
        build = _step_run(_read(RELEASE), "Build release artifacts")
        self.assertIn('SEMVER="${VERSION#v}"', build)
        self.assertIn("buildinfo.Version=${SEMVER}", build)
        self.assertNotIn("buildinfo.Version=${VERSION}", build)
        self.assertIn('stage_python_engine.py "${outdir}" "${SEMVER}"', build)
        self.assertRegex(build, r'release_contract\.sh "[^"]+" "\$\{SEMVER\}"')
        # Archive names keep the tag so package manifests stay valid.
        self.assertIn('base="makewand_${VERSION}_${goos}_${goarch}"', build)

    @unittest.skipUnless(shutil.which("bash") and shutil.which("git"), "bash and git required")
    def test_release_notes_install_the_prebuilt_archive(self):
        script = _step_run(_read(RELEASE), "Build release notes")
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            env = dict(os.environ, GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM="1",
                       GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@example.invalid",
                       GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@example.invalid")
            env.pop("PACKAGE_REPO_TOKEN", None)
            env.update(GITHUB_REF_NAME="v9.8.7", GITHUB_REPOSITORY="acme/makewand")
            for args in (["init", "-q"], ["commit", "-q", "--allow-empty", "-m", "first"],
                         ["tag", "v9.8.6"], ["commit", "-q", "--allow-empty", "-m", "second"],
                         ["tag", "v9.8.7"]):
                subprocess.run(["git", *args], cwd=repo, env=env, check=True)
            (repo / "dist").mkdir()
            subprocess.run(["bash", "-c", script], cwd=repo, env=env, check=True)
            notes = (repo / "dist" / "release-notes.md").read_text(encoding="utf-8")
        install = notes.index("## Install (prebuilt")
        archive = notes.index("https://github.com/acme/makewand/releases/download/v9.8.7/")
        source = notes.index("scripts/install.sh | bash")
        self.assertLess(install, archive)
        self.assertLess(archive, source, "the Go source installer must not be the primary install path")
        self.assertIn("makewand_v9.8.7_${PLATFORM}.tar.gz", notes)
        self.assertIn("sha256sum -c -", notes)
        self.assertEqual(notes.count("```") % 2, 0, "unbalanced code fences")
        self.assertNotIn("brew install", notes, "package managers are only advertised when published")
        self.assertIn("**Full Changelog**: https://github.com/acme/makewand/compare/v9.8.6...v9.8.7", notes)


@unittest.skipUnless(shutil.which("node"), "node required to execute the github-script body")
class NormalizeReleaseMetadataTests(unittest.TestCase):
    RELEASES = [
        {
            "id": 1, "tag_name": "v1.0.0", "draft": False, "prerelease": False,
            "body": "## Highlights\n- fix a\n- fix a\n\n## Install\n```bash\necho one\n```\n\n"
                    "```bash\necho two\n```\nsee https://github.com/oldowner/makewand/issues/1",
        },
        {
            "id": 2, "tag_name": "v1.1.0", "draft": False, "prerelease": False,
            "body": "- b\n\n**Full Changelog**: https://github.com/acme/makewand/compare/v1.0.0...v1.1.0",
        },
    ]

    def _run(self, **env_overrides):
        script = _script_block(_read(NORMALIZE))
        harness = (
            "const releases = " + json.dumps(self.RELEASES) + ";\n"
            "const context = {repo: {owner: 'acme', repo: 'makewand'}};\n"
            "const updates = [], logs = [];\n"
            "const core = {info: (m) => logs.push(String(m))};\n"
            "const github = {paginate: async () => releases,\n"
            "  rest: {repos: {listReleases: {}, updateRelease: async (a) => { updates.push(a); }}}};\n"
            "(async () => {\n" + script + "\n})().then(() => console.log(JSON.stringify({updates, logs})),\n"
            "  (e) => { console.error(e); process.exit(1); });\n"
        )
        env = {k: v for k, v in os.environ.items() if k not in ("DRY_RUN", "LEGACY_REPO_URLS")}
        env.update(env_overrides)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "harness.js"
            path.write_text(harness, encoding="utf-8")
            result = subprocess.run(["node", str(path)], env=env, capture_output=True, text=True, timeout=60)
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout.strip().splitlines()[-1])

    def test_manual_trigger_only(self):
        text = _read(NORMALIZE)
        on_block = text.split("\non:", 1)[1].split("\npermissions:", 1)[0]
        self.assertIn("workflow_dispatch:", on_block)
        self.assertNotRegex(on_block, r"(?m)^  push:")
        self.assertRegex(on_block, r"dry_run:[\s\S]*?default: true")

    def test_no_hardcoded_repository_owner(self):
        text = _read(NORMALIZE)
        self.assertEqual(re.findall(r"https://github\.com/[A-Za-z0-9_.-]+/", text), [])

    def test_defaults_to_dry_run(self):
        out = self._run()
        self.assertEqual(out["updates"], [])
        self.assertIn("[dry run] Would update: v1.0.0", out["logs"])

    def test_apply_rewrites_legacy_links_and_keeps_code_fences(self):
        out = self._run(DRY_RUN="false", LEGACY_REPO_URLS="https://github.com/oldowner/makewand/")
        self.assertEqual([u["release_id"] for u in out["updates"]], [1])
        body = out["updates"][0]["body"]
        self.assertIn("https://github.com/acme/makewand/issues/1", body)
        self.assertNotIn("oldowner", body)
        self.assertEqual(body.count("- fix a"), 1, "duplicate bullets are removed")
        self.assertEqual(body.count("```"), 4, "code fences must survive de-duplication")
        self.assertTrue(body.endswith("**Full Changelog**: https://github.com/acme/makewand/commits/v1.0.0"))


if __name__ == "__main__":
    unittest.main()
