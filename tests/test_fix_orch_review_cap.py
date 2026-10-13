"""Oversized review diffs are UNVERIFIED but never discard paid work (py-orch-N3).

A diff larger than the review cap (256 KiB by default, MAKEWAND_REVIEW_DIFF_MAX_BYTES
to override) is not truncated for review. The task reports UNVERIFIED (exit 11),
also after a recovered provider failure, and the complete patch is saved before
rollback/cleanup as git's own binary, full-index bytes with three context lines
whatever diff.context says, so plain git apply reproduces every change (a shadow
worktree is kept when it cannot be saved). A reviewer that cannot receive the
whole review prompt (agy over 31 KiB) is never asked for a verdict; another usable
provider reviews instead.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import re
import shutil
import stat
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from makewand import config, orchestrator as orch, review_verdict
from makewand.execution_contract import EXIT_UNVERIFIED
from makewand.git_helper import run_git_cmd

VERDICT_TAG = re.compile(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)")
GEOMETRY = "def area(w, h):\n    result = w * h\n    return result\n\n\ndef perimeter(w, h):\n    return 2 * (w + h)\n"
BINARY = bytes((index * 37 + 11) % 256 for index in range(4096)) + b"\0\0PNG\0" * 10
UNICODE_PATH = "pkg/数据 表.py"


def module_source(lines):
    return "".join(f"ENTRY_{index:05d} = ({index}, 'value-{index}')\n" for index in range(lines))


class ReviewCapTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="makewand-review-cap-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.calls = []
        self.module_lines = 400
        self.failing_coder = None
        self.coder = self.write_tables_module

    def write_tables_module(self, workspace):
        (workspace / "app.py").write_text("VALUE = 2\n")
        (workspace / "tables.py").write_text(module_source(self.module_lines))

    def make_repo(self, dirty, files=None, name=None):
        repo = self.root / (name or ("dirty" if dirty else "clean"))
        repo.mkdir()
        (repo / "app.py").write_text("VALUE = 1\n")
        (repo / "notes.md").write_text("notes\n")
        for relative, data in (files or {}).items():
            (repo / relative).parent.mkdir(parents=True, exist_ok=True)
            (repo / relative).write_bytes(data)
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            self.assertEqual(run_git_cmd(["git", *arguments], cwd=str(repo))[0], 0)
        if dirty:
            (repo / "notes.md").write_text("user work in progress\n")
        return repo

    @contextlib.contextmanager
    def fixtures(self, engines=("claude", "codex"), reviewers=None):
        coders = [name for name in engines if name in ("claude", "codex")]
        reviewers = list(reviewers or reversed(coders))

        def provider(engine):
            def execute(prompt, **kwargs):
                self.calls.append((engine, bool(kwargs.get("readonly"))))
                if kwargs.get("readonly"):
                    tag = VERDICT_TAG.search(prompt)
                    return True, (tag.group(0) if tag else "MAKEWAND_VERDICT") + ': {"pass": true, "defects": []}', None
                if engine == self.failing_coder:
                    return False, None, "Bubblewrap 沙箱构建失败，拒绝执行"
                self.coder(Path(kwargs["cwd"]))
                return True, "implemented", None
            return execute

        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch.os, "getloadavg", return_value=(0, 0, 0)))
            stack.enter_context(patch("makewand.git_helper.get_active_interactive_working_trees", return_value={}))
            stack.enter_context(patch("makewand.sandbox.verify_writable_sandbox_or_authorized", return_value=(True, None, None)))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={name: {"status": "healthy"} for name in engines}))
            stack.enter_context(patch.object(orch, "select_optimal_engine_pair", return_value=(
                coders, reviewers, dict(primary_coder=coders[0], primary_reviewer=reviewers[0], reasons=[]))))
            stack.enter_context(patch("makewand.config.is_provider_enabled", side_effect=lambda name: name in engines))
            stack.enter_context(patch("makewand.config.get_active_providers", return_value=list(engines)))
            stack.enter_context(patch("makewand.usage.record_engine_usage"))
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, "fixture tests passed")))
            for name in engines:
                stack.enter_context(patch.object(orch, f"execute_{name}_task", side_effect=provider(name)))
            yield

    def artifacts(self):
        root = Path(config.ARTIFACTS_DIR)
        return {path for path in root.rglob("*.patch")} if root.exists() else set()

    def run_task(self, repo, **fixture_options):
        before = self.artifacts()
        out = io.StringIO()
        with self.fixtures(**fixture_options), contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
            result = orch.run_workflow("Add lookup tables module", cwd=str(repo), force_code=True,
                                       auto_fix=False, total_timeout=120)
        return result, out.getvalue(), sorted(self.artifacts() - before)

    def test_default_cap_is_256_kib_with_environment_override(self):
        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("MAKEWAND_REVIEW_DIFF_MAX_BYTES", None)
            self.assertEqual(orch.review_diff_max_bytes(), 256 * 1024)
            os.environ["MAKEWAND_REVIEW_DIFF_MAX_BYTES"] = "4096"
            self.assertEqual(orch.review_diff_max_bytes(), 4096)
            os.environ["MAKEWAND_REVIEW_DIFF_MAX_BYTES"] = "not-a-number"
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(orch.review_diff_max_bytes(), 256 * 1024)

    def test_oversized_diff_is_unverified_and_keeps_the_complete_patch(self):
        self.module_lines = 400  # about 13 KB of new module
        with patch.dict(os.environ, {"MAKEWAND_REVIEW_DIFF_MAX_BYTES": "4096"}):
            for dirty in (True, False):
                with self.subTest(mode="shadow" if dirty else "host"):
                    self.calls.clear()
                    repo = self.make_repo(dirty)
                    result, log, patches = self.run_task(repo)
                    self.assertEqual(result.status, "UNVERIFIED")
                    self.assertEqual(result.exit_code, EXIT_UNVERIFIED)
                    self.assertEqual([readonly for _, readonly in self.calls], [False])  # no truncated review
                    self.assertIn("4096", log)
                    self.assertIn("MAKEWAND_REVIEW_DIFF_MAX_BYTES", log)
                    unverified = [path for path in patches if path.name == "unverified.patch"]
                    self.assertEqual(len(unverified), 1, patches)
                    text = unverified[0].read_text()
                    self.assertIn("tables.py", text)
                    self.assertIn(f"ENTRY_{self.module_lines - 1:05d}", text)
                    self.assertIn("+VALUE = 2", text)
                    self.assertIn(str(unverified[0]), log)
                    # Nothing unreviewed reached the workspace; the user's WIP is intact.
                    self.assertEqual((repo / "app.py").read_text(), "VALUE = 1\n")
                    self.assertFalse((repo / "tables.py").exists())
                    self.assertEqual((repo / "notes.md").read_text(),
                                     "user work in progress\n" if dirty else "notes\n")

    def test_diff_over_default_cap_keeps_new_module_in_saved_patch(self):
        self.module_lines = 8600  # about 300 KB of new module, over the 256 KiB default
        for dirty in (True, False):
            with self.subTest(mode="shadow" if dirty else "host"):
                self.calls.clear()
                repo = self.make_repo(dirty)
                with patch.dict(os.environ, {}, clear=False):
                    os.environ.pop("MAKEWAND_REVIEW_DIFF_MAX_BYTES", None)
                    result, log, patches = self.run_task(repo)
                self.assertEqual(result.status, "UNVERIFIED")
                self.assertIn(str(256 * 1024), log)
                unverified = [path for path in patches if path.name == "unverified.patch"]
                self.assertEqual(len(unverified), 1, patches)
                self.assertIn(f"ENTRY_{self.module_lines - 1:05d}", unverified[0].read_text())
                self.assertFalse((repo / "tables.py").exists())

    def test_diff_between_64_and_256_kib_is_reviewed_by_default(self):
        self.module_lines = 2400  # about 85 KB of new module
        repo = self.make_repo(False)
        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("MAKEWAND_REVIEW_DIFF_MAX_BYTES", None)
            result, _, _ = self.run_task(repo)
        self.assertEqual(result.status, "PASSED")
        self.assertEqual([readonly for _, readonly in self.calls], [False, True])
        self.assertTrue((repo / "tables.py").exists())

    def test_review_command_honours_the_configured_cap(self):
        diff = "diff --git a/big.py b/big.py\n+" + "a" * 5000
        with tempfile.TemporaryDirectory() as workspace, \
                patch.dict(os.environ, {"MAKEWAND_REVIEW_DIFF_MAX_BYTES": "4096"}), \
                patch.object(orch, "get_git_diff", return_value=diff), \
                patch.object(orch, "get_or_update_status", return_value={}), \
                patch.object(orch, "dispatch_task") as dispatch, \
                contextlib.redirect_stdout(io.StringIO()) as out:
            code = orch.run_review(cwd=workspace, output_json=True)
        self.assertEqual(code, EXIT_UNVERIFIED)
        dispatch.assert_not_called()
        report = json.loads(out.getvalue())
        self.assertFalse(report["pass"])
        self.assertIn("MAKEWAND_REVIEW_DIFF_MAX_BYTES", report["error"])

    def test_oversized_fallback_diff_after_a_sandbox_failure_is_unverified(self):
        """A recovered SANDBOX_UNAVAILABLE of the first coder must not become the outcome (exit 15)."""
        self.failing_coder = "claude"
        with patch.dict(os.environ, {"MAKEWAND_REVIEW_DIFF_MAX_BYTES": "4096"}):
            for dirty in (True, False):
                with self.subTest(mode="shadow" if dirty else "host"):
                    self.calls.clear()
                    repo = self.make_repo(dirty)
                    result, log, patches = self.run_task(repo)
                    self.assertEqual(self.calls, [("claude", False), ("codex", False)])
                    self.assertEqual(result.status, "UNVERIFIED", log)
                    self.assertEqual(result.exit_code, EXIT_UNVERIFIED)
                    self.assertIn("4096", result.error)
                    self.assertEqual(len([path for path in patches if path.name == "unverified.patch"]), 1)

    def write_mixed_change(self, workspace):
        self.write_tables_module(workspace)
        (workspace / "assets").mkdir(exist_ok=True)
        (workspace / "assets" / "logo.bin").write_bytes(BINARY)
        (workspace / "legacy.txt").unlink()
        (workspace / "run.sh").chmod(0o755)
        (workspace / UNICODE_PATH).parent.mkdir(parents=True, exist_ok=True)
        (workspace / UNICODE_PATH).write_text("UNICODE = True\n", encoding="utf-8")
        # Last file of the diff: the hunk ends with the two blank context lines of PEP 8.
        (workspace / "zz_geometry.py").write_text(GEOMETRY.replace("result = w * h", "result = abs(w) * abs(h)"))

    def assert_patch_reproduces(self, patch_file, workspace, expected, executable=(), subdir="."):
        """git apply onto a copy of workspace (in subdir, e.g. a submodule) reproduces expected."""
        copy = self.root / f"apply-check-{len(list(self.root.glob('apply-check-*')))}"
        shutil.copytree(workspace, copy, symlinks=True)
        check = copy / subdir
        applied = subprocess.run(["git", "apply", str(patch_file)], cwd=str(check), capture_output=True, text=True)
        self.assertEqual(applied.returncode, 0, applied.stderr)
        for relative, data in expected.items():
            if data is None:
                self.assertFalse((check / relative).exists(), relative)
            else:
                self.assertEqual((check / relative).read_bytes(), data, relative)
        for relative in executable:
            self.assertTrue(os.stat(check / relative).st_mode & 0o100, relative)

    def test_saved_patch_reproduces_every_change_byte_for_byte(self):
        """Binary and unicode-path files, a deletion, a mode change and trailing blank context lines."""
        self.coder = self.write_mixed_change
        expected = {"app.py": b"VALUE = 2\n", "tables.py": module_source(self.module_lines).encode(),
                    "assets/logo.bin": BINARY, "legacy.txt": None, UNICODE_PATH: "UNICODE = True\n".encode(),
                    "zz_geometry.py": GEOMETRY.replace("result = w * h", "result = abs(w) * abs(h)").encode(),
                    "run.sh": b"#!/bin/sh\necho run\n"}
        with patch.dict(os.environ, {"MAKEWAND_REVIEW_DIFF_MAX_BYTES": "4096"}):
            for dirty in (True, False):
                with self.subTest(mode="shadow" if dirty else "host"):
                    repo = self.make_repo(dirty, files={"legacy.txt": b"legacy\n" * 40, "run.sh": b"#!/bin/sh\necho run\n",
                                                        "zz_geometry.py": GEOMETRY.encode()})
                    result, log, patches = self.run_task(repo)
                    self.assertEqual(result.status, "UNVERIFIED", log)
                    unverified = [path for path in patches if path.name == "unverified.patch"]
                    self.assertEqual(len(unverified), 1, patches)
                    # Nothing unreviewed reached the workspace; in shadow mode the patch is the only copy.
                    self.assertEqual((repo / "app.py").read_text(), "VALUE = 1\n")
                    self.assertFalse((repo / "assets").exists())
                    self.assert_patch_reproduces(unverified[0], repo, expected, executable=["run.sh"])
                    self.assertEqual((repo / "notes.md").read_text(), "user work in progress\n" if dirty else "notes\n")
                    self.assertEqual(stat.S_IMODE(unverified[0].stat().st_mode), 0o600)

    def test_saved_patch_keeps_a_submodule_change_in_its_own_patch(self):
        origin = self.root / "library"
        origin.mkdir()
        (origin / "lib.txt").write_text("LIB = 1\n")
        for arguments in (["init", "-q"], ["add", "."], ["-c", "user.name=F", "-c", "user.email=f@e.invalid", "commit", "-qm", "lib"]):
            subprocess.run(["git", *arguments], cwd=str(origin), check=True, capture_output=True)
        repo = self.make_repo(False)
        for arguments in (["-c", "protocol.file.allow=always", "submodule", "add", "-q", str(origin), "vendor/lib"],
                          ["-c", "user.name=F", "-c", "user.email=f@e.invalid", "commit", "-qm", "add library"]):
            subprocess.run(["git", *arguments], cwd=str(repo), check=True, capture_output=True)
        base = subprocess.run(["git", "rev-parse", "HEAD"], cwd=str(repo), check=True, capture_output=True, text=True).stdout.strip()
        sub_base = subprocess.run(["git", "rev-parse", "HEAD"], cwd=str(repo / "vendor/lib"), check=True,
                                  capture_output=True, text=True).stdout.strip()
        pristine = self.root / "pristine"
        shutil.copytree(repo, pristine, symlinks=True)
        (repo / "app.py").write_text("VALUE = 2\n")
        (repo / "vendor/lib/lib.txt").write_text("LIB = 2\n")
        (repo / "vendor/lib/data.bin").write_bytes(BINARY)
        patch_file, error = review_verdict._save_unverified_artifacts(str(repo), base, {"vendor/lib": sub_base}, None, "probe")
        self.assertIsNone(error)
        manifest = json.loads((patch_file.parent / "manifest.json").read_text())
        self.assertEqual(manifest["submodule_baselines"], {"vendor/lib": sub_base})
        sub_patch = patch_file.parent / manifest["patches"]["vendor/lib"]
        self.assertEqual(manifest["patches"]["."], "unverified.patch")
        self.assert_patch_reproduces(patch_file, pristine, {"app.py": b"VALUE = 2\n"})
        self.assert_patch_reproduces(sub_patch, pristine, {"lib.txt": b"LIB = 2\n", "data.bin": BINARY}, subdir="vendor/lib")

    def zero_context_git_config(self, scope, repo):
        """diff.context=0 in the repository's or the user's global (the isolated HOME's) git config."""
        if scope == "repository":
            self.assertEqual(run_git_cmd(["git", "config", "diff.context", "0"], cwd=str(repo))[0], 0)
            return
        config_file = Path(os.environ["HOME"]) / ".gitconfig"
        previous = config_file.read_bytes() if config_file.exists() else None
        config_file.write_text("[diff]\n\tcontext = 0\n")
        self.addCleanup(lambda: config_file.write_bytes(previous) if previous is not None else config_file.unlink())

    def test_saved_patch_keeps_context_lines_whatever_diff_context_says(self):
        """n3-context0: a zero-context hunk inside a file does not apply with plain git apply."""
        changed = GEOMETRY.replace("result = w * h", "result = abs(w) * abs(h)")
        for scope in ("repository", "global"):
            with self.subTest(scope=scope):
                repo = self.make_repo(False, files={"zz_geometry.py": GEOMETRY.encode()}, name=f"context0-{scope}")
                base = subprocess.run(["git", "rev-parse", "HEAD"], cwd=str(repo), check=True,
                                      capture_output=True, text=True).stdout.strip()
                pristine = self.root / f"pristine-{scope}"
                shutil.copytree(repo, pristine, symlinks=True)
                self.zero_context_git_config(scope, repo)
                (repo / "app.py").write_text("VALUE = 2\n")
                (repo / "zz_geometry.py").write_text(changed)
                patch_file, error = review_verdict._save_unverified_artifacts(str(repo), base, None, None, "probe")
                self.assertIsNone(error)
                self.assertNotIn(b"@@ -2 +2 @@", patch_file.read_bytes())
                self.assertIn(b"@@ -1,5 +1,5 @@", patch_file.read_bytes())
                self.assert_patch_reproduces(patch_file, pristine, {"app.py": b"VALUE = 2\n",
                                                                    "zz_geometry.py": changed.encode()})

    def test_pipeline_patch_applies_under_a_zero_context_user_config(self):
        """The n3v diffcontext0 scenario: an oversized change saved by the pipeline in both modes."""
        changed = GEOMETRY.replace("return 2 * (w + h)", "return 2 * w + 2 * h").replace("result = w * h", "result = h * w")

        def coder(workspace):
            self.write_tables_module(workspace)
            (workspace / "zz_geometry.py").write_text(changed)
        self.coder = coder
        self.zero_context_git_config("global", None)
        expected = {"app.py": b"VALUE = 2\n", "tables.py": module_source(self.module_lines).encode(),
                    "zz_geometry.py": changed.encode()}
        with patch.dict(os.environ, {"MAKEWAND_REVIEW_DIFF_MAX_BYTES": "4096"}):
            for dirty in (True, False):
                with self.subTest(mode="shadow" if dirty else "host"):
                    repo = self.make_repo(dirty, files={"zz_geometry.py": GEOMETRY.encode()})
                    result, log, patches = self.run_task(repo)
                    self.assertEqual(result.status, "UNVERIFIED", log)
                    unverified = [path for path in patches if path.name == "unverified.patch"]
                    self.assertEqual(len(unverified), 1, patches)
                    self.assert_patch_reproduces(unverified[0], repo, expected)

    def test_shadow_delivery_patch_applies_under_a_zero_context_user_config(self):
        """A reviewed shadow-mode change is delivered: its patch keeps three context lines too."""
        changed = GEOMETRY.replace("result = w * h", "result = abs(w) * abs(h)")
        self.coder = lambda workspace: (workspace / "zz_geometry.py").write_text(changed)
        self.zero_context_git_config("global", None)
        repo = self.make_repo(True, files={"zz_geometry.py": GEOMETRY.encode()})
        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("MAKEWAND_REVIEW_DIFF_MAX_BYTES", None)
            result, log, patches = self.run_task(repo)
        self.assertEqual(result.status, "PASSED", log)
        delivery = [path for path in patches if path.name == "makewand_delivery.patch"]
        self.assertEqual(len(delivery), 1, patches)
        self.assertNotIn(b"@@ -2 +2 @@", delivery[0].read_bytes())
        self.assert_patch_reproduces(delivery[0], repo, {"zz_geometry.py": changed.encode()})

    def test_shadow_worktree_is_kept_when_the_patch_cannot_be_saved(self):
        with patch.dict(os.environ, {"MAKEWAND_REVIEW_DIFF_MAX_BYTES": "4096"}), \
                patch.object(orch, "_save_unverified_artifacts", return_value=(None, "disk full")):
            repo = self.make_repo(True)
            result, log, _ = self.run_task(repo)
        self.assertEqual(result.exit_code, EXIT_UNVERIFIED)
        self.assertIn("改动补丁保存失败 (disk full)", log)
        shadow = Path(re.search(r"已自动建立影子工作树: (\S+)", log).group(1))
        self.addCleanup(shutil.rmtree, shadow, True)
        self.assertEqual((shadow / "tables.py").read_text(), module_source(self.module_lines))

    def test_reviewer_that_cannot_receive_the_whole_prompt_is_skipped(self):
        """agy reads a prompt over 32 KiB through its own (paging) file tools: no verdict from it.

        Another usable provider reviews instead, also when agy is the only routed
        reviewer; the result is UNVERIFIED only when nobody else (never the coder)
        can take the whole prompt.
        """
        self.module_lines = 1200  # about 40 KB: under the review cap, over agy's whole-prompt limit
        for engines, reviewers, expected, reviews in ((("claude", "codex", "agy"), ["agy", "codex"], "PASSED", ["codex"]),
                                                      (("claude", "codex", "agy"), ["agy"], "PASSED", ["codex"]),
                                                      (("claude", "agy"), ["agy"], "UNVERIFIED", [])):
            with self.subTest(engines=engines, reviewers=reviewers):
                self.calls.clear()
                repo = self.make_repo(expected == "UNVERIFIED", name=f"repo-{len(engines)}-{len(reviewers)}")
                result, log, _ = self.run_task(repo, engines=engines, reviewers=reviewers)
                self.assertEqual(result.status, expected, log)
                self.assertEqual([engine for engine, readonly in self.calls if readonly], reviews)
                self.assertIn("AGY", log)
                self.assertIn(str(orch.reviewer_prompt_limit("agy")), log)

    def test_review_command_skips_a_reviewer_that_cannot_receive_the_whole_prompt(self):
        small, large = "diff --git a/a.py b/a.py\n+" + "a" * 100, "diff --git a/a.py b/a.py\n+" + "a" * 40000
        engines = {"agy"}  # codex and grok disabled: only agy is left on the review ladder
        for diff, dispatched in ((small, ["agy"]), (large, [])):
            with self.subTest(dispatched=dispatched), tempfile.TemporaryDirectory() as workspace, \
                    patch.object(orch, "get_git_diff", return_value=diff), \
                    patch.object(orch, "get_or_update_status", return_value={"agy": {"status": "healthy"}}), \
                    patch("makewand.config.is_provider_enabled", side_effect=lambda name: name in engines), \
                    patch.object(orch, "dispatch_task", side_effect=lambda engine, prompt, **kwargs: (
                        True, VERDICT_TAG.search(prompt).group(0) + ': {"pass": true, "defects": []}', None)) as dispatch, \
                    contextlib.redirect_stdout(io.StringIO()) as out:
                code = orch.run_review(cwd=workspace, output_json=True)
            self.assertEqual([call.args[0] for call in dispatch.call_args_list], dispatched)
            if dispatched:
                self.assertEqual(code, 0, out.getvalue())
            else:
                self.assertEqual(code, EXIT_UNVERIFIED)
                self.assertIn("AGY", json.loads(out.getvalue())["defects"][0])

    def test_verdict_follow_up_is_not_sent_to_a_reviewer_that_cannot_receive_it_whole(self):
        # The follow-up quotes up to 12000 characters of the review: 36 KB of CJK text.
        review = "详细审查结论，没有给出裁决行。" * 3000
        for engine, followups in (("agy", 0), ("codex", 1)):
            with self.subTest(engine=engine):
                dispatch = []
                text, verdict = review_verdict.resolve_review_verdict(
                    review, engine, timeout=30, quiet=True, nonce="0a1b2c3d",
                    dispatch_fn=lambda *args, **kwargs: dispatch.append(args) or (True, "no verdict", None))
                self.assertEqual(len(dispatch), followups)
                self.assertEqual(verdict["status"], review_verdict.REVIEW_UNVERIFIED)


class SavedPatchDocTests(unittest.TestCase):
    def test_format_review_diff_documents_the_current_review_cap(self):
        doc = review_verdict.format_review_diff.__doc__
        self.assertNotIn("64KB", doc)
        self.assertIn("256 KiB", doc)


if __name__ == "__main__":
    unittest.main()
