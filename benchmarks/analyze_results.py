#!/usr/bin/env python3
"""Read one local benchmark phase and describe its fixed, paired sample.

No model, CLI, or health command is invoked. The original summary is evidence,
not an output target. Missing measurements stay null; output defaults to stdout.
"""
import argparse
from collections import Counter, defaultdict
import hashlib
import itertools
import json
import math
import os
from pathlib import Path
import random
import re
import stat
import statistics
import sys


LIMIT = 16 * 1024 * 1024
STATUS_CODES = {"PASSED": 0, "INTERNAL_ERROR": 1, "INVALID_REQUEST": 2,
                "FAILED": 10, "UNVERIFIED": 11, "CANCELLED": 12,
                "BUDGET_EXHAUSTED": 13, "APPLY_CONFLICT": 14,
                "SANDBOX_UNAVAILABLE": 15, "TIMEOUT": 16, "UNKNOWN": 17}
OUTCOMES = ("successful_delivery", "quality_failure", "quota_refusal", "timeout",
            "unknown", "cancelled", "budget_exhausted", "unverified", "other_failure")
INTERPRETATION = ("Descriptive comparison of this preregistered fixed sample only. "
                  "Repeats share fixtures; fixture-cluster bootstrap intervals do not "
                  "establish general model superiority or population confidence.")
QUOTA = re.compile(
    r"\b(?:quota (?:exceeded|exhausted)|(?:usage|rate) limit (?:reached|exceeded|exhausted)"
    r"|insufficient_quota|usage_limit_reached|rate_limit_exceeded|quota_exceeded"
    r"|too many requests|you['’]?ve hit your (?:usage )?limit|you['’]?re out of (?:extra )?usage)\b"
    r"|\b429\b[^\n]*(?:rate|quota|too many requests)", re.IGNORECASE)


def _read_text(path, limit=LIMIT):
    # Read an atomically replaced ledger without taking the live dispatch lock.
    # A descriptor binds one version; nonblocking/no-follow rejects FIFO inputs.
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_size > limit:
        raise ValueError("input must be a bounded regular file")
    flags = os.O_RDONLY | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags)
    with os.fdopen(descriptor, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > limit:
            raise ValueError("input must be a bounded regular file")
        data = stream.read(limit + 1)
    if len(data) > limit:
        raise ValueError("input exceeds the 16 MiB protocol bound")
    return data.decode("utf-8")


def _reject_constant(_value):
    raise ValueError("nonfinite JSON value")


def _json(text):
    return json.loads(text, parse_constant=_reject_constant)


def _integer(value, minimum=0):
    return type(value) is int and value >= minimum


def _measurement(value):
    if type(value) not in (int, float):
        return None
    try:
        return value if value >= 0 and math.isfinite(value) else None
    except OverflowError:
        return None


def _key(row):
    if (not isinstance(row, dict) or not isinstance(row.get("case"), str) or not row["case"]
            or not isinstance(row.get("arm"), str) or not row["arm"]
            or not _integer(row.get("repeat"))):
        raise ValueError("invalid case, arm, or repeat identity")
    return row["case"], row["arm"], row["repeat"]


def _plan(path):
    try:
        contents = _read_text(path)
    except FileNotFoundError:
        return None, None, None
    plan = _json(contents)
    if (not isinstance(plan, dict) or type(plan.get("schema")) is not int or plan["schema"] != 1
            or not isinstance(plan.get("schedule"), list)):
        raise ValueError("plan must contain a schedule")
    schedule = []
    for entry in plan["schedule"]:
        if not isinstance(entry, list) or len(entry) != 3:
            raise ValueError("invalid planned trial")
        schedule.append(_key(dict(case=entry[0], arm=entry[1], repeat=entry[2])))
    if len(set(schedule)) != len(schedule):
        raise ValueError("duplicate planned trial")
    return plan, schedule, hashlib.sha256(contents.encode()).hexdigest()


def _adapter_metadata(row, directory, stderr, diagnostics):
    records = []
    if isinstance(row.get("adapter_metadata"), dict):
        records.append(row["adapter_metadata"])
    for line in stderr.splitlines():
        marker = "MAKEWAND_BENCHMARK_ADAPTER: "
        if line.startswith(marker):
            try:
                item = _json(line[len(marker):])
                if isinstance(item, dict):
                    records.append(item)
            except ValueError:
                diagnostics.append(directory.name + ": malformed adapter marker")
    for path in sorted(directory.glob("makewand-state*/adapter.json")):
        try:
            item = _json(_read_text(path, 65536))
            if isinstance(item, dict):
                records.append(item)
        except (OSError, ValueError, UnicodeError):
            diagnostics.append(directory.name + ": unreadable adapter metadata")
    selected = []
    for record in records:
        if (record.get("task_id") is not None and row.get("task_id") is not None
                and record["task_id"] != row["task_id"]):
            diagnostics.append(directory.name + ": adapter belongs to another task")
            continue
        status = record.get("status")
        if (type(record.get("schema")) is int and record["schema"] == 1
                and status in STATUS_CODES and record.get("exit_code") == STATUS_CODES[status]
                and type(record.get("exit_code")) is int):
            selected.append(record)
    return selected


def _normalize(row, path, diagnostics):
    key = _key(row)
    if (type(row.get("passed")) is not bool or not isinstance(row.get("generation"), dict)
            or not isinstance(row.get("acceptance"), dict)):
        raise ValueError("result lacks explicit delivery and stage outcomes")
    for stage in (row["generation"], row["acceptance"]):
        if stage.get("exit_code") is not None and type(stage["exit_code"]) is not int:
            raise ValueError("stage exit code must be an integer or null")
    stderr = ""
    try:
        stderr = _read_text(path.parent / "stderr.txt")
    except FileNotFoundError:
        pass
    except (OSError, ValueError, UnicodeError):
        diagnostics.append(path.parent.name + ": stderr unavailable for quota classification")
    adapters = _adapter_metadata(row, path.parent, stderr, diagnostics)
    statuses = {record["status"] for record in adapters}
    if any(record.get("outcome_known") is False and record["status"] in ("PASSED", "FAILED") for record in adapters):
        statuses.add("UNKNOWN")
    attempts = row.get("attempts")
    if isinstance(attempts, list):
        for attempt in attempts:
            if isinstance(attempt, dict):
                if attempt.get("result_status") in STATUS_CODES:
                    statuses.add(attempt["result_status"])
                if attempt.get("status") == "started":
                    statuses.add("UNKNOWN")
                if attempt.get("outcome_known") is False and attempt.get("result_status") in (None, "PASSED", "FAILED"):
                    statuses.add("UNKNOWN")
    generation, acceptance = row["generation"], row["acceptance"]
    code = generation.get("exit_code")
    integrity = all(row.get(field) is not False for field in
                    ("artifact_unchanged", "trusted_inputs_unchanged", "protocol_files_unchanged"))
    delivered = (row["passed"] and generation.get("status") == "completed" and code == 0
                 and acceptance.get("status") == "completed" and acceptance.get("exit_code") == 0
                 and integrity and row.get("measurement_error") is None)
    timeout = (generation.get("status") == "timeout" or acceptance.get("status") == "timeout"
               or code == 16 or "TIMEOUT" in statuses)
    unknown = (code == 17 or "UNKNOWN" in statuses or generation.get("status") == "incomplete_output"
               or generation.get("status") == "completed" and isinstance(code, int) and code < 0)
    quota_evidence = None
    # Uncertainty takes priority over messages mentioning quota. An arbitrary
    # nonzero process exit alone does not establish a known provider refusal.
    known_failure = (generation.get("status") == "completed"
                     and (code == 10 or any(record["status"] == "FAILED" and record["exit_code"] == code for record in adapters)))
    if known_failure and not timeout and not unknown:
        if any(record.get("quota_refusal") is True or record.get("error_kind") == "rate_limit" for record in adapters):
            quota_evidence = "adapter-metadata"
        elif QUOTA.search(stderr):
            quota_evidence = "known-failure-stderr"
    if timeout:
        outcome = "timeout"
    elif unknown:
        outcome = "unknown"
    elif code == 12 or "CANCELLED" in statuses:
        outcome = "cancelled"
    elif code == 13 or "BUDGET_EXHAUSTED" in statuses:
        outcome = "budget_exhausted"
    elif quota_evidence:
        outcome = "quota_refusal"
    elif delivered:
        outcome = "successful_delivery"
    elif generation.get("status") == "completed" and code == 0 and (
            not integrity or acceptance.get("status") == "completed" and acceptance.get("exit_code") not in (None, 0)):
        outcome = "quality_failure"
    elif code == 11 or "UNVERIFIED" in statuses:
        outcome = "unverified"
    else:
        outcome = "other_failure"
    delivered = outcome == "successful_delivery"
    if row["passed"] and not delivered:
        diagnostics.append(path.parent.name + ": reported pass conflicts with recorded execution evidence")
    calls = row.get("provider_calls")
    calls = calls if _integer(calls) else None
    provider_statuses = Counter(attempt.get("result_status") for attempt in attempts
                               if isinstance(attempt, dict) and attempt.get("result_status") in STATUS_CODES) if isinstance(attempts, list) else None
    adapter_status = next((record["status"] for record in adapters if record["exit_code"] == code), None)
    return {"key": key, "case": key[0], "arm": key[1], "repeat": key[2], "passed": delivered,
            "outcome": outcome, "provider_calls": calls, "wall_seconds": _measurement(row.get("total_seconds")),
            "generation_seconds": _measurement(generation.get("seconds")), "generation_exit_code": code,
            "generation_status": generation.get("status"), "acceptance_status": acceptance.get("status"),
            "execution_status": next((status for status, exit_code in STATUS_CODES.items() if exit_code == code), None),
            "adapter_status": adapter_status, "provider_status_counts": dict(provider_statuses) if provider_statuses is not None else None,
            "acceptance_exit_code": acceptance.get("exit_code"), "quota_evidence": quota_evidence,
            "risk": row.get("risk") if row.get("risk") in ("low", "medium", "high") else None,
            "benchmark_run": row.get("benchmark_run"), "input_file": path.parent.name + "/result.json"}


def _percentile(values, quantile):
    if not values:
        return None
    ordered = sorted(values)
    return ordered[max(0, math.ceil(quantile * len(ordered)) - 1)]


def _times(rows, field):
    measured = [row[field] for row in rows if row[field] is not None]
    passed = [row[field] for row in rows if row["passed"]]
    return {"measured_trials": len(measured), "all_trials_total": _measurement(sum(measured)) if rows and len(measured) == len(rows) else None,
            "successful_mean": statistics.mean(passed) if passed and all(value is not None for value in passed) else None,
            "successful_measured_trials": sum(value is not None for value in passed),
            "p50": _percentile(measured, .5), "p95": _percentile(measured, .95)}


def _bootstrap(pairs, repetitions, seed, arm_a, arm_b):
    clusters = defaultdict(list)
    for a, b in pairs:
        wall = b["wall_seconds"] - a["wall_seconds"] if (a["passed"] and b["passed"]
                and a["wall_seconds"] is not None and b["wall_seconds"] is not None) else None
        clusters[a["case"]].append((int(b["passed"]) - int(a["passed"]), wall))
    wall_clusters = sum(any(wall is not None for _, wall in cluster) for cluster in clusters.values())
    result = {"unit": "fixture", "repetitions": repetitions, "seed": seed, "fixture_clusters": len(clusters),
              "wall_fixture_clusters": wall_clusters, "interpretation": INTERPRETATION,
              "success_rate_difference_b_minus_a_95pct": None, "median_wall_difference_b_minus_a_95pct": None,
              "wall_effective_repetitions": 0}
    if repetitions == 0 or len(clusters) < 2:
        return result
    derived = int.from_bytes(hashlib.sha256(f"{seed}:{arm_a}:{arm_b}".encode()).digest()[:8], "big")
    generator = random.Random(derived)
    groups = [clusters[case] for case in sorted(clusters)]
    successes, walls = [], []
    for _ in range(repetitions):
        values = [value for _ in groups for value in generator.choice(groups)]
        successes.append(statistics.mean(value[0] for value in values))
        measured = [value[1] for value in values if value[1] is not None]
        if measured and wall_clusters >= 2:
            walls.append(statistics.median(measured))
    result["success_rate_difference_b_minus_a_95pct"] = [_percentile(successes, .025), _percentile(successes, .975)]
    result["wall_effective_repetitions"] = len(walls)
    if walls:
        result["median_wall_difference_b_minus_a_95pct"] = [_percentile(walls, .025), _percentile(walls, .975)]
    return result


def _ledger(plan, directory, diagnostics):
    budget = plan.get("budget") if isinstance(plan, dict) else None
    path_value = budget.get("path") if isinstance(budget, dict) else None
    result = {"path": path_value, "configured_maximum": budget.get("configured_maximum") if isinstance(budget, dict) else None,
              "status": "not-configured", "maximum": None, "total_reserved": None,
              "phase_reserved": None, "phase_unfinished_attempts": None}
    if not isinstance(path_value, str) or not path_value:
        return result
    path = Path(path_value)
    if not path.is_absolute():
        path = directory / path
    try:
        ledger = _json(_read_text(path))
        if (not isinstance(ledger, dict) or type(ledger.get("schema")) is not int or ledger["schema"] != 1
                or not _integer(ledger.get("maximum"), 1) or not isinstance(ledger.get("attempts"), list)):
            raise ValueError("invalid ledger")
        seen = set()
        for attempt in ledger["attempts"]:
            if (not isinstance(attempt, dict) or not isinstance(attempt.get("id"), str) or not attempt["id"]
                    or attempt["id"] in seen):
                raise ValueError("invalid or duplicate ledger attempt")
            seen.add(attempt["id"])
        run_id = plan.get("run_id")
        phase = [attempt for attempt in ledger["attempts"] if isinstance(run_id, str) and run_id
                 and isinstance(attempt.get("benchmark_run"), str) and attempt["benchmark_run"].startswith(run_id + ":")]
        result.update(status="measured", maximum=ledger["maximum"], total_reserved=len(ledger["attempts"]))
        if isinstance(run_id, str) and run_id:
            result.update(phase_reserved=len(phase), phase_unfinished_attempts=sum(attempt.get("status") != "completed" for attempt in phase))
        else:
            result["status"] = "attribution-unavailable"
    except FileNotFoundError:
        result["status"] = "missing"
    except (OSError, ValueError, UnicodeError):
        result["status"] = "invalid"
        diagnostics.append("budget ledger unavailable or invalid; no call count was inferred")
    return result


def analyze_phase(directory, bootstrap_repetitions=5000, seed=20260930):
    """Return a JSON-ready description of validated, unique local trial rows."""
    if not _integer(bootstrap_repetitions) or bootstrap_repetitions > 100000 or type(seed) is not int:
        raise ValueError("bootstrap repetitions must be 0..100000 and seed must be an integer")
    directory = Path(directory).resolve(strict=True)
    if not directory.is_dir():
        raise ValueError("phase input must be a directory")
    diagnostics = []
    plan, schedule, plan_hash = _plan(directory / "plan.json")
    if plan is None:
        diagnostics.append("plan is missing; planned, remaining, and completeness are unknown")
    files = sorted(directory.glob("*/result.json"))
    grouped = defaultdict(list)
    invalid = 0
    for path in files:
        try:
            row = _normalize(_json(_read_text(path)), path, diagnostics)
            grouped[row["key"]].append(row)
        except (OSError, ValueError, UnicodeError):
            invalid += 1
            diagnostics.append(path.parent.name + ": invalid result excluded")
    duplicates = [key for key, values in grouped.items() if len(values) != 1]
    for case, arm, repeat in duplicates:
        diagnostics.append(f"duplicate result identity excluded: {case}/{arm}/{repeat}")
    rows = [values[0] for values in grouped.values() if len(values) == 1]
    unexpected = 0
    if schedule is not None:
        planned = set(schedule)
        unexpected = sum(row["key"] not in planned for row in rows)
        rows = [row for row in rows if row["key"] in planned]
        if unexpected:
            diagnostics.append("results outside the preregistered schedule were excluded")
    rows.sort(key=lambda row: row["key"])
    arm_names = sorted({key[1] for key in schedule} if schedule is not None else {row["arm"] for row in rows})
    planned_count = len(schedule) if schedule is not None else None
    remaining = planned_count - len(rows) if planned_count is not None else None
    arms = {}
    for arm in arm_names:
        selected = [row for row in rows if row["arm"] == arm]
        counts = Counter(row["outcome"] for row in selected)
        expected = sum(key[1] == arm for key in schedule) if schedule is not None else None
        calls = [row["provider_calls"] for row in selected]
        dispatches = sum(calls) if calls and all(value is not None for value in calls) else None
        deliveries = counts["successful_delivery"]
        arms[arm] = {"planned": expected, "completed": len(selected), "remaining": expected - len(selected) if expected is not None else None,
                     "outcomes": {outcome: counts[outcome] for outcome in OUTCOMES},
                     "provider_calls": dispatches, "provider_calls_measured_trials": sum(value is not None for value in calls),
                     "dispatches_per_accepted_delivery": dispatches / deliveries if dispatches and deliveries else None,
                     "wall_seconds": _times(selected, "wall_seconds"), "generation_seconds": _times(selected, "generation_seconds"),
                     "tokens": None, "monetary_cost": None, "peak_rss_bytes": None}
    comparisons = []
    for arm_a, arm_b in itertools.combinations(arm_names, 2):
        a_rows = {(row["case"], row["repeat"]): row for row in rows if row["arm"] == arm_a}
        b_rows = {(row["case"], row["repeat"]): row for row in rows if row["arm"] == arm_b}
        pairs = [(a_rows[key], b_rows[key]) for key in sorted(a_rows.keys() & b_rows.keys())]
        expected_pairs = None
        if schedule is not None:
            a_keys = {(case, repeat) for case, arm, repeat in schedule if arm == arm_a}
            b_keys = {(case, repeat) for case, arm, repeat in schedule if arm == arm_b}
            expected_pairs = len(a_keys & b_keys)
        passed_pairs = [(a, b) for a, b in pairs if a["passed"] and b["passed"]]
        measured = [(a["wall_seconds"], b["wall_seconds"]) for a, b in passed_pairs
                    if a["wall_seconds"] is not None and b["wall_seconds"] is not None]
        comparisons.append({"arm_a": arm_a, "arm_b": arm_b, "planned_pairs": expected_pairs,
            "paired_trials": len(pairs), "both_passed": len(passed_pairs),
            "a_only_passed": sum(a["passed"] and not b["passed"] for a, b in pairs),
            "b_only_passed": sum(b["passed"] and not a["passed"] for a, b in pairs),
            "neither_passed": sum(not a["passed"] and not b["passed"] for a, b in pairs),
            "success_rate_difference_b_minus_a": statistics.mean(int(b["passed"]) - int(a["passed"]) for a, b in pairs) if pairs else None,
            "both_passed_wall_seconds": {"pairs": len(passed_pairs), "measured_pairs": len(measured),
                "median_a": statistics.median(a for a, _ in measured) if measured else None,
                "median_b": statistics.median(b for _, b in measured) if measured else None,
                "median_difference_b_minus_a": statistics.median(b - a for a, b in measured) if measured else None},
            "fixture_cluster_bootstrap": _bootstrap(pairs, bootstrap_repetitions, seed, arm_a, arm_b)})
    summary_hash = None
    try:
        summary_hash = hashlib.sha256(_read_text(directory / "summary.json").encode()).hexdigest()
    except FileNotFoundError:
        pass
    except (OSError, ValueError, UnicodeError):
        diagnostics.append("original summary unreadable; trial rows were analyzed independently")
    ledger = _ledger(plan, directory, diagnostics)
    public_rows = [{key: value for key, value in row.items() if key != "key"} for row in rows]
    observed_calls = [row["provider_calls"] for row in rows]
    outcomes = Counter(row["outcome"] for row in rows)
    return {"schema": 1, "interpretation": INTERPRETATION,
            "definitions": {"quality_failure": "Generation completed with exit 0, but independent acceptance or recorded integrity failed.",
                "quota_refusal": "Known FAILED outcome with explicit quota evidence; UNKNOWN and TIMEOUT take precedence.",
                "latency_quantiles": "Nearest rank on observed measurements; missing measurements are not zero.",
                "paired_wall": "Median of per-pair B minus A total wall time, restricted to both delivered and measured.",
                "cost_scope": "Observed completed trial dispatches, including failed trials; ledger additionally includes unfinished phase attempts."},
            "phase": {"planned": planned_count, "completed": len(rows), "remaining": remaining,
                "incomplete": remaining != 0 if remaining is not None else None,
                "observed_result_files": len(files), "unexpected": unexpected, "invalid_result_files": invalid,
                "duplicate_keys": len(duplicates), "run_id": plan.get("run_id") if plan else None,
                "evidence_kind": plan.get("evidence_kind") if plan else None,
                "outcomes": {outcome: outcomes[outcome] for outcome in OUTCOMES},
                "completed_trial_provider_calls": sum(observed_calls) if observed_calls and all(value is not None for value in observed_calls) else None,
                "provider_calls": ledger["phase_reserved"],
                "wall_seconds": _times(rows, "wall_seconds"), "generation_seconds": _times(rows, "generation_seconds")},
            "source_metadata": {"input_directory": str(directory), "plan_sha256": plan_hash,
                "original_summary_sha256": summary_hash, "repository": plan.get("repository") if plan else None,
                "ledger": ledger},
            "arms": arms, "paired_comparisons": comparisons, "trials": public_rows,
            "diagnostics": diagnostics, "tokens": None, "monetary_cost": None, "peak_rss_bytes": None}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", required=True, type=Path, help="One phase directory containing plan and trial results")
    parser.add_argument("--output", type=Path, help="New output file; existing files are never overwritten")
    parser.add_argument("--bootstrap-repetitions", type=int, default=5000, help="Fixture cluster resamples; 0 disables intervals")
    parser.add_argument("--seed", type=int, default=20260930)
    args = parser.parse_args(argv)
    try:
        analysis = analyze_phase(args.input, args.bootstrap_repetitions, args.seed)
        rendered = json.dumps(analysis, ensure_ascii=False, indent=2, allow_nan=False) + "\n"
        if args.output is None:
            sys.stdout.write(rendered)
        else:
            # Exclusive creation protects plan, original summary, result files,
            # and live ledgers from an accidental output path collision.
            flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
            descriptor = os.open(args.output, flags, 0o600)
            with os.fdopen(descriptor, "w", encoding="utf-8") as output:
                output.write(rendered)
    except (OSError, ValueError, UnicodeError) as error:
        parser.exit(2, "analysis failed: " + str(error) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
