"""Strict result protocols for the native Windows release gate."""
import ast
import json
import re

MODULES = ["test_native_windows", "test_native_windows_pipeline", "test_native_delivery",
           "test_candidate_recovery", "test_windows_runtime_regression"]
WINDOWS_SKIPS = {
    "test_native_windows.WindowsPathTests.test_windows_backends_are_not_silently_emulated_on_other_platforms": "The native backend exists on this platform",
    **{"test_candidate_recovery.CandidateRecoveryTests." + name: reason for name, reason in (
        ("test_copy_checks_captured_root_identity_after_path_is_restored", "Windows pins the workspace against root replacement"),
        ("test_legacy_windows_journal_without_security_stops_before_recovery", "protocol stub complements native Windows DACL tests"),
        ("test_recovery_rechecks_restored_modes_before_publishing_completion", "requires POSIX file modes"),
        ("test_remove_checks_captured_root_identity_after_path_is_restored", "Windows pins the workspace against root replacement"),
        ("test_windows_existing_file_security_conflict_blocks_all_recovery", "protocol stub complements native Windows DACL tests"),
        ("test_windows_new_file_security_conflict_blocks_all_recovery", "protocol stub complements native Windows DACL tests"),
        ("test_workspace_replacement_preserves_new_directory_during_failure", "Windows pins the workspace against root replacement"),
    )},
}


class ResultError(ValueError):
    def __init__(self, message, report):
        super().__init__(message)
        self.report = report


def python_inventory(root):
    """The current five modules define direct unittest methods, not generated tests.

    Avoid importing test fixtures during metadata collection. The actual verbose
    execution must match every declared method, so dynamic or inherited additions
    fail closed until the inventory contract is explicitly extended.
    """
    names = set()
    for module in MODULES:
        tree = ast.parse((root / "tests" / (module + ".py")).read_text(encoding="utf-8"))
        for cls in tree.body:
            if isinstance(cls, ast.ClassDef):
                for method in cls.body:
                    if isinstance(method, (ast.FunctionDef, ast.AsyncFunctionDef)) and method.name.startswith("test_"):
                        names.add(module + "." + cls.name + "." + method.name)
    if not names:
        raise ValueError("empty Python method inventory")
    return names


def summarize_python(data, expected, encoding):
    text = data.decode(encoding, errors="strict")
    rows, pending, problems = [], None, []
    start = re.compile(r"^\s*test\S+ \(([^)]+)\)(?: \([^)]*\))* \.\.\. (.*)$")
    status = re.compile(r"^(ok|FAIL|ERROR|skipped(?:\s.*)?)$")
    prefix = re.split(r"(?m)^={70}\r?\n(?:FAIL|ERROR): ", text, maxsplit=1)[0]
    for line in prefix.splitlines():
        match = start.match(line)
        if match:
            if pending is not None:
                problems.append("unfinished method: " + pending)
            pending, suffix = match[1], match[2].strip()
        else:
            suffix = line.strip()
        if pending is not None and status.fullmatch(suffix):
            rows.append({"name": pending, "result": suffix})
            pending = None
    if pending is not None:
        problems.append("unfinished method: " + pending)
    observed = [row["name"] for row in rows]
    if len(observed) != len(set(observed)) or set(observed) != set(expected):
        problems.append("method outcomes differ from the complete source inventory")
    footer = re.search(r"(?m)^Ran (\d+) tests? in [^\r\n]+\r?\n\s*OK(?: \(skipped=(\d+)\))?\s*\Z", text)
    if not footer or int(footer[1]) != len(expected):
        problems.append("missing or mismatched successful standard unittest footer")
    skips = []
    for row in rows:
        result = row["result"]
        if result == "ok":
            continue
        if result.startswith("skipped "):
            try:
                reason = ast.literal_eval(result[len("skipped "):])
            except (SyntaxError, ValueError):
                reason = None
            skips.append({"test": row["name"], "reason": reason})
            if WINDOWS_SKIPS.get(row["name"]) != reason:
                problems.append("unexpected skip: " + row["name"])
        else:
            problems.append("failed method: " + row["name"])
    if footer and int(footer[2] or 0) != len(skips):
        problems.append("unittest skip count differs from explicit records")
    report = {"ran": len(rows), "expected_methods": sorted(expected), "methods": rows,
              "pass": sum(r["result"] == "ok" for r in rows), "skips": skips, "modules": MODULES}
    if problems:
        raise ResultError("; ".join(problems), report)
    return report


def summarize_go(data, expected, *, full_race=False):
    packages, started, tests, output, problems = {}, set(), {}, {}, []
    for line in data.decode("utf-8", errors="strict").splitlines():
        row = json.loads(line)
        package, test, action = row.get("Package"), row.get("Test"), row.get("Action")
        if "Package" in row and package not in expected:
            problems.append("event from unexpected package: " + str(package))
            continue
        if not package:
            continue  # build diagnostics remain in the raw stream and exit code
        key = (package, test)
        if action == "output" and test:
            output.setdefault(key, []).append(row.get("Output", ""))
        if action == "run" and test:
            if key in started:
                problems.append("duplicate test start: " + test)
            started.add(key)
        if action in ("pass", "fail", "skip"):
            target, identity = (tests, key) if test else (packages, package)
            if identity in target:
                problems.append("duplicate/conflicting completion: " + str(identity))
            else:
                target[identity] = action
    if set(packages) != set(expected):
        problems.append("package completions differ from the exact expected package set")
    if set(tests) != started:
        problems.append("test RUN/completion sets differ")
    for package, has_tests in expected.items():
        passed = any(p == package and a == "pass" for (p, _), a in tests.items())
        if packages.get(package) == "fail" or (has_tests and (not passed or packages.get(package) != "pass")):
            problems.append("failed/missing/entirely skipped test package: " + package)
    skips = []
    for (package, test), action in tests.items():
        if action == "fail":
            problems.append("failed test: " + package + "." + test)
        if action == "skip":
            reason = "".join(output.get((package, test), []))
            skips.append({"package": package, "test": test, "reason": reason})
            if test.startswith(("TestWindows", "TestStoreWindows", "TestApplyWindows", "TestPendingApproval", "TestCLIProcess", "TestRawCLI", "TestEngineProcess", "TestPreviewProcess", "TestOutputCapture", "TestMixedGoPython", "TestPythonJudge", "TestClaudeCLIError", "TestClaudeCLIExecutionError", "TestClaudeCLIStreamError")) or test == "TestStoreRejectsSymlinkLockWithoutChangingTarget":
                problems.append("required native test skipped: " + test)
    report = {"packages": packages, "no_test_file_packages": [p for p, has in expected.items() if not has],
              "pass": sum(a == "pass" for a in tests.values()), "tests": [{"package": p, "test": t, "result": a} for (p, t), a in tests.items()],
              "skips": skips, "full_race": full_race}
    if problems:
        raise ResultError("; ".join(problems), report)
    return report
