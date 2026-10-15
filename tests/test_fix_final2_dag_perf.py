"""A multi-stage DAG binds its trees within the snapshot limits and hashes unchanged files once (dag-perf).

DagStaging hashed the whole host repository twice - ignored data and logs included -
with no budget, and every stage rebuilt the staging manifest from scratch: a 2 GB
repository spent minutes hashing and copying before workspace_snapshot finally refused
it. Now (candidate.build_manifest with ManifestBudget and ManifestCache):

* the host manifest binds only what an isolated copy receives (no gitignored file);
* every manifest of a DAG counts files and bytes against the workspace snapshot limits
  (MAKEWAND_WORKSPACE_SNAPSHOT_MAX_FILES / _MAX_BYTES / _TIMEOUT_SECS) before hashing
  anything: a tree over them fails closed at once, naming the limit;
* one cache per DAG run reuses the digest of a file whose stat identity (device, inode,
  size, mtime, ctime) is unchanged, and never one of a file read while its timestamps
  were recent.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import os
import stat
import tempfile
import time
import types
import unittest
from unittest.mock import patch

from makewand import candidate as candidate_module, git_helper, task_dag
from makewand.candidate import build_manifest
from test_fix_orch_dag_r3 import DagFixture, git

FAR_PAST_OK = -10 ** 15  # settle_ns that caches every file, however recent


@contextlib.contextmanager
def hashed_paths():
    """The paths build_manifest reads (file_sha256 before the fix, _manifest_digest now)."""
    hashed = []
    old, new = candidate_module.file_sha256, getattr(candidate_module, "_manifest_digest", None)

    def spy_old(path):
        hashed.append(str(path))
        return old(path)

    def spy_new(path, budget):
        hashed.append(str(path))
        return new(path, budget)

    with patch.object(candidate_module, "file_sha256", side_effect=spy_old), \
            patch.object(candidate_module, "_manifest_digest", side_effect=spy_new, create=True):
        yield hashed


class FakeClock:
    def __init__(self, step):
        self.now, self.step = 1000.0, step

    def __call__(self):
        self.now += self.step
        return self.now


@unittest.skipIf(os.name == "nt", "build_manifest delegates to native_windows on Windows")
class ManifestBudgetAndCacheTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-dag-perf-")
        self.addCleanup(temporary.cleanup)
        self.tree = Path(temporary.name) / "tree"
        (self.tree / "src").mkdir(parents=True)
        for index in range(10):
            (self.tree / "src" / f"f{index}.txt").write_bytes(b"x" * 1000)

    def limits(self, **values):
        names = {"files": "MAKEWAND_WORKSPACE_SNAPSHOT_MAX_FILES", "bytes": "MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES",
                 "seconds": "MAKEWAND_WORKSPACE_SNAPSHOT_TIMEOUT_SECS"}
        return patch.dict(os.environ, {names[key]: str(value) for key, value in values.items()})

    def test_records_are_the_same_with_cache_and_budget(self):
        plain = build_manifest(self.tree)
        cache = candidate_module.ManifestCache(settle_ns=FAR_PAST_OK)
        for _ in range(2):
            self.assertEqual(build_manifest(self.tree, cache=cache, budget=candidate_module.ManifestBudget("t")), plain)
        self.assertEqual(len(cache), 10)

    def test_a_tree_over_the_file_or_byte_limit_fails_before_anything_is_hashed(self):
        for limits, name in (({"files": 9}, "MAKEWAND_WORKSPACE_SNAPSHOT_MAX_FILES"),
                             ({"bytes": 9999}, "MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES")):
            with self.subTest(limit=name), self.limits(**limits), hashed_paths() as hashed:
                with self.assertRaises(candidate_module.ManifestLimitExceeded) as raised:
                    build_manifest(self.tree, budget=candidate_module.ManifestBudget("测试树 "))
                self.assertEqual(hashed, [])
                self.assertIn(name, str(raised.exception))
                self.assertIn("测试树", str(raised.exception))
        with self.limits(files=10, bytes=10000):
            self.assertEqual(len(build_manifest(self.tree, budget=candidate_module.ManifestBudget("t"))), 10)

    def test_the_time_limit_stops_hashing(self):
        with self.limits(seconds=3), hashed_paths() as hashed:
            with self.assertRaises(candidate_module.ManifestLimitExceeded) as raised:
                build_manifest(self.tree, budget=candidate_module.ManifestBudget("t", clock=FakeClock(1.0)))
        self.assertIn("MAKEWAND_WORKSPACE_SNAPSHOT_TIMEOUT_SECS", str(raised.exception))
        self.assertLess(len(hashed), 10)

    def test_an_unchanged_file_is_read_once_and_a_rewritten_one_again(self):
        cache = candidate_module.ManifestCache(settle_ns=FAR_PAST_OK)
        first = build_manifest(self.tree, cache=cache)
        target = self.tree / "src" / "f3.txt"
        before = os.stat(target)
        with hashed_paths() as hashed:
            self.assertEqual(build_manifest(self.tree, cache=cache), first)
        self.assertEqual(hashed, [])
        # Same size, same mtime: only the inode change time tells the rewrite apart.
        time.sleep(0.02)
        target.write_bytes(b"y" * 1000)
        os.utime(target, ns=(before.st_atime_ns, before.st_mtime_ns))
        self.assertEqual(os.stat(target).st_mtime_ns, before.st_mtime_ns)
        with hashed_paths() as hashed:
            second = build_manifest(self.tree, cache=cache)
        self.assertEqual(hashed, [str(target)])
        self.assertNotEqual(second["src/f3.txt"]["sha256"], first["src/f3.txt"]["sha256"])
        self.assertEqual(second, build_manifest(self.tree))
        time.sleep(0.02)
        target.chmod(0o600)
        self.assertEqual(build_manifest(self.tree, cache=cache)["src/f3.txt"]["mode"], 0o600)

    def test_a_recently_written_file_is_not_cached(self):
        self.assertEqual(candidate_module.ManifestCache().settle_ns, 2 * 10 ** 9)
        # Every file here was written less than an hour before it is read.
        cache = candidate_module.ManifestCache(settle_ns=3600 * 10 ** 9)
        build_manifest(self.tree, cache=cache)
        self.assertEqual(len(cache), 0)
        with hashed_paths() as hashed:
            build_manifest(self.tree, cache=cache)
        self.assertEqual(len(hashed), 10)

    def test_a_file_that_changed_while_it_was_read_is_not_cached(self):
        cache = candidate_module.ManifestCache(settle_ns=FAR_PAST_OK)
        path = self.tree / "src" / "f0.txt"
        info = os.stat(path)
        fields = ("st_dev", "st_ino", "st_size", "st_mtime_ns", "st_ctime_ns", "st_mode")
        before = types.SimpleNamespace(**{name: getattr(info, name) for name in fields})
        after = types.SimpleNamespace(**dict(vars(before), st_mtime_ns=info.st_mtime_ns + 1, st_ctime_ns=info.st_ctime_ns + 1))
        with patch.object(candidate_module, "_manifest_digest", return_value=("0" * 64, before, after, time.time_ns())):
            record = candidate_module._manifest_record(str(path), info, cache, None)
        self.assertEqual(record, {"sha256": "0" * 64, "mode": stat.S_IMODE(info.st_mode)})
        self.assertEqual(len(cache), 0)
        with patch.object(candidate_module, "_manifest_digest", return_value=("1" * 64, before, before, time.time_ns())):
            candidate_module._manifest_record(str(path), info, cache, None)
        self.assertEqual(len(cache), 1)


@unittest.skipUnless(git_helper.HOST_TRANSACTION_SUPPORTED, "multi-stage DAGs need POSIX host transactions")
class DagStagingBudgetTests(DagFixture):
    def test_a_repository_over_the_limits_fails_closed_before_any_copy_or_hash(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", "big.bin": "z" * 300000})
        with patch.dict(os.environ, {"MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES": "200000"}), hashed_paths() as hashed, \
                patch.object(git_helper, "clone_isolated_worktree", side_effect=AssertionError("copy of an oversized tree")):
            with self.assertRaises(OSError) as raised:
                task_dag.DagStaging(str(repo))
        self.assertNotIsInstance(raised.exception, AssertionError)
        self.assertIn("MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES", str(raised.exception))
        self.assertIn(str(repo.resolve()), str(raised.exception))
        self.assertEqual(hashed, [])
        self.assertEqual(self.store(), [])

    def test_the_dag_reports_the_limit_and_runs_no_stage(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", "big.bin": "z" * 300000})
        with patch.dict(os.environ, {"MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES": "200000"}), \
                patch("makewand.orchestrator.run_pipeline") as pipeline, contextlib.redirect_stdout(io.StringIO()):
            ok, summary, results = task_dag.execute_task_dag(self.code_chain(), cwd=str(repo))
        self.assertFalse(ok)
        self.assertIn("MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES", summary)
        self.assertIn(f"工作区 {repo.resolve()} 中需要计算摘要的文件超过 200000 字节", summary)
        self.assertIn("未执行任何子任务", summary)
        pipeline.assert_not_called()
        self.assertEqual(results, [])
        from makewand.workflow import last_result
        self.assertEqual(last_result().status, "UNVERIFIED")

    def test_the_host_manifest_never_hashes_ignored_files(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", ".gitignore": "data/\n*.log\n.env\n"}).resolve()
        (repo / "data" / "shard").mkdir(parents=True)
        for index in range(3):
            (repo / "data" / "shard" / f"part{index}.bin").write_bytes(b"d" * 150000)
        (repo / "run.log").write_text("log\n")
        (repo / ".env").write_text("SECRET=1\n")
        (repo / "notes.txt").write_text("untracked, not ignored\n")
        # The ignored data alone is over the byte limit; what the copies receive is not.
        with patch.dict(os.environ, {"MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES": "200000"}), hashed_paths() as hashed:
            staging = task_dag.DagStaging(str(repo))
        self.addCleanup(staging.discard)
        host = [path for path in hashed if path.startswith(str(repo) + os.sep)]
        self.assertIn(str(repo / "app.py"), host)
        self.assertIn(str(repo / "notes.txt"), host)
        for ignored in ("data", "run.log", ".env"):
            self.assertEqual([path for path in host if path.startswith(str(repo / ignored))], [], ignored)
        self.assertNotIn("data/shard/part0.bin", staging.host_manifest)
        self.assertEqual(set(staging.host_manifest), set(staging.frozen_manifest))

    def test_an_unchanged_staging_copy_is_not_hashed_again(self):
        repo = self.make_repo(files={f"src/m{index}.py": f"M = {index}\n" for index in range(12)})
        staging = task_dag.DagStaging(str(repo))
        self.addCleanup(staging.discard)
        staging._manifests = candidate_module.ManifestCache(settle_ns=FAR_PAST_OK)
        self.assertTrue(staging.unchanged_since_last_checkpoint())  # fills the cache
        with hashed_paths() as hashed:
            self.assertTrue(staging.unchanged_since_last_checkpoint())
        self.assertEqual(hashed, [])
        changed = staging.workspace / "src" / "m3.py"
        changed.write_text("M = 33\n")
        with hashed_paths() as hashed:
            manifest = staging._staging_manifest()
        self.assertEqual(hashed, [str(changed)])
        self.assertNotEqual(manifest["src/m3.py"], staging.verified_manifest["src/m3.py"])
        self.assertFalse(staging.unchanged_since_last_checkpoint())
        self.assertEqual(git(staging.workspace, "status", "--porcelain").strip(), "M src/m3.py")

    def staging_with_an_edit_during_the_freeze(self, repo, relative, data):
        """DagStaging whose caches reuse every record, while relative is rewritten in the host
        during the first copy, keeping its size and modification time."""
        real_clone = git_helper.clone_isolated_worktree
        clones = []

        def clone(source, target):
            real_clone(source, target)
            clones.append(target)
            if len(clones) == 1:
                path = repo / relative
                before = os.stat(path)
                self.assertEqual(len(data), before.st_size)
                time.sleep(0.02)
                path.write_bytes(data)
                os.utime(path, ns=(before.st_atime_ns, before.st_mtime_ns))

        class EagerCache(candidate_module.ManifestCache):
            def __init__(self):
                super().__init__(settle_ns=FAR_PAST_OK)

        with patch.object(git_helper, "clone_isolated_worktree", side_effect=clone), \
                patch.object(candidate_module, "ManifestCache", EagerCache):
            return task_dag.DagStaging(str(repo))

    def test_the_freeze_check_still_sees_an_edit_that_keeps_size_and_mtime(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", "lib.py": "L = 1\n"}).resolve()
        with self.assertRaises(OSError) as raised:
            self.staging_with_an_edit_during_the_freeze(repo, "app.py", b"A = 2\n")
        self.assertIn("工作区在冻结 DAG 基线期间发生变化", str(raised.exception))
        self.assertEqual(self.store(), [])

    def test_an_ignored_file_changing_during_the_freeze_is_no_change_of_the_inputs(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", ".gitignore": "*.log\n"}).resolve()
        (repo / "server.log").write_bytes(b"line 1\n")
        staging = self.staging_with_an_edit_during_the_freeze(repo, "server.log", b"line 2\n")
        self.addCleanup(staging.discard)
        self.assertNotIn("server.log", staging.frozen_manifest)
        self.assertEqual(staging.frozen_manifest["app.py"], candidate_module.file_record(repo / "app.py"))


if __name__ == "__main__":
    unittest.main()
