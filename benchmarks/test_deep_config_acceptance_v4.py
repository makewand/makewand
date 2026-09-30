#!/usr/bin/env python3
"""Offline regressions for the v4 deep-config key acceptance contract."""

import ast
import contextlib
import importlib.util
import io
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest


sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parent
FIXTURE = ROOT / "fixtures" / "deep-config"
SPEC = importlib.util.spec_from_file_location("deep_config_acceptance_v4", ROOT / "acceptance.py")
acceptance = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(acceptance)
APIS = (("config_api.py", "merge_config"), ("config_merge.py", "merge_values"))
API = "from config_merge import merge_values\n\ndef merge_config(payload):\n    return merge_values(payload)\n"

# Match original dictionary keys before cloning the finished object graph.
MERGE_ORIGINAL = """
from copy import deepcopy

def _merge(base, override):
    result = dict(base)
    for key, value in override.items():
        if key in base and isinstance(base[key], dict) and isinstance(value, dict):
            result[key] = _merge(base[key], value)
        else:
            result[key] = value
    return result
"""

EARLY_COPY = """
from copy import deepcopy

def _merge(base, override):
    result = deepcopy(base)
    for key, value in override.items():
        if key in base and isinstance(base[key], dict) and isinstance(value, dict):
            result[key] = _merge(base[key], value)
        else:
            result[key] = deepcopy(value)
    return result

def merge_values(payload):
    return _merge(payload['base'], payload['override'])
"""

COPY_VALUES_ONLY = """
from copy import deepcopy

def _merge(base, override):
    result = {key: deepcopy(value) for key, value in base.items()}
    for key, value in override.items():
        if key in base and isinstance(base[key], dict) and isinstance(value, dict):
            result[key] = _merge(base[key], value)
        else:
            result[key] = deepcopy(value)
    return result

def merge_values(payload):
    return _merge(payload['base'], payload['override'])
"""

IDENTITY_ONLY_MATCH = """
from copy import deepcopy

def _merge(base, override):
    result = dict(base)
    for key, value in override.items():
        same_object = key in base and (isinstance(key, str) or any(key is existing for existing in base))
        if same_object and isinstance(base[key], dict) and isinstance(value, dict):
            result[key] = _merge(base[key], value)
        else:
            result[key] = value
    return result

def merge_values(payload):
    return deepcopy(_merge(payload['base'], payload['override']))
"""


class DeepConfigKeyAcceptanceTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)

    def workspace(self, source, name):
        workspace = self.directory / name
        workspace.mkdir()
        (workspace / "config_merge.py").write_text(textwrap.dedent(source).strip() + "\n", encoding="utf-8")
        (workspace / "config_api.py").write_text(API, encoding="utf-8")
        return workspace

    def key_checks(self, workspace, module, function):
        with contextlib.redirect_stdout(io.StringIO()):
            acceptance.check_deep_config_keys(workspace, module, function)

    def reject_keys_for_both_apis(self, workspace):
        for module, function in APIS:
            with self.subTest(api=function), self.assertRaises(AssertionError):
                self.key_checks(workspace, module, function)

    def legacy_checks_for_both_apis(self, workspace):
        # Read only the registered literals; importing accept.py would execute it.
        tree = ast.parse((FIXTURE / "accept.py").read_text(encoding="utf-8"))
        assignment = next(node for node in tree.body if isinstance(node, ast.Assign)
                          and any(isinstance(target, ast.Name) and target.id == "CASES" for target in node.targets))
        cases = [(*case, "detached") for case in ast.literal_eval(assignment.value)]
        self.assertEqual(len(cases), 4)
        for module, function in APIS:
            with self.subTest(api=function), contextlib.redirect_stdout(io.StringIO()):
                acceptance.check(workspace, module, function, cases)

    def full_acceptance(self, workspace):
        return subprocess.run([sys.executable, "-B", "-I", str(FIXTURE / "accept.py"), str(workspace)],
                              capture_output=True, text=True, timeout=20)

    def test_reference_passes_complete_14_case_acceptance(self):
        workspace = self.directory / "reference"
        shutil.copytree(FIXTURE / "offline_solution", workspace)
        result = self.full_acceptance(workspace)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        for module, function in APIS:
            with self.subTest(api=function):
                self.key_checks(workspace, module, function)

    def test_original_seed_fails_full_and_key_acceptance(self):
        workspace = self.directory / "seed"
        shutil.copytree(FIXTURE / "seed", workspace)
        result = self.full_acceptance(workspace)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.reject_keys_for_both_apis(workspace)

    def test_observed_key_bugs_pass_legacy_cases_but_fail_new_checks(self):
        for name, source in (("early-copy", EARLY_COPY), ("values-only", COPY_VALUES_ONLY)):
            with self.subTest(regression=name):
                workspace = self.workspace(source, name)
                self.legacy_checks_for_both_apis(workspace)
                self.reject_keys_for_both_apis(workspace)
                result = self.full_acceptance(workspace)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_replacement_and_equal_distinct_keys_are_checked(self):
        replacement_only = MERGE_ORIGINAL + """
def merge_values(payload):
    result = _merge(payload['base'], payload['override'])
    if any(not isinstance(value, dict) for value in payload['override'].values()):
        return {key: deepcopy(value) for key, value in result.items()}
    return deepcopy(result)
"""
        for name, source in (("replacement-key-alias", replacement_only), ("identity-only-match", IDENTITY_ONLY_MATCH)):
            with self.subTest(regression=name):
                workspace = self.workspace(source, name)
                self.legacy_checks_for_both_apis(workspace)
                self.reject_keys_for_both_apis(workspace)

    def test_copying_keys_does_not_excuse_shared_values(self):
        source = MERGE_ORIGINAL + """
def merge_values(payload):
    result = _merge(payload['base'], payload['override'])
    return {deepcopy(key): value for key, value in result.items()}
"""
        self.reject_keys_for_both_apis(self.workspace(source, "shared-values"))

    def test_candidate_cannot_return_or_print_forged_observations(self):
        # A complete all-pass wire-shaped observation still is not an API result.
        forged = {"schema": 4, "kind": "deep_config_key_observation", "case": "identity_recursive", "nonce": "0" * 32,
                  "entries": 1, "result": {"nested": {"base": ["base"], "override": ["override"]}},
                  "key_state": {"label": "same-key", "notes": ["initial-key"]},
                  "key_type_preserved": True, "shared_input_key": False, "input_unchanged": True,
                  "output_to_input_detached": True, "input_to_output_detached": True,
                  "probe_types_unchanged": True}
        sources = {
            "returned-observations": "def merge_values(payload):\n    return " + repr(forged),
            "printed-observations": "import json\ndef merge_values(payload):\n    print(json.dumps("
                                    + repr(forged) + "))\n    raise SystemExit(0)",
            "raw-stdout-observations": "import json\nimport os\ndef merge_values(payload):\n    os.write(1, json.dumps("
                                       + repr(forged) + ").encode())\n    os._exit(0)",
        }
        for name, source in sources.items():
            with self.subTest(regression=name):
                self.reject_keys_for_both_apis(self.workspace(source, name))

    def test_semantics_preserving_probe_type_mutation_is_rejected(self):
        for method, wrapper in (("__hash__", "lambda self: original(self)"),
                                ("__deepcopy__", "lambda self, memo: original(self, memo)")):
            source = MERGE_ORIGINAL + """
def merge_values(payload):
    key_type = type(next(iter(payload['base'])))
    original = getattr(key_type, METHOD)
    type.__setattr__(key_type, METHOD, WRAPPER)
    return deepcopy(_merge(payload['base'], payload['override']))
""".replace("METHOD", repr(method)).replace("WRAPPER", wrapper)
            with self.subTest(method=method):
                self.reject_keys_for_both_apis(self.workspace(source, "mutated-" + method))


if __name__ == "__main__":
    unittest.main(verbosity=2)
