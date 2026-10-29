"""route / mark / outcome 子命令的实现。

- ``route`` 默认在完整目录里选（``select(full=True)``）。候选来自 discovery
  已经落盘的缓存，不跑 ``agy models``，也不写状态、不调用模型。
  ``--simple`` 退回精简三档表；``--legacy`` 把旧版本模型纳入完整目录的排序。
  文本最多列这一档的前 5 个候选。
- ``mark`` 把一家工具标记为额度受限，到期前路由跳过它；
- ``outcome`` 追加一条验收成绩，供之后的 Thompson 采样使用。
  ``--model`` / ``--effort`` 可选，写进账本后按候选统计。

额度默认走 route_select 的官方读取。环境变量 MAKEWAND_ROUTE_QUOTA_FILE
指向一个 JSON 文件时改用文件内容，格式如
``{"claude": [91, "10-18 20:00"], "codex": [93, ""]}``，不再调用
makewand-server；文件缺失或损坏时当作没有额度数据。
"""

from __future__ import annotations

import json
import math
import os
import re
import shlex
import sys
import tempfile
import time
from datetime import datetime
from pathlib import Path

from makewand.outcome_ledger import OutcomeLedger
from makewand.route_select import load_policy, select
from makewand.routing_catalog import PROVIDER_FAMILIES, TASK_TYPES

__all__ = [
    "cmd_mark",
    "cmd_outcome",
    "cmd_route",
    "parse_duration",
]

EXIT_USAGE_ERROR = 2
_QUOTA_FILE_ENV = "MAKEWAND_ROUTE_QUOTA_FILE"
_TEXT_CANDIDATE_LIMIT = 5
_OUTCOME_HINT = (
    "验收后记录成绩：makewand outcome <工具> <档> <类型> pass|fail"
    " --model <模型> --effort <推理深度>"
    " --request-id <请求ID> --caller <发起工具>"
    "（review 另带 --candidate-verdict pass|fail|unverified 与实测 --seconds/--calls，批准须带 --candidate-hash <SHA256>）"
)
_DURATION_PART = re.compile(r"(\d+)([dhms])")
_UNIT_SECONDS = {"d": 86400, "h": 3600, "m": 60, "s": 1}
_UNIT_ORDER = {"d": 0, "h": 1, "m": 2, "s": 3}
_DURATION_HINT = "时长格式不对：只用 d/h/m/s 组合，例如 6h、93h17m、2d、45m"
_MAX_DURATION_SECONDS = 365 * 86400
_MAX_DURATION_HINT = "时长太长：最长支持 365 天（例如 365d、8760h）"


def _fail(message: str):
    print(message, file=sys.stderr)
    raise SystemExit(EXIT_USAGE_ERROR)


def parse_duration(text) -> int:
    """把 6h、93h17m、2d、45m 这类时长解析成秒。

    只接受 d/h/m/s 组合，每种单位最多一次且顺序固定，总时长必须为正。
    非法输入抛 ValueError，信息里带合法例子。
    """
    if not isinstance(text, str) or not text.strip():
        raise ValueError(f"invalid duration {text!r}; {_DURATION_HINT}")
    value = text.strip()
    total = 0
    seen = set()
    last_order = -1
    position = 0
    while position < len(value):
        match = _DURATION_PART.match(value, position)
        if match is None:
            raise ValueError(f"invalid duration {text!r}; {_DURATION_HINT}")
        unit = match.group(2)
        if unit in seen or _UNIT_ORDER[unit] < last_order:
            raise ValueError(f"invalid duration {text!r}; {_DURATION_HINT}")
        seen.add(unit)
        last_order = _UNIT_ORDER[unit]
        total += int(match.group(1)) * _UNIT_SECONDS[unit]
        position = match.end()
    if total <= 0:
        raise ValueError(f"invalid duration {text!r}; duration must be positive; {_DURATION_HINT}")
    if total > _MAX_DURATION_SECONDS:
        raise ValueError(f"invalid duration {text!r}; {_MAX_DURATION_HINT}; {_DURATION_HINT}")
    return total


def _validate_provider(raw) -> str:
    if isinstance(raw, str) and raw.strip().lower() in PROVIDER_FAMILIES:
        return raw.strip().lower()
    legal = ", ".join(PROVIDER_FAMILIES)
    raise ValueError(f"invalid provider {raw!r}; legal values: {legal}")


def _quota_reader_from_env():
    """MAKEWAND_ROUTE_QUOTA_FILE 已设置时返回文件读取器，否则 None。

    返回 None 时 select 使用默认的 makewand-server 读取。一旦设置了
    环境变量就只认文件：读不到或格式不对时返回空 dict，不回退到外部命令。
    """
    path = os.environ.get(_QUOTA_FILE_ENV, "").strip()
    if not path:
        return None
    quota_path = Path(path).expanduser()

    def reader():
        try:
            data = json.loads(quota_path.read_text(encoding="utf-8"))
        except (OSError, UnicodeError, ValueError):
            return {}
        if not isinstance(data, dict):
            return {}
        quota = {}
        for name, entry in data.items():
            if not isinstance(name, str) or not name.strip():
                continue
            percent, resets = _quota_entry(entry)
            if percent is None:
                continue
            quota[name.strip().lower()] = (percent, resets)
        return quota

    return reader


def _quota_entry(entry):
    resets = ""
    if isinstance(entry, (list, tuple)):
        if not entry:
            return None, ""
        percent = entry[0]
        if len(entry) > 1 and entry[1] is not None:
            resets = str(entry[1])
    else:
        percent = entry
    if isinstance(percent, bool) or not isinstance(percent, (int, float)):
        return None, ""
    number = float(percent)
    if not math.isfinite(number):
        return None, ""
    return number, resets


def _execution_options(task_type, difficulty, tier=1, total_timeout=None) -> dict:
    timeouts = (1200, 3600, 10800) if task_type == "implement" else (900, 1800, 5400)
    timeout = timeouts[max(("mechanical", "normal", "hard").index(difficulty), tier - 1)]
    if total_timeout is not None:
        timeout = total_timeout
    if float(timeout).is_integer():
        timeout = int(timeout)
    return {"timeout": timeout, "total_timeout": timeout, "readonly": task_type == "review"}


def _request_arguments(execution_options, *, include_maximum=False):
    parts = []
    request_id = execution_options.get("request_id")
    if request_id:
        parts += ["--request-id=" + request_id] if request_id.startswith("-") else ["--request-id", request_id]
        if include_maximum:
            parts += ["--max-model-calls", str(execution_options["request_budget"])]
    if execution_options.get("caller"):
        parts += ["--caller", execution_options["caller"]]
    if execution_options.get("budget_file"):
        parts += ["--call-budget-file", shlex.quote(execution_options["budget_file"])]
    return parts


def _command_for(candidate, execution_options) -> str:
    parts = ["makewand", candidate.provider, *candidate.args,
             "-C", "<目录>", "-f", "<子任务文件>", "--max-model-calls",
             str(execution_options.get("request_budget", 1))]
    parts += _request_arguments(execution_options)
    parts += ["--timeout", str(math.ceil(execution_options["timeout"])),
              "--total-timeout", str(execution_options["total_timeout"])]
    if execution_options["readonly"]:
        parts.append("--readonly")
    return " ".join(parts)


def _candidate_payload(item, execution_options) -> dict:
    return {
        "provider": item.provider,
        "model": item.model,
        "effort": item.effort,
        "candidate_id": item.candidate_id,
        "args": list(item.args),
        "label": item.label,
        "is_self": bool(item.is_self),
        "command": _command_for(item, execution_options),
        **execution_options,
    }


def cmd_route(args) -> None:
    task_type = args.task_type
    difficulty = args.difficulty
    risk_raw = getattr(args, "risk", None) or "low"
    risk = risk_raw if risk_raw in ("low", "high") else "low"
    self_tool = getattr(args, "self_tool", None)
    not_tool = getattr(args, "not_tool", None)
    if self_tool is not None:
        try:
            self_tool = _validate_provider(self_tool)
        except ValueError as error:
            _fail(f"错误：--self {error}")
    simple = bool(getattr(args, "simple", False))
    try:
        policy = load_policy()
    except ValueError as error:
        _fail(f"错误：{error}")
    options = dict(
        risk=risk,
        self_tool=self_tool,
        not_tool=not_tool,
        tier_override=getattr(args, "route_tier", None),
        policy=policy,
        quota_reader=_quota_reader_from_env(),
        full=not simple,
        include_legacy=bool(getattr(args, "legacy", False)) and not simple,
        request_id=getattr(args, "request_id", None),
        retry_approved=bool(getattr(args, "retry_approved", False)),
    )
    try:
        try:
            result = select(task_type, difficulty, ledger=OutcomeLedger(), **options)
        except PermissionError:
            if getattr(args, "request_id", None):
                raise ValueError("cannot verify request routing history; restore readable state before dispatch")
            # Read-only state directory: fall back to an empty ledger and still
            # produce a normal route answer instead of a traceback.
            with tempfile.TemporaryDirectory(prefix="makewand-route-empty-") as empty:
                result = select(task_type, difficulty, ledger=OutcomeLedger(Path(empty) / "routing"), **options)
    except (ValueError, OSError) as error:
        _fail(f"错误：{error}")
    budget = int(getattr(policy, "request_budget", 3) or 3)
    execution_options = _execution_options(task_type, difficulty, result.tier, getattr(args, "total_timeout", None))
    if getattr(args, "request_id", None):
        budget = int(os.environ.get("MAKEWAND_MAX_MODEL_CALLS") or budget)
        execution_options.update(request_id=args.request_id, request_budget=budget)
        if getattr(args, "call_budget_file", None):
            execution_options["budget_file"] = args.call_budget_file
    if getattr(args, "caller", None):
        execution_options["caller"] = args.caller

    if getattr(args, "json", False):
        payload = {
            "tier": result.tier,
            "candidates": [_candidate_payload(item, execution_options) for item in result.candidates],
            **execution_options,
            "skipped": [
                {"provider": provider, "reason": reason}
                for provider, reason in result.skipped
            ],
            "escalate_tier": result.escalate_tier,
            "budget": budget,
        }
        print(json.dumps(payload, ensure_ascii=False, indent=2))
        return

    shown = result.candidates
    hidden = 0
    if len(shown) > _TEXT_CANDIDATE_LIMIT:
        hidden = len(shown) - _TEXT_CANDIDATE_LIMIT
        shown = shown[:_TEXT_CANDIDATE_LIMIT]
    lines = [
        f"子任务：{task_type} / {difficulty} → 第 {result.tier} 档"
        f"（1 便宜快，3 强且推理深）；每个请求默认最多 {budget} 次调用"
    ]
    for index, item in enumerate(shown, 1):
        line = f"  {index}. {_command_for(item, execution_options)}   # {item.label}"
        if item.is_self:
            line += "（和你同一家，尽量不用）"
        lines.append(line)
    if hidden:
        lines.append(f"  还有 {hidden} 个，用 --json 看全部")
    if result.skipped:
        skipped = "；".join(f"{provider}（{reason}）" for provider, reason in result.skipped)
        lines.append(f"  跳过：{skipped}")
    if not result.candidates:
        if result.escalate_tier is not None:
            lines.append("  没有可用候选：可以升一档，或等额度恢复、暂停结束后再试。")
        else:
            lines.append("  没有可用候选：已是最高档，等额度恢复、暂停结束后再试。")
    if result.escalate_tier is not None:
        # Carry the constraints that shaped this answer, so following the
        # hint re-runs the same selection one tier up instead of silently
        # dropping --risk/--self/--not/--simple/--legacy.
        escalate_cmd = f"makewand route {task_type} {difficulty} --tier {result.escalate_tier}"
        if getattr(args, "total_timeout", None) is not None:
            escalate_cmd += f" --total-timeout {args.total_timeout:g}"
        if risk == "high":
            escalate_cmd += " --risk high"
        if self_tool:
            escalate_cmd += f" --self {self_tool}"
        if not_tool and str(not_tool).strip():
            escalate_cmd += f" --not {str(not_tool).strip()}"
        if simple:
            escalate_cmd += " --simple"
        elif bool(getattr(args, "legacy", False)):
            escalate_cmd += " --legacy"
        request_arguments = _request_arguments(execution_options, include_maximum=True)
        if request_arguments:
            escalate_cmd += " " + " ".join(request_arguments)
        lines.append(f"验收不过就升一档：{escalate_cmd}")
    outcome_hint = _OUTCOME_HINT
    outcome_req_args = _request_arguments(
        {"request_id": execution_options.get("request_id"), "caller": execution_options.get("caller")},
        include_maximum=False,
    )
    if outcome_req_args:
        outcome_hint += " " + " ".join(outcome_req_args)
    lines.append(
        "派活遇到 429 / 额度用完：makewand mark <工具> <剩余时长>；"
        + outcome_hint
    )
    print("\n".join(lines))


def _infer_recent_request_id(
    ledger: OutcomeLedger,
    provider: str,
    model: str | None,
    budget_file: str | None = None,
    max_age_seconds: float = 3600.0,
    existing_rows: list[dict] | None = None,
) -> str | None:
    """When `makewand outcome` is called in a fresh shell without `--request-id`
    or `MAKEWAND_TASK_ID`, match the most recent completed call in `call-budget.json`
    for the same provider (and model, if specified) that still lacks an outcome row.
    """
    try:
        from makewand.execution_contract import identifier
        from makewand.layered_budget import default_path, project_ref

        path_str = budget_file or os.environ.get("MAKEWAND_CALL_BUDGET_FILE") or default_path()
        if not path_str:
            return None
        current_project = project_ref()
        data = json.loads(Path(path_str).expanduser().read_text(encoding="utf-8"))
    except (OSError, UnicodeError, ValueError):
        return None
    if not isinstance(data, dict):
        return None
    attempts = data.get("attempts")
    if not isinstance(attempts, list) or not attempts:
        return None

    def _matches_project(entry: dict) -> bool:
        scopes = entry.get("scopes")
        if not isinstance(scopes, list):
            return False
        for item in scopes:
            if isinstance(item, list) and len(item) == 2 and item[0] == "projects" and isinstance(item[1], str):
                proj_hash = item[1].rpartition(":")[0] or item[1]
                return proj_hash == current_project
        return False

    now = time.time()
    recorded_counts: dict[tuple[str, str], int] = {}
    recorded_model_counts: dict[tuple[str, str, str | None], int] = {}
    recorded_ts: dict[tuple[str, str], list[float]] = {}
    recorded_model_ts: dict[tuple[str, str, str | None], list[float]] = {}
    rows_to_scan = existing_rows if existing_rows is not None else ledger._read_rows(strict=False)
    for row in rows_to_scan:
        if not isinstance(row, dict):
            continue
        req_id = row.get("request_id")
        prov = row.get("provider")
        if isinstance(req_id, str) and req_id and isinstance(prov, str) and prov:
            recorded_counts[(req_id, prov)] = recorded_counts.get((req_id, prov), 0) + 1
            rm = row.get("model")
            norm_rm = rm.strip() if isinstance(rm, str) and rm.strip() else None
            recorded_model_counts[(req_id, prov, norm_rm)] = (
                recorded_model_counts.get((req_id, prov, norm_rm), 0) + 1
            )
            r_ts = row.get("ts")
            if not isinstance(r_ts, bool) and isinstance(r_ts, (int, float)) and math.isfinite(r_ts):
                recorded_ts.setdefault((req_id, prov), []).append(float(r_ts))
                recorded_model_ts.setdefault((req_id, prov, norm_rm), []).append(float(r_ts))

    attempt_counts: dict[tuple[str, str], int] = {}
    attempt_model_counts: dict[tuple[str, str, str | None], int] = {}
    recent_attempt_ts: dict[tuple[str, str], list[float]] = {}
    recent_attempt_model_ts: dict[tuple[str, str, str | None], list[float]] = {}
    cross_project_tasks: set[str] = set()
    for entry in attempts:
        if not isinstance(entry, dict):
            continue
        tid = entry.get("task_id")
        if isinstance(tid, str) and tid.strip():
            tid = tid.strip()
            if not _matches_project(entry):
                cross_project_tasks.add(tid)
                continue
        else:
            continue
        if entry.get("status") != "completed":
            continue
        eng = entry.get("engine")
        if isinstance(eng, str) and eng:
            attempt_counts[(tid, eng)] = attempt_counts.get((tid, eng), 0) + 1
            em = entry.get("requested_model")
            norm_em = em.strip() if isinstance(em, str) and em.strip() else None
            attempt_model_counts[(tid, eng, norm_em)] = (
                attempt_model_counts.get((tid, eng, norm_em), 0) + 1
            )
            a_ts = entry.get("finished_at") if entry.get("finished_at") is not None else entry.get("started_at")
            if (
                not isinstance(a_ts, bool)
                and isinstance(a_ts, (int, float))
                and math.isfinite(a_ts)
                and -300.0 <= now - float(a_ts) <= max_age_seconds
            ):
                recent_attempt_ts.setdefault((tid, eng), []).append(float(a_ts))
                recent_attempt_model_ts.setdefault((tid, eng, norm_em), []).append(float(a_ts))

    norm_model = model.strip() if isinstance(model, str) and model.strip() else None
    candidate_tasks: list[str] = []
    for entry in reversed(attempts):
        if not isinstance(entry, dict) or entry.get("status") != "completed":
            continue
        if entry.get("engine") != provider:
            continue
        if not _matches_project(entry):
            continue
        task_id = entry.get("task_id")
        if not isinstance(task_id, str) or not task_id.strip():
            continue
        task_id = task_id.strip()
        if task_id in cross_project_tasks:
            continue
        try:
            identifier(task_id, "request_id")
        except ValueError:
            continue
        ts = entry.get("finished_at") if entry.get("finished_at") is not None else entry.get("started_at")
        if isinstance(ts, bool) or not isinstance(ts, (int, float)) or not math.isfinite(ts):
            continue
        ts_val = float(ts)
        if not (-300.0 <= now - ts_val <= max_age_seconds):
            continue
        req_model = entry.get("requested_model")
        norm_req_model = req_model.strip() if isinstance(req_model, str) and req_model.strip() else None
        if norm_req_model != norm_model:
            continue
        prov_recent_attempts = recent_attempt_ts.get((task_id, provider), [])
        model_recent_attempts = recent_attempt_model_ts.get((task_id, provider, norm_model), [])
        prov_recent_outcomes = [
            t for t in recorded_ts.get((task_id, provider), []) if -300.0 <= now - t <= max_age_seconds
        ]
        model_recent_outcomes = [
            t
            for t in recorded_model_ts.get((task_id, provider, norm_model), [])
            if -300.0 <= now - t <= max_age_seconds
        ]
        floor_ts = math.floor(ts_val)
        if (
            attempt_counts.get((task_id, provider), 0) > recorded_counts.get((task_id, provider), 0)
            and attempt_model_counts.get((task_id, provider, norm_model), 0)
            > recorded_model_counts.get((task_id, provider, norm_model), 0)
            and len(prov_recent_attempts) > len(prov_recent_outcomes)
            and len(model_recent_attempts) > len(model_recent_outcomes)
            and sum(1 for t in prov_recent_attempts if t >= ts_val)
            > sum(1 for t in prov_recent_outcomes if t >= floor_ts)
            and sum(1 for t in model_recent_attempts if t >= ts_val)
            > sum(1 for t in model_recent_outcomes if t >= floor_ts)
        ):
            if task_id not in candidate_tasks:
                candidate_tasks.append(task_id)
    return candidate_tasks[0] if len(candidate_tasks) == 1 else None


def cmd_mark(args) -> None:
    try:
        provider = _validate_provider(args.provider)
        seconds = parse_duration(args.duration)
        reason = getattr(args, "reason", None) or ""
        until = time.time() + seconds
        OutcomeLedger().mark(provider, until, reason)
        when = datetime.fromtimestamp(until).strftime("%m-%d %H:%M")
    except ValueError as error:
        _fail(f"错误：{error}")
    except OverflowError:
        _fail(f"错误：{_MAX_DURATION_HINT}")
    except OSError as error:
        _fail(f"错误：写入额度标记失败：{error}")
    print(f"已记录：{provider} 约 {when} 前不再派活")


def cmd_outcome(args) -> None:
    try:
        provider = _validate_provider(args.provider)
        tier = args.outcome_tier
        if tier not in (1, 2, 3):
            raise ValueError(f"invalid tier {tier!r}; legal values: 1, 2, 3")
        task_type = args.task_type
        if task_type not in TASK_TYPES:
            legal = ", ".join(TASK_TYPES)
            raise ValueError(f"invalid task type {task_type!r}; legal values: {legal}")
        seconds = getattr(args, "seconds", None)
        if seconds is not None and (not math.isfinite(seconds) or seconds < 0):
            raise ValueError("invalid --seconds: must be a non-negative number")
        calls = getattr(args, "calls", None)
        if calls is not None and calls < 0:
            raise ValueError("invalid --calls: must be a non-negative integer")
        model = getattr(args, "model", None)
        effort = getattr(args, "effort", None)
        passed = args.result == "pass"
        details = {}
        verdict = getattr(args, "candidate_verdict", None)
        if verdict is None and task_type == "review":
            # Recorded anyway: a missing verdict must not cost the row or invite made-up timings.
            print("makewand: warning: review outcome without --candidate-verdict; the code verdict "
                  "(pass|fail|unverified) is what shows whether cross-model review is worth it. "
                  "It needs measured --seconds and actual --calls; if you have none, record as is.",
                  file=sys.stderr)
        if verdict is not None:
            if seconds is None or calls is None:
                raise ValueError("--candidate-verdict requires measured --seconds and actual --calls")
            details.update(review_validated=passed, candidate_approved={"pass": True, "fail": False, "unverified": None}[verdict])
        for name in ("caller", "request_id", "candidate_hash", "failure_kind"):
            value = getattr(args, name, None)
            if value is not None:
                details[name] = value
        ledger = OutcomeLedger()
        recorded = ledger.record(
            provider, tier, task_type, passed,
            seconds=seconds, calls=calls, model=model, effort=effort,
            infer_request_id=(
                lambda rows: _infer_recent_request_id(
                    ledger,
                    provider,
                    model,
                    budget_file=getattr(args, "call_budget_file", None),
                    existing_rows=rows,
                )
            ),
            **details,
        )
    except ValueError as error:
        _fail(f"错误：{error}")
    except OSError as error:
        _fail(f"错误：写入成绩失败：{error}")
    if not recorded.get("request_id"):
        # Recorded anyway: an unlinked row still counts, it just cannot be matched to its call.
        print("makewand: warning: outcome not linked to a dispatched call: no --request-id or "
              "MAKEWAND_TASK_ID, and no single recent unrecorded call of this provider/model "
              "to link automatically. Pass the request id the dispatch used.", file=sys.stderr)
    detail = ""
    if model or effort:
        detail = f"（model={model or '-'} effort={effort or '-'}）"
    print(f"已记录：{provider} 第 {tier} 档 {task_type} {args.result}{detail}")
