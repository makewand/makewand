"""成绩账本与额度标记：makewand 私有状态目录下 routing/ 的持久化。

outcomes.jsonl 只追加，每行一条 JSON（ts、provider、tier、task_type、
passed、seconds、calls），写入时用 makewand.filelock 的跨进程文件锁，
保证多进程并发追加不会交错损坏。坏行在读取时跳过，不让整个账本失效。

stats() 按指数时间衰减加权计算 Beta 后验（先验 1/1）、有效样本数
与加权平均耗时/调用次数，now 与 half_life_days 均可注入。
观察时刻之后的记录不计入，因此注入的历史 now 不会读到“未来”样本。

marks.json 记 provider -> {until, reason}，写入时在锁内读-改-写并
通过临时文件 + os.replace 原子替换；文件损坏时当成空账本。
"""

from __future__ import annotations

import json
import math
import os
import tempfile
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Iterator, Optional

from makewand import filelock

__all__ = [
    "OutcomeLedger",
    "Stats",
    "Mark",
    "routing_dir",
    "default_ledger",
    "record",
    "record_outcome",
    "stats",
    "get_stats",
    "mark",
    "active_marks",
]

_PRIOR_ALPHA = 1.0
_PRIOR_BETA = 1.0
_SECONDS_PER_DAY = 86400.0


def _coerce_ts(value: Any, name: str = "ts") -> float:
    """把 epoch 秒 / datetime 统一成 float epoch 秒。"""
    if isinstance(value, datetime):
        moment = value if value.tzinfo is not None else value.replace(tzinfo=timezone.utc)
        return float(moment.timestamp())
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise TypeError(f"{name} must be an epoch timestamp or datetime, got {type(value).__name__}")
    result = float(value)
    if not math.isfinite(result):
        raise ValueError(f"{name} must be finite")
    return result


def _resolve_now(now: Any) -> float:
    return time.time() if now is None else _coerce_ts(now, "now")


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(float(value))


def _default_state_root() -> Path:
    """复用 makewand.config 已有的状态目录逻辑（尊重 XDG_STATE_HOME）。"""
    try:
        from makewand import config as _config

        factory = getattr(_config, "_default_state_dir", None)
        if callable(factory):
            return Path(factory())
    except Exception:
        pass
    xdg_state = os.environ.get("XDG_STATE_HOME", "")
    if xdg_state and Path(xdg_state).expanduser().is_absolute():
        base = Path(xdg_state).expanduser()
    else:
        base = Path.home() / ".local" / "state"
    return base / "makewand"


def routing_dir() -> Path:
    """默认 routing 目录：<私有状态目录>/makewand/routing（每次调用动态解析）。"""
    override = os.environ.get("MAKEWAND_ROUTING_DIR", "")
    if override:
        return Path(override).expanduser()
    return _default_state_root() / "routing"


class Mark(dict):
    """一条额度标记：dict 形式 {until, reason}，同时支持属性与数值比较。"""

    def __init__(self, until: float, reason: str = ""):
        super().__init__(until=float(until), reason=str(reason))
        self.until = float(until)
        self.reason = str(reason)

    @property
    def until_ts(self) -> float:
        return self.until

    def __float__(self) -> float:
        return float(self.until)

    def __eq__(self, other: Any) -> bool:
        if isinstance(other, bool):
            return False
        if isinstance(other, (int, float)):
            return float(self.until) == float(other)
        return super().__eq__(other)

    __hash__ = dict.__hash__  # type: ignore[assignment]


@dataclass
class Stats:
    """带时间衰减的 Beta 后验统计。没有数据时 alpha/beta 为先验 1/1，
    n 为 0，平均耗时与平均调用次数为 None。"""

    alpha: float = _PRIOR_ALPHA
    beta: float = _PRIOR_BETA
    n: float = 0.0
    avg_seconds: Optional[float] = None
    avg_calls: Optional[float] = None
    count: int = 0

    @property
    def mean_seconds(self) -> Optional[float]:
        return self.avg_seconds

    @property
    def average_seconds(self) -> Optional[float]:
        return self.avg_seconds

    @property
    def mean_calls(self) -> Optional[float]:
        return self.avg_calls

    @property
    def average_calls(self) -> Optional[float]:
        return self.avg_calls

    @property
    def effective_n(self) -> float:
        return self.n

    def to_dict(self) -> Dict[str, Any]:
        return {
            "alpha": self.alpha,
            "beta": self.beta,
            "n": self.n,
            "avg_seconds": self.avg_seconds,
            "avg_calls": self.avg_calls,
        }

    def __getitem__(self, key: str) -> Any:
        aliases = {
            "mean_seconds": "avg_seconds",
            "average_seconds": "avg_seconds",
            "mean_calls": "avg_calls",
            "average_calls": "avg_calls",
            "effective_n": "n",
        }
        return self.to_dict()[aliases.get(key, key)]

    def get(self, key: str, default: Any = None) -> Any:
        try:
            return self[key]
        except KeyError:
            return default

    def keys(self):
        return self.to_dict().keys()

    def items(self):
        return self.to_dict().items()

    def __contains__(self, key: object) -> bool:
        return key in self.to_dict()


def _validate_row(row: Any) -> Optional[Dict[str, Any]]:
    """校验一行账本记录；坏行（缺字段、类型错、非有限/负数）返回 None。"""
    if not isinstance(row, dict):
        return None
    for key in ("ts", "provider", "tier", "task_type", "passed", "seconds", "calls"):
        if key not in row:
            return None
    if not _is_number(row["ts"]):
        return None
    if not isinstance(row["provider"], str) or not row["provider"].strip():
        return None
    tier = row["tier"]
    if isinstance(tier, bool) or not isinstance(tier, (int, float)):
        return None
    if isinstance(tier, float):
        if not float(tier).is_integer():
            return None
        tier = int(tier)
    if not isinstance(row["task_type"], str) or not row["task_type"].strip():
        return None
    if type(row["passed"]) is not bool:
        return None
    seconds = row["seconds"]
    if seconds is not None and (not _is_number(seconds) or float(seconds) < 0):
        return None
    calls = row["calls"]
    if calls is not None:
        if isinstance(calls, bool) or not isinstance(calls, (int, float)):
            return None
        if isinstance(calls, float):
            if not float(calls).is_integer():
                return None
            calls = int(calls)
        if int(calls) < 0:
            return None
    return {
        "ts": float(row["ts"]),
        "provider": row["provider"],
        "tier": int(tier),
        "task_type": row["task_type"],
        "passed": row["passed"],
        "seconds": None if seconds is None else float(seconds),
        "calls": None if calls is None else int(calls),
    }


def _normalize_tier(tier: Any) -> int:
    if isinstance(tier, bool):
        raise TypeError("tier must be an int")
    if isinstance(tier, float) and float(tier).is_integer():
        tier = int(tier)
    if isinstance(tier, str) and tier.strip().lstrip("+-").isdigit():
        tier = int(tier.strip())
    if not isinstance(tier, int):
        raise TypeError("tier must be an int")
    if tier < 1:
        raise ValueError("tier must be >= 1")
    return int(tier)


def _normalize_passed(passed: Any) -> bool:
    if type(passed) is bool:
        return passed
    if isinstance(passed, str):
        lowered = passed.strip().lower()
        if lowered in ("pass", "passed", "true", "1", "ok", "success"):
            return True
        if lowered in ("fail", "failed", "false", "0", "error"):
            return False
    if isinstance(passed, int) and passed in (0, 1):
        return bool(passed)
    raise TypeError("passed must be a bool")


def _normalize_seconds(seconds: Any) -> Optional[float]:
    if seconds is None:
        return None
    if not _is_number(seconds):
        raise TypeError("seconds must be a number or None")
    value = float(seconds)
    if value < 0:
        raise ValueError("seconds must be >= 0")
    return value


def _normalize_calls(calls: Any) -> Optional[int]:
    if calls is None:
        return None
    if isinstance(calls, bool) or not isinstance(calls, (int, float)):
        raise TypeError("calls must be an int or None")
    if isinstance(calls, float):
        if not float(calls).is_integer():
            raise ValueError("calls must be an integer")
        calls = int(calls)
    if int(calls) < 0:
        raise ValueError("calls must be >= 0")
    return int(calls)


class OutcomeLedger:
    """routing/ 目录下的成绩账本（outcomes.jsonl）与额度标记（marks.json）。"""

    def __init__(
        self,
        base_dir: Any = None,
        *,
        directory: Any = None,
        state_dir: Any = None,
        routing_dir_path: Any = None,
        root: Any = None,
        path: Any = None,
        create: bool = True,
    ):
        explicit = None
        for candidate in (base_dir, directory, routing_dir_path, root, path):
            if candidate is not None:
                explicit = candidate
                break
        self._is_default = explicit is None and state_dir is None
        if explicit is not None:
            chosen = Path(os.fspath(explicit)).expanduser()
            if chosen.suffix == ".jsonl":
                chosen = chosen.parent
            self._dir = chosen
            self._state_root: Optional[Path] = None
        elif state_dir is not None:
            chosen = Path(os.fspath(state_dir)).expanduser()
            self._dir = chosen if chosen.name == "routing" else chosen / "routing"
            self._state_root = chosen if chosen.name == "routing" else chosen
        else:
            self._state_root = _default_state_root()
            override = os.environ.get("MAKEWAND_ROUTING_DIR", "")
            self._dir = Path(override).expanduser() if override else self._state_root / "routing"
        if create:
            self._ensure_dir()

    @property
    def directory(self) -> Path:
        return self._dir

    @property
    def base_dir(self) -> Path:
        return self._dir

    @property
    def dir(self) -> Path:
        return self._dir

    @property
    def outcomes_path(self) -> Path:
        return self._dir / "outcomes.jsonl"

    @property
    def marks_path(self) -> Path:
        return self._dir / "marks.json"

    @property
    def _marks_lock_path(self) -> Path:
        return self._dir / "marks.json.lock"

    def _ensure_dir(self) -> Path:
        from makewand.config import ensure_private_dir

        if self._is_default and self._state_root is not None and self._state_root != self._dir:
            ensure_private_dir(self._state_root)
        return Path(ensure_private_dir(self._dir))

    def record(
        self,
        provider: str,
        tier: Any,
        task_type: str,
        passed: Any,
        seconds: Any = None,
        calls: Any = None,
        ts: Any = None,
        *,
        now: Any = None,
        **kwargs: Any,
    ) -> Dict[str, Any]:
        """追加一条成绩记录。ts/now 可注入（epoch 秒或 datetime），默认真实时钟。"""
        if seconds is None and "duration" in kwargs:
            seconds = kwargs.pop("duration")
        if seconds is None and "duration_seconds" in kwargs:
            seconds = kwargs.pop("duration_seconds")
        if seconds is None and "elapsed" in kwargs:
            seconds = kwargs.pop("elapsed")
        if calls is None and "call_count" in kwargs:
            calls = kwargs.pop("call_count")
        if ts is None and "timestamp" in kwargs:
            ts = kwargs.pop("timestamp")
        if kwargs:
            raise TypeError(f"unexpected record arguments: {', '.join(sorted(kwargs))}")
        if not isinstance(provider, str) or not provider.strip():
            raise ValueError("provider must be a non-empty string")
        if not isinstance(task_type, str) or not task_type.strip():
            raise ValueError("task_type must be a non-empty string")
        stamp = _resolve_now(ts if ts is not None else now)
        row = {
            "ts": stamp,
            "provider": provider,
            "tier": _normalize_tier(tier),
            "task_type": task_type,
            "passed": _normalize_passed(passed),
            "seconds": _normalize_seconds(seconds),
            "calls": _normalize_calls(calls),
        }
        payload = (json.dumps(row, ensure_ascii=False, allow_nan=False) + "\n").encode("utf-8")
        self._ensure_dir()
        fd = os.open(self.outcomes_path, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
        try:
            if os.name != "nt":
                try:
                    os.fchmod(fd, 0o600)
                except OSError:
                    pass
            filelock.flock(fd, filelock.LOCK_EX)
            try:
                view = memoryview(payload)
                while view:
                    written = os.write(fd, view)
                    view = view[written:]
                os.fsync(fd)
            finally:
                filelock.flock(fd, filelock.LOCK_UN)
        finally:
            os.close(fd)
        return dict(row)

    record_outcome = record
    append = record

    def _read_rows(self) -> Iterator[Dict[str, Any]]:
        try:
            stream = open(self.outcomes_path, "r", encoding="utf-8", errors="replace")
        except (FileNotFoundError, NotADirectoryError, IsADirectoryError):
            return
        with stream:
            try:
                filelock.flock(stream.fileno(), filelock.LOCK_SH)
            except OSError:
                pass
            try:
                for line in stream:
                    text = line.strip()
                    if not text:
                        continue
                    try:
                        parsed = json.loads(text)
                    except ValueError:
                        continue
                    row = _validate_row(parsed)
                    if row is not None:
                        yield row
            finally:
                try:
                    filelock.flock(stream.fileno(), filelock.LOCK_UN)
                except OSError:
                    pass

    def records(self) -> Iterator[Dict[str, Any]]:
        """逐条产出账本中的有效记录（坏行跳过）。"""
        yield from self._read_rows()

    def stats(
        self,
        provider: str,
        tier: Any,
        task_type: str,
        now: Any = None,
        half_life_days: float = 14,
        **kwargs: Any,
    ) -> Stats:
        """带指数时间衰减的 Beta 后验统计。

        权重 w = 0.5 ** (age / half_life)，age 以 half_life_days 为半衰期；
        alpha = 1 + Σ w(通过)，beta = 1 + Σ w(失败)，n = Σ w（有效样本数），
        平均耗时/调用次数为同一组权重下的加权平均（没有计入的数据时为 None）。
        ts 晚于 now 的记录视为尚未发生，直接跳过。
        """
        if now is None and "now_ts" in kwargs:
            now = kwargs.pop("now_ts")
        if kwargs:
            raise TypeError(f"unexpected stats arguments: {', '.join(sorted(kwargs))}")
        if isinstance(half_life_days, bool) or not isinstance(half_life_days, (int, float)):
            raise TypeError("half_life_days must be a number")
        if not math.isfinite(float(half_life_days)) or float(half_life_days) <= 0:
            raise ValueError("half_life_days must be a positive finite number")
        moment = _resolve_now(now)
        wanted_tier = _normalize_tier(tier)
        half_life_seconds = float(half_life_days) * _SECONDS_PER_DAY
        alpha = _PRIOR_ALPHA
        beta = _PRIOR_BETA
        n = 0.0
        count = 0
        seconds_weighted = 0.0
        seconds_weights = 0.0
        calls_weighted = 0.0
        calls_weights = 0.0
        for row in self._read_rows():
            if row["provider"] != provider or row["tier"] != wanted_tier or row["task_type"] != task_type:
                continue
            age = moment - row["ts"]
            if age < 0:
                continue
            weight = 0.5 ** (age / half_life_seconds)
            count += 1
            n += weight
            if row["passed"]:
                alpha += weight
            else:
                beta += weight
            if row["seconds"] is not None:
                seconds_weighted += weight * row["seconds"]
                seconds_weights += weight
            if row["calls"] is not None:
                calls_weighted += weight * row["calls"]
                calls_weights += weight
        return Stats(
            alpha=alpha,
            beta=beta,
            n=n,
            avg_seconds=(seconds_weighted / seconds_weights) if seconds_weights > 0 else None,
            avg_calls=(calls_weighted / calls_weights) if calls_weights > 0 else None,
            count=count,
        )

    get_stats = stats

    def _read_marks(self) -> Dict[str, Mark]:
        try:
            raw = self.marks_path.read_bytes()
        except (FileNotFoundError, NotADirectoryError, IsADirectoryError):
            return {}
        try:
            parsed = json.loads(raw.decode("utf-8"))
        except (ValueError, UnicodeDecodeError):
            return {}
        if not isinstance(parsed, dict):
            return {}
        result: Dict[str, Mark] = {}
        for provider, entry in parsed.items():
            if not isinstance(provider, str) or not provider:
                continue
            if isinstance(entry, dict):
                until = entry.get("until", entry.get("until_ts"))
                reason = entry.get("reason", "")
            elif _is_number(entry):
                until, reason = entry, ""
            else:
                continue
            if not _is_number(until):
                continue
            if not isinstance(reason, str):
                continue
            result[provider] = Mark(float(until), reason)
        return result

    def mark(self, provider: str, until_ts: Any, reason: str = "") -> Mark:
        """写入一条额度标记：锁内读-改-写，临时文件 + os.replace 原子替换。"""
        if not isinstance(provider, str) or not provider.strip():
            raise ValueError("provider must be a non-empty string")
        if not isinstance(reason, str):
            raise TypeError("reason must be a string")
        until = _coerce_ts(until_ts, "until_ts")
        self._ensure_dir()
        lock_fd = os.open(self._marks_lock_path, os.O_CREAT | os.O_RDWR, 0o600)
        try:
            filelock.flock(lock_fd, filelock.LOCK_EX)
            try:
                marks = self._read_marks()
                marks[provider] = Mark(until, reason)
                payload = json.dumps(
                    {name: {"until": item.until, "reason": item.reason} for name, item in marks.items()},
                    ensure_ascii=False,
                    allow_nan=False,
                    sort_keys=True,
                )
                fd, temporary = tempfile.mkstemp(prefix=".marks-", suffix=".tmp", dir=str(self._dir))
                try:
                    with os.fdopen(fd, "w", encoding="utf-8") as stream:
                        if os.name != "nt":
                            try:
                                os.fchmod(stream.fileno(), 0o600)
                            except OSError:
                                pass
                        stream.write(payload)
                        stream.flush()
                        os.fsync(stream.fileno())
                    os.replace(temporary, self.marks_path)
                    temporary = None
                    try:
                        dir_fd = os.open(self._dir, os.O_RDONLY)
                        try:
                            os.fsync(dir_fd)
                        finally:
                            os.close(dir_fd)
                    except OSError:
                        pass
                finally:
                    if temporary is not None:
                        try:
                            os.unlink(temporary)
                        except FileNotFoundError:
                            pass
            finally:
                filelock.flock(lock_fd, filelock.LOCK_UN)
        finally:
            os.close(lock_fd)
        return Mark(until, reason)

    def active_marks(self, now: Any = None) -> Dict[str, Mark]:
        """只返回没过期的标记（until > now，到点即过期）；marks.json 损坏时当成空。"""
        moment = _resolve_now(now)
        return {name: item for name, item in self._read_marks().items() if item.until > moment}


def default_ledger() -> OutcomeLedger:
    """按当前环境（XDG_STATE_HOME 等）解析出的默认账本。"""
    return OutcomeLedger()


def record(provider: str, tier: Any, task_type: str, passed: Any, seconds: Any = None,
           calls: Any = None, ts: Any = None, *, now: Any = None,
           ledger: Optional[OutcomeLedger] = None, base_dir: Any = None, **kwargs: Any) -> Dict[str, Any]:
    target = ledger if ledger is not None else OutcomeLedger(base_dir) if base_dir is not None else OutcomeLedger()
    return target.record(provider, tier, task_type, passed, seconds, calls, ts, now=now, **kwargs)


def record_outcome(*args: Any, **kwargs: Any) -> Dict[str, Any]:
    return record(*args, **kwargs)


def stats(provider: str, tier: Any, task_type: str, now: Any = None, half_life_days: float = 14,
          *, ledger: Optional[OutcomeLedger] = None, base_dir: Any = None, **kwargs: Any) -> Stats:
    target = ledger if ledger is not None else OutcomeLedger(base_dir) if base_dir is not None else OutcomeLedger()
    return target.stats(provider, tier, task_type, now, half_life_days, **kwargs)


def get_stats(*args: Any, **kwargs: Any) -> Stats:
    return stats(*args, **kwargs)


def mark(provider: str, until_ts: Any, reason: str = "", *, now: Any = None,
         ledger: Optional[OutcomeLedger] = None, base_dir: Any = None) -> Mark:
    target = ledger if ledger is not None else OutcomeLedger(base_dir) if base_dir is not None else OutcomeLedger()
    return target.mark(provider, until_ts, reason)


def active_marks(now: Any = None, *, ledger: Optional[OutcomeLedger] = None, base_dir: Any = None) -> Dict[str, Mark]:
    target = ledger if ledger is not None else OutcomeLedger(base_dir) if base_dir is not None else OutcomeLedger()
    return target.active_marks(now)
