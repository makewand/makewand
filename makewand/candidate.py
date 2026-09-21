"""
Makewand Candidate Lifecycle Manager: inspect, apply, and discard race candidates.
"""

import os
import sys
import json
import shutil
import time
from pathlib import Path
from datetime import datetime
from typing import Dict, Any, List, Optional, Tuple

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
from makewand.git_helper import run_git_cmd, get_git_diff

def get_candidate_files_changed(candidate_dir: Path) -> Dict[str, str]:
    """
    Returns a dict mapping relative file path to change status ('M' modified, 'A' added, 'D' deleted).
    """
    code, out, _ = run_git_cmd("git status --porcelain", cwd=str(candidate_dir))
    changes = {}
    if code == 0 and out:
        for line in out.strip().splitlines():
            line = line.strip()
            if len(line) >= 3:
                status = line[:2].strip()
                path = line[2:].strip().strip('"')
                if status in ("M", "MM", "AM"):
                    changes[path] = "M"
                elif status in ("A", "??"):
                    changes[path] = "A"
                elif status == "D":
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

        meta = {
            "race_id": race_id,
            "prompt": prompt,
            "base_cwd": os.path.abspath(base_cwd),
            "baseline_commit": baseline_commit,
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

        return race_dir

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
    def detect_conflicts(base_cwd: str, candidate_dir: Path) -> List[str]:
        """
        Checks if any file modified by the candidate has also been modified
        in base_cwd since the race baseline.
        """
        candidate_changes = get_candidate_files_changed(candidate_dir)
        conflicts = []

        code, diff_out, _ = run_git_cmd("git status --porcelain", cwd=base_cwd)
        if code == 0 and diff_out:
            base_dirty_files = set()
            for line in diff_out.strip().splitlines():
                line = line.strip()
                if len(line) >= 3:
                    fpath = line[2:].strip().strip('"')
                    base_dirty_files.add(fpath)

            for changed_file in candidate_changes:
                if changed_file in base_dirty_files:
                    conflicts.append(changed_file)

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
        Returns (success, applied_files, message).
        """
        race = CandidateManager.get_race(race_id)
        if not race:
            return False, [], "未找到指定的候选竞速记录"

        r_id = race.get("race_id", "")
        base_cwd = race.get("base_cwd", "")
        if not os.path.exists(base_cwd):
            return False, [], f"原始工作区不存在: {base_cwd}"

        # Choose candidate (default to winner or 'B')
        label = (candidate_label or race.get("winner") or "B").upper()
        if label not in ("A", "B"):
            label = "B"

        cand_info = race.get("candidates", {}).get(label, {})
        cand_path_str = cand_info.get("path")
        if not cand_path_str or not os.path.exists(cand_path_str):
            return False, [], f"候选选手 {label} 的工作区目录已丢失: {cand_path_str}"

        candidate_dir = Path(cand_path_str)
        changes = get_candidate_files_changed(candidate_dir)
        if not changes:
            return True, [], f"候选选手 {label} 没有产生任何有效的文件变更"

        # Conflict Detection
        if not force:
            conflicts = CandidateManager.detect_conflicts(base_cwd, candidate_dir)
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
                target_file = Path(base_cwd) / rel_path
                src_file = candidate_dir / rel_path

                # Backup existing
                if target_file.exists():
                    bak_file = backup_dir / rel_path
                    bak_file.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copy2(target_file, bak_file)
                    journal.append({"path": rel_path, "action": "restore", "bak": str(bak_file)})
                else:
                    journal.append({"path": rel_path, "action": "delete"})

                # Apply Change
                if status in ("M", "A"):
                    target_file.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copy2(src_file, target_file)
                    applied_files.append(f"A/M {rel_path}")
                elif status == "D":
                    if target_file.exists():
                        target_file.unlink()
                        applied_files.append(f"D   {rel_path}")

            with open(backup_dir / "journal.json", "w", encoding="utf-8") as jf:
                json.dump(journal, jf, indent=2)

            return True, applied_files, f"成功应用候选方案 {label} ({len(applied_files)} 个变更已同步)"

        except Exception as e:
            # Rollback
            for item in reversed(journal):
                rel_p = item["path"]
                t_file = Path(base_cwd) / rel_p
                if item["action"] == "restore":
                    shutil.copy2(item["bak"], t_file)
                elif item["action"] == "delete" and t_file.exists():
                    t_file.unlink()

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
