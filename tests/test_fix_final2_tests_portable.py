"""The Python tests pass wherever the checkout and the temporary directory live (tests-portable-py).

* CI's first step (scripts/check_secrets.sh) refuses workstation home paths in every
  tracked file, tests included, unless the user name is one of its placeholders; two
  test files used a one-letter user name that is not one.
* With the checkout itself under /tmp, a test running a DAG in the hard-coded workspace
  "/tmp" was refused by the test harness (the workspace contains the source tree), and
  so was the nested `unittest discover` run that includes it.
* test_tmp_workspace_scan_is_shallow_and_fast assumed tempfile puts its directory
  directly in /tmp, and failed with TMPDIR=/tmp/x.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import ast
import io
import os
import re
import tempfile
import unittest
from unittest.mock import patch

TESTS_DIR = Path(__file__).resolve().parent
REPO_ROOT = TESTS_DIR.parent
# Entry points that write to (or snapshot) their workspace, and cwd's position when passed positionally.
WRITING_ENTRY_POINTS = {"execute_task_dag": 1, "run_pipeline": 1, "_run_pipeline_impl": 1, "run_workflow": None,
                        "run_race": 1, "_run_race_impl": 1}
SHARED_TEMPORARY_ROOTS = {"/tmp", "/tmp/", "/var/tmp", "/var/tmp/", "/private/tmp"}


def _placeholder_users():
    script = (REPO_ROOT / "scripts" / "check_secrets.sh").read_text(encoding="utf-8")
    return set(re.search(r'^PLACEHOLDER_USERS="([^"]*)"', script, re.M).group(1).split())


class PortableTestTreeTests(unittest.TestCase):
    def test_no_python_test_names_a_workstation_home(self):
        allowed = _placeholder_users()
        self.assertIn("alice", allowed)
        rule = re.compile(r"(?:^|[^A-Za-z0-9_.~$-])/(?:home|Users)/([A-Za-z0-9._-]+)/")
        found = []
        for path in sorted(TESTS_DIR.glob("*.py")):
            for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
                found += [f"{path.name}:{number}: {user}" for user in rule.findall(line) if user.lower() not in allowed]
        self.assertEqual(found, [])

    def test_no_test_runs_a_writing_entry_point_in_the_shared_temporary_directory(self):
        def shared(node):
            if isinstance(node, ast.Constant) and isinstance(node.value, str):
                return node.value in SHARED_TEMPORARY_ROOTS
            return (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
                    and node.func.attr == "gettempdir")

        offenders = []
        for path in sorted(TESTS_DIR.glob("test_*.py")):
            tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
            for node in ast.walk(tree):
                if not isinstance(node, ast.Call):
                    continue
                name = node.func.id if isinstance(node.func, ast.Name) else getattr(node.func, "attr", None)
                if name not in WRITING_ENTRY_POINTS:
                    continue
                position = WRITING_ENTRY_POINTS[name]
                cwd = [keyword.value for keyword in node.keywords if keyword.arg == "cwd"]
                if position is not None and len(node.args) > position:
                    cwd.append(node.args[position])
                if any(shared(value) for value in cwd):
                    offenders.append(f"{path.name}:{node.lineno} {name}")
        self.assertEqual(offenders, [])

    @unittest.skipIf(os.name == "nt", "a POSIX /tmp and pathname sockets")
    def test_the_tmp_scan_test_does_not_depend_on_tmpdir(self):
        import test_g4_sandbox_scan
        other = tempfile.mkdtemp(prefix="tdx", dir="/tmp")  # short: the test binds pathname sockets
        self.addCleanup(os.rmdir, other)
        suite = unittest.defaultTestLoader.loadTestsFromName(
            "TestBoundedWorkspaceScan.test_tmp_workspace_scan_is_shallow_and_fast", test_g4_sandbox_scan)
        with patch.object(tempfile, "tempdir", other):
            result = unittest.TextTestRunner(stream=io.StringIO(), verbosity=0).run(suite)
        self.assertEqual(result.testsRun, 1)
        self.assertTrue(result.wasSuccessful(), result.failures + result.errors)
        self.assertEqual(os.listdir(other), [])


if __name__ == "__main__":
    unittest.main()
