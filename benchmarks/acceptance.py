"""Trusted assertions compare child results; candidate early exit is a failure."""
import json
import os
import secrets
import signal
import subprocess
import sys
import tempfile
from pathlib import Path

WORKER = Path(__file__).with_name("acceptance_worker.py")


def _run_candidate(workspace, module_name, function_name, request):
    module_path = Path(workspace).resolve() / module_name
    with tempfile.TemporaryFile() as output, tempfile.TemporaryFile() as errors:
        process = subprocess.Popen([sys.executable, "-I", str(WORKER), str(module_path), function_name],
                                   stdin=subprocess.PIPE, stdout=output, stderr=errors,
                                   cwd=workspace, start_new_session=os.name != "nt",
                                   env=dict(os.environ, MAKEWAND_ACCEPTANCE_PARENT_PID=str(os.getpid())))
        try:
            process.communicate(json.dumps(request).encode(), timeout=5)
        except subprocess.TimeoutExpired:
            if os.name == "nt":
                subprocess.run(["taskkill", "/F", "/T", "/PID", str(process.pid)], capture_output=True)
            else:
                os.killpg(process.pid, signal.SIGKILL)
            process.wait()
            raise AssertionError("candidate timed out")
        if process.returncode != 0:
            raise AssertionError(f"candidate exited {process.returncode}")
        if output.tell() > 1024 * 1024:
            raise AssertionError("candidate result exceeds protocol limit")
        output.seek(0)
        try:
            return json.loads(output.read())
        except (ValueError, UnicodeError) as error:
            raise AssertionError("candidate did not return a complete result") from error


def check(workspace, module_name, function_name, cases):
    for case in cases:
        values, expected = case[:2]
        request = {"values": values, "iterator": len(case) > 2 and case[2] == "iterator",
                   "verify_detached": len(case) > 2 and case[2] == "detached"}
        response = _run_candidate(workspace, module_name, function_name, request)
        canonical_input = json.loads(json.dumps(values))
        if not isinstance(response, dict) or response.get("schema") != 1 or response.get("input") != canonical_input:
            raise AssertionError("candidate mutated input or returned an invalid protocol")
        if expected is ValueError:
            if set(response) != {"schema", "kind", "input"} or response["kind"] != "value_error":
                raise AssertionError("reversed bounds accepted")
        else:
            if set(response) != {"schema", "kind", "input", "result"} or response["kind"] != "result":
                raise AssertionError("candidate did not return a result")
            canonical_expected = json.loads(json.dumps(expected))
            if response["result"] != canonical_expected:
                raise AssertionError((values, response["result"], canonical_expected))
    print("independent acceptance passed")


def check_deep_config_keys(workspace, module_name, function_name):
    """Protocol v4: fixed worker-owned keys, with assertions in this parent.

    Observations are produced by the harness, never by a candidate callback or
    a candidate-supplied probe type. This is not a malicious-code security boundary.
    """
    if (module_name, function_name) not in (("config_api.py", "merge_config"), ("config_merge.py", "merge_values")):
        raise ValueError("unsupported deep-config public API")
    for case in ("identity_recursive", "identity_replace", "equal_recursive"):
        nonce = secrets.token_hex(16)
        response = _run_candidate(workspace, module_name, function_name,
                                  {"operation": "deep_config_keys_v4", "case": case, "nonce": nonce})
        expected = {"schema": 4, "kind": "deep_config_key_observation", "case": case,
                    "nonce": nonce,
                    "entries": 1, "result": ["override"] if case == "identity_replace" else
                    {"nested": {"base": ["base"], "override": ["override"]}},
                    "key_state": {"label": "same-key", "notes": ["initial-key"]},
                    "key_type_preserved": True, "shared_input_key": False,
                    "input_unchanged": True, "output_to_input_detached": True,
                    "input_to_output_detached": True, "probe_types_unchanged": True}
        if not isinstance(response, dict) or set(response) != set(expected):
            raise AssertionError("invalid deep-config v4 observation protocol")
        if type(response.get("schema")) is not int or type(response.get("entries")) is not int:
            raise AssertionError("invalid deep-config v4 observation types")
        for field in ("key_type_preserved", "shared_input_key", "input_unchanged",
                      "output_to_input_detached", "input_to_output_detached", "probe_types_unchanged"):
            if type(response.get(field)) is not bool:
                raise AssertionError("invalid deep-config v4 observation types")
        if response != expected:
            raise AssertionError("deep-config v4 key matching or input isolation failed")
    print("independent deep-config key acceptance v4 passed")
