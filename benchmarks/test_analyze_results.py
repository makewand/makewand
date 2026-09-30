#!/usr/bin/env python3
"""Offline analysis regressions built from isolated trial directories."""

import importlib.util
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("benchmark_analysis", ROOT / "analyze_results.py")
analysis = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = analysis
SPEC.loader.exec_module(analysis)


class AnalysisTests(unittest.TestCase):
    ARMS = ("arm-a", "arm-b", "arm-c", "arm-d")
    CASES = ("alpha", "beta")

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.phase = self.root / "phase"
        self.phase.mkdir()
        schedule = [[case, arm, repeat] for repeat in range(2)
                    for case in self.CASES for arm in self.ARMS]
        self.plan = {
            "schema": 1, "run_id": "offline-analysis", "seed": 20260930,
            "repeats": 2, "evidence_kind": "offline",
            "arms": {arm: ["offline-stub"] for arm in self.ARMS},
            "registered_tasks": {case: {"risk": "low"} for case in self.CASES},
            "statistics": {"fixture_count": 2, "arm_count": 4,
                           "runs_per_arm": 4, "total_runs": 16},
            "budget": {"path": None, "configured_maximum": None},
            "schedule": schedule,
        }
        (self.phase / "plan.json").write_text(json.dumps(self.plan), encoding="utf-8")
        self.sequence = 0

    def trial(self, arm="arm-a", case="alpha", repeat=0, *, passed=True,
              exit_code=0, acceptance_exit=0, provider_calls=1, wall=10,
              generation_seconds=8, stderr="", adapter=None, **changes):
        # These names deliberately do not encode the logical identity: analysis
        # must match the result's case, arm, and repeat rather than directory order.
        self.sequence += 1
        directory = self.phase / f"trial-{100 - self.sequence:03d}"
        directory.mkdir()
        row = {
            "case": case, "arm": arm, "repeat": repeat, "passed": passed,
            "generation": {"status": "completed", "exit_code": exit_code,
                           "seconds": generation_seconds},
            "acceptance": {"status": "completed", "exit_code": acceptance_exit,
                           "seconds": 1},
            "artifact_unchanged": True, "trusted_inputs_unchanged": True,
            "protocol_files_unchanged": True, "measurement_error": None,
            "total_seconds": wall, "provider_calls": provider_calls,
            "tokens": None, "monetary_cost": None, "execution_events": None,
            "evidence_kind": "offline",
        }
        row.update(changes)
        (directory / "result.json").write_text(json.dumps(row), encoding="utf-8")
        (directory / "stderr.txt").write_text(stderr, encoding="utf-8")
        if adapter is not None:
            state = directory / "makewand-state-test"
            state.mkdir()
            (state / "adapter.json").write_text(json.dumps(adapter), encoding="utf-8")
        return directory

    def analyze(self, **kwargs):
        return analysis.analyze_phase(self.phase, bootstrap_repetitions=0, **kwargs)

    @staticmethod
    def pair(report, arm_a, arm_b):
        return next(pair for pair in report["paired_comparisons"]
                    if pair["arm_a"] == arm_a and pair["arm_b"] == arm_b)

    def test_partial_phase_counts_terminal_outcomes_without_relabeling_unknown(self):
        empty = self.analyze()
        self.assertEqual((empty["phase"]["planned"], empty["phase"]["completed"],
                          empty["phase"]["remaining"]), (16, 0, 16))
        self.assertTrue(empty["phase"]["incomplete"])
        for arm in empty["arms"].values():
            self.assertIsNone(arm["provider_calls"])
            self.assertEqual(arm["provider_calls_measured_trials"], 0)
            for measurement in ("wall_seconds", "generation_seconds"):
                self.assertEqual(arm[measurement]["measured_trials"], 0)
                for field in ("all_trials_total", "successful_mean", "p50", "p95"):
                    self.assertIsNone(arm[measurement][field])

        self.trial()
        self.trial(repeat=1, passed=False, acceptance_exit=1, provider_calls=2, wall=20)
        self.trial(case="beta", passed=False, exit_code=17,
                   stderr="quota exhausted while the provider outcome was unknown", wall=30)
        self.trial(case="beta", repeat=1, passed=False, exit_code=16, wall=40)
        self.trial("arm-b", passed=False, exit_code=10, stderr="Rate limit exceeded")
        self.trial("arm-b", repeat=1, passed=False, exit_code=10, stderr="Provider rejected request")
        self.trial("arm-b", case="beta", passed=False, exit_code=12)
        self.trial("arm-b", case="beta", repeat=1, passed=False, exit_code=13)
        self.trial("arm-c", passed=False, exit_code=11)

        report = self.analyze()
        self.assertEqual(report["phase"]["planned"], 16)
        self.assertEqual(report["phase"]["completed"], 9)
        self.assertEqual(report["phase"]["remaining"], 7)
        self.assertTrue(report["phase"]["incomplete"])
        self.assertEqual(report["phase"]["observed_result_files"], 9)
        a = report["arms"]["arm-a"]
        self.assertEqual((a["planned"], a["completed"], a["remaining"]), (4, 4, 0))
        self.assertEqual(a["outcomes"]["successful_delivery"], 1)
        self.assertEqual(a["outcomes"]["quality_failure"], 1)
        self.assertEqual(a["outcomes"]["unknown"], 1)
        self.assertEqual(a["outcomes"]["timeout"], 1)
        self.assertEqual(a["outcomes"]["quota_refusal"], 0)
        b = report["arms"]["arm-b"]["outcomes"]
        for outcome in ("quota_refusal", "other_failure", "cancelled", "budget_exhausted"):
            self.assertEqual(b[outcome], 1)
        self.assertEqual(report["arms"]["arm-c"]["outcomes"]["unverified"], 1)
        self.assertEqual(report["arms"]["arm-d"]["remaining"], 4)
        self.assertEqual(report["arms"]["arm-d"]["completed"], 0)

    def test_delivery_cost_includes_failures_and_preserves_missing_measurements(self):
        self.trial(provider_calls=2, wall=10)
        self.trial(repeat=1, passed=False, acceptance_exit=1, provider_calls=3, wall=30)
        self.trial("arm-b", passed=False, exit_code=10, provider_calls=2, wall=12)
        self.trial("arm-b", repeat=1, passed=False, exit_code=10, provider_calls=4, wall=18)
        self.trial("arm-c", provider_calls=2, wall=5)
        missing = self.trial("arm-c", repeat=1, provider_calls=None, wall=7)
        row = json.loads((missing / "result.json").read_text())
        row.pop("provider_calls")
        (missing / "result.json").write_text(json.dumps(row))
        self.trial("arm-d", provider_calls=0, wall=None)
        missing = self.trial("arm-d", repeat=1, provider_calls=0, wall=None)
        row = json.loads((missing / "result.json").read_text())
        row.pop("total_seconds")
        (missing / "result.json").write_text(json.dumps(row))

        report = self.analyze()
        a = report["arms"]["arm-a"]
        self.assertEqual(a["provider_calls"], 5)
        self.assertEqual(a["provider_calls_measured_trials"], 2)
        self.assertEqual(a["dispatches_per_accepted_delivery"], 5)
        self.assertEqual(a["wall_seconds"]["all_trials_total"], 40)
        self.assertEqual(a["wall_seconds"]["successful_mean"], 10)
        self.assertEqual(a["wall_seconds"]["measured_trials"], 2)
        self.assertIsNone(report["arms"]["arm-b"]["dispatches_per_accepted_delivery"])
        self.assertIsNone(report["arms"]["arm-b"]["wall_seconds"]["successful_mean"])
        self.assertEqual(report["arms"]["arm-b"]["provider_calls"], 6)
        self.assertIsNone(report["arms"]["arm-c"]["provider_calls"])
        self.assertEqual(report["arms"]["arm-c"]["provider_calls_measured_trials"], 1)
        self.assertIsNone(report["arms"]["arm-c"]["dispatches_per_accepted_delivery"])
        self.assertEqual(report["arms"]["arm-d"]["provider_calls"], 0)
        self.assertEqual(report["arms"]["arm-d"]["provider_calls_measured_trials"], 2)
        self.assertIsNone(report["arms"]["arm-d"]["dispatches_per_accepted_delivery"])
        for field in ("all_trials_total", "successful_mean", "p50", "p95"):
            self.assertIsNone(report["arms"]["arm-d"]["wall_seconds"][field])
        self.assertEqual(report["arms"]["arm-d"]["wall_seconds"]["measured_trials"], 0)
        for field in ("tokens", "monetary_cost", "peak_rss_bytes"):
            self.assertIsNone(report[field])

    def test_pairing_uses_case_and_repeat_and_median_of_passed_pair_differences(self):
        # Inserting B and A in different orders catches accidental positional joins.
        for case, repeat, wall in (("beta", 0, 1000), ("alpha", 1, 20), ("alpha", 0, 10)):
            self.trial("arm-b", case=case, repeat=repeat, wall=wall)
        self.trial("arm-b", case="beta", repeat=1, wall=15)
        for case, repeat, wall in (("alpha", 0, 1), ("beta", 0, 101), ("alpha", 1, 100)):
            self.trial("arm-a", case=case, repeat=repeat, wall=wall)
            self.trial("arm-c", case=case, repeat=repeat, wall=None)
        self.trial("arm-a", case="beta", repeat=1, passed=False, acceptance_exit=1, wall=600)

        report = self.analyze()
        pair = self.pair(report, "arm-a", "arm-b")
        self.assertEqual((pair["planned_pairs"], pair["paired_trials"]), (4, 4))
        self.assertEqual((pair["both_passed"], pair["a_only_passed"],
                          pair["b_only_passed"], pair["neither_passed"]), (3, 0, 1, 0))
        self.assertEqual(pair["success_rate_difference_b_minus_a"], 0.25)
        wall = pair["both_passed_wall_seconds"]
        self.assertEqual((wall["pairs"], wall["measured_pairs"]), (3, 3))
        self.assertEqual(wall["median_a"], 100)
        self.assertEqual(wall["median_b"], 20)
        self.assertEqual(wall["median_difference_b_minus_a"], 9)
        missing = self.pair(report, "arm-a", "arm-c")["both_passed_wall_seconds"]
        self.assertEqual((missing["pairs"], missing["measured_pairs"]), (3, 0))
        self.assertIsNone(missing["median_a"])
        self.assertIsNone(missing["median_b"])
        self.assertIsNone(missing["median_difference_b_minus_a"])

        # Cluster resampling has independently enumerable support here:
        # alpha contributes success difference 0, beta contributes .5. The
        # paired wall medians are -35.5, 9, or 899 for the possible case draws.
        bootstrapped = analysis.analyze_phase(self.phase, bootstrap_repetitions=300, seed=123)
        repeated = analysis.analyze_phase(self.phase, bootstrap_repetitions=300, seed=123)
        self.assertEqual(bootstrapped, repeated)
        bootstrap = self.pair(bootstrapped, "arm-a", "arm-b")["fixture_cluster_bootstrap"]
        self.assertEqual(bootstrap["unit"], "fixture")
        self.assertEqual((bootstrap["repetitions"], bootstrap["seed"]), (300, 123))
        self.assertEqual((bootstrap["fixture_clusters"], bootstrap["wall_fixture_clusters"]), (2, 2))
        self.assertEqual(bootstrap["success_rate_difference_b_minus_a_95pct"], [0, 0.5])
        self.assertEqual(bootstrap["median_wall_difference_b_minus_a_95pct"], [-35.5, 899])
        self.assertEqual(bootstrap["wall_effective_repetitions"], 300)

    def test_quota_requires_known_matching_failure_and_explicit_evidence(self):
        quota = {"schema": 1, "status": "FAILED", "exit_code": 10, "error_kind": "rate_limit"}
        self.trial("arm-a", passed=False, exit_code=10, adapter=quota)
        marker = "MAKEWAND_BENCHMARK_ADAPTER: " + json.dumps({
            "schema": 1, "status": "FAILED", "exit_code": 10, "quota_refusal": True})
        self.trial("arm-a", repeat=1, passed=False, exit_code=10, stderr=marker)
        self.trial("arm-a", case="beta", passed=False, exit_code=17, adapter=quota)
        self.trial("arm-a", case="beta", repeat=1, passed=False, exit_code=16,
                   stderr="Quota exhausted", adapter=quota)
        self.trial("arm-b", passed=False, exit_code=10,
                   adapter=dict(quota, exit_code=17))
        self.trial("arm-b", repeat=1, passed=False, exit_code=1, stderr="Rate limit exceeded")
        self.trial("arm-b", case="beta", passed=False, acceptance_exit=1,
                   stderr="Quota exhausted")
        self.trial("arm-b", case="beta", repeat=1, passed=False, exit_code=12,
                   stderr="Quota exhausted")
        self.trial("arm-c", passed=False, exit_code=10, stderr="Quota exhausted",
                   attempts=[{"status": "completed", "result_status": "FAILED", "outcome_known": False}])
        self.trial("arm-c", repeat=1, passed=False, exit_code=12,
                   attempts=[{"status": "completed", "result_status": "CANCELLED", "outcome_known": False}])

        report = self.analyze()
        a, b = (report["arms"][arm]["outcomes"] for arm in ("arm-a", "arm-b"))
        self.assertEqual((a["quota_refusal"], a["unknown"], a["timeout"]), (2, 1, 1))
        self.assertEqual(b["quota_refusal"], 0)
        self.assertEqual((b["other_failure"], b["quality_failure"], b["cancelled"]), (2, 1, 1))
        c = report["arms"]["arm-c"]["outcomes"]
        self.assertEqual((c["quota_refusal"], c["unknown"], c["cancelled"]), (0, 1, 1))

    def test_duplicate_malformed_and_spoofed_success_cannot_create_deliveries(self):
        self.trial()
        self.trial(passed=False, acceptance_exit=1)
        malformed = self.phase / "bad-json"
        malformed.mkdir()
        (malformed / "result.json").write_text("{", encoding="utf-8")
        self.trial("arm-b", passed="true")
        self.trial("not-planned")
        self.trial("arm-c", case="beta", repeat=1)
        self.trial("arm-d", passed=True, exit_code=17)

        report = self.analyze()
        phase = report["phase"]
        self.assertEqual(phase["observed_result_files"], 7)
        self.assertEqual(phase["duplicate_keys"], 1)
        self.assertEqual(phase["invalid_result_files"], 2)
        self.assertEqual(phase["unexpected"], 1)
        self.assertEqual((phase["completed"], phase["remaining"]), (2, 14))
        self.assertEqual(report["arms"]["arm-a"]["completed"], 0)
        self.assertEqual(report["arms"]["arm-c"]["outcomes"]["successful_delivery"], 1)
        self.assertEqual(report["arms"]["arm-d"]["outcomes"]["unknown"], 1)
        self.assertEqual(report["arms"]["arm-d"]["outcomes"]["successful_delivery"], 0)
        self.assertTrue(report["diagnostics"])

    def test_cli_writes_new_report_and_refuses_to_overwrite_existing_evidence(self):
        self.trial()
        ledger = self.root / "budget.json"
        ledger.write_text(json.dumps({
            "schema": 1, "maximum": 192,
            "attempts": [
                {"id": "other-attempt", "benchmark_run": "other-phase:trial", "status": "completed"},
                {"id": "completed-attempt", "benchmark_run": self.plan["run_id"] + ":completed",
                 "status": "completed"},
                {"id": "pending-attempt", "benchmark_run": self.plan["run_id"] + ":pending",
                 "status": "started"},
            ],
        }), encoding="utf-8")
        ledger_before = ledger.read_bytes()
        self.plan["budget"] = {"path": str(ledger), "configured_maximum": 192}
        (self.phase / "plan.json").write_text(json.dumps(self.plan), encoding="utf-8")
        summary = self.phase / "summary.json"
        original = b'{"existing_runner_summary": true}\n'
        summary.write_bytes(original)
        before = {path.relative_to(self.phase): path.read_bytes()
                  for path in self.phase.rglob("*") if path.is_file()}
        command = [sys.executable, "-I", str(ROOT / "analyze_results.py"),
                   "--input", str(self.phase), "--bootstrap-repetitions", "0", "--seed", "7"]
        rejected = subprocess.run(command + ["--output", str(summary)],
                                  capture_output=True, text=True, timeout=10)
        self.assertEqual(rejected.returncode, 2, rejected.stdout + rejected.stderr)
        self.assertEqual(summary.read_bytes(), original)
        output = self.root / "analysis.json"
        completed = subprocess.run(command + ["--output", str(output)],
                                   capture_output=True, text=True, timeout=10)
        self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
        report = json.loads(output.read_text())
        self.assertEqual((report["phase"]["planned"], report["phase"]["completed"]), (16, 1))
        measured_ledger = report["source_metadata"]["ledger"]
        self.assertEqual(measured_ledger["status"], "measured")
        self.assertEqual(measured_ledger["maximum"], 192)
        self.assertEqual(measured_ledger["total_reserved"], 3)
        self.assertEqual(measured_ledger["phase_reserved"], 2)
        self.assertEqual(measured_ledger["phase_unfinished_attempts"], 1)
        self.assertEqual(report["arms"]["arm-a"]["provider_calls"], 1)
        self.assertEqual(report["arms"]["arm-a"]["dispatches_per_accepted_delivery"], 1)
        saved = output.read_bytes()
        rejected = subprocess.run(command + ["--output", str(output)],
                                  capture_output=True, text=True, timeout=10)
        self.assertEqual(rejected.returncode, 2, rejected.stdout + rejected.stderr)
        self.assertEqual(output.read_bytes(), saved)
        after = {path.relative_to(self.phase): path.read_bytes()
                 for path in self.phase.rglob("*") if path.is_file()}
        self.assertEqual(after, before)
        self.assertEqual(ledger.read_bytes(), ledger_before)


if __name__ == "__main__":
    unittest.main()
