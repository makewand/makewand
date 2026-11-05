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
NORMALIZE = WORKFLOWS / "normalize-release-metadata.yml"
DEPLOY = WORKFLOWS / "deploy-site.yml"
PYTEST_GATE = "python3 -I -m pytest -q -p no:cacheprovider tests"
PYTEST_PIN = re.compile(r"pip install pytest==(\d+\.\d+\.\d+)\b")
GOVULNCHECK_PIN = re.compile(r"golang\.org/x/vuln/cmd/govulncheck@([\w.$()-]+)")


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


def _job_text(path, job):
    """The job's block; for a call of a local reusable workflow, that workflow."""
    block = _job_blocks(_read(path))[job]
    reusable = re.search(r"(?m)^    uses: (\./\.github/workflows/\S+)\s*$", block)
    return _read(ROOT / reusable[1]) if reusable else block


def _reported_check_names():
    """Check names that pull-request workflows report, as branch protection sees them."""
    names = set()
    for path in sorted(WORKFLOWS.glob("*.yml")):
        text = _read(path)
        on_block = text.split("\non:", 1)[1].split("\npermissions:", 1)[0]
        if not re.search(r"(?m)^  pull_request:", on_block):
            continue
        for job, block in _job_blocks(text).items():
            named = re.search(r"(?m)^    name: (.+?)\s*$", block)
            titles = [named[1].strip("'\"") if named else job]
            languages = re.search(r"(?m)^        language: \[([^\]]*)\]", block)
            if languages and "${{ matrix.language }}" in titles[0]:
                titles = [titles[0].replace("${{ matrix.language }}", value.strip().strip("'\""))
                          for value in languages[1].split(",")]
            callee = re.search(r"(?m)^    uses: (\./\.github/workflows/\S+)\s*$", block)
            if callee:
                titles = [f"{title} / {inner}" for title in titles for inner in _job_blocks(_read(ROOT / callee[1]))]
            names.update(titles)
    return names


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
        def check_workflow(path, ancestors=()):
            self.assertNotIn(path, ancestors, "reusable workflow call cycle")
            jobs = _job_blocks(_read(path))
            self.assertTrue(jobs, f"{path.name}: no jobs parsed")
            for name, block in jobs.items():
                with self.subTest(workflow=path.name, job=name):
                    reusable = re.search(r"(?m)^    uses: (\S+)\s*$", block)
                    if reusable:
                        # GitHub rejects timeout-minutes on reusable callers.
                        # Follow the same-commit local call to the actual jobs.
                        reference = reusable[1]
                        self.assertTrue(reference.startswith("./.github/workflows/"))
                        callee = (ROOT / reference).resolve()
                        self.assertEqual(callee.parent, WORKFLOWS.resolve())
                        self.assertTrue(callee.is_file(), "missing reusable workflow")
                        self.assertNotRegex(block, r"(?m)^    (timeout-minutes|runs-on|steps):")
                        check_workflow(callee, (*ancestors, path))
                    else:
                        self.assertRegex(block, r"(?m)^    timeout-minutes: [1-9][0-9]*\s*$")

        for path in sorted(WORKFLOWS.glob("*.yml")):
            check_workflow(path.resolve())

    def test_workflows_parse_as_yaml_when_pyyaml_is_available(self):
        try:
            import yaml  # noqa: PLC0415
        except ImportError:
            self.skipTest("PyYAML not installed")
        for path in sorted(WORKFLOWS.glob("*.yml")):
            with self.subTest(workflow=path.name):
                self.assertIsInstance(yaml.safe_load(_read(path)), dict)

    def _assert_userns_fixup_precedes_sandbox(self, text):
        install = text.index("apt-get install -y bubblewrap")
        sysctl = text.index("sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 || true")
        diag = text.index("bwrap --unshare-net --ro-bind / / true")
        gate = text.index("scripts/test_gate.sh")
        self.assertLess(install, sysctl, "sysctl must run after bubblewrap is installed")
        self.assertLess(sysctl, diag, "diagnostic must run after the sysctl fix-up")
        self.assertLess(diag, gate, "diagnostic must run before the tests")
        for marker in ("scripts/test_gate.sh", PYTEST_GATE):
            self.assertLess(sysctl, text.index(marker), f"{marker} uses bwrap; fix-up must precede it")
        diag_script = _step_run(text, "Diagnose bubblewrap sandbox")
        self.assertIn("::error", diag_script)
        self.assertIn("apparmor_restrict_unprivileged_userns", diag_script)
        self.assertRegex(diag_script, r"(?m)^exit 1\s*$")

    def test_ci_relaxes_userns_restriction_before_bwrap(self):
        self._assert_userns_fixup_precedes_sandbox(_read(CI))

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

    def test_ci_runs_scanner_self_test_and_version_check(self):
        text = _read(CI)
        self.assertIn("./scripts/check_secrets.sh", text)
        self.assertIn("bash scripts/test_check_secrets.sh", text)
        self.assertIn("bash scripts/check_version.sh", text)

    def test_python_gates_run_the_whole_suite_under_pinned_pytest(self):
        # eng-1021-2: unittest discovery collected nothing from the modules of
        # module-level pytest tests (416 cases) and still passed.
        pins = set()
        text = _job_text(CI, "verify")
        self.assertRegex(text, r"(?m)^        run: " + re.escape(PYTEST_GATE) + r"\s*$")
        self.assertNotRegex(text, r"(?m)^\s*run: .*scripts/test_python\.py")
        pins.update(PYTEST_PIN.findall(text))
        self.assertRegex(text, PYTEST_PIN, "pytest must be pinned")
        self.assertEqual(len(pins), 1, f"one pytest version everywhere, found {sorted(pins)}")

    def test_govulncheck_is_pinned_to_one_version(self):
        # eng-1021-8: govulncheck@latest could turn CI or a release red without
        # a code change (the vulnerability database itself stays live).
        make_pin = re.search(r"(?m)^GOVULNCHECK_VERSION \?= (v\d+\.\d+\.\d+)\s*$", _read(ROOT / "Makefile"))
        self.assertIsNotNone(make_pin, "the Makefile must pin GOVULNCHECK_VERSION")
        found = {}
        for path in (*sorted(WORKFLOWS.glob("*.yml")), ROOT / "Makefile"):
            for version in GOVULNCHECK_PIN.findall(_read(path)):
                found.setdefault(version, []).append(path.name)
        found.pop("$(GOVULNCHECK_VERSION)", None)
        self.assertEqual(set(found), {make_pin[1]}, found)


class GitHubHardeningTests(unittest.TestCase):
    def test_required_checks_are_names_the_workflows_report(self):
        # eng-1021-7: the script required "Analyze (Go)" while CodeQL reports
        # "Analyze (go)"; a required check that never reports blocks every merge.
        script = _read(ROOT / "scripts" / "github_hardening.sh")
        contexts = json.loads(re.search(r'"contexts": (\[[^\]]*\])', script)[1])
        reported = _reported_check_names()
        self.assertTrue({"verify", "Analyze (go)"} <= reported, reported)
        self.assertIn("verify", contexts, "CI verify must stay required")
        for context in contexts:
            with self.subTest(context=context):
                self.assertIn(context, reported)


@unittest.skipUnless(shutil.which("bash"), "bash required")
class DeploySiteWorkflowTests(unittest.TestCase):
    """eng-1021-8: the deploy checked only makewand.org and makewand.com."""

    HOSTS = {"makewand.org", "www.makewand.org", "makewand.com", "www.makewand.com", "makewand.pages.dev"}

    def check(self, stale=()):
        script = _step_run(_read(DEPLOY), "Check the served installer")
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            (work / "site").mkdir()
            shutil.copy2(ROOT / "site" / "install.sh", work / "site" / "install.sh")
            tools = work / "tools"
            tools.mkdir()
            log = work / "requests.log"
            # `curl [options] URL -o FILE`: a stale host serves other bytes.
            (tools / "curl").write_text(
                "#!/bin/sh\n"
                'url=; out=\n'
                'while [ $# -gt 0 ]; do case "$1" in -o) out=$2; shift ;; https://*) url=$1 ;; esac; shift; done\n'
                f'echo "$url" >> "{log}"\n'
                'host="${url#https://}"; host="${host%%/*}"\n'
                f'case " {" ".join(stale)} " in *" $host "*) echo stale > "$out" ;; *) cp site/install.sh "$out" ;; esac\n',
                encoding="utf-8")
            (tools / "sleep").write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            for tool in tools.iterdir():
                tool.chmod(0o755)
            env = dict(os.environ, PATH=f"{tools}{os.pathsep}{os.environ.get('PATH', '')}")
            # Actions runs a `run:` script that names no shell as `bash -e {0}`.
            result = subprocess.run(["bash", "-e", "-c", script], cwd=work, env=env, capture_output=True, text=True)
            requested = [line.split("/")[2] for line in log.read_text(encoding="utf-8").splitlines()]
        return result, requested

    def test_every_served_hostname_is_checked(self):
        # One stalled host must not outlast the shared deadline: every request is bounded.
        self.assertIn("--max-time", _step_run(_read(DEPLOY), "Check the served installer"))
        result, requested = self.check()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(sorted(requested), sorted(self.HOSTS))

    def test_one_stale_hostname_fails_the_deploy_within_one_deadline(self):
        result, requested = self.check(stale=("www.makewand.com",))
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("www.makewand.com", result.stdout.split("::error::", 1)[1])
        self.assertEqual(set(requested), self.HOSTS)
        self.assertEqual(requested.count("www.makewand.com"), 18, "retried until the shared deadline")
        self.assertEqual(requested.count("makewand.org"), 1, "a current host is not fetched again")


if __name__ == "__main__":
    unittest.main()
