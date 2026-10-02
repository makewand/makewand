"""
Makewand Task DAG:
Topological task graph definition, semantic decomposition, and staged pipeline execution.
"""

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
) -> Tuple[bool, str, List[Dict[str, Any]]]:
    """
    Executes a TaskDAG in topological stages with optional Architect-Worker tiered dispatch.
    - Architect (deep reasoning, e.g. Claude 3.7 / Codex): handles Stage 1 design/contracts & final audit.
    - Worker (fast lightweight, e.g. local / fast API): executes intermediate implementation nodes.
    - local_only / offline constraint is strictly propagated to every stage and task.
    Each stage executes its task nodes and verifies changes through tests and red-team review.
    """
    valid, errors = dag.validate()
    if not valid:
        err_msg = f"DAG 拓扑结构校验失败: {'; '.join(errors)}"
        print(c(f"❌ {err_msg}", COLOR_RED + COLOR_BOLD))
        return False, err_msg, []

    stages = dag.topological_stages()
    stage_results: List[Dict[str, Any]] = []

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

            ok = pipeline_fn(
                task_prompt,
                cwd=cwd,
                tier=task_tier,
                forced_engine=task_forced_engine,
                stream=stream,
                auto_fix=auto_fix,
                repo_trust=repo_trust,
                local_only=local_only,
            )

            if ok:
                task.status = "passed"
                print(c(f"✔ 子任务 [{task.task_id}] 交付验收通过！", COLOR_GREEN + COLOR_BOLD))
            else:
                task.status = "failed"
                msg = f"子任务 [{task.task_id}: {task.title}] 未通过质量验收，DAG 流水线终止。"
                print(c(f"❌ {msg}", COLOR_RED + COLOR_BOLD))
                stage_results.append({"stage": stage_idx, "task": task.task_id, "status": "failed"})
                return False, msg, stage_results

            stage_results.append({"stage": stage_idx, "task": task.task_id, "status": "passed"})

    return True, "All DAG stages executed successfully", stage_results
