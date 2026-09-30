"""Hybrid candidates bind complete merged text, test evidence and saved plans."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import tempfile
import unittest
import contextlib
import io
import py_compile
import sys
import importlib.util
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.orchestrator as orch
from makewand.candidate import CandidateManager, build_manifest, get_candidate_files_changed, remove_new_generated_bytecode
from makewand.git_helper import clone_isolated_worktree, ensure_git_worktree, run_git_cmd
from makewand.merger import ast_merge_python_file, semantic_merge_candidate_worktrees


class HybridIntegrityTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="makewand-hybrid-test-")
        self.root = Path(self.temporary.name)
        self.addCleanup(self.temporary.cleanup)
        for name, relative in (("CONFIG_DIR", "config"), ("CANDIDATES_DIR", "config/candidates"),
                               ("BACKUPS_DIR", "config/backups")):
            override = patch.object(config, name, self.root / relative)
            override.start()
            self.addCleanup(override.stop)
        config.ensure_config_dir()

    def race(self, tests=True):
        base = self.root / "workspace"
        base.mkdir()
        self.assertTrue(ensure_git_worktree(str(base)))
        source = "VALUE = 1\n\ndef left(): return 1\ndef right(): return 2\n"
        (base / "module.py").write_text(source)
        (base / "remove.txt").write_text("remove me\n")
        (base / "run.sh").write_text("#!/bin/sh\necho old\n")
        (base / "run.sh").chmod(0o755)
        if tests:
            (base / "test_module.py").write_text("def test_valid():\n    assert True\n")
        self.assertEqual(run_git_cmd(["git", "add", "-A"], cwd=str(base))[0], 0)
        self.assertEqual(run_git_cmd(["git", "commit", "-m", "fixture"], cwd=str(base))[0], 0)
        baseline = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(base))[1].strip()
        directory = config.CANDIDATES_DIR / "hybrid"
        directory.mkdir(parents=True)
        a, b, frozen = [directory / name for name in ("A", "B", "baseline")]
        for target in (a, b, frozen):
            clone_isolated_worktree(str(base), target)
        (a / "module.py").write_text(source.replace("VALUE = 1", "VALUE = 3").replace("left(): return 1", "left(): return 10"))
        (a / "remove.txt").unlink()
        (a / "run.sh").write_text("#!/bin/sh\necho new\n")
        (b / "module.py").write_text(source.replace("right(): return 2", "right(): return 20"))
        agents = []
        for label, path in (("A", a), ("B", b)):
            candidate_baseline = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(path))[1].strip()
            agents.append({"model": label, "path": str(path), "baseline_commit": candidate_baseline,
                           "success": True, "test_passed": True, "review_passed": True})
        CandidateManager.save_race("hybrid", "merge", str(base), baseline, *agents,
                                   baseline_dir=frozen, baseline_manifest=build_manifest(base),
                                   frozen_baseline_manifest=build_manifest(frozen))
        return base, a, b, frozen

    def synthesize(self, result=(True, None), side_effect=None):
        with patch("makewand.orchestrator.run_local_tests", return_value=result, side_effect=side_effect):
            return CandidateManager.create_hybrid_candidate("hybrid")

    def test_bytecode_cleanup_preserves_baseline_tracked_and_arbitrary_cache_inputs(self):
        root = self.root / "bytecode"
        root.mkdir()
        self.assertTrue(ensure_git_worktree(str(root)))
        for module in ("baseline", "tracked", "generated", "malformed"):
            (root / (module + ".py")).write_text("VALUE = 42\n")
        baseline_cache = Path(py_compile.compile(str(root / "baseline.py"), doraise=True))
        baseline = build_manifest(root)
        tracked_cache = Path(py_compile.compile(str(root / "tracked.py"), doraise=True))
        self.assertEqual(run_git_cmd(["git", "add", str(tracked_cache)], cwd=str(root))[0], 0)
        generated = [Path(py_compile.compile(str(root / "generated.py"), doraise=True, optimize=level)) for level in (0, 1, 2)]
        malformed = root / "__pycache__" / f"malformed.{sys.implementation.cache_tag}.pyc"
        malformed.write_bytes(importlib.util.MAGIC_NUMBER + b"\0" * 12 + b"invalid marshal lengths")
        source = root / "__pycache__/actual_source.py"
        source.write_text("SOURCE = True\n")
        before = {path: path.read_bytes() for path in (baseline_cache, tracked_cache, malformed, source)}
        removed = remove_new_generated_bytecode(root, baseline)
        self.assertEqual(set(removed), {path.relative_to(root).as_posix() for path in generated})
        for path, content in before.items():
            self.assertEqual(path.read_bytes(), content)

    def test_clone_preserves_tracked_ignored_cache_source_and_nested_ignore_rules(self):
        source = self.root / "copy-source"
        source.mkdir()
        self.assertTrue(ensure_git_worktree(str(source)))
        for relative in ("__pycache__/tracked.py", ".cache/tracked.py", "node_modules/tracked.py"):
            path = source / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("VALUE = 1\n")
        (source / ".gitignore").write_text(".env\n__pycache__/*.pyc\n.cache/ignored.txt\n__pycache__/tracked.py\n")
        (source / ".env").write_text("SECRET\n")
        (source / ".cache/ignored.txt").write_text("IGNORED\n")
        (source / "__pycache__/fresh.py").write_text("FRESH = True\n")
        (source / "__pycache__/generated.pyc").write_bytes(b"IGNORED CACHE")
        for relative in ("__pycache__/tracked.py", ".cache/tracked.py", "node_modules/tracked.py"):
            self.assertEqual(run_git_cmd(["git", "add", "-f", "--", relative], cwd=str(source))[0], 0)
        nested = source / "nested"
        nested.mkdir()
        for command in (["git", "init", "-q"], ["git", "config", "user.name", "Nested Fixture"],
                        ["git", "config", "user.email", "nested@example.invalid"]):
            self.assertEqual(run_git_cmd(command, cwd=str(nested))[0], 0)
        (nested / ".gitignore").write_text(".env\n__pycache__/*.pyc\n")
        (nested / "__pycache__").mkdir()
        (nested / "__pycache__/source.py").write_text("NESTED = True\n")
        (nested / "__pycache__/ignored.pyc").write_bytes(b"IGNORED NESTED CACHE")
        (nested / ".env").write_text("NESTED SECRET\n")
        for repo in (nested, source):
            self.assertEqual(run_git_cmd(["git", "add", "-A"], cwd=str(repo))[0], 0)
            self.assertEqual(run_git_cmd(["git", "commit", "-m", "fixture"], cwd=str(repo))[0], 0)
        target = self.root / "copy-target"
        clone_isolated_worktree(str(source), target)
        for relative in ("__pycache__/tracked.py", ".cache/tracked.py", "node_modules/tracked.py", "__pycache__/fresh.py", "nested/__pycache__/source.py"):
            self.assertEqual((target / relative).read_bytes(), (source / relative).read_bytes())
        for relative in (".env", ".cache/ignored.txt", "__pycache__/generated.pyc", "nested/.env", "nested/__pycache__/ignored.pyc"):
            self.assertFalse((target / relative).exists(), relative)
        baseline = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(target))[1].strip()
        (target / "__pycache__/tracked.py").write_text("VALUE = 2\n")
        self.assertEqual(get_candidate_files_changed(target, baseline)["__pycache__/tracked.py"], "M")

    def _run_race_fixture(self, *, size=0, no_tests=False, generated=False, clock=None, synthesize=False):
        base = self.root / "race-workspace"
        base.mkdir()
        self.assertTrue(ensure_git_worktree(str(base)))
        (base / "module.py").write_text("VALUE = 1\n")
        if not no_tests:
            (base / "test_module.py").write_text("def test_valid():\n    assert True\n")
        if generated:
            (base / ".makewand").mkdir()
            (base / ".makewand/playbook.json").write_text('{"project_conventions": ["preserve me"]}\n')
        self.assertEqual(run_git_cmd(["git", "add", "-A"], cwd=str(base))[0], 0)
        self.assertEqual(run_git_cmd(["git", "commit", "-m", "race fixture"], cwd=str(base))[0], 0)
        self.race_calls = []
        def dispatch(engine, prompt, cwd=None, readonly=False, **kwargs):
            self.race_calls.append((readonly, prompt, kwargs))
            if readonly:
                if clock is not None:
                    clock[0] = 10.0
                return True, 'MAKEWAND_RACE_VERDICT: {"pass": true, "winner": "A", "defects": []}', None
            path = Path(cwd)
            (path / "module.py").write_text("VALUE = " + (repr("x" * size) if size else "2") + "\n# VISIBLE_REVIEW_TAIL\n")
            if generated:
                py_compile.compile(str(path / "module.py"), doraise=True)
                (path / "__pycache__/actual_source.py").write_text("SOURCE = True\n")
            return True, "implemented", None
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "CANDIDATES_DIR", config.CANDIDATES_DIR))
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=False))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={"claude": {"status": "healthy"}, "codex": {"status": "healthy"}, "agy": {"status": "healthy"}}))
            stack.enter_context(patch("makewand.config.is_provider_enabled", return_value=True))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=dispatch))
            stack.enter_context(patch("makewand.sandbox.run_in_sandbox", return_value=(0, "", "", None)))
            if clock is not None:
                stack.enter_context(patch.object(orch.time, "monotonic", side_effect=lambda: clock[0]))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            code = orch.run_race("Implement update", cwd=str(base), engine_a="claude", engine_b="codex", timeout=1 if clock is not None else 30, synthesize_hybrid=synthesize)
        return base, CandidateManager.get_race(), code

    def test_clone_rejects_tracked_cache_path_through_external_parent_link(self):
        source = self.root / "linked-copy-source"
        source.mkdir()
        self.assertTrue(ensure_git_worktree(str(source)))
        (source / ".gitignore").write_text(".cache\n")
        (source / ".cache").mkdir()
        (source / ".cache/source.py").write_text("ORIGINAL = True\n")
        self.assertEqual(run_git_cmd(["git", "add", "-f", "--", ".cache/source.py"], cwd=str(source))[0], 0)
        self.assertEqual(run_git_cmd(["git", "add", ".gitignore"], cwd=str(source))[0], 0)
        self.assertEqual(run_git_cmd(["git", "commit", "-m", "tracked ignored cache"], cwd=str(source))[0], 0)
        (source / ".cache/source.py").unlink()
        (source / ".cache").rmdir()
        external = self.root / "external-cache"
        external.mkdir()
        secret = external / "source.py"
        secret.write_text("SECRET = 'outside workspace'\n")
        (source / ".cache").symlink_to(external, target_is_directory=True)
        target = self.root / "linked-copy-target"
        with self.assertRaisesRegex(OSError, "directory symlink"):
            clone_isolated_worktree(str(source), target)
        self.assertFalse((target / ".cache/source.py").exists())
        self.assertEqual(secret.read_text(), "SECRET = 'outside workspace'\n")
        (source / ".cache").unlink()
        (source / ".cache").mkdir()
        (source / ".cache/source.py").write_text("ORIGINAL = True\n")
        (target / ".cache").symlink_to(external, target_is_directory=True)
        with self.assertRaisesRegex(OSError, "directory symlink"):
            clone_isolated_worktree(str(source), target)
        self.assertEqual(secret.read_text(), "SECRET = 'outside workspace'\n")
        linked_target = self.root / "linked-root-target"
        linked_target.symlink_to(external, target_is_directory=True)
        with self.assertRaisesRegex(OSError, "target must not be a symlink"):
            clone_isolated_worktree(str(source), linked_target)
        self.assertEqual(secret.read_text(), "SECRET = 'outside workspace'\n")

    def test_race_removes_new_bytecode_before_tests_and_preserves_workspace_playbook(self):
        base, race, code = self._run_race_fixture(generated=True)
        self.assertEqual(code, 0)
        candidate = race["candidates"]["A"]
        self.assertTrue(candidate["test_passed"])
        self.assertIn("__pycache__/actual_source.py", candidate["manifest"])
        self.assertFalse(any(name.endswith(".pyc") for name in candidate["manifest"]))
        self.assertNotIn(".makewand/playbook.json", candidate["changes"])
        self.assertTrue(CandidateManager.apply_candidate(race["race_id"], "A")[0])
        self.assertEqual((base / "__pycache__/actual_source.py").read_text(), "SOURCE = True\n")
        self.assertEqual((base / ".makewand/playbook.json").read_text(), '{"project_conventions": ["preserve me"]}\n')

    def test_race_review_receives_complete_diff_and_large_candidates_remain_unverified(self):
        _, race, code = self._run_race_fixture(size=20000)
        self.assertEqual(code, 0)
        review_prompt = next(prompt for readonly, prompt, _ in self.race_calls if readonly)
        self.assertIn(race["candidates"]["A"]["diff"], review_prompt)
        self.assertIn("VISIBLE_REVIEW_TAIL", review_prompt)

    def test_oversized_race_diff_is_inspectable_without_judge_or_recommendation(self):
        _, race, code = self._run_race_fixture(size=70 * 1024)
        self.assertEqual(code, 11)
        self.assertFalse(any(readonly for readonly, _, _ in self.race_calls))
        self.assertIsNone(race["winner"])
        self.assertGreater(len(race["candidates"]["A"]["diff"].encode()), 64 * 1024)
        self.assertFalse(race["candidates"]["A"]["review_passed"])

    def test_race_without_tests_never_marks_candidates_passed_or_applies_normally(self):
        _, race, code = self._run_race_fixture(no_tests=True)
        self.assertEqual(code, 11)
        self.assertIsNone(race["winner"])
        for label in ("A", "B"):
            self.assertIsNone(race["candidates"][label]["test_passed"])
        self.assertFalse(CandidateManager.apply_candidate(race["race_id"], "A")[0])

    def test_expired_race_budget_does_not_start_hybrid(self):
        with patch.object(CandidateManager, "create_hybrid_candidate") as create:
            _, _, _ = self._run_race_fixture(clock=[0.0], synthesize=True)
        create.assert_not_called()

    def test_hybrid_optional_deadline_is_shared_with_merge_and_test(self):
        self.race()
        clock = [0.0]
        from makewand.merger import semantic_merge_candidate_worktrees as merge
        def delayed_merge(*args, **kwargs):
            result = merge(*args, **kwargs)
            clock[0] = 4.0
            return result
        with patch("makewand.candidate.time.monotonic", side_effect=lambda: clock[0]), \
             patch("makewand.merger.semantic_merge_candidate_worktrees", side_effect=delayed_merge), \
             patch("makewand.orchestrator.run_local_tests", return_value=(True, "test command passed")) as tests:
            ok, _, detail = CandidateManager.create_hybrid_candidate("hybrid", test_timeout=5)
        self.assertTrue(ok, detail)
        self.assertEqual(tests.call_args.kwargs["timeout"], 1)
        with patch("makewand.git_helper.clone_isolated_worktree") as clone:
            self.assertFalse(CandidateManager.create_hybrid_candidate("hybrid", test_timeout=0)[0])
        clone.assert_not_called()

    def test_independent_review_binds_saved_hybrid_without_host_apply(self):
        import contextlib
        import io
        import json
        from makewand.orchestrator import review_saved_hybrid
        base, _, _, _ = self.race()
        self.assertTrue(self.synthesize()[0])
        before = build_manifest(base)
        def review(**kwargs):
            print(json.dumps({"pass": True, "engine": "codex", "exit_code": 0,
                              "raw_summary": 'MAKEWAND_VERDICT: {"pass": true, "defects": []}'}))
            return 0
        with patch("makewand.orchestrator.run_review", side_effect=review), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(review_saved_hybrid("hybrid", output_json=True), 0)
        self.assertTrue(CandidateManager.get_race("hybrid")["candidates"]["M"]["review_passed"])
        self.assertEqual(build_manifest(base), before)

    def test_review_rejects_artifact_mutation_and_empty_diff_success(self):
        import contextlib
        import io
        import json
        from makewand.orchestrator import review_saved_hybrid
        self.race()
        self.assertTrue(self.synthesize()[0])
        def review(**kwargs):
            print(json.dumps({"pass": True, "engine": None, "exit_code": 0}))
            return 0
        with patch("makewand.orchestrator.run_review", side_effect=review), contextlib.redirect_stdout(io.StringIO()):
            self.assertNotEqual(review_saved_hybrid("hybrid"), 0)
        saved = CandidateManager.get_race("hybrid")["candidates"]["M"]
        self.assertIsNone(saved["review_passed"])
        def mutate(**kwargs):
            (Path(kwargs["cwd"]) / "module.py").write_text("forged = True\n")
            print(json.dumps({"pass": True, "engine": "codex", "exit_code": 0,
                              "raw_summary": 'MAKEWAND_VERDICT: {"pass": true, "defects": []}'}))
            return 0
        with patch("makewand.orchestrator.run_review", side_effect=mutate), contextlib.redirect_stdout(io.StringIO()):
            self.assertNotEqual(review_saved_hybrid("hybrid"), 0)
        self.assertIsNone(CandidateManager.get_race("hybrid")["candidates"]["M"]["review_passed"])

    def test_hybrid_review_uses_frozen_baseline_after_candidate_commits(self):
        import contextlib
        import io
        from makewand.orchestrator import review_saved_hybrid
        self.race()
        self.assertTrue(self.synthesize()[0])
        hybrid = CandidateManager.get_race("hybrid")["candidates"]["M"]
        path = Path(hybrid["path"])
        self.assertEqual(run_git_cmd(["git", "add", "-A"], cwd=str(path))[0], 0)
        self.assertEqual(run_git_cmd(["git", "commit", "-m", "hide changes from HEAD diff"], cwd=str(path))[0], 0)
        captured = []
        def review(engine, prompt, **kwargs):
            captured.append(prompt)
            return True, 'MAKEWAND_VERDICT: {"pass": true, "defects": []}', None
        with patch("makewand.orchestrator.dispatch_task", side_effect=review), patch("makewand.orchestrator.get_or_update_status", return_value={}), patch("makewand.orchestrator._engine_usable", return_value=(True, "")), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(review_saved_hybrid("hybrid"), 0)
        self.assertEqual(len(captured), 1, "committing candidate changes must not create an empty review")
        for change in ("VALUE = 3", "left(): return 10", "right(): return 20"):
            self.assertIn(change, captured[0])

    def test_replace_refs_cannot_hide_sealed_hybrid_changes(self):
        import contextlib
        import io
        from makewand.orchestrator import review_saved_hybrid
        self.race()
        self.assertTrue(self.synthesize()[0])
        hybrid = CandidateManager.get_race("hybrid")["candidates"]["M"]
        path, baseline = Path(hybrid["path"]), hybrid["baseline_commit"]
        def git(*args, input_data=None):
            code, output, error = run_git_cmd(["git", *args], cwd=str(path), input_data=input_data)
            self.assertEqual(code, 0, error)
            return output.strip()
        original = git("show", baseline + ":module.py")
        blob = git("hash-object", "-w", "--stdin", input_data=original.replace("VALUE = 1", "VALUE = 3") + "\n")
        git("read-tree", baseline)
        git("update-index", "--cacheinfo", "100644", blob, "module.py")
        tree = git("write-tree")
        replacement = git("commit-tree", tree, "-p", baseline, "-m", "replacement")
        git("replace", baseline, replacement)
        captured = []
        def review(engine, prompt, **kwargs):
            captured.append(prompt)
            return True, 'MAKEWAND_VERDICT: {"pass": true, "defects": []}', None
        with patch("makewand.orchestrator.dispatch_task", side_effect=review), patch("makewand.orchestrator.get_or_update_status", return_value={}), patch("makewand.orchestrator._engine_usable", return_value=(True, "")), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(review_saved_hybrid("hybrid"), 0)
        self.assertIn("+VALUE = 3", captured[0])

    def test_complete_save_load_review_apply_lifecycle(self):
        base, _, _, _ = self.race()
        ok, hybrid, detail = self.synthesize()
        self.assertTrue(ok, detail)
        self.assertIs(hybrid["test_passed"], True)
        self.assertIsNone(hybrid["review_passed"])
        saved = CandidateManager.get_race("hybrid")["candidates"]["M"]
        self.assertEqual(saved, hybrid)
        merged = (Path(saved["path"]) / "module.py").read_text()
        for change in ("VALUE = 3", "left(): return 10", "right(): return 20"):
            self.assertIn(change, merged)
        self.assertEqual(saved["changes"]["remove.txt"], "D")
        self.assertEqual(saved["manifest"]["run.sh"]["mode"], 0o755)
        self.assertIn("VALUE = 3", saved["diff"])
        self.assertFalse(CandidateManager.apply_candidate("hybrid", "M")[0])
        approved, detail = CandidateManager.approve_hybrid_candidate(
            "hybrid", saved["manifest"], saved["changes"], 'MAKEWAND_VERDICT: {"pass": true, "defects": []}')
        self.assertTrue(approved, detail)
        ok, applied, detail = CandidateManager.apply_candidate("hybrid", "M")
        self.assertTrue(ok, detail)
        self.assertTrue(applied)
        self.assertEqual((base / "module.py").read_text(), merged)
        self.assertFalse((base / "remove.txt").exists())
        self.assertEqual((base / "run.sh").stat().st_mode & 0o777, 0o755)

    def test_failed_test_tuple_is_false_and_not_reviewed(self):
        self.race()
        ok, hybrid, detail = self.synthesize((False, "deliberate test failure"))
        self.assertTrue(ok, detail)
        self.assertIs(hybrid["test_passed"], False)
        self.assertIsNone(hybrid["review_passed"])
        self.assertFalse(CandidateManager.apply_candidate("hybrid", "M")[0])
        self.assertFalse(CandidateManager.approve_hybrid_candidate("hybrid", hybrid["manifest"], hybrid["changes"])[0])

    def test_no_tests_never_grants_test_or_review_approval(self):
        self.race(tests=False)
        ok, hybrid, detail = self.synthesize()
        self.assertTrue(ok, detail)
        self.assertIsNone(hybrid["test_passed"])
        self.assertIsNone(hybrid["review_passed"])
        self.assertFalse(CandidateManager.apply_candidate("hybrid", "M")[0])

    def test_hybrid_review_api_rejects_missing_failed_and_contradictory_reports(self):
        self.race()
        ok, hybrid, detail = self.synthesize()
        self.assertTrue(ok, detail)
        reports = ["", "LGTM", 'MAKEWAND_VERDICT: {"pass": false, "defects": ["bug"]}',
                   'MAKEWAND_VERDICT: {"pass": true, "defects": ["bug"]}',
                   'MAKEWAND_VERDICT: {"pass": true, "defects": []}\nMAKEWAND_VERDICT: {"pass": false, "defects": []}']
        for report in reports:
            with self.subTest(report=report):
                self.assertFalse(CandidateManager.approve_hybrid_candidate("hybrid", hybrid["manifest"], hybrid["changes"], report)[0])
                self.assertIsNone(CandidateManager.get_race("hybrid")["candidates"]["M"]["review_passed"])

    def test_test_mutations_reject_registration(self):
        self.race()
        def mutate(cwd):
            path = Path(cwd) / "module.py"
            path.write_text(path.read_text() + "INJECTED = True\n")
            return True, None
        ok, _, detail = self.synthesize(side_effect=mutate)
        self.assertFalse(ok, detail)
        self.assertNotIn("M", CandidateManager.get_race("hybrid")["candidates"])

    def test_original_candidate_tampering_cannot_be_laundered_by_merge(self):
        _, a, _, _ = self.race()
        (a / "module.py").write_text("tampered = True\n")
        ok, _, detail = self.synthesize()
        self.assertFalse(ok, detail)
        self.assertFalse(CandidateManager.apply_candidate("hybrid", merge=True, force=True)[0])
        self.assertNotIn("M", CandidateManager.get_race("hybrid")["candidates"])

    def test_git_plan_tampering_cannot_be_laundered(self):
        _, a, _, _ = self.race()
        self.assertEqual(run_git_cmd(["git", "update-index", "--force-remove", "test_module.py"], cwd=str(a))[0], 0)
        # Removing an index entry changes the application plan without changing
        # the file bytes that an ordinary manifest check sees.
        self.assertFalse(self.synthesize()[0])

    def test_current_workspace_does_not_replace_frozen_baseline(self):
        base, _, _, _ = self.race()
        (base / "module.py").write_text("user_change = True\n")
        ok, hybrid, detail = self.synthesize()
        self.assertTrue(ok, detail)
        self.assertNotIn("user_change", (Path(hybrid["path"]) / "module.py").read_text())
        self.assertFalse(CandidateManager.apply_candidate("hybrid", "M")[0])

    def test_mutated_frozen_baseline_rejects_merge(self):
        _, _, _, frozen = self.race()
        (frozen / "module.py").write_text("different_baseline = True\n")
        self.assertFalse(self.synthesize()[0])

    def test_clone_error_does_not_copy_ignored_secrets(self):
        base, _, _, _ = self.race()
        (base / ".env").write_text("PRIVATE=value\n")
        with patch("makewand.git_helper.clone_isolated_worktree", side_effect=OSError("clone failed")):
            self.assertFalse(self.synthesize()[0])
        self.assertFalse(list((config.CANDIDATES_DIR / "hybrid").glob("candidate_M_*/.env")))

    def test_force_cannot_apply_mutated_hybrid_or_reseal_it(self):
        self.race()
        ok, hybrid, detail = self.synthesize()
        self.assertTrue(ok, detail)
        (Path(hybrid["path"]) / "module.py").chmod(0o600)
        self.assertFalse(CandidateManager.apply_candidate("hybrid", "M", force=True)[0])
        self.assertFalse(self.synthesize()[0])

    def test_concurrent_synthesis_keeps_one_sealed_candidate(self):
        self.race()
        with patch("makewand.orchestrator.run_local_tests", return_value=(True, None)) as tests:
            with ThreadPoolExecutor(max_workers=2) as executor:
                results = list(executor.map(lambda _: CandidateManager.create_hybrid_candidate("hybrid"), range(2)))
        self.assertTrue(all(result[0] for result in results), results)
        self.assertEqual(results[0][1]["path"], results[1][1]["path"])
        self.assertEqual(tests.call_count, 1)

    def test_merge_dry_run_has_no_synthesis_side_effect(self):
        self.race()
        with patch("makewand.orchestrator.run_local_tests") as tests:
            self.assertFalse(CandidateManager.apply_candidate("hybrid", merge=True, dry_run=True)[0])
            tests.assert_not_called()
        self.assertNotIn("M", CandidateManager.get_race("hybrid")["candidates"])


class CompleteTextMergeTests(unittest.TestCase):
    def test_constants_deletions_decorators_and_statements_survive(self):
        base = "VALUE = 1\n\n@old\ndef left(): return 1\n\ndef obsolete(): return 0\n\ndef right(): return 2\n\nprint(VALUE)\n"
        a = base.replace("VALUE = 1", "VALUE = 9").replace("@old", "@new").replace("def obsolete(): return 0\n\n", "")
        b = base.replace("right(): return 2", "right(): return 20").replace("print(VALUE)", "print(VALUE + 1)")
        ok, merged, strategy = ast_merge_python_file(base, a, b)
        self.assertTrue(ok, strategy)
        self.assertIn("VALUE = 9", merged)
        self.assertIn("@new", merged)
        self.assertNotIn("@old", merged)
        self.assertNotIn("obsolete", merged)
        self.assertIn("right(): return 20", merged)
        self.assertIn("print(VALUE + 1)", merged)

    def test_same_symbol_with_different_decorators_conflicts(self):
        base = "@old\ndef value(): return 1\n"
        self.assertFalse(ast_merge_python_file(base, base.replace("@old", "@a"), base.replace("@old", "@b"))[0])

    def test_identical_added_symbol_is_kept(self):
        base = "VALUE = 1\n"
        addition = "\ndef added(): return VALUE\n"
        a, b = base + addition, base.replace("VALUE = 1", "VALUE = 2") + addition
        ok, merged, detail = ast_merge_python_file(base, a, b)
        self.assertTrue(ok, detail)
        self.assertEqual(merged.count("def added"), 1)
        self.assertIn("VALUE = 2", merged)

    def test_non_utf8_conflict_never_replaces_bytes(self):
        with tempfile.TemporaryDirectory() as temporary:
            roots = [Path(temporary) / name for name in ("base", "a", "b", "out")]
            for path in roots:
                path.mkdir()
            for path, data in zip(roots, (b"\xffbase", b"\xffa", b"\xffb", b"\xffbase")):
                (path / "binary.dat").write_bytes(data)
            ok, _, conflicts, _ = semantic_merge_candidate_worktrees(str(roots[0]), roots[1], roots[2], roots[3])
            self.assertFalse(ok)
            self.assertIn("binary.dat", conflicts)
            self.assertEqual((roots[3] / "binary.dat").read_bytes(), b"\xffbase")


if __name__ == "__main__":
    unittest.main()
