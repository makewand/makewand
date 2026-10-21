"""``makewand budget clock``: show, and with --repair clear, a call budget clock ahead of real time.

The shared ledger (call-budget.json) dates daily buckets and lease and hold expiry
from its persisted clock. A ledger written while the system clock was ahead keeps
that clock and the day bucket ahead after the clock is corrected. Admission keeps
working (it counts into the latest day and trusts at most CLOCK_SLACK seconds of
the lead); --repair brings both back to real time under the ledger lock without
lowering any count. No model is called.
"""

import json
import sys

from makewand import call_budget, layered_budget
from makewand.account_leases import TTL

__all__ = ["cmd_budget"]

EXIT_LEDGER_ERROR = 1


def cmd_budget(args) -> None:
    repair = bool(getattr(args, "repair", False))
    try:
        report = call_budget.repair_clock(apply=repair)
    except call_budget.BudgetError as error:
        print(f"错误：{error}", file=sys.stderr)
        raise SystemExit(EXIT_LEDGER_ERROR)
    if getattr(args, "json", False):
        print(json.dumps(report, ensure_ascii=False, indent=2))
        return
    print("\n".join(_report_lines(report, repair)))


def _report_lines(report, repair):
    lines = [f"调用账本：{report['path']}"]
    if not report["exists"]:
        return lines + ["账本不存在，无需修复。"]
    if not report["layered"]:
        return lines + ["这是没有每日计数的终身账本，没有需要修复的时钟。"]
    zone = "本地时区" if report["timezone"] == "local" else report["timezone"]
    lines.append(f"日期桶：账本最新 {report['day']}，今天 {report['today']}（{zone}）")
    lead = report["clock_lead_seconds"]
    if lead > layered_budget.CLOCK_SLACK:
        lines.append(f"账本时钟：领先实际时间 {lead:.0f} 秒，租约与预留到期最多按领先 "
                     f"{layered_budget.CLOCK_SLACK} 秒计算")
    else:
        lines.append(f"账本时钟：领先实际时间 {lead:.0f} 秒，在允许的 {layered_budget.CLOCK_SLACK} 秒以内")
    if report["skewed_leases"] or report["skewed_holds"]:
        lines.append(f"时钟领先时写下的到期时间：租约 {report['skewed_leases']} 个，预留 {report['skewed_holds']} 个")
    if repair:
        if not report["applied"]:
            return lines + ["无需修改。"]
        done = ["账本时钟回到实际时间"] if lead > 0 else []
        if report["day"] > report["today"]:
            moved = f"日期桶回到 {report['today']}"
            if report["carried_buckets"]:
                moved += (f"，后面日期的 {report['carried_buckets']} 个计数桶同时计入今天"
                          "（已计的调用次数不退还）")
            if report["moved_holds"]:
                moved += f"，{report['moved_holds']} 个预留随之改计入今天"
            done.append(moved)
        if report["skewed_leases"]:
            done.append(f"租约改为最多 {TTL} 秒后到期（仍在运行的进程会继续续租）")
        if report["skewed_holds"]:
            done.append("预留按各自的领先量移回实际时间，时长不变")
        return lines + ["已修复：" + "；".join(done or ["领先实际时间的时间戳回到实际时间"])]
    if report["skewed"]:
        return lines + ["派活不受阻；运行 makewand budget clock --repair 让时钟回到实际时间（不退还已计的调用次数）"]
    return lines + ["时钟正常，无需修复。"]
