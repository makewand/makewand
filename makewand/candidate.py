"""
Makewand Candidate Lifecycle Manager: inspect, apply, and discard race candidates.
"""

import os
import sys
import json
import shutil
import time
from makewand import filelock as fcntl
import uuid
import stat
from pathlib import Path
from datetime import datetime
from typing import Dict, Any, List, Optional, Tuple, Union

import makewand.config as config
from makewand.config import (
    ensure_config_dir,
    c,
    COLOR_BOLD,
    COLOR_CYAN,
    COLOR_GREEN,
    COLOR_YELLOW,
    COLOR_RED,
    COLOR_RESET,
)
import hashlib
from makewand.git_helper import run_git_cmd, get_git_diff

def file_sha256(path: Path) -> Optional[str]:
    """Computes SHA-256 hex digest of a regular file. Returns None for links/missing."""
    if not path.is_file() or os.path.islink(path):
        return None
    try:
        h = hashlib.sha256()
        with open(path, "rb") as f:
            while chunk := f.read(65536):
                h.update(chunk)
        return h.hexdigest()
    except Exception:
        return None

def file_record(path: Path) -> Optional[Dict[str, Any]]:
    digest = file_sha256(path)
    if digest is None:
        return None
    return {"sha256": digest, "mode": stat.S_IMODE(path.stat().st_mode)}


def build_manifest(dir_path: Path) -> Dict[str, Any]:
    """Bind regular file content AND permissions; old hash-only manifests fail closed."""
    manifest = {}
    if not dir_path.exists():
        return manifest
    for root, directories, files in os.walk(str(dir_path)):
        if Path(root) == dir_path:
            directories[:] = [directory for directory in directories if directory != ".git"]
        for f in files:
            p = Path(root) / f
            if not os.path.islink(p):
                rel = p.relative_to(dir_path).as_posix()
                parts = Path(rel).parts
                if parts and parts[0] == ".git":
                    continue
                record = file_record(p)
                if record is not None:
                    manifest[rel] = record
    return manifest


def _atomic_copy(workspace: str, rel_path: str, source: Path, expected=None):
    """Write through directory handles and atomically replace, preserving mode.

    The source is checked while copying, so a changed candidate cannot win the
    gap between manifest validation and application. No shared inode is edited.
    """
    parts = Path(rel_path).parts
    if not parts or Path(rel_path).is_absolute() or any(x in (".", "..") for x in parts):
        raise ValueError("invalid workspace-relative path")
    directory_flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    directory = os.open(workspace, directory_flags)
    temp_name = ".makewand-" + uuid.uuid4().hex
    created = False
    try:
        for component in parts[:-1]:
            try:
                os.mkdir(component, 0o755, dir_fd=directory)
            except FileExistsError:
                pass
            child = os.open(component, directory_flags, dir_fd=directory)
            os.close(directory)
            directory = child
        source_fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(source_fd, "rb") as src:
            source_stat = os.fstat(src.fileno())
            if not stat.S_ISREG(source_stat.st_mode):
                raise ValueError("source must be a regular file")
            mode = stat.S_IMODE(source_stat.st_mode)
            if expected is not None and mode & 0o7000:
                raise ValueError("candidate cannot introduce special permission bits")
            fd = os.open(temp_name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=directory)
            created = True
            with os.fdopen(fd, "wb") as dst:
                digest = hashlib.sha256()
                while chunk := src.read(65536):
                    digest.update(chunk)
                    dst.write(chunk)
                if expected is not None and expected != {"sha256": digest.hexdigest(), "mode": mode}:
                    raise ValueError("candidate changed during apply")
                dst.flush()
                os.fchmod(dst.fileno(), mode)
                os.fsync(dst.fileno())
        os.replace(temp_name, parts[-1], src_dir_fd=directory, dst_dir_fd=directory)
        created = False
        os.fsync(directory)
    finally:
        if created:
            os.unlink(temp_name, dir_fd=directory)
        os.close(directory)

def _verify_safe_target_path(base_cwd: Union[str, Path], rel_path: str) -> Path:
    canonical_base = os.path.realpath(base_cwd)
    cur = Path(base_cwd)
    for part in Path(rel_path).parts:
        cur = cur / part
        if os.path.islink(cur) or cur.is_symlink():
            raise ValueError(f"安全越界风险: 路径组件 {part} 包含符号链接")
        if cur.exists():
            resolved = os.path.realpath(cur)
            if not (resolved == canonical_base or resolved.startswith(canonical_base + os.sep)):
                raise ValueError(f"安全越界风险: 路径组件 {part} 逃逸出工作区 ({resolved})")
    return cur


def _atomic_remove(workspace: str, rel_path: str):
    """Remove a workspace entry without following mutable parent symlinks."""
    parts = Path(rel_path).parts
    if not parts or Path(rel_path).is_absolute() or any(x in (".", "..") for x in parts):
        raise ValueError("invalid workspace-relative path")
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    directory = os.open(workspace, flags)
    try:
        for component in parts[:-1]:
            child = os.open(component, flags, dir_fd=directory)
            os.close(directory)
            directory = child
        try:
            os.unlink(parts[-1], dir_fd=directory)
        except FileNotFoundError:
            pass
        os.fsync(directory)
    finally:
        os.close(directory)

def get_candidate_files_changed(candidate_dir: Path, baseline_commit: Optional[str] = None) -> Dict[str, str]:
    """
    Returns a dict mapping relative file path to change status ('M' modified, 'A' added, 'D' deleted).
    Captures BOTH uncommitted changes AND commits made by candidate since baseline_commit.
    Uses NUL-delimited parsing (--no-renames --name-status -z) and os.fsdecode to safely handle
    spaces, tabs, renames, and binary paths.
    """
    run_git_cmd(["git", "add", "-A", "--intent-to-add"], cwd=str(candidate_dir))
    changes = {}

    # 1. Compare directly against baseline_commit (covers both committed changes and working tree changes)
    ref = baseline_commit if baseline_commit else "HEAD"
    code, out_b, _ = run_git_cmd(["git", "diff", "--no-renames", "--name-status", "-z", ref], cwd=str(candidate_dir), binary=True)
    if code != 0 and not baseline_commit:
        code, out_b, _ = run_git_cmd(["git", "diff", "--no-renames", "--name-status", "-z"], cwd=str(candidate_dir), binary=True)

    if code == 0 and out_b:
        tokens = out_b.split(b"\0")
        i = 0
        while i < len(tokens) - 1:
            st_b = tokens[i]
            if not st_b:
                i += 1
                continue
            path_b = tokens[i + 1]
            i += 2
            if not path_b:
                continue
            st = os.fsdecode(st_b).strip()
            path = os.fsdecode(path_b)
            if st.startswith("D"):
                changes[path] = "D"
            elif st.startswith("A"):
                changes[path] = "A"
            else:
                changes[path] = "M"

    # 2. Also incorporate uncommitted worktree changes with safe 2-token rename parsing
    code, out_b, _ = run_git_cmd(["git", "status", "-z", "--porcelain"], cwd=str(candidate_dir), binary=True)
    if code == 0 and out_b:
        tokens = [t for t in out_b.split(b"\0") if t]
        idx = 0
        while idx < len(tokens):
            token = tokens[idx]
            if len(token) < 3:
                idx += 1
                continue
            st = os.fsdecode(token[:2]).strip()
            path = os.fsdecode(token[3:])
            idx += 1
            if st.startswith("R") or st.startswith("C"):
                # git status -z provides: <status> <new_path>\0<old_path>\0
                orig_path = os.fsdecode(tokens[idx]) if idx < len(tokens) else ""
                idx += 1
                if orig_path and orig_path not in changes:
                    changes[orig_path] = "D"
                if path and path not in changes:
                    changes[path] = "A"
                continue

            if path and path not in changes:
                if st in ("M", "MM", "AM"):
                    changes[path] = "M"
                elif st in ("A", "??"):
                    changes[path] = "A"
                elif st == "D":
                    changes[path] = "D"
                else:
                    changes[path] = "M"

    return changes

class CandidateManager:
    """Manages the lifecycle of race candidates."""

    @staticmethod
    def save_race(
        race_id: str,
        prompt: str,
        base_cwd: str,
        baseline_commit: str,
        agent_a: Dict[str, Any],
        agent_b: Dict[str, Any],
        judge_report: str = "",
        winner: Optional[str] = None
    ) -> Path:
        ensure_config_dir()
        race_dir = config.CANDIDATES_DIR / race_id
        race_dir.mkdir(parents=True, exist_ok=True)

        base_path = Path(base_cwd).resolve()
        baseline_manifest = build_manifest(base_path)

        # Attach frozen candidate manifests and ensure test_passed is explicitly set
        if "test_passed" not in agent_a:
            agent_a["test_passed"] = agent_a.get("success", True)
        if "test_passed" not in agent_b:
            agent_b["test_passed"] = agent_b.get("success", True)
        if "path" in agent_a and os.path.exists(agent_a["path"]):
            current = build_manifest(Path(agent_a["path"]))
            if "manifest" in agent_a and agent_a["manifest"] != current:
                raise ValueError("candidate A changed after review")
            agent_a["manifest"] = current
        if "path" in agent_b and os.path.exists(agent_b["path"]):
            current = build_manifest(Path(agent_b["path"]))
            if "manifest" in agent_b and agent_b["manifest"] != current:
                raise ValueError("candidate B changed after review")
            agent_b["manifest"] = current

        # Freeze the complete application plan, including deletions, outside the
        # candidate's writable Git metadata. A caller that performed a review
        # supplies the pre-review plan and any later change fails closed.
        for label, agent in (("A", agent_a), ("B", agent_b)):
            if agent.get("path") and os.path.exists(agent["path"]):
                changes = get_candidate_files_changed(
                    Path(agent["path"]), agent.get("baseline_commit") or baseline_commit)
                if "changes" in agent and agent["changes"] != changes:
                    raise ValueError(f"candidate {label} application plan changed after review")
                agent["changes"] = changes

        meta = {
            "race_id": race_id,
            "prompt": prompt,
            "base_cwd": str(base_path),
            "baseline_commit": baseline_commit,
            "baseline_manifest": baseline_manifest,
            "created_at": datetime.now().isoformat(),
            "status": "completed",
            "winner": winner,
            "judge_report": judge_report,
            "candidates": {
                "A": agent_a,
                "B": agent_b,
            }
        }

        meta_file = race_dir / "meta.json"
        with open(meta_file, "w", encoding="utf-8") as f:
            json.dump(meta, f, indent=2, ensure_ascii=False)

        # LRU eviction: keep only latest 3-5 candidates (default: 5)
        try:
            CandidateManager.prune_old_candidates(max_candidates=5)
        except Exception:
            pass

        return race_dir

    @staticmethod
    def prune_old_candidates(max_candidates: int = 5) -> int:
        """
        LRU eviction to keep only the latest candidates (default: 5)
        and clean up older candidate directories to prevent disk exhaustion.
        """
        ensure_config_dir()
        if not config.CANDIDATES_DIR.exists():
            return 0

        entries = []
        for entry in config.CANDIDATES_DIR.iterdir():
            if entry.is_dir() or entry.is_symlink():
                ts = 0.0
                meta_file = entry / "meta.json"
                if meta_file.exists():
                    try:
                        with open(meta_file, "r", encoding="utf-8") as f:
                            data = json.load(f)
                            c_str = data.get("created_at")
                            if c_str:
                                ts = datetime.fromisoformat(c_str).timestamp()
                    except Exception:
                        pass
                if ts <= 0.0:
                    try:
                        ts = entry.stat().st_mtime
                    except Exception:
                        ts = 0.0
                entries.append((entry, ts))

        # Sort descending by timestamp (newest first)
        entries.sort(key=lambda x: x[1], reverse=True)

        evicted = 0
        if len(entries) > max_candidates:
            to_remove = entries[max_candidates:]
            for entry, _ in to_remove:
                try:
                    if entry.is_symlink() or not entry.is_dir():
                        entry.unlink(missing_ok=True)
                    else:
                        shutil.rmtree(entry, ignore_errors=True)
                    evicted += 1
                except Exception:
                    pass
        return evicted

    @staticmethod
    def list_races() -> List[Dict[str, Any]]:
        ensure_config_dir()
        races = []
        if not config.CANDIDATES_DIR.exists():
            return races

        for entry in config.CANDIDATES_DIR.iterdir():
            if entry.is_dir():
                meta_file = entry / "meta.json"
                if meta_file.exists():
                    try:
                        with open(meta_file, "r", encoding="utf-8") as f:
                            data = json.load(f)
                            races.append(data)
                    except Exception:
                        pass

        races.sort(key=lambda x: x.get("created_at", ""), reverse=True)
        return races

    @staticmethod
    def get_race(race_id: Optional[str] = None) -> Optional[Dict[str, Any]]:
        races = CandidateManager.list_races()
        if not races:
            return None
        if not race_id:
            return races[0]

        for race in races:
            if race.get("race_id") == race_id or race.get("race_id", "").startswith(race_id):
                return race
        return None

    @staticmethod
    def detect_conflicts(
        base_cwd: str,
        candidate_dir: Path,
        baseline_manifest: Optional[Dict[str, str]] = None,
        baseline_commit: Optional[str] = None,
        candidate_baseline_commit: Optional[str] = None
    ) -> List[str]:
        """
        Checks if any file modified by the candidate has also been modified
        in base_cwd since the race baseline (uncommitted or committed).
        """
        candidate_changes = get_candidate_files_changed(candidate_dir, baseline_commit=candidate_baseline_commit)
        conflicts = []

        # 1. Uncommitted changes check in base_cwd (NUL-delimited parsing)
        code, diff_out, _ = run_git_cmd(["git", "status", "-z", "--porcelain"], cwd=base_cwd, binary=True)
        base_dirty_files = set()
        if code == 0 and diff_out:
            for token in diff_out.split(b"\0"):
                if len(token) >= 3:
                    fpath = token[3:].decode("utf-8", errors="replace").strip()
                    if fpath:
                        base_dirty_files.add(fpath)

        for changed_file in candidate_changes:
            if changed_file in base_dirty_files and changed_file not in conflicts:
                conflicts.append(changed_file)

        # 2. Baseline manifest hash comparison (preimage check)
        if baseline_manifest:
            for changed_file in candidate_changes:
                target = Path(base_cwd) / changed_file
                cur_hash = file_record(target) if target.exists() else None
                base_hash = baseline_manifest.get(changed_file)
                if cur_hash != base_hash and changed_file not in conflicts:
                    conflicts.append(changed_file)

        # 3. Git commit divergence check if baseline_commit was recorded
        if baseline_commit:
            c_code, c_out, _ = run_git_cmd(["git", "diff", "--no-renames", "--name-only", "-z", baseline_commit, "HEAD"], cwd=base_cwd, binary=True)
            if c_code == 0 and c_out:
                for token in c_out.split(b"\0"):
                    f = token.decode("utf-8", errors="replace").strip()
                    if f and f in candidate_changes and f not in conflicts:
                        conflicts.append(f)

        return conflicts

    @staticmethod
    def apply_candidate(
        race_id: Optional[str] = None,
        candidate_label: Optional[str] = None,
        dry_run: bool = False,
        force: bool = False
    ) -> Tuple[bool, List[str], str]:
        """
        Safely applies candidate changes to base_cwd with conflict detection and rollback journal.
        Guarded with a file lock to serialize Makewand apply operations.
        Returns (success, applied_files, message).
        """
        if os.name != "posix":
            return False, [], "安全候选应用需要 POSIX 目录句柄；Windows 请在 WSL2 中运行 makewand apply。"
        ensure_config_dir()
        lock_file = config.CONFIG_DIR / "apply.lock"
        lock_fd = None
        try:
            lock_fd = open(lock_file, "a")
            try:
                fcntl.flock(lock_fd, fcntl.LOCK_EX)
            except Exception as e:
                return False, [], f"无法获取候选应用独占锁 (apply.lock): {e}"
            return CandidateManager._do_apply_candidate(
                race_id=race_id,
                candidate_label=candidate_label,
                dry_run=dry_run,
                force=force
            )
        except Exception as e:
            return False, [], f"打开候选应用锁失败: {e}"
        finally:
            if lock_fd:
                try:
                    fcntl.flock(lock_fd, fcntl.LOCK_UN)
                    lock_fd.close()
                except Exception:
                    pass

    @staticmethod
    def _do_apply_candidate(
        race_id: Optional[str] = None,
        candidate_label: Optional[str] = None,
        dry_run: bool = False,
        force: bool = False
    ) -> Tuple[bool, List[str], str]:
        race = CandidateManager.get_race(race_id)
        if not race:
            return False, [], "未找到指定的候选竞速记录"

        r_id = race.get("race_id", "")
        base_cwd = race.get("base_cwd", "")
        if not os.path.exists(base_cwd):
            return False, [], f"原始工作区不存在: {base_cwd}"

        # Choose candidate (require explicit candidate if no winner)
        if not candidate_label and not race.get("winner"):
            return False, [], "竞速裁判未决出胜者，请显式指定待应用的候选方案: --candidate A 或 --candidate B"

        label = (candidate_label or race.get("winner")).upper()
        if label not in ("A", "B"):
            return False, [], f"无效的候选方案标识: {label}，仅支持 A 或 B"

        cand_info = race.get("candidates", {}).get(label, {})
        cand_path_str = cand_info.get("path")
        if not cand_path_str or not os.path.exists(cand_path_str):
            return False, [], f"候选选手 {label} 的工作区目录已丢失: {cand_path_str}"

        # Prevent applying failed candidate unless forced
        if not cand_info.get("success", True) and not force:
            return False, [], f"候选选手 {label} 任务执行状态为失败/未完成，已阻止应用未就绪的方案 (如需强制应用请使用 --force)"

        if cand_info.get("test_passed") is not True and not force:
            return False, [], f"候选选手 {label} 本地单元测试未通过或未完成测试验证 (test_passed != True)，已阻止应用存在缺陷的方案 (如需强制应用请使用 --force)"

        if cand_info.get("review_passed") is False and not force:
            return False, [], f"候选选手 {label} 未获裁判批准，已阻止应用 (人工确认后可使用 --force)"

        candidate_dir = Path(cand_path_str)

        # Integrity check: verify candidate files haven't been mutated after save
        expected_manifest = cand_info.get("manifest")
        if expected_manifest is None:
            return False, [], f"候选选手 {label} 缺少完整性清单 (manifest 缺失)，拒绝应用未审查内容"
        current_manifest = build_manifest(candidate_dir)
        if current_manifest != expected_manifest:
            return False, [], f"候选选手 {label} 的文件自封存后已被外部修改 (哈希校验不匹配)，拒绝应用未审查内容"

        cand_baseline = cand_info.get("baseline_commit") or race.get("baseline_commit")
        changes = cand_info.get("changes")
        if not isinstance(changes, dict):
            return False, [], "候选缺少封存的变更计划，请重新运行竞速后再应用"
        if changes != get_candidate_files_changed(candidate_dir, baseline_commit=cand_baseline):
            return False, [], "候选的 Git 变更计划自复审后发生变化，拒绝应用未审查内容"
        if not changes:
            return True, [], f"候选选手 {label} 没有产生任何有效的文件变更"

        # Boundary & Symlink security checks (non-bypassable, evaluated before conflict detection)
        canonical_base = os.path.realpath(base_cwd)
        for rel_path in changes:
            target_file = Path(base_cwd) / rel_path
            src_file = candidate_dir / rel_path
            # Candidate file must not be a symlink
            if os.path.islink(src_file) or src_file.is_symlink():
                return False, [], f"安全风险: 候选文件 {rel_path} 为符号链接，已拒绝应用"

            if changes[rel_path] not in ("A", "M", "D"):
                return False, [], f"候选变更类型无效: {rel_path}"
            if changes[rel_path] == "D" and (rel_path in expected_manifest or src_file.exists()):
                return False, [], f"删除计划与已审核文件清单不一致: {rel_path}"
            if changes[rel_path] != "D" and rel_path not in expected_manifest:
                return False, [], f"候选文件 {rel_path} 未包含在已审核清单中"

            # Target file in workspace must not be a symlink
            if os.path.islink(target_file) or target_file.is_symlink():
                return False, [], f"安全风险: 目标文件 {rel_path} 为符号链接，已拒绝写入覆盖"

            # Resolve canonical path of target
            resolved_target = os.path.realpath(target_file)
            if not resolved_target.startswith(canonical_base + os.sep) and resolved_target != canonical_base:
                return False, [], f"安全越界风险: 目标文件 {rel_path} 解析落点位于工作区外部 ({resolved_target})，已拒绝写入"

            # Verify parent directories are not symlinks pointing outside
            parent = target_file.parent
            while parent != Path(base_cwd) and parent != parent.parent:
                if os.path.islink(parent):
                    resolved_parent = os.path.realpath(parent)
                    if not resolved_parent.startswith(canonical_base + os.sep) and resolved_parent != canonical_base:
                        return False, [], f"安全越界风险: 目标父目录包含指向外部的符号链接，已拒绝写入"
                parent = parent.parent

        # Conflict Detection (dirty files + baseline manifest + baseline commit)
        if not force:
            conflicts = CandidateManager.detect_conflicts(
                base_cwd,
                candidate_dir,
                baseline_manifest=race.get("baseline_manifest"),
                baseline_commit=race.get("baseline_commit"),
                candidate_baseline_commit=cand_baseline
            )
            if conflicts:
                msg = f"检测到工作区冲突: 以下文件在基线后已被修改，已阻止覆盖: {', '.join(conflicts)}"
                return False, conflicts, msg

        if dry_run:
            preview = [f"{status} {path}" for path, status in changes.items()]
            return True, preview, f"[Dry-run] 演练完成，共涉及 {len(changes)} 个文件的增删改"

        # Create Backup Journal
        ensure_config_dir()
        ts = datetime.now().strftime("%Y%m%d_%H%M%S")
        backup_dir = config.BACKUPS_DIR / f"{r_id}_{ts}"
        backup_dir.mkdir(parents=True, exist_ok=True)

        journal = []
        applied_files = []

        try:
            for rel_path, status in changes.items():
                target_file = _verify_safe_target_path(base_cwd, rel_path)
                src_file = candidate_dir / rel_path

                if os.path.islink(src_file) or src_file.is_symlink():
                    raise ValueError(f"安全越界风险: 候选文件 {rel_path} 为符号链接")

                # Backup existing
                if target_file.exists():
                    if os.path.islink(target_file) or target_file.is_symlink():
                        raise ValueError(f"安全越界风险: 目标文件 {rel_path} 为符号链接")
                    bak_file = backup_dir / rel_path
                    bak_file.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copy2(target_file, bak_file)
                    journal.append({"path": rel_path, "action": "restore", "bak": str(bak_file)})
                else:
                    journal.append({"path": rel_path, "action": "delete"})

                # Apply Change
                if status in ("M", "A"):
                    target_file.parent.mkdir(parents=True, exist_ok=True)
                    _verify_safe_target_path(base_cwd, rel_path)

                    _atomic_copy(base_cwd, rel_path, src_file, expected_manifest.get(rel_path))
                    applied_files.append(f"A/M {rel_path}")
                elif status == "D":
                    if target_file.exists():
                        _verify_safe_target_path(base_cwd, rel_path)
                        _atomic_remove(base_cwd, rel_path)
                        applied_files.append(f"D   {rel_path}")

            with open(backup_dir / "journal.json", "w", encoding="utf-8") as jf:
                json.dump(journal, jf, indent=2)

            return True, applied_files, f"成功应用候选方案 {label} ({len(applied_files)} 个变更已同步)"

        except Exception as e:
            # Rollback
            rollback_errors = []
            for item in reversed(journal):
                rel_p = item["path"]
                try:
                    t_file = _verify_safe_target_path(base_cwd, rel_p)
                    if item["action"] == "restore":
                        _atomic_copy(base_cwd, rel_p, Path(item["bak"]))
                    elif item["action"] == "delete" and t_file.exists():
                        _verify_safe_target_path(base_cwd, rel_p)
                        _atomic_remove(base_cwd, rel_p)
                except Exception as rollback_error:
                    rollback_errors.append(f"{rel_p}: {rollback_error}")

            if rollback_errors:
                return False, [], (f"应用失败: {e}；部分文件回滚失败: {'; '.join(rollback_errors)}。"
                                   f"备份保留于 {backup_dir}")
            return False, [], f"应用过程中发生异常并已自动回滚: {str(e)}"

    @staticmethod
    def discard_race(race_id: Optional[str] = None, all_races: bool = False) -> Tuple[bool, str]:
        ensure_config_dir()
        if all_races:
            if config.CANDIDATES_DIR.exists():
                shutil.rmtree(config.CANDIDATES_DIR, ignore_errors=True)
                config.CANDIDATES_DIR.mkdir(parents=True, exist_ok=True)
            return True, "已清理所有已保存的候选工作区"

        race = CandidateManager.get_race(race_id)
        if not race:
            return False, "未找到指定的候选记录"

        r_id = race.get("race_id", "")
        target_dir = config.CANDIDATES_DIR / r_id
        if target_dir.exists():
            shutil.rmtree(target_dir, ignore_errors=True)
        return True, f"已清理候选记录: {r_id}"
