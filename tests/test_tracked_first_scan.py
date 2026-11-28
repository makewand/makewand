"""A read-only review sees its tracked files and its history even when the workspace scan is cut short.

The workspace scan hides what it did not reach behind an empty tmpfs. It used to walk the
tree level by level under one 15-second budget, so in a large workspace (hundreds of
thousands of untracked files in worktrees, virtual environments, data) the reviewer found
tracked source directories and parts of .git empty: measured on this host on 2026-11-28,
501 of 9787 tracked files in one project, 226 of 2671 in another. The scan now walks the
directories the git index names, and .git, before everything else.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import os
import socket
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from makewand import git_helper, sandbox
from makewand.sandbox import wrap_bwrap

GIT_ENV = {**os.environ, "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1",
           "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.invalid",
           "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.invalid"}
TREE = ("top.txt", "a/b/c.txt", "a/d.txt", "x/y/z/w.txt")
TREE_DIRS = {"a", "a/b", "x", "x/y", "x/y/z"}


def git(repo, *arguments):
    subprocess.run(["git", *arguments], cwd=str(repo), env=GIT_ENV, check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def checkout(root, *init_options, files=TREE):
    root = Path(root)
    git(root, "init", "-q", *init_options)
    for name in files:
        (root / name).parent.mkdir(parents=True, exist_ok=True)
        (root / name).write_text("x\n")
    git(root, "add", "-A")
    return root


def index_bytes(paths, version=2, flags=None, oid_bytes=20):
    """An index file holding `paths` (sorted bytes), written the way git writes the version."""
    out = bytearray(b"DIRC" + version.to_bytes(4, "big") + len(paths).to_bytes(4, "big"))
    previous = b""
    for number, path in enumerate(paths):
        start = len(out)
        entry_flags = min(len(path), 0xFFF) if flags is None else flags[number]
        out += bytes(40 + oid_bytes) + entry_flags.to_bytes(2, "big")
        if version == 4:
            common = 0
            while common < min(len(previous), len(path)) and previous[common] == path[common]:
                common += 1
            strip = len(previous) - common
            encoded = bytearray([strip & 0x7F])  # git's offset varint, most significant group first
            strip >>= 7
            while strip:
                strip -= 1
                encoded.insert(0, 0x80 | (strip & 0x7F))
                strip >>= 7
            out += bytes(encoded) + path[common:] + b"\0"
        else:
            out += path
            out += bytes(((len(out) - start + 8) & ~7) - (len(out) - start))
        previous = path
    return bytes(out) + bytes(oid_bytes)


def scanned_and_unscanned(root, **options):
    """(directories the scan listed to the end, unscanned set, sockets) for one _scan_tree call."""
    listed, unscanned = [], set()
    real_scandir = os.scandir

    class Listing:
        def __init__(self, path):
            self.path, self.inner = os.fspath(path), real_scandir(path)

        def __enter__(self):
            self.inner.__enter__()
            return self

        def __exit__(self, *exc):
            return self.inner.__exit__(*exc)

        def __iter__(self):
            yield from self.inner
            listed.append(self.path)  # only a listing read to its end counts

    settings = dict(max_depth=None, entry_limit=1_000_000, collect_git=False, skip_dirs=frozenset(),
                    descend_git=True, priority_paths=(str(root),), unbounded_paths=(str(root),),
                    unscanned_dirs=unscanned)
    settings.update(options)
    with patch("makewand.sandbox.os.scandir", side_effect=Listing):
        _git, sockets, _truncated = sandbox._scan_tree([str(root)], **settings)
    return set(listed), unscanned, sockets


def covered(path, unscanned):
    return any(path == d or path.startswith(d + os.sep) for d in unscanned)


def tmpfs_masks(cmd):
    return {cmd[i + 1] for i, x in enumerate(cmd[:-1]) if x == "--tmpfs"}


@contextlib.contextmanager
def clock_past_every_budget():
    """Each look at the clock is two seconds later: a one-second budget is over at once."""
    now = [100.0]

    def tick():
        now[0] += 2.0
        return now[0]

    with patch("makewand.sandbox.time.monotonic", side_effect=tick), \
            patch.dict(os.environ, {"MAKEWAND_SANDBOX_SCAN_TIMEOUT_SECONDS": "1.0"}):
        yield


class TrackedDirectoriesTests(unittest.TestCase):
    def test_lists_the_directories_of_tracked_files_with_their_parents(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = checkout(tmp)
            (root / "untracked" / "deep").mkdir(parents=True)
            (root / "untracked" / "deep" / "f").write_text("x")
            self.assertEqual(TREE_DIRS, git_helper.tracked_directories(root))

    def test_reads_every_index_version_git_writes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = checkout(tmp)
            versions = set()
            for version in ("2", "4"):
                git(root, "update-index", "--index-version", version)
                versions.add((root / ".git" / "index").read_bytes()[7])
                self.assertEqual(TREE_DIRS, git_helper.tracked_directories(root), version)
            (root / "later" / "sub").mkdir(parents=True)
            (root / "later" / "sub" / "n.txt").write_text("x")
            git(root, "update-index", "--index-version", "2")
            git(root, "add", "-N", "later/sub/n.txt")  # intent-to-add is an extended flag: version 3
            versions.add((root / ".git" / "index").read_bytes()[7])
            self.assertEqual(TREE_DIRS | {"later", "later/sub"}, git_helper.tracked_directories(root))
            self.assertEqual({2, 3, 4}, versions)

    def test_reads_a_sha256_repository(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = checkout(tmp, "--object-format=sha256")
            self.assertEqual(TREE_DIRS, git_helper.tracked_directories(root))

    def test_follows_the_git_file_of_a_linked_worktree(self):
        with tempfile.TemporaryDirectory() as tmp:
            main = checkout(Path(tmp) / "main" if (Path(tmp) / "main").mkdir() is None else tmp)
            git(main, "commit", "-q", "-m", "one")
            linked = Path(tmp) / "linked"
            git(main, "worktree", "add", "-q", str(linked))
            self.assertTrue((linked / ".git").is_file())
            (linked / "only" / "here").mkdir(parents=True)
            (linked / "only" / "here" / "f.txt").write_text("x")
            git(linked, "add", "-A")
            self.assertEqual(TREE_DIRS | {"only", "only/here"}, git_helper.tracked_directories(linked))
            self.assertEqual(TREE_DIRS, git_helper.tracked_directories(main))

    def test_prefix_compression_and_long_names(self):
        long_dir = b"d" * 150
        paths = sorted([long_dir + b"/a/one", long_dir + b"/a/two", long_dir + b"/b/three", b"z" * 4094 + b"/f",
                        b"plain"])  # the last name is longer than the 12 bits an entry has for its length
        want = {os.fsdecode(long_dir), os.fsdecode(long_dir + b"/a"), os.fsdecode(long_dir + b"/b"), "z" * 4094}
        for version in (2, 3, 4):
            for oid_bytes in (20, 32):
                with tempfile.TemporaryDirectory() as tmp:
                    (Path(tmp) / ".git").mkdir()
                    (Path(tmp) / ".git" / "index").write_bytes(index_bytes(paths, version, oid_bytes=oid_bytes))
                    self.assertEqual(want, git_helper.tracked_directories(tmp), (version, oid_bytes))

    def test_no_index_it_can_read_is_none(self):
        good = index_bytes([b"a/b", b"c/d"])
        cases = {
            "truncated": good[:70],
            "not an index": b"PACK" + good[4:],
            "unknown version": good[:4] + (5).to_bytes(4, "big") + good[8:],
            "more entries than bytes": good[:8] + (9).to_bytes(4, "big") + good[12:],
            "name length does not match": index_bytes([b"a/b", b"c/d"], flags=[3, 9]),
            "prefix longer than the previous path": index_bytes([b"a/b"], version=4)[:74] + b"\x05b\0" + bytes(20),
            "varint that never ends": index_bytes([b"a/b"], version=4)[:74] + b"\xff" * 12 + bytes(20),
            "no checksum after the entries": good[:-20],
            "entry without its padding or a checksum": good[:12] + bytes(60) + b"\x00\x03a/b\0",
            "extended flag in version 2": index_bytes([b"a/b"], flags=[0x4003]),
            "extension longer than the file": good[:-20] + b"TREE" + (99).to_bytes(4, "big") + bytes(20),
            "split index": good[:-20] + b"link" + (20).to_bytes(4, "big") + bytes(20) + bytes(20),
            "empty": b"",
        }
        for label, data in cases.items():
            with tempfile.TemporaryDirectory() as tmp:
                (Path(tmp) / ".git").mkdir()
                (Path(tmp) / ".git" / "index").write_bytes(data)
                self.assertIsNone(git_helper.tracked_directories(tmp), label)
        with tempfile.TemporaryDirectory() as tmp:
            self.assertIsNone(git_helper.tracked_directories(tmp), "no checkout")
            (Path(tmp) / ".git").mkdir()
            self.assertIsNone(git_helper.tracked_directories(tmp), "no index file")
            (Path(tmp) / ".git" / "index").write_bytes(good)
            self.assertEqual({"a", "c"}, git_helper.tracked_directories(tmp))
            with patch.object(git_helper, "_GIT_INDEX_MAX_BYTES", len(good) - 1):
                self.assertIsNone(git_helper.tracked_directories(tmp), "too large")
            with patch.object(git_helper, "_GIT_INDEX_MAX_DIRECTORIES", 1):
                self.assertIsNone(git_helper.tracked_directories(tmp), "too many directories")
            (Path(tmp) / ".git" / "index").write_bytes(index_bytes([b"d" * 4096 + b"/f"], version=4))
            self.assertIsNone(git_helper.tracked_directories(tmp), "a path no checkout can hold")
            (Path(tmp) / ".git" / "index").write_bytes(good[:-20] + b"TREE" + (3).to_bytes(4, "big") + b"abc" + bytes(20))
            self.assertEqual({"a", "c"}, git_helper.tracked_directories(tmp), "an extension before the checksum")

    def test_a_split_index_is_not_read(self):
        # Review round 2: with core.splitIndex the paths sit in .git/sharedindex.<id>; the index
        # itself lists none, and an empty answer would read as "nothing tracked".
        with tempfile.TemporaryDirectory() as tmp:
            root = checkout(tmp)
            self.assertEqual(TREE_DIRS, git_helper.tracked_directories(root))
            git(root, "update-index", "--split-index")
            self.assertTrue(list((root / ".git").glob("sharedindex.*")))
            self.assertIsNone(git_helper.tracked_directories(root))

    def test_a_few_deep_paths_cannot_name_megabytes_of_directories(self):
        # Review round 1: 49 paths of 2000 levels (a 200 KB index) named 98,049 parent directories of
        # 2 KB on average: 600 MB, and 110 seconds in the launch that counted the hidden ones.
        deep = sorted((f"{number:02d}/" + "a/" * 2000 + "f").encode() for number in range(49))
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / ".git").mkdir()
            (Path(tmp) / ".git" / "index").write_bytes(index_bytes(deep))
            self.assertIsNone(git_helper.tracked_directories(tmp))
            (Path(tmp) / ".git" / "index").write_bytes(index_bytes(deep[:1]))
            self.assertEqual(2001, len(git_helper.tracked_directories(tmp)))

    def test_an_index_that_is_not_a_file_never_blocks(self):
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / ".git").mkdir()
            os.mkfifo(Path(tmp) / ".git" / "index")  # a plain open() would wait for a writer
            self.assertIsNone(git_helper.tracked_directories(tmp))
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / ".git").mkdir()
            (Path(tmp) / ".git" / "index").mkdir()
            self.assertIsNone(git_helper.tracked_directories(tmp))

    def test_no_git_process_is_started(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = checkout(tmp)
            with patch("subprocess.run", side_effect=AssertionError("no process")), \
                    patch("subprocess.Popen", side_effect=AssertionError("no process")):
                self.assertEqual(TREE_DIRS, git_helper.tracked_directories(root))


class WalkFirstTests(unittest.TestCase):
    def tree(self, tmp):
        root = Path(tmp)
        for name in ("src/pkg/deep/core", "src/pkg/other", ".git/objects/ab", ".git/refs/heads/feat",
                     "junk/a/b/c", "junk/d", "more/e/f", "src/pkg/deep/untracked_cache/g"):
            (root / name).mkdir(parents=True)
        first = {str(root / name) for name in ("src", "src/pkg", "src/pkg/deep", "src/pkg/deep/core", "src/pkg/other")}
        return root, first

    def test_named_directories_are_walked_when_the_time_is_already_over(self):
        with tempfile.TemporaryDirectory() as tmp:
            root, first = self.tree(tmp)
            listener = socket.socket(socket.AF_UNIX)
            listener.bind(str(root / "src/pkg/deep/core/agent.sock"))
            try:
                with clock_past_every_budget():
                    listed, unscanned, sockets = scanned_and_unscanned(
                        root, timeout_seconds=1.0, first_dirs=first, first_roots=[str(root / ".git")])
            finally:
                listener.close()
            for name in ("src/pkg/deep/core", "src/pkg/other", ".git/objects/ab", ".git/refs/heads/feat"):
                self.assertIn(str(root / name), listed, name)
                self.assertFalse(covered(str(root / name), unscanned), name)
            self.assertIn(str(root / "src/pkg/deep/core/agent.sock"), sockets)
            # The budget starts at the first directory that was not named: it is walked, the rest is not.
            others = [str(root / name) for name in ("junk", "more", "src/pkg/deep/untracked_cache")]
            self.assertEqual(1, sum(path in listed for path in others), listed)
            self.assertEqual(2, sum(path in unscanned for path in others), unscanned)

    def test_without_named_directories_the_budget_runs_from_the_start(self):
        with tempfile.TemporaryDirectory() as tmp:
            root, _first = self.tree(tmp)
            with clock_past_every_budget():
                listed, unscanned, _sockets = scanned_and_unscanned(root, timeout_seconds=1.0)
            self.assertEqual({str(root)}, listed)
            self.assertEqual({str(root / name) for name in (".git", "junk", "more", "src")}, unscanned)

    def test_the_entry_limit_stops_named_directories_too(self):
        with tempfile.TemporaryDirectory() as tmp:
            root, first = self.tree(tmp)
            for number in range(40):
                (root / "src/pkg" / f"file{number:02d}").write_text("x")
            listed, unscanned, _sockets = scanned_and_unscanned(
                root, entry_limit=20, first_dirs=first, first_roots=[str(root / ".git")])
            self.assertIn(str(root / "src/pkg"), unscanned)
            self.assertNotIn(str(root / "src/pkg"), listed)
            for name in ("src/pkg/deep/core", "junk", "more"):
                self.assertTrue(covered(str(root / name), unscanned), name)

    def test_every_directory_is_walked_to_its_end_or_left_unscanned(self):
        with tempfile.TemporaryDirectory() as tmp:
            root, first = self.tree(tmp)
            for directory in ("src/pkg/deep/core", "junk/a/b", ".git/objects/ab", "more/e"):
                for number in range(70):
                    (root / directory / f"f{number:02d}").write_text("x")
            listeners = []
            try:
                for directory in ("src/pkg/other", "junk/a/b/c", "more/e/f", ".git/refs/heads/feat"):
                    listener = socket.socket(socket.AF_UNIX)
                    listener.bind(str(root / directory / "s"))
                    listeners.append((listener, str(root / directory / "s")))
                everything = {str(root)} | {os.path.join(base, name) for base, names, _files in os.walk(root)
                                            for name in names}
                roots = [str(root / ".git")]
                for limit in (3, 20, 75, 150, 290, 1_000_000):
                    for named in (first, frozenset()):
                        for timed in (False, True):
                            with contextlib.ExitStack() as stack:
                                if timed:
                                    stack.enter_context(clock_past_every_budget())
                                listed, unscanned, sockets = scanned_and_unscanned(
                                    root, entry_limit=limit, timeout_seconds=1.0 if timed else None,
                                    first_dirs=named, first_roots=roots if named else ())
                            case = (limit, bool(named), timed)
                            for directory in everything:
                                self.assertTrue(directory in listed or covered(directory, unscanned),
                                                (case, directory))
                            for _listener, path in listeners:
                                self.assertTrue(path in sockets or covered(path, unscanned), (case, path))
            finally:
                for listener, _path in listeners:
                    listener.close()


class FoldMasksTests(unittest.TestCase):
    def test_untracked_parents_are_folded_before_tracked_ones(self):
        unscanned = {f"/w/src/cache{n}" for n in range(4)} | {f"/w/junk/sub/d{n}" for n in range(4)}
        self.assertEqual(
            sorted({f"/w/src/cache{n}" for n in range(4)} | {"/w/junk/sub"}),
            sandbox._collapse_unscanned_dirs(unscanned, ("/w",), max_masks=5, keep={"/w/src"}))
        # As before when nothing is named: the deepest parent with the most children goes first.
        self.assertEqual(sorted({f"/w/src/cache{n}" for n in range(4)} | {"/w/junk/sub"}),
                         sandbox._collapse_unscanned_dirs(unscanned, ("/w",), max_masks=5))
        shallow = {f"/w/src/cache{n}" for n in range(4)} | {f"/w/junk/d{n}" for n in range(3)}
        self.assertEqual(sorted({f"/w/junk/d{n}" for n in range(3)} | {"/w/src"}),
                         sandbox._collapse_unscanned_dirs(shallow, ("/w",), max_masks=4))
        self.assertEqual(sorted({f"/w/src/cache{n}" for n in range(4)} | {"/w/junk"}),
                         sandbox._collapse_unscanned_dirs(shallow, ("/w",), max_masks=5, keep={"/w/src"}))

    def test_a_lone_directory_is_not_lifted_for_nothing(self):
        # Review round 2: one unscanned directory below untracked parents was lifted level by level
        # (to /w/notes, hiding files the scan had walked) without saving a mask, before the tracked
        # parent was folded anyway.
        unscanned = {f"/w/src/pkg/cache{n}" for n in range(600)} | {"/w/notes/drafts/roi-image-cache"}
        plain = sandbox._collapse_unscanned_dirs(unscanned, ("/w",))
        self.assertEqual(["/w/notes/drafts/roi-image-cache", "/w/src/pkg"], plain)
        self.assertEqual(plain, sandbox._collapse_unscanned_dirs(
            unscanned, ("/w",), keep={"/w/src", "/w/src/pkg"}, keep_roots=["/w/.git"]))

    def test_deep_lone_directories_cost_one_pass(self):
        # Review round 2: with 512 unscanned directories under a kept parent 100 levels down, one
        # deep untracked directory made the loop run once per level (3 seconds; minutes at 1000).
        depth = 150
        tracked = "/w/" + "t/" * depth
        keep, parent = set(), tracked.rstrip("/")
        while parent != "/w":
            keep.add(parent)
            parent = os.path.dirname(parent)
        in_git = "/w/.git/refs/heads/" + "a/" * depth
        lone = "/w/" + "u/" * depth + "roi-image-cache"
        for label, unscanned, kept in (
                ("kept parent", {tracked + f"c{n:03d}" for n in range(512)} | {lone}, keep),
                ("below .git", {in_git + f"b{n:03d}" for n in range(512)} | {lone}, set())):
            passes = []
            real = sandbox._dedup_norm_ancestors
            with patch.object(sandbox, "_dedup_norm_ancestors", side_effect=lambda paths: passes.append(1) or real(paths)):
                got = sandbox._collapse_unscanned_dirs(unscanned, ("/w",), keep=kept, keep_roots=["/w/.git"])
            self.assertEqual(2, len(passes), label)  # once before the loop, once after the single fold
            self.assertEqual(sandbox._collapse_unscanned_dirs(unscanned, ("/w",)), got, label)
            self.assertIn(lone, got, label)

    def test_git_is_not_folded_before_a_tracked_parent(self):
        # Review round 1: one unscanned directory in .git climbed to .git itself (hiding all history)
        # before the tracked parent with 600 unscanned children was folded.
        unscanned = {f"/w/a/b/c/d/pkg/cache{n}" for n in range(600)} | {"/w/.git/objects/ab"}
        keep = {"/w/a", "/w/a/b", "/w/a/b/c", "/w/a/b/c/d", "/w/a/b/c/d/pkg"}
        self.assertEqual(["/w/.git/objects/ab", "/w/a/b/c/d/pkg"],
                         sandbox._collapse_unscanned_dirs(unscanned, ("/w",), keep=keep, keep_roots=["/w/.git"]))
        self.assertEqual(["/w/.git/objects/ab", "/w/a/b/c/d/pkg"], sandbox._collapse_unscanned_dirs(unscanned, ("/w",)))

    def test_a_tracked_parent_is_folded_when_nothing_else_is_left(self):
        unscanned = {f"/w/src/cache{n}" for n in range(6)} | {"/w/lib/tmp"}
        without = sandbox._collapse_unscanned_dirs(unscanned, ("/w",), max_masks=3)
        self.assertEqual(without, sandbox._collapse_unscanned_dirs(
            unscanned, ("/w",), max_masks=3, keep={"/w/src", "/w/lib"}))
        self.assertEqual(["/w/lib/tmp", "/w/src"], without)


class ReadOnlySandboxTests(unittest.TestCase):
    def repo(self, tmp):
        root = checkout(tmp, files=("src/pkg/deep/core/m.py", "src/pkg/other/n.py", "README"))
        git(root, "commit", "-q", "-m", "one")
        for name in ("junk/a/b/c", "junk/d", "more/e/f", "build/out"):
            (root / name).mkdir(parents=True)
        return root

    def build(self, root, **overrides):
        err = io.StringIO()
        with contextlib.ExitStack() as stack:
            for target, value in overrides.items():
                stack.enter_context(patch.object(sandbox, target, value))
            stack.enter_context(patch("sys.stderr", err))
            cmd = wrap_bwrap(["true"], workspace=str(root), readonly=True)
        return tmpfs_masks(cmd), err.getvalue()

    def test_a_scan_cut_short_hides_untracked_directories_only(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = self.repo(tmp)
            with clock_past_every_budget():
                masks, warning = self.build(root)
            inside = {mask for mask in masks if mask.startswith(str(root) + os.sep)}
            self.assertTrue(inside)
            for mask in inside:
                top = os.path.relpath(mask, root).split(os.sep)[0]
                self.assertIn(top, ("junk", "more", "build"), mask)
            self.assertIn("工作区扫描未能完成", warning)
            self.assertIn("超过 1 秒", warning)
            self.assertIn(f"{len(inside)} 个未扫描的目录", warning)
            self.assertIn("受版本管理的目录和 .git 已先扫完", warning)
            self.assertIn("MAKEWAND_SANDBOX_SCAN_TIMEOUT_SECONDS", warning)

    def test_the_warning_counts_tracked_directories_it_had_to_hide(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = self.repo(tmp)
            for number in range(60):
                (root / f"file{number:02d}").write_text("x")
            masks, warning = self.build(root, READONLY_SCAN_ENTRY_LIMIT=30)
            self.assertIn(str(root / "src"), masks)
            self.assertIn(str(root / ".git"), masks)
            # One mask (src) holds the five tracked directories; they stopped at the entry limit, not at the time.
            self.assertIn("被遮住的包括1 个含受版本管理文件的目录和.git 里的内容，在沙箱里读不到", warning)
            self.assertIn("这部分不计时，要看到它们，调高 MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT或恢复目录的读取权限", warning)
            self.assertNotIn("已先扫完", warning)
            self.assertNotIn("MAKEWAND_SANDBOX_SCAN_TIMEOUT_SECONDS", warning)

    def test_the_warning_says_when_only_history_is_hidden(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = self.repo(tmp)
            real_scandir = os.scandir
            unreadable = str(root / ".git" / "objects")

            def scandir(path):
                if os.fspath(path) == unreadable:
                    raise OSError(5, "Input/output error")
                return real_scandir(path)

            with patch("makewand.sandbox.os.scandir", side_effect=scandir):
                masks, warning = self.build(root)
            self.assertEqual({unreadable}, {mask for mask in masks if mask.startswith(str(root) + os.sep)})
            self.assertIn("被遮住的包括.git 里的内容，在沙箱里读不到", warning)
            self.assertNotIn("MAKEWAND_SANDBOX_SCAN_TIMEOUT_SECONDS", warning)

    def test_counting_what_is_hidden_does_not_walk_up_every_tracked_directory(self):
        # Review round 1: the count walked up from each tracked directory; 2000 levels of one path cost seconds.
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / ".git").mkdir()
            (root / ".git" / "HEAD").write_text("ref: refs/heads/main\n")
            (root / ".git" / "index").write_bytes(index_bytes([("a/" * 2000 + "f").encode()]))
            (root / "one" / "sub").mkdir(parents=True)
            (root / "two").mkdir()
            self.assertEqual(2000, len(git_helper.tracked_directories(root)))
            steps = []
            real = sandbox._covered_by_norm_set
            with patch.object(sandbox, "_covered_by_norm_set", side_effect=lambda *a: steps.append(1) or real(*a)), \
                    clock_past_every_budget():
                masks, warning = self.build(root)
            self.assertIn("工作区扫描未能完成", warning)
            self.assertIn("已先扫完", warning)
            self.assertLess(len(steps), 20)

    def test_the_warning_says_when_there_is_no_index(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name in ("one/a", "two/b"):
                (root / name).mkdir(parents=True)
            with clock_past_every_budget():
                masks, warning = self.build(root)
            self.assertEqual({str(root / "one"), str(root / "two")},
                             {mask for mask in masks if mask.startswith(str(root) + os.sep)})
            self.assertIn("没有读到完整的 git 索引", warning)
            self.assertIn("调高 MAKEWAND_SANDBOX_SCAN_TIMEOUT_SECONDS（秒），条目超限时调高 MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT", warning)

    def test_the_warning_does_not_speak_for_a_checkout_whose_index_it_could_not_read(self):
        # Review round 2: two checkouts are mounted, one index is read and the other is not.
        with tempfile.TemporaryDirectory() as tmp, tempfile.TemporaryDirectory() as other:
            root = self.repo(tmp)
            second = checkout(other, files=("lib/x.py",))
            git(second, "update-index", "--split-index")
            (second / "extra" / "deep").mkdir(parents=True)
            err = io.StringIO()
            with clock_past_every_budget(), patch("sys.stderr", err):
                cmd = wrap_bwrap(["true"], workspace=str(root), repo_root=str(second), readonly=True)
            self.assertTrue({mask for mask in tmpfs_masks(cmd) if mask.startswith((str(root) + os.sep, str(second) + os.sep))})
            self.assertIn("没有读到完整的 git 索引", err.getvalue())
            self.assertNotIn("已先扫完", err.getvalue())

    def test_the_warning_says_when_folding_hid_a_tracked_directory(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = checkout(tmp, files=("src/pkg/m.py",))
            for name in ("u1", "u2", "u3", "u4"):
                (root / "src" / "pkg" / name / "deep").mkdir(parents=True)
            real = sandbox._collapse_unscanned_dirs
            with clock_past_every_budget(), \
                    patch.object(sandbox, "_collapse_unscanned_dirs", side_effect=lambda *a, **k: real(*a, max_masks=2, **k)):
                masks, warning = self.build(root)
            self.assertEqual({str(root / "src" / "pkg")}, {mask for mask in masks if mask.startswith(str(root) + os.sep)})
            self.assertIn("被遮住的包括1 个含受版本管理文件的目录，在沙箱里读不到", warning)
            self.assertIn("未扫描的目录多于 512 个，合并成上级目录遮蔽时把它们一并遮住了", warning)
            self.assertIn("调高 MAKEWAND_SANDBOX_SCAN_TIMEOUT_SECONDS（秒）", warning)

    def test_a_writable_session_is_not_reordered_and_still_fails_closed(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = self.repo(tmp)
            with patch.object(git_helper, "tracked_directories", side_effect=AssertionError("read-only only")), \
                    clock_past_every_budget(), patch("sys.stderr", io.StringIO()):
                with self.assertRaises(sandbox.SandboxConfigError):
                    wrap_bwrap(["true"], workspace=str(root))

    def test_a_complete_scan_masks_nothing_and_does_not_warn(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = self.repo(tmp)
            masks, warning = self.build(root)
            self.assertFalse({mask for mask in masks if mask.startswith(str(root) + os.sep)})
            self.assertNotIn("工作区扫描未能完成", warning)


if __name__ == "__main__":
    unittest.main()
