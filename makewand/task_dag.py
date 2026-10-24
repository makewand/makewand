"""
Makewand Task DAG:
Topological task graph definition, semantic decomposition, and staged pipeline execution.
"""

import os
import re
import sys
from typing import Optional, Tuple, List, Dict, Any

from makewand.config import (
    c,
    COLOR_BOLD,
    COLOR_GREEN,
    COLOR_YELLOW,
    COLOR_RED,
    COLOR_BLUE,
    COLOR_CYAN,
    COLOR_PURPLE,
)


class TaskNode:
    """Represents a discrete atomic task node within a topological task DAG."""
    def __init__(
        self,
        task_id: str,
        title: str,
        description: str = "",
        target_files: Optional[List[str]] = None,
        dependencies: Optional[List[str]] = None,
        status: str = "pending",
    ):
        self.task_id = str(task_id).strip()
        self.title = str(title).strip()
        self.description = str(description).strip()
        self.target_files = list(target_files or [])
        self.dependencies = list(dict.fromkeys(str(d).strip() for d in (dependencies or []) if str(d).strip()))
        self.status = status
        self.result_patch: Optional[str] = None
        self.verdict: Optional[Dict[str, Any]] = None

    def to_dict(self) -> Dict[str, Any]:
        return {
            "id": self.task_id,
            "title": self.title,
            "description": self.description,
            "target_files": self.target_files,
            "dependencies": self.dependencies,
            "status": self.status,
        }


class TaskDAG:
    """Directed Acyclic Graph of structured tasks with topological stage resolution."""
    def __init__(self, goal: str, tasks: List[TaskNode]):
        self.goal = goal
        self.tasks: Dict[str, TaskNode] = {t.task_id: t for t in tasks}

    def topological_stages(self) -> List[List[TaskNode]]:
        """
        Groups tasks into sequential stages where tasks within each stage
        depend only on tasks completed in earlier stages.
        """
        in_degree = {tid: len([d for d in t.dependencies if d in self.tasks and d != tid]) for tid, t in self.tasks.items()}
        stages: List[List[TaskNode]] = []
        processed = set()

        while len(processed) < len(self.tasks):
            current_stage = [
                self.tasks[tid] for tid, deg in in_degree.items()
                if deg == 0 and tid not in processed
            ]
            if not current_stage:
                # Cycle or broken dependency: salvage remaining unexecuted tasks
                remaining = [t for tid, t in self.tasks.items() if tid not in processed]
                stages.append(remaining)
                break

            for t in current_stage:
                processed.add(t.task_id)
                for other_id, other_task in self.tasks.items():
                    if t.task_id in other_task.dependencies:
                        in_degree[other_id] = max(0, in_degree[other_id] - 1)
            stages.append(current_stage)

        return stages

    def validate(self) -> Tuple[bool, List[str]]:
        """
        Validates DAG integrity: checks for unknown dependencies, self-dependencies,
        and circular dependencies. Returns (is_valid, error_list).
        """
        errors = []
        for tid, t in self.tasks.items():
            for dep in t.dependencies:
                if dep == tid:
                    errors.append(f"任务节点 [{tid}] 存在自循环依赖")
                elif dep not in self.tasks:
                    errors.append(f"任务节点 [{tid}] 依赖了不存在的任务 [{dep}]")

        visited: Dict[str, int] = {}
        def _has_cycle(curr: str, path: List[str]) -> bool:
            visited[curr] = 1
            for dep in self.tasks[curr].dependencies:
                if dep not in self.tasks:
                    continue
                if visited.get(dep, 0) == 1:
                    cycle_str = " -> ".join(path + [curr, dep])
                    errors.append(f"发现循环依赖环路: {cycle_str}")
                    return True
                if visited.get(dep, 0) == 0:
                    if _has_cycle(dep, path + [curr]):
                        return True
            visited[curr] = 2
            return False

        for tid in self.tasks:
            if visited.get(tid, 0) == 0:
                _has_cycle(tid, [])

        return (len(errors) == 0, errors)

    def to_dict(self) -> Dict[str, Any]:
        stages = self.topological_stages()
        return {
            "goal": self.goal,
            "tasks": [t.to_dict() for t in self.tasks.values()],
            "stages": [[t.task_id for t in s] for s in stages]
        }

    def render_terminal(self) -> None:
        stages = self.topological_stages()
        print(f"\n🎯 工程目标: {c(self.goal, COLOR_BOLD)}")
        print(f"📊 任务拓扑图 (共 {len(self.tasks)} 个任务节点, 分为 {len(stages)} 个拓扑阶段):\n")
        for i, stage in enumerate(stages, start=1):
            stage_title = f"▶ 拓扑阶段 {i} (阶段任务数: {len(stage)})"
            print(c(stage_title, COLOR_BOLD + COLOR_CYAN))
            for t in stage:
                dep_str = f" [依赖: {', '.join(t.dependencies)}]" if t.dependencies else " [根依赖: 无]"
                files_str = f" [重点文件: {', '.join(t.target_files)}]" if t.target_files else ""
                print(f"   • [{c(t.task_id, COLOR_YELLOW)}] {c(t.title, COLOR_BOLD)}{dep_str}{files_str}")
                if t.description:
                    print(f"     说明: {t.description}")
            print()


def decompose_task_to_dag(
    prompt: str,
    cwd: Optional[str] = None,
    tier: str = "deep",
    local_only: bool = False
) -> TaskDAG:
    """
    Decomposes an engineering goal into a structured TaskDAG using semantic list
    extraction or deterministic architectural 3-stage partitioning (Contracts -> Logic -> Verification).
    """
    clean_goal = prompt.strip()
    tasks: List[TaskNode] = []

    # 1. Check for user-provided numbered or bulleted list directly in the prompt
    matches: List[str] = []
    if "\n" in clean_goal:
        matches = [re.sub(r"^\s*(?:[-*•]|\d+[.)])\s*", "", line).strip() for line in clean_goal.splitlines() if re.match(r"^\s*(?:[-*•]|\d+[.)])\s+", line)]
    if len(matches) < 2:
        parts = re.split(r"(?:^|\s+)\d+[.)]\s+", clean_goal)
        parts = [p.strip() for p in parts if p.strip()]
        if len(parts) >= 2:
            matches = parts

    if len(matches) >= 2:
        prev_id = None
        for i, line in enumerate(matches, start=1):
            tid = f"task-{i}"
            title = line.strip()
            # Extract potential target files mentioned in backticks
            files = re.findall(r"`([^`]+)`", title)
            deps = [prev_id] if prev_id else []
            tasks.append(TaskNode(tid, title=title, description=title, target_files=files, dependencies=deps))
            prev_id = tid
        return TaskDAG(clean_goal, tasks)

    # 2. Standard 3-phase decomposition for complex goals
    # Phase 1: Core Contracts & Data Structures
    # Phase 2: Implementation & Business Logic
    # Phase 3: Test Suites, Integration & Quality Gating
    tasks = [
        TaskNode(
            "task-1",
            title="数据结构与接口契约设计 (Core Data Model & Interfaces)",
            description=f"针对目标 '{clean_goal[:60]}' 梳理并定义核心类型、接口与数据结构。",
            dependencies=[],
        ),
        TaskNode(
            "task-2",
            title="核心功能与业务逻辑实现 (Core Implementation & Logic)",
            description=f"基于阶段 1 的结构定义，实现主要逻辑与适配器代码。",
            dependencies=["task-1"],
        ),
        TaskNode(
            "task-3",
            title="自动化测试与端到端质校验收 (Tests & Quality Gate)",
            description=f"补充单元测试、覆盖异常边界并确保全工程质检通过。",
            dependencies=["task-2"],
        ),
    ]
    return TaskDAG(clean_goal, tasks)


# Regenerable tool caches: their absence from a private copy is not worth a note.
_CACHE_NAMES = {"__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".hypothesis", ".cache",
                ".nox", ".nyc_output", "htmlcov", ".coverage"}


def _native_windows() -> bool:
    return os.name == "nt"


def _short_list(items: List[str], limit: int = 6) -> str:
    shown = ", ".join(items[:limit])
    return shown + (f" 等 {len(items)} 项" if len(items) > limit else "")


def _tree_entries(root: str) -> Dict[str, Tuple[Any, ...]]:
    """lstat signature of every entry below root except root's own .git directory.

    Sees what the Git-based checks cannot: ignored files, new or empty
    directories, nested repositories and special files. Rewriting a file changes
    its inode change time even when content, size and modification time are kept.
    """
    import stat
    entries: Dict[str, Tuple[Any, ...]] = {}
    for directory, directories, files in os.walk(root):
        base = os.path.relpath(directory, root)
        if base == ".":
            directories[:] = [name for name in directories if name != ".git"]
        for name in directories + files:
            try:
                info = os.lstat(os.path.join(directory, name))
            except OSError:
                continue
            relative = name if base == "." else f"{base}/{name}"
            if stat.S_ISDIR(info.st_mode):
                entries[relative] = ("dir", stat.S_IMODE(info.st_mode))
            else:
                entries[relative] = (stat.S_IFMT(info.st_mode), stat.S_IMODE(info.st_mode), info.st_ino,
                                     info.st_size, info.st_mtime_ns, info.st_ctime_ns)
    return entries


def _ancestors(path: str) -> List[str]:
    parts = path.split("/")
    return ["/".join(parts[:index]) for index in range(1, len(parts))]


def _top_most(paths) -> List[str]:
    kept: set = set()
    for path in sorted(paths):
        if not any(parent in kept for parent in _ancestors(path)):
            kept.add(path)
    return sorted(kept)


def _stray_entries(before: Dict[str, Tuple[Any, ...]], after: Dict[str, Tuple[Any, ...]],
                   tracked: set) -> Tuple[List[str], List[str]]:
    """What a read-only stage left that Git does not restore: (entries to remove, entries lost).

    To remove: new entries, entries whose type changed, and untracked (ignored)
    files it rewrote. Lost: untracked entries it deleted, which no copy can
    restore. Both are reduced to their top-most paths.
    """
    tracked_dirs = {parent for path in tracked for parent in _ancestors(path)}
    remove = [path for path, record in after.items()
              if path not in before or record[0] != before[path][0]
              or (path not in tracked and record[0] != "dir" and record != before[path])]
    lost = [path for path in before if path not in after and path not in tracked and path not in tracked_dirs]
    return _top_most(remove), _top_most(lost)


def _entry_names(paths: List[str], entries: Dict[str, Tuple[Any, ...]], limit: int = 20) -> List[str]:
    """Names to report for top-most entries: a directory's files, or "dir/" for a large tree or a repository."""
    names = []
    for path in paths:
        if entries.get(path, ("",))[0] != "dir":
            names.append(path)
            continue
        below = [other for other in entries if other.startswith(path + "/")]
        files = sorted(other for other in below if entries[other][0] != "dir")
        if not files or len(files) > limit or any(".git" in other[len(path) + 1:].split("/") for other in below):
            names.append(path + "/")
        else:
            names.extend(files)
    return names


def _remove_tree(path: str) -> None:
    """shutil.rmtree that also removes a tree its writer made read-only; links are never followed."""
    import shutil
    import stat

    def writable(directory: str) -> None:
        os.chmod(directory, stat.S_IMODE(os.lstat(directory).st_mode) | stat.S_IRWXU)

    writable(path)
    for directory, directories, _ in os.walk(path):
        for name in directories:
            child = os.path.join(directory, name)
            if not os.path.islink(child):
                writable(child)
    shutil.rmtree(path)


def _blocked_entries(blocked: Dict[str, Any]) -> str:
    """The entries that keep a cumulative result from makewand apply (DagStaging.blocked)."""
    if blocked.get("reason") == "unwritable":
        return f"makewand apply 无法写入的条目 (带特殊权限位、未纳入封存清单或文件被目录取代: {_short_list(blocked['paths'])})"
    return f"符号链接/子模块条目 ({_short_list(blocked['paths'])})"


def _readonly_intent(prompt: str) -> Optional[str]:
    """The read-only intent ("review", "explain", ...) the pipeline gives this node, or None.

    Every DAG node is dispatched with force_code=True, yet the pipeline still runs
    an explicitly read-only request without a transaction; the same predicates
    decide here, so the DAG knows which nodes cannot have produced a change.
    """
    orch = sys.modules.get("makewand.orchestrator")
    explicit = getattr(orch, "_explicit_readonly_request", None) if orch else None
    classify = getattr(orch, "classify_prompt_intent", None) if orch else None
    if explicit is None or classify is None:
        from makewand.task_admission import _explicit_readonly_request as explicit, classify_prompt_intent as classify
    if not explicit(prompt):
        return None
    intent = classify(prompt)
    return "explain" if intent == "code" else intent


class DagStaging:
    """One private workspace in which every stage of a multi-stage DAG runs in order.

    The repository root is staged, as stages running in place would see it, and
    each stage runs in the subdirectory the DAG was started from, under the
    pipeline's host transaction. A passed code stage is committed inside the copy,
    so the next stage builds on the previous, already tested and reviewed, result.
    A read-only stage (review/explain) sees everything since the DAG base as
    uncommitted changes; anything it writes is discarded, never committed. Only
    the cumulative result is delivered to the user's workspace, once, through the
    sealed candidate transaction (`makewand apply`), never one shadow patch per stage.
    """

    def __init__(self, target: str):
        import uuid
        from pathlib import Path
        from makewand import config
        from makewand.candidate import CandidateClaim, ManifestCache
        from makewand.git_helper import clone_isolated_worktree, find_git_root, run_git_cmd, _refused_init_roots
        from makewand.artifact import workspace_snapshot
        self.target = os.path.abspath(target)
        resolved = Path(self.target).resolve()
        if resolved in [root.resolve() for root in _refused_init_roots()]:
            raise OSError(f"拒绝在系统目录或用户主目录 ({self.target}) 中执行多阶段 DAG，请进入具体项目目录")
        git_root = find_git_root(resolved)
        self.root = str(Path(git_root).resolve()) if git_root else str(resolved)
        self.relative = resolved.relative_to(self.root).as_posix() if git_root else "."
        config.ensure_config_dir()
        config.ensure_private_dir(config.CANDIDATES_DIR)
        self.dag_id = "dg_" + uuid.uuid4().hex[:12]
        self.session = config.ensure_private_dir(config.CANDIDATES_DIR / self.dag_id)
        self.baseline = self.session / "baseline"
        self.workspace = self.session / "agent_a"
        self.stage_dir = self.workspace / self.relative if self.relative != "." else self.workspace
        self.sealed = False
        self.passed: List[str] = []
        self.code_passed: List[str] = []
        self.readonly_passed: Dict[str, str] = {}
        self.reviewed_nothing: List[str] = []
        self.discarded: Dict[str, List[str]] = {}
        self.blocked: Optional[Dict[str, Any]] = None
        self.missing_inputs: List[str] = []
        self._readonly_entries: Optional[Dict[str, Tuple[Any, ...]]] = None
        self._claim = None
        # The staging copy's file records, reused by every stage for unchanged files.
        self._manifests = ManifestCache()
        self.base_commit = self.head_commit = ""
        try:
            # The candidate store is pruned by concurrent saves: claim this copy first.
            self._claim = CandidateClaim(self.session)
            # Only what the copies receive needs to hold still: ignored files and an
            # untracked dependency tree (node_modules/) are never copied, nor hashed here.
            # A repository over the snapshot limits fails here, before any copy is made.
            # The host's records serve the freeze check below only; the baseline is bound once.
            host_records = ManifestCache()
            self.host_manifest = self._manifest(Path(self.root), host=True, cache=host_records)
            clone_isolated_worktree(self.root, self.baseline)
            self.frozen_manifest = self._manifest(self.baseline)
            clone_isolated_worktree(str(self.baseline), self.workspace)
            if git_root:
                self._carry_ignore_rules(self.workspace)
            if self._manifest(Path(self.root), host=True, cache=host_records) != self.host_manifest:
                raise OSError("工作区在冻结 DAG 基线期间发生变化")
            code, head, error = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(self.workspace))
            if code or not head.strip():
                raise OSError(f"无法读取 DAG 暂存区基线提交: {error}")
            self.base_commit = self.head_commit = head.strip()
            self.host_head = ""
            if git_root:
                code, head, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=self.root)
                self.host_head = head.strip() if code == 0 else ""
            if not self.stage_dir.is_dir():
                code, _, _ = run_git_cmd(["git", "check-ignore", "-q", "--", self.relative], cwd=self.root)
                if code == 0 or any(resolved.iterdir()):
                    raise OSError(f"工作目录 {self.target} 被 .gitignore 忽略或属于未跟踪的依赖/构建目录，"
                                  "其内容不会复制到私有暂存区；请在仓库中未被忽略的项目目录执行多阶段 DAG")
                # An empty directory is not copied; recreate it so stages run there.
                self.stage_dir.mkdir(parents=True, exist_ok=True)
            self.missing_inputs = self._missing_inputs()
            self.verified_snapshot = workspace_snapshot(self.workspace)
            self.verified_manifest = self._staging_manifest()
        except BaseException:
            self.discard()
            raise

    def _manifest(self, path, host: bool = False, cache: Optional[Any] = None) -> Dict[str, Any]:
        """build_manifest of path within the workspace snapshot limits (candidate.ManifestBudget):
        a tree too large to bind fails closed with the limit named, before it is hashed,
        instead of hashing for minutes. cache (a candidate.ManifestCache) supplies the records
        of files unchanged since it saw them."""
        from makewand.candidate import ManifestBudget, build_manifest
        what = f"工作区 {self.root} " if host else "DAG 私有暂存区"
        return build_manifest(path, host=host, cache=cache, budget=ManifestBudget(what))

    def _staging_manifest(self) -> Dict[str, Any]:
        """The staging copy's manifest; every stage reuses the records of its unchanged files."""
        return self._manifest(self.workspace, cache=self._manifests)

    def _missing_inputs(self) -> List[str]:
        """Ignored files and skipped dependency/build directories the private copy lacks."""
        from makewand.git_helper import find_git_root, run_git_cmd
        names: List[str] = []
        if find_git_root(self.root):
            for extra in (["--ignored"], []):
                code, out, _ = run_git_cmd(["git", "ls-files", "-z", "--others", "--exclude-standard", "--directory", *extra],
                                           cwd=self.root, binary=True)
                if code == 0:
                    names += [os.fsdecode(name) for name in out.split(b"\0") if name]
        else:
            try:
                names = sorted(os.listdir(self.root))
            except OSError:
                return []
        missing = []
        for name in dict.fromkeys(names):
            parts = [part for part in name.split("/") if part]
            if (not parts or ".git" in parts or any(part in _CACHE_NAMES for part in parts)
                    or name.endswith((".pyc", ".pyo"))):
                continue
            if not os.path.lexists(self.workspace / "/".join(parts)):
                missing.append(name)
        return missing

    def _carry_ignore_rules(self, copy) -> None:
        """Give a private copy the repository's ignore rules beyond its .gitignore files.

        .gitignore files come with the copied tree, but $GIT_DIR/info/exclude and a
        core.excludesFile set in the repository's own configuration do not: a path
        the user's repository ignores only there (a .venv/ in info/exclude) would be
        untracked and not ignored in the copy, so a checkpoint would commit it and
        apply would deliver it. The copy gets the repository's info/exclude and, when
        the effective core.excludesFile differs, the repository's one. The copy's
        info/exclude may be a link git init copied from init.templateDir: it is
        replaced by a regular file, never written through (write_repository_file).
        """
        from makewand.git_helper import run_git_cmd, write_repository_file

        def git_path(cwd: str, *arguments: str) -> Optional[str]:
            """A path Git prints, or None when it prints none; "" when the value is set empty."""
            code, out, _ = run_git_cmd(["git", *arguments], cwd=cwd)
            if code:
                return None
            value = out.rstrip("\r\n")
            # Relative to the work tree root, as Git resolves both.
            return os.path.join(cwd, value) if value else ""

        source, target = (git_path(path, "rev-parse", "--git-path", "info/exclude") for path in (self.root, str(copy)))
        if not source or not target:
            raise OSError("无法定位仓库的 .git/info/exclude，私有暂存区无法沿用其忽略规则")
        git_dir = os.path.join(str(copy), ".git")
        relative = os.path.relpath(target, git_dir)
        if os.path.isabs(relative) or relative.split(os.sep)[0] == os.pardir:
            raise OSError(f"私有暂存区的 info/exclude ({target}) 不在其 .git 目录中，无法沿用仓库的忽略规则")
        try:
            with open(source, "rb") as stream:
                rules = stream.read()
        except FileNotFoundError:
            rules = b""
        write_repository_file(git_dir, relative.replace(os.sep, "/"), rules)
        # Unset means the default excludes file, the same for both; set empty means none.
        excludes, current = (git_path(path, "config", "--type=path", "--get", "core.excludesFile")
                             for path in (self.root, str(copy)))
        if excludes is not None and excludes != current:
            code, _, error = run_git_cmd(["git", "config", "core.excludesFile", excludes], cwd=str(copy))
            if code:
                raise OSError(f"私有暂存区无法沿用仓库的 core.excludesFile: {error}")

    def _unsupported_entries(self, revision: str) -> List[str]:
        """Staged changes against revision that involve a symlink or a gitlink.

        Candidate application writes regular files only and refuses these, also
        with --force, so a result containing one can never be delivered by apply.
        """
        from makewand.git_helper import run_git_cmd
        code, out, error = run_git_cmd(["git", "diff-index", "--cached", "--raw", "-z", "--no-renames", revision],
                                       cwd=str(self.workspace), binary=True)
        if code:
            raise OSError(f"无法检查暂存区条目类型: {os.fsdecode(error) if isinstance(error, bytes) else error}")
        tokens = out.split(b"\0")
        paths = []
        for header, name in zip(tokens[0::2], tokens[1::2]):
            modes = header.lstrip(b":").split()[:2]
            if {b"120000", b"160000"} & set(modes):
                paths.append(os.fsdecode(name))
        return paths

    def _unwritable_entries(self) -> List[str]:
        """Changes since the DAG base that makewand apply refuses to write, also with --force.

        apply writes only files of the sealed manifest, never with special
        permission bits (setuid, setgid, sticky), and deletes only a path the
        result no longer has at all (not a file that became a directory);
        symlinks and gitlinks are found by _unsupported_entries.
        """
        from makewand.candidate import get_candidate_files_changed
        paths = []
        for path, status in get_candidate_files_changed(self.workspace, self.base_commit).items():
            record = self.verified_manifest.get(path)
            if status == "D":
                if record is not None or (self.workspace / path).exists():
                    paths.append(path)
            elif record is None or record.get("mode", 0) & 0o7000:
                paths.append(path)
        return sorted(paths)

    def _content_drift(self, revision: str) -> List[str]:
        """Git-visible paths whose content differs from revision, every file hashed afresh.

        A private index without stat data makes Git read each file, so a write
        that keeps size and timestamps is still seen (file snapshots may not).
        """
        from makewand.git_helper import _read_workspace_diff
        out, error = _read_workspace_diff(str(self.workspace), revision,
                                          arguments=("--no-renames", "--name-only", "-z"), binary=True)
        if error:
            raise OSError(f"无法核对 DAG 暂存区内容: {error}")
        return sorted({os.fsdecode(name) for name in out.split(b"\0") if name})

    def checkpoint(self, task_id: str) -> None:
        """Commit a passed code stage inside the private copy without changing its content."""
        from makewand.artifact import workspace_snapshot
        from makewand.git_helper import add_all, dirty_submodule_paths, run_git_cmd
        root = str(self.workspace)
        verified = workspace_snapshot(root)
        code, _, error = add_all(root)
        if code:
            raise OSError(f"无法暂存阶段 {task_id} 的结果: {error}")
        unsupported = self._unsupported_entries("HEAD")
        code, staged, error = run_git_cmd(["git", "diff", "--cached", "--name-only"], cwd=root)
        if code:
            raise OSError(f"无法读取阶段 {task_id} 的暂存结果: {error}")
        if staged.strip():
            code, _, error = run_git_cmd(["git", "-c", "user.name=Makewand", "-c", "user.email=makewand@local",
                                          "commit", "--no-verify", "-q", "-m", f"makewand dag: {task_id}"], cwd=root)
            if code:
                raise OSError(f"无法提交阶段 {task_id} 的结果: {error}")
        code, status, error = run_git_cmd(["git", "status", "--porcelain"], cwd=root)
        if code == 0 and not status.strip():
            status = " ".join(dirty_submodule_paths(root))  # git status no longer enters a submodule
        if code or status.strip():
            raise OSError(f"阶段 {task_id} 提交后暂存区仍有未受控改动: {(status or error).strip()[:200]}")
        if workspace_snapshot(root) != verified:
            raise OSError(f"阶段 {task_id} 的已验收内容在提交期间发生变化")
        code, head, error = run_git_cmd(["git", "rev-parse", "--verify", "HEAD^{commit}"], cwd=root)
        if code or not head.strip():
            raise OSError(f"无法读取阶段 {task_id} 的提交: {error}")
        self.head_commit = head.strip()
        self.verified_snapshot = verified
        self.verified_manifest = self._staging_manifest()
        self.passed.append(task_id)
        self.code_passed.append(task_id)
        if unsupported and self.blocked is None:
            self.blocked = {"task": task_id, "paths": unsupported}
        if self.blocked is None:
            unwritable = self._unwritable_entries()
            if unwritable:
                self.blocked = {"task": task_id, "paths": unwritable, "reason": "unwritable"}

    def show_cumulative_changes(self) -> str:
        """Present everything since the DAG base as uncommitted changes, for a read-only stage.

        HEAD and the index move back to the base commit; no file changes. Returns
        the checkpoint commit that restore_after_readonly() returns to.
        """
        from makewand.git_helper import run_git_cmd
        root = str(self.workspace)
        if not self.unchanged_since_last_checkpoint():
            raise OSError("DAG 暂存区内容与最后一次验收结果不一致")
        self._readonly_entries = _tree_entries(root)
        code, _, error = run_git_cmd(["git", "reset", "-q", self.base_commit], cwd=root)
        if code:
            run_git_cmd(["git", "reset", "-q", self.head_commit], cwd=root)
            raise OSError(f"无法向只读子任务呈现累积改动: {error}")
        return self.head_commit

    def restore_after_readonly(self, head: str) -> List[str]:
        """Return to the checkpoint after a read-only stage; discard and list anything it wrote.

        Git restores tracked content. Whatever else the stage left (new files and
        directories, nested repositories, ignored files it wrote, including in
        dependency/build directories) is found by comparing the whole tree with the
        walk taken before the stage, and removed. Ignored files it deleted cannot be
        restored (the copy keeps no backup of tool output); they are listed too.
        """
        from makewand.artifact import workspace_snapshot
        from makewand.git_helper import run_git_cmd
        root = str(self.workspace)
        code, _, error = run_git_cmd(["git", "reset", "-q", head], cwd=root)
        if code:
            raise OSError(f"无法恢复 DAG 暂存区的验收提交: {error}")
        before, self._readonly_entries = self._readonly_entries, None
        stray: List[str] = []
        reported: List[str] = []
        if before is not None:
            after = _tree_entries(root)
            # Put back directory modes first: a directory made unreadable hides its entries.
            modes = sorted(path for path, record in after.items()
                           if path in before and record[0] == before[path][0] == "dir" and record != before[path])
            for path in modes:
                os.chmod(os.path.join(root, path), before[path][1])
            if modes:
                after = _tree_entries(root)
            stray, lost = _stray_entries(before, after, set(self.verified_snapshot))
            reported = _entry_names(stray, after) + _entry_names(lost, before) + [path + "/" for path in modes]
            for path in stray:
                self._discard_entry(path, "无法丢弃只读子任务写入的内容")
        snapshot, manifest, drift = workspace_snapshot(root), self._staging_manifest(), self._content_drift(head)
        if (not reported and not drift and snapshot == self.verified_snapshot
                and manifest == self.verified_manifest):
            return []
        written = sorted(set(reported) | set(drift)
                         | {path for current, verified in ((snapshot, self.verified_snapshot), (manifest, self.verified_manifest))
                            for path in current.keys() | verified.keys() if current.get(path) != verified.get(path)})
        # Tracked content comes back from the checkpoint commit.
        for command in (["git", "reset", "-q", "--hard", head], ["git", "clean", "-fdq"]):
            code, _, error = run_git_cmd(command, cwd=root)
            if code:
                raise OSError(f"无法丢弃只读子任务写入的内容: {error}")
        # Git skips files whose stat data looks unchanged: rewrite any still differing.
        for path in self._content_drift(head):
            self._discard_entry(path, "无法丢弃只读子任务改动的文件")
            code, _, _ = run_git_cmd(["git", "cat-file", "-e", f"{head}:{path}"], cwd=root)
            if code == 0:
                code, _, error = run_git_cmd(["git", "checkout", "-q", head, "--", path], cwd=root)
                if code:
                    raise OSError(f"无法恢复文件 {path}: {error}")
        if before is not None:
            # Directories hold no content of their own, and git clean drops empty
            # ones: put back every directory the checkpoint had (the stage's cwd may be one).
            for path in sorted(path for path, record in before.items() if record[0] == "dir"):
                target = os.path.join(root, path)
                if not os.path.lexists(target):
                    os.mkdir(target)
                    os.chmod(target, before[path][1])
        # Git restores content and the executable bit only; put back other modes.
        current = workspace_snapshot(root)
        for path, record in self.verified_snapshot.items():
            now = current.get(path)
            if record[0] == "file" and now is not None and now[:2] == record[:2] and now[2] != record[2]:
                os.chmod(os.path.join(root, path), record[2])
        leftover = sorted(path for path in _tree_entries(root) if path not in before) if before is not None else []
        if self._content_drift(head) or workspace_snapshot(root) != self.verified_snapshot or leftover:
            raise OSError("只读子任务写入的内容无法完全还原到最后验收状态"
                          + (f" (仍残留: {_short_list(leftover)})" if leftover else ""))
        self.verified_manifest = self._staging_manifest()
        return written

    def _discard_entry(self, relative: str, failure: str) -> None:
        """Remove one entry of the staging copy: a file, a link or a whole directory (also a nested repository)."""
        import stat
        from makewand.candidate import _atomic_remove
        path = os.path.join(str(self.workspace), relative)
        try:
            if stat.S_ISDIR(os.lstat(path).st_mode):
                _remove_tree(path)
            else:
                _atomic_remove(str(self.workspace), relative)
        except FileNotFoundError:
            return
        except (OSError, ValueError) as exc:
            raise OSError(f"{failure} {relative}: {exc}")

    def record_readonly(self, task_id: str, intent: str, had_changes: bool) -> None:
        self.passed.append(task_id)
        self.readonly_passed[task_id] = intent
        if intent == "review" and not had_changes:
            self.reviewed_nothing.append(task_id)

    def unchanged_since_last_checkpoint(self) -> bool:
        from makewand.artifact import workspace_snapshot
        try:
            return (workspace_snapshot(self.workspace) == self.verified_snapshot
                    and self._staging_manifest() == self.verified_manifest)
        except OSError:
            return False

    def has_changes(self) -> bool:
        """True when some code stage committed a change since the DAG base."""
        return self.head_commit != self.base_commit

    def stage_summary(self, total: int) -> str:
        """What the passed stages did, without claiming tests or reviews that never ran."""
        if not self.readonly_passed:
            if len(self.passed) == total:
                return f"全部 {total} 个子任务均通过本地测试与独立审查"
            return f"子任务 {', '.join(self.passed)} 均通过本地测试与独立审查"
        parts = []
        if self.code_passed:
            parts.append(f"代码子任务 {', '.join(self.code_passed)} 均通过本地测试与独立审查")
        labels = {"review": "审查", "explain": "解释", "identity": "问答"}
        readonly = []
        for task_id, intent in self.readonly_passed.items():
            note = labels.get(intent, intent)
            if intent == "review":
                note += ": 当时没有累积改动可审查" if task_id in self.reviewed_nothing else ": 已独立审查此前的累积改动"
            readonly.append(f"{task_id} ({note})")
        parts.append(f"只读子任务 {', '.join(readonly)} 未修改代码")
        head = f"全部 {total} 个子任务已完成" if len(self.passed) == total else f"子任务 {', '.join(self.passed)} 已完成"
        return f"{head} ({'；'.join(parts)})"

    def discarded_note(self) -> str:
        if not self.discarded:
            return ""
        items = "; ".join(f"{task_id}: {_short_list(paths)}" for task_id, paths in self.discarded.items())
        return f"只读子任务运行期间写入或改动的文件已丢弃 (未审查、未提交、未交付): {items}"

    def report(self, total: int, approved: bool, rejected_by: Optional[str] = None) -> str:
        complete = len(self.passed) == total
        lines = [f"[Makewand DAG] {'全部' if complete else '部分'}子任务 ({len(self.passed)}/{total}: {', '.join(self.passed)}) "
                 "在同一私有暂存区依次执行，每个子任务基于前序子任务的结果；" + self.stage_summary(total) + "。"]
        if self.discarded:
            lines.append(self.discarded_note() + "。")
        if not approved:
            lines.append(f"累积改动未通过只读审查子任务 {rejected_by} 的独立审查，未获批准。" if rejected_by
                         else "累积改动未获批准。")
        if self.blocked:
            lines.append(f"累积改动含{_blocked_entries(self.blocked)}，"
                         "makewand apply 无法交付，需从私有暂存区人工恢复。")
        lines.append("本候选为已验收代码改动的累积结果。")
        return "\n".join(lines)

    def record(self, total: int) -> Dict[str, Any]:
        stages = [{"task": task_id, "kind": "code" if task_id in self.code_passed else self.readonly_passed.get(task_id)}
                  for task_id in self.passed]
        return {"total": total, "complete": len(self.passed) == total, "stages": stages,
                "root": self.root, "subdirectory": self.relative, "base_commit": self.base_commit,
                "discarded": self.discarded, "blocked": self.blocked}

    def seal(self, goal: str, total: int, approved: bool = True, rejected_by: Optional[str] = None) -> str:
        """Seal the verified cumulative result as candidate A of a record in the candidate store.

        save_race re-checks the content manifest and freezes the application plan.
        """
        from makewand.candidate import CandidateManager
        from makewand.git_helper import dirty_submodule_paths, get_git_diff_status, run_git_cmd
        if not self.unchanged_since_last_checkpoint():
            raise OSError("DAG 暂存区内容与最后一次验收结果不一致，拒绝封存")
        # An interrupted or failed stage may leave HEAD or the index elsewhere;
        # the files are verified, so only the Git pointers return to the checkpoint.
        code, _, error = run_git_cmd(["git", "reset", "-q", self.head_commit], cwd=str(self.workspace))
        if code:
            raise OSError(f"无法恢复 DAG 暂存区的验收提交: {error}")
        code, status, error = run_git_cmd(["git", "status", "--porcelain"], cwd=str(self.workspace))
        if code == 0 and not status.strip():
            status = " ".join(dirty_submodule_paths(self.workspace))  # git status no longer enters a submodule
        if code or status.strip() or not self.unchanged_since_last_checkpoint() or self._content_drift(self.head_commit):
            raise OSError(f"DAG 暂存区与最后一次验收提交不一致，拒绝封存: {(status or error).strip()[:200]}")
        unsupported = self._unsupported_entries(self.base_commit)
        if unsupported and self.blocked is None:
            self.blocked = {"task": None, "paths": unsupported}
        if self.blocked is None:
            unwritable = self._unwritable_entries()
            if unwritable:
                self.blocked = {"task": None, "paths": unwritable, "reason": "unwritable"}
        diff, error = get_git_diff_status(str(self.workspace), base_rev=self.base_commit)
        if error:
            raise OSError(f"无法导出 DAG 累积改动: {error}")
        complete = len(self.passed) == total
        CandidateManager.save_race(
            race_id=self.dag_id,
            prompt=goal if complete else f"[部分结果 {len(self.passed)}/{total}] {goal}",
            base_cwd=self.root,
            baseline_commit=self.host_head,
            agent_a={
                "model": "makewand-dag",
                "path": str(self.workspace),
                "duration": 0.0,
                "success": True,
                "test_passed": True,
                "test_details": f"DAG 代码子任务 {', '.join(self.code_passed)} 的本地测试均已通过",
                "review_passed": approved,
                "manifest": self.verified_manifest,
                "diff": diff,
                "baseline_commit": self.base_commit,
            },
            agent_b={},
            judge_report=self.report(total, approved, rejected_by),
            winner="A",
            baseline_dir=self.baseline,
            # The preimage apply checks is what the stages saw: the frozen copy.
            # A workspace file it lacks (ignored, untracked dependency/build
            # output) is never overwritten unchecked.
            baseline_manifest=self.frozen_manifest,
            frozen_baseline_manifest=self.frozen_manifest,
            dag=self.record(total),
        )
        self.sealed = True
        self.release()
        return self.dag_id

    def release(self) -> None:
        """Let the candidate store manage (prune) this copy like any other entry."""
        claim, self._claim = self._claim, None
        if claim is not None:
            claim.release()

    def discard(self) -> None:
        import shutil
        if not self.sealed:
            shutil.rmtree(self.session, ignore_errors=True)
        self.release()


def _dag_result(ok: bool, status: str, summary: str):
    """Expose the DAG outcome to callers that map it to an exit status.

    Always recorded, or raised: the result a stage's pipeline left behind must never
    stand for the DAG's own outcome. A NUL in the summary (which no result text may
    carry) is escaped; an outcome the execution contract refuses otherwise (a status
    it does not know, PASSED for a run that did not pass) is a defect and raises
    ValueError.
    """
    from makewand.execution_contract import ExecutionResult
    from makewand.execution_runtime import task_id
    from makewand.workflow import remember_result
    error = None if ok else summary.replace("\0", "\\0")
    remember_result(ExecutionResult(ok if status == "PASSED" else False, None, error,
                                    status=status, task_id=task_id(), stage="workflow", engine="dag"))


def execute_task_dag(
    dag: TaskDAG,
    cwd: Optional[str] = None,
    tier: str = "auto",
    auto_fix: bool = True,
    repo_trust: str = "trusted",
    stream: bool = False,
    tiered: bool = False,
    architect_engine: Optional[str] = None,
    worker_engine: Optional[str] = None,
    local_only: bool = False,
    effort: Optional[str] = None,
) -> Tuple[bool, str, List[Dict[str, Any]]]:
    """
    Executes a TaskDAG in topological stages with optional Architect-Worker tiered dispatch.
    - Architect (deep reasoning, e.g. Claude 3.7 / Codex): handles Stage 1 design/contracts & final audit.
    - Worker (fast lightweight, e.g. local / fast API): executes intermediate implementation nodes.
    - local_only / offline constraint is strictly propagated to every stage and task.
    Each task executes through the verified pipeline (tests and red-team review).
    With more than one task, every task runs in one private staging copy of the
    repository so it builds on the earlier tasks' results; read-only tasks
    (review/explain) see the cumulative changes and never contribute any. The
    cumulative result is delivered once at the end (applied when the workspace is
    safe to write, otherwise sealed and pending). The returned summary states
    exactly what was delivered or is still pending.
    """
    from makewand.workflow import last_result, remember_result
    valid, errors = dag.validate()
    if not valid:
        err_msg = f"DAG 拓扑结构校验失败: {'; '.join(errors)}"
        print(c(f"❌ {err_msg}", COLOR_RED + COLOR_BOLD))
        _dag_result(False, "INVALID_REQUEST", err_msg)
        return False, err_msg, []

    stages = dag.topological_stages()
    stage_results: List[Dict[str, Any]] = []
    total = sum(len(stage) for stage in stages)
    target = os.path.abspath(cwd or os.getcwd())
    # Every task writes to target, a multi-task result even outside any pipeline:
    # the test harness refuses makewand's own source tree before any staging copy
    # is made or any task runs. The rule and its one source root
    # (git_helper._makewand_source_root, which tests replace with a stand-in) are
    # those the final apply asks too, so the early refusal and the apply never disagree.
    from makewand.git_helper import harness_source_tree_conflict
    harness_conflict = harness_source_tree_conflict(target, "执行 DAG 子任务")
    if harness_conflict:
        msg = f"{harness_conflict}，未执行任何子任务。"
        print(c(f"❌ [Makewand Test Harness] {msg}", COLOR_RED + COLOR_BOLD))
        _dag_result(False, "UNVERIFIED", msg)
        return False, msg, []

    staging: Optional[DagStaging] = None
    if total > 1:
        if _native_windows():
            msg = ("原生 Windows 暂不支持多阶段 DAG：各子任务须在同一私有暂存区中依次执行并基于前序结果继续，"
                   "该暂存模式依赖 POSIX 主机事务。请在 WSL2 中运行，或把各子任务作为单独任务逐个执行。"
                   "未执行任何子任务，工作区未修改。")
            print(c(f"❌ [Makewand DAG Engine] {msg}", COLOR_RED + COLOR_BOLD))
            _dag_result(False, "UNVERIFIED", msg)
            return False, msg, []
        try:
            staging = DagStaging(target)
        except (OSError, ValueError) as exc:
            msg = f"无法建立 DAG 私有暂存工作区，未执行任何子任务、工作区未修改: {exc}"
            print(c(f"❌ [Makewand DAG Engine] {msg}", COLOR_RED + COLOR_BOLD))
            _dag_result(False, "UNVERIFIED", msg)
            return False, msg, []
        print(c(f"🧱 [Makewand DAG Engine] {total} 个子任务将在同一私有暂存区依次执行 (后一子任务基于前序已验收结果): {staging.workspace}", COLOR_CYAN))
        if staging.relative != ".":
            print(c(f"   已完整暂存仓库根目录 {staging.root}，各子任务在其中的 {staging.relative} 目录执行。", COLOR_CYAN))
        print(c("   工作区在全部子任务通过前不会被修改；累积结果最后一次性交付。", COLOR_CYAN))
        if staging.missing_inputs:
            print(c(f"   ℹ {_missing_inputs_note(staging)}", COLOR_YELLOW))

    failure = None
    try:
        for stage_idx, stage in enumerate(stages, start=1):
            print(c(f"\n==================================================", COLOR_BOLD + COLOR_CYAN))
            print(c(f"🚀 开始执行拓扑阶段 {stage_idx}/{len(stages)} (包含 {len(stage)} 个任务节点)", COLOR_BOLD + COLOR_CYAN))
            print(c(f"==================================================", COLOR_BOLD + COLOR_CYAN))

            for task in stage:
                print(c(f"\n▶ 正在推进子任务 [{task.task_id}]: {task.title}", COLOR_BOLD + COLOR_YELLOW))
                task_prompt = (
                    f"【DAG 拓扑子任务 {task.task_id}: {task.title}】\n"
                    f"子任务要求: {task.description}\n"
                )
                if task.target_files:
                    task_prompt += f"重点改动文件: {', '.join(task.target_files)}\n"
                task_prompt += f"全局最终目标: {dag.goal}\n"
                if staging is not None and staging.code_passed:
                    task_prompt += f"前序子任务 {', '.join(staging.code_passed)} 的已验收结果已在当前工作目录中，请在其基础上继续。\n"

                # Determine task tier and engine when tiered dispatch is enabled
                task_tier = tier
                task_forced_engine = None
                if tiered:
                    # Stage 1 (contracts & architecture) and final stage (quality audit / review) use Architect (power tier).
                    # Intermediate implementation stages use Worker (fast tier).
                    is_architect_stage = (stage_idx == 1) or (len(stages) >= 3 and stage_idx == len(stages))
                    if not is_architect_stage:
                        title_desc = f"{task.title} {task.description}".lower()
                        if any(kw in title_desc for kw in ("audit", "review", "verification", "终审", "审查", "验收")):
                            is_architect_stage = True

                    if is_architect_stage:
                        task_tier = "power" if tier == "auto" else tier
                        task_forced_engine = architect_engine
                        role_desc = "架构设计" if stage_idx == 1 else "终审质检"
                        print(c(f"🏛️  [Architect-Worker] 子任务指派架构师角色 ({role_desc}, Tier: {task_tier})", COLOR_PURPLE))
                    else:
                        task_tier = "fast" if tier == "auto" else tier
                        task_forced_engine = worker_engine
                        print(c(f"⚡ [Architect-Worker] 子任务指派执行工兵角色 (功能实施, Tier: {task_tier})", COLOR_BLUE))

                task.status = "running"

                orch = sys.modules.get("makewand.orchestrator")
                pipeline_fn = getattr(orch, "run_pipeline", None) if orch else None
                if pipeline_fn is None:
                    from makewand.orchestrator import run_pipeline
                    pipeline_fn = run_pipeline

                options = dict(
                    cwd=target if staging is None else str(staging.stage_dir),
                    tier=task_tier,
                    effort=effort,
                    forced_engine=task_forced_engine,
                    stream=stream,
                    auto_fix=auto_fix,
                    force_code=True,
                    repo_trust=repo_trust,
                    local_only=local_only,
                )
                readonly = _readonly_intent(task_prompt) if staging is not None else None
                if staging is not None:
                    options["_private_workspace"] = True
                remember_result(None)
                ok, failure_detail, discarded, had_changes = False, "", [], False
                if readonly is not None:
                    # A read-only node reviews/explains the cumulative result, which the
                    # checkpoints keep committed: show it as uncommitted changes, then
                    # return to the checkpoint and drop whatever the node wrote.
                    head = None
                    try:
                        head = staging.show_cumulative_changes()
                        had_changes = head != staging.base_commit
                    except OSError as exc:
                        failure_detail = str(exc)
                    if head is not None:
                        restore_error = None
                        try:
                            ok = pipeline_fn(task_prompt, **options)
                        finally:
                            try:
                                discarded = staging.restore_after_readonly(head)
                            except OSError as exc:
                                restore_error = str(exc)
                        if restore_error:
                            ok, failure_detail = False, restore_error
                else:
                    ok = pipeline_fn(task_prompt, **options)
                outcome = last_result()
                stage_status = getattr(outcome, "status", None) or ("PASSED" if ok else "FAILED")
                stage_error = outcome[2] if isinstance(outcome, tuple) and len(outcome) == 3 and isinstance(outcome[2], str) else ""
                if failure_detail:
                    stage_status = "UNVERIFIED"
                if discarded:
                    staging.discarded[task.task_id] = discarded
                    print(c(f"⚠️ [Makewand DAG Engine] 只读子任务 [{task.task_id}] 运行期间写入或改动了文件，已全部丢弃 "
                            f"(未审查、不会提交或交付): {_short_list(discarded)}", COLOR_YELLOW + COLOR_BOLD))
                if ok and staging is not None:
                    if readonly is not None:
                        staging.record_readonly(task.task_id, readonly, had_changes)
                    else:
                        try:
                            staging.checkpoint(task.task_id)
                        except (OSError, ValueError) as exc:
                            ok, stage_status, failure_detail = False, "UNVERIFIED", str(exc)

                kind = {} if staging is None else {"kind": readonly or "code"}
                if not ok:
                    task.status = "failed"
                    stage_results.append({"stage": stage_idx, "task": task.task_id, "status": "failed", **kind})
                    failure = (task, stage_status, failure_detail, readonly, stage_error)
                    break

                task.status = "passed"
                print(c(f"✔ 子任务 [{task.task_id}] 交付验收通过！", COLOR_GREEN + COLOR_BOLD))
                stage_results.append({"stage": stage_idx, "task": task.task_id, "status": "passed", **kind})
                if staging is not None and staging.blocked:
                    break
            if failure is not None or (staging is not None and staging.blocked):
                break

        if failure is not None:
            return _report_failure(staging, dag.goal, total, stage_results, *failure)
        if staging is None:
            msg = "DAG 仅含 1 个子任务，已按单任务流水线完成验收；交付方式与位置见上方流水线交付报告。"
            print(c(f"\n✔ [Makewand DAG Engine] {msg}", COLOR_GREEN + COLOR_BOLD))
            _dag_result(True, "PASSED", msg)
            return True, msg, stage_results
        if staging.blocked:
            return _deliver_blocked(staging, dag.goal, total, stage_results)
        return _deliver_dag(staging, dag.goal, total, stage_results)
    except BaseException as exc:
        if staging is not None:
            _abandon_staging(staging, total, dag.goal, exc)
        raise
    finally:
        if staging is not None:
            staging.release()


def _kept_note(staging: "DagStaging") -> str:
    if not staging.workspace.exists():
        return "私有暂存区已不存在"
    return f"私有暂存区保留于 (未封存，候选数量超过上限时可能被自动清理): {staging.workspace}"


def _missing_inputs_note(staging: "DagStaging") -> str:
    return (f"私有暂存区不包含被 .gitignore 忽略的文件或未跟踪的依赖/构建目录 ({_short_list(staging.missing_inputs)})；"
            "依赖它们的测试会在暂存区失败，可让测试命令自行准备依赖，或把子任务作为单独任务直接在工作区执行。")


def _outside_staging(staging: "DagStaging", path: str) -> bool:
    """Whether path, or a directory above it, was in the workspace but not copied into the staging copy."""
    for name in staging.missing_inputs:
        name = name.rstrip("/")
        if path == name or path.startswith(name + "/"):
            return True
    return False


def _manual_recovery(staging: "DagStaging") -> str:
    """Where the verified changes wait for manual recovery when makewand apply cannot deliver them."""
    if not staging.workspace.exists():
        return "私有暂存区已不存在"
    return (f"私有暂存区: {staging.workspace} (可用 git -C {staging.workspace} diff {staging.base_commit[:12]} HEAD "
            "导出这些改动后人工应用)")


def _apply_obstacles(staging: "DagStaging") -> Dict[str, str]:
    """Workspace entries that keep every makewand apply of the result from succeeding, also with --force."""
    from pathlib import Path
    from makewand.candidate import application_obstacles, get_candidate_files_changed
    changes = get_candidate_files_changed(Path(staging.workspace), staging.base_commit)
    return application_obstacles(staging.root, changes)


def _obstacle_guidance(staging: "DagStaging", obstacles: Dict[str, str]) -> str:
    """No apply to offer: say why and where the result is kept."""
    listed = _short_list([f"{path} ({why})" for path, why in sorted(obstacles.items())])
    return (f"工作区中的 {listed} 使 makewand apply 无法交付该结果 (apply 只写入普通文件且从不经过符号链接，"
            f"--force 也不例外)，因此不提供应用命令；已验收的累积改动保留供人工恢复，{_manual_recovery(staging)}")


def _apply_guidance(staging: "DagStaging", race_id: str) -> str:
    """How the pending candidate can be applied, checked the way apply will check it."""
    from pathlib import Path
    from makewand.candidate import CandidateManager, committable_work_in_progress, uncommitted_deletions
    apply_hint = f"makewand apply {race_id} --candidate A"
    inspect_hint = f"makewand inspect {race_id} --candidate A"
    baseline = staging.frozen_manifest  # the preimage the sealed record carries
    try:
        obstacles = _apply_obstacles(staging)
        conflicts = [] if obstacles else CandidateManager.detect_conflicts(
            staging.root, Path(staging.workspace), baseline_manifest=baseline,
            baseline_commit=staging.host_head or None, candidate_baseline_commit=staging.base_commit)
        committable = committable_work_in_progress(staging.root, conflicts, baseline)
    except (OSError, ValueError) as exc:
        return f"审查: {inspect_hint}；应用: {apply_hint} (无法预先检查冲突: {exc})"
    if obstacles:
        # Not even --force can write over them: no part of the guidance below could succeed.
        return f"{_obstacle_guidance(staging, obstacles)}。审查: {inspect_hint}"
    if not conflicts:
        return f"审查: {inspect_hint}；应用: {apply_hint}"
    parts = []
    deleted = uncommitted_deletions(committable, baseline)
    edited = [path for path in committable if path not in deleted]
    if edited:
        parts.append(f"本次改动涉及含未提交修改的文件 ({_short_list(edited)})，候选中的这些文件已包含你开始时的这些修改："
                     f"请先提交这些修改 (git commit；stash 会改变文件内容，无法解除冲突)，再执行: {apply_hint}")
    if deleted:
        parts.append(f"文件 {_short_list(deleted)} 在 DAG 开始前就已被删除 (删除尚未提交)，而 DAG 重新创建了这些文件："
                     f"要采用 DAG 的版本，请先提交这次删除 (git rm 这些文件后 git commit)，再执行: {apply_hint}；"
                     "要保留删除则不要应用")
    others = [path for path in conflicts if path not in committable]
    unstaged = [path for path in others if _outside_staging(staging, path)]
    if unstaged:
        parts.append(f"工作区中的 {_short_list(unstaged)} 在 DAG 开始时就已存在，但未复制进私有暂存区 "
                     f"(被 .gitignore 忽略或属于未跟踪的依赖/构建目录)，DAG 新建的同名文件会覆盖它们；"
                     f"请先核对，确认要覆盖时才执行: {apply_hint} --force")
    changed = [path for path in others if path not in unstaged]
    if changed:
        parts.append(f"文件 {_short_list(changed)} 在 DAG 运行期间被修改或新建，直接应用会被拒绝；请先核对差异，"
                     f"确认要用候选内容覆盖时才执行: {apply_hint} --force")
    return "；".join(parts) + f"。审查: {inspect_hint}"


def _undelivered_progress(staging: Optional["DagStaging"], total: int, goal: str,
                          rejected_by: Optional[str] = None) -> str:
    """Describe passed-but-undelivered stages after a failure or interrupt; seal them as pending."""
    if staging is None:
        return ""
    if not staging.passed:
        staging.discard()
        return " 没有任何子任务通过，工作区未被修改。"
    if not staging.has_changes() and staging.unchanged_since_last_checkpoint():
        staging.discard()
        return f" 已通过的子任务 ({', '.join(staging.passed)}) 没有产生需要交付的改动，工作区未被修改。"
    if not staging.workspace.exists():
        return (f" 前序 {len(staging.passed)} 个子任务已通过验收，但私有暂存区 {staging.workspace} 已不存在 (被外部删除)，"
                "其结果已丢失；工作区未被修改。")
    if not staging.unchanged_since_last_checkpoint():
        return (f" 前序 {len(staging.passed)} 个子任务已通过验收但未交付；暂存区未能还原到最后验收状态，"
                f"仅供人工检查: {_kept_note(staging)}")
    try:
        race_id = staging.seal(goal, total, approved=rejected_by is None, rejected_by=rejected_by)
    except (OSError, ValueError) as exc:
        return f" 前序 {len(staging.passed)} 个子任务已通过验收但未交付；封存失败 ({exc})，{_kept_note(staging)}"
    discarded = f"{staging.discarded_note()}。" if staging.discarded else ""
    if staging.blocked:  # first: not even --force could apply it
        return (f" 工作区未被修改。{discarded}已验收的累积改动含{_blocked_entries(staging.blocked)}，"
                f"makewand apply 无法交付；已封存为候选 {race_id} 仅供审查与人工恢复: makewand inspect {race_id} --candidate A；"
                f"私有暂存区: {staging.workspace}")
    if rejected_by:
        lead = (f" 工作区未被修改。{discarded}前序代码子任务 ({', '.join(staging.code_passed)}) 的累积改动未通过子任务 "
                f"{rejected_by} 的独立审查，已封存为候选 {race_id} 仅供检查 (未获批准): makewand inspect {race_id} --candidate A；")
        try:
            obstacles = _apply_obstacles(staging)
        except (OSError, ValueError):
            obstacles = {}
        if obstacles:
            return lead + _obstacle_guidance(staging, obstacles)
        return lead + f"人工确认后才可用 makewand apply {race_id} --candidate A --force 应用"
    return (f" 工作区未被修改。{discarded}前序 {len(staging.passed)}/{total} 个子任务 ({', '.join(staging.passed)}) 的已验收累积结果"
            f"已封存为待应用候选 {race_id}。如需部分结果，{_apply_guidance(staging, race_id)}")


def _report_failure(staging, goal, total, stage_results, task, stage_status, detail, readonly, stage_error):
    if readonly == "review" and stage_status == "FAILED" and not detail:
        msg = f"只读审查子任务 [{task.task_id}: {task.title}] 判定此前的累积改动不通过，DAG 流水线终止。"
        msg += _undelivered_progress(staging, total, goal, rejected_by=task.task_id)
    else:
        msg = f"子任务 [{task.task_id}: {task.title}] 未通过质量验收{f' ({detail})' if detail else ''}，DAG 流水线终止。"
        # The copy lacks ignored dependencies; say so when tests may be why the stage failed.
        if (staging is not None and readonly is None and staging.missing_inputs and not detail
                and (not stage_error or "测试" in stage_error)):
            msg += f" 注意: {_missing_inputs_note(staging)}"
        msg += _undelivered_progress(staging, total, goal)
    print(c(f"❌ {msg}", COLOR_RED + COLOR_BOLD))
    _dag_result(False, stage_status if stage_status != "PASSED" else "FAILED", msg)
    return False, msg, stage_results


def _sealed_note(staging: "DagStaging") -> str:
    return (f"累积结果已封存为候选 {staging.dag_id}，工作区可能尚未应用该结果；"
            f"查看与可用操作: makewand inspect {staging.dag_id}、makewand candidates")


def _abandon_staging(staging: "DagStaging", total: int, goal: str, error: BaseException) -> None:
    """An interrupted or crashed DAG never leaves a silent, unsealed staging copy behind.

    A second Ctrl-C while this seals stops the seal, not the report: what is
    kept is still printed and recorded, and the original error propagates.
    """
    reason = "被中断" if isinstance(error, KeyboardInterrupt) else f"异常终止 ({type(error).__name__})"
    if not staging.sealed and not staging.passed:
        try:
            staging.discard()
        except KeyboardInterrupt:
            pass  # an unclaimed leftover is pruned like any abandoned copy
        return
    try:
        note = _sealed_note(staging) if staging.sealed else _undelivered_progress(staging, total, goal).strip()
    except KeyboardInterrupt:
        # save_race publishes meta.json atomically: it exists only for a complete record.
        if (staging.session / "meta.json").is_file():
            staging.sealed = True
            note = f"封存过程中再次被中断，但{_sealed_note(staging)}"
        else:
            note = f"封存过程中再次被中断，已验收结果未能封存；{_kept_note(staging)}"
    except Exception as exc:  # noqa: BLE001 - reported; the original error propagates
        note = f"已验收结果未能封存 ({exc})；{_kept_note(staging)}"
    msg = f"DAG {reason}，前序 {len(staging.passed)} 个子任务已通过验收。{note}"
    print(c(f"\n⚠️ [Makewand DAG Engine] {msg}", COLOR_YELLOW + COLOR_BOLD))
    _dag_result(False, "CANCELLED" if isinstance(error, KeyboardInterrupt) else "INTERNAL_ERROR", msg)


def _deliver_blocked(staging: "DagStaging", goal: str, total: int, stage_results: List[Dict[str, Any]],
                     race_id: Optional[str] = None):
    """A stage produced a symlink or gitlink, which apply never writes: stop and keep it for recovery."""
    blocked = staging.blocked
    remaining = total - len(staging.passed)
    who = f"子任务 [{blocked['task']}] 在私有暂存区中" if blocked.get("task") else "DAG 的累积改动中"
    verb = "产生了" if blocked.get("reason") == "unwritable" else "创建、修改或删除了"
    msg = (f"{who}{verb}{_blocked_entries(blocked)}。"
           "makewand apply 出于安全原因只写入普通文件 (--force 也不例外)，无法交付该结果")
    msg += f"，因此 DAG 在此停止 (后续 {remaining} 个子任务未执行)" if remaining else ""
    msg += "；工作区未被修改。"
    try:
        race_id = race_id or staging.seal(goal, total)
        msg += f"全部已验收改动 (含该子任务) 已封存为候选 {race_id}，仅供审查与人工恢复: makewand inspect {race_id} --candidate A；"
    except (OSError, ValueError) as exc:
        msg += f"已验收改动封存失败 ({exc})；"
    msg += _manual_recovery(staging)
    print(c(f"\n❌ [Makewand DAG Engine] {msg}", COLOR_RED + COLOR_BOLD))
    _dag_result(False, "UNVERIFIED", msg)
    return False, msg, stage_results


def _deliver_dag(staging: "DagStaging", goal: str, total: int, stage_results: List[Dict[str, Any]]):
    """Deliver the cumulative result once: apply when safe, otherwise leave it pending."""
    done = staging.stage_summary(total)
    discarded = f" {staging.discarded_note()}。" if staging.discarded else ""
    if not staging.has_changes():
        staging.discard()
        msg = f"{done}；没有产生需要交付的代码改动，工作区未被修改。{discarded}"
        print(c(f"\n✔ [Makewand DAG Engine] {msg}", COLOR_GREEN + COLOR_BOLD))
        _dag_result(True, "PASSED", msg)
        return True, msg, stage_results
    try:
        race_id = staging.seal(goal, total)
    except (OSError, ValueError) as exc:
        msg = f"{done}，但累积结果封存失败 ({exc})，未交付到工作区；{_kept_note(staging)}"
        print(c(f"❌ [Makewand DAG Engine] {msg}", COLOR_RED + COLOR_BOLD))
        _dag_result(False, "UNVERIFIED", msg)
        return False, msg, stage_results
    if staging.blocked:
        return _deliver_blocked(staging, goal, total, stage_results, race_id)
    try:
        obstacles = _apply_obstacles(staging)
    except (OSError, ValueError):
        obstacles = {}  # apply checks its targets itself
    if obstacles:
        # Not even --force could write over them: attempt no apply and offer none.
        msg = (f"{done}，累积结果已封存为候选 {race_id}，但无法交付到工作区: {_obstacle_guidance(staging, obstacles)}。"
               f"审查: makewand inspect {race_id} --candidate A{discarded}")
        print(c(f"\n❌ [Makewand DAG Engine] {msg}", COLOR_RED + COLOR_BOLD))
        _dag_result(False, "APPLY_CONFLICT", msg)
        return False, msg, stage_results
    orch = sys.modules.get("makewand.orchestrator")
    isolation_check = getattr(orch, "check_working_tree_isolation", None) if orch else None
    if isolation_check is None:
        from makewand.git_helper import check_working_tree_isolation as isolation_check
    try:
        safe, why = isolation_check(staging.root)
    except Exception as exc:  # noqa: BLE001 - reported; never assume safe
        safe, why = False, f"工作区隔离检查异常 ({exc})"
    if not safe:
        msg = (f"{done}，累积结果已封存为候选 {race_id}，尚未交付到工作区 "
               f"(工作区当前不适合直接写入: {why})。{_apply_guidance(staging, race_id)}{discarded}")
        print(c(f"\n📦 [Makewand DAG Engine] {msg}", COLOR_YELLOW + COLOR_BOLD))
        _dag_result(True, "PASSED", msg)
        return True, msg, stage_results
    from makewand.candidate import CandidateManager, harness_source_tree_conflict
    from makewand.execution_contract import STATUS_CODES
    result = CandidateManager.apply_candidate(race_id=race_id, candidate_label="A")
    applied, files, detail = result
    if not applied:
        # apply's typed outcome (ApplyResult.status), never words of its message.
        status = getattr(result, "status", None)
        if status not in STATUS_CODES or status == "PASSED":
            status = "FAILED"
        conflict = status == "APPLY_CONFLICT"
        # The test harness refuses this workspace whatever is done there.
        refused = status == "UNVERIFIED" and harness_source_tree_conflict(staging.root) is not None
        if conflict:
            guidance = _apply_guidance(staging, race_id)
        elif refused:
            guidance = f"候选仅供审查: makewand inspect {race_id} --candidate A"
        else:
            guidance = f"处理后可执行: makewand apply {race_id} --candidate A"
        msg = f"{done}，累积结果已封存为候选 {race_id}，但自动应用到工作区失败: {detail}。{guidance}{discarded}"
        print(c(f"\n❌ [Makewand DAG Engine] {msg}", COLOR_RED + COLOR_BOLD))
        _dag_result(False, status, msg)
        return False, msg, stage_results
    msg = (f"{done}，累积结果已一次性应用到工作区 {staging.root} "
           f"({len(files)} 个文件变更，候选 {race_id})。{discarded}")
    for name in files:
        print(f"  • {name}")
    print(c(f"\n✔ [Makewand DAG Engine] {msg}", COLOR_GREEN + COLOR_BOLD))
    _dag_result(True, "PASSED", msg)
    return True, msg, stage_results
