"""
Makewand Orchestrator: Multi-model pipeline, task tiering, auto-fix loop, and race engine.
"""

import os
import sys
import time
import tempfile
import concurrent.futures
from pathlib import Path
from typing import Optional

from makewand.config import (
    c,
    COLOR_BOLD,
    COLOR_GREEN,
    COLOR_YELLOW,
    COLOR_RED,
    COLOR_BLUE,
    COLOR_CYAN,
    COLOR_PURPLE
)
from makewand.git_helper import ensure_git_worktree, get_git_diff, clone_isolated_worktree
from makewand.health import get_or_update_status
from makewand.providers.agy import execute_agy_task
from makewand.providers.claude import execute_claude_task
from makewand.providers.codex import execute_codex_task
from makewand.providers.muse import execute_muse_task

def detect_task_tier(prompt: str) -> str:
    p_lower = prompt.lower()
    deep_keywords = ["审查", "审计", "review", "死锁", "并发", "安全", "漏洞", "架构", "设计", "deep", "complex", "formal", "重构"]
    fast_keywords = ["简单", "探测", "查看", "快速", "拼写", "probe", "quick", "fast", "typo", "format"]

    if any(k in p_lower for k in deep_keywords):
        return "deep"
    if any(k in p_lower for k in fast_keywords):
        return "fast"
    return "standard"

def has_critical_defects(review_text: str) -> bool:
    if not review_text:
        return False
    lower = review_text.lower()
    pass_signals = ["没有发现明显缺陷", "无需修改", "建议直接合并", "审核通过", "lgtm", "所有用例均通过且无安全漏洞", "未发现严重漏洞"]
    if any(sig in lower for sig in pass_signals) and not any(p in lower for p in ["[p1]", "[p2]", "致命缺陷", "建议修改后再合并"]):
        return False
    defect_patterns = ["[p1]", "[p2]", "致命缺陷", "并发漏洞", "数据竞态", "内存泄露", "资源泄露", "建议修改后再合并", "未被此次校验覆盖", "overflowerror", "race condition"]
    return any(p in lower for p in defect_patterns)

def run_pipeline(
    prompt: str,
    cwd: Optional[str] = None,
    tier: str = "auto",
    model: Optional[str] = None,
    stream: bool = False,
    auto_fix: bool = True,
    max_fix: int = 2,
    timeout: int = 300
):
    if not cwd:
        cwd = os.getcwd()
    if tier == "auto":
        tier = detect_task_tier(prompt)

    print(c(f"🚀 Makewand 流水线启动: '{prompt}' (自适应模型档位: {tier})", COLOR_BOLD))
    print(f"工作目录: {cwd}\n")

    # Step 1: Health inspection
    cache = get_or_update_status(force_probe=False)
    c_status = cache.get("claude", {}).get("status")
    x_status = cache.get("codex", {}).get("status")
    m_status = cache.get("muse", {}).get("status")

    # Ensure git tracking in non-git directories
    ensure_git_worktree(cwd)

    # Step 2: Implementation routing with fallback
    print(c(f"▶ 阶段 1: 代码编写与实现 (Implementation - Tier: {tier})", COLOR_BOLD + COLOR_BLUE))
    coder_output = None
    coder_engine = None

    # Priority 1: Claude Code
    if c_status != "limited":
        success, out, err = execute_claude_task(prompt, cwd=cwd, timeout=timeout, tier=tier, model=model, stream=stream)
        if success:
            print(c("✔ Claude Code 完成代码编写与修改。", COLOR_GREEN))
            coder_output = out
            coder_engine = "claude"
        else:
            print(c(f"⚠ Claude Code 遇到限制或故障: {err}", COLOR_YELLOW))
            print(c("→ 自动切换备用引擎接管实现...", COLOR_YELLOW))

    full_prompt = f"目标工作目录绝对路径: {cwd}\n请在该目录下创建/修改对应代码文件并落盘：\n{prompt}"

    # Priority 2: Codex CLI
    if coder_output is None and x_status != "limited":
        print(c("→ 使用 Codex 作为主力编码引擎...", COLOR_CYAN))
        success, out, err = execute_codex_task(full_prompt, cwd=cwd, timeout=timeout, tier=tier, model=model, stream=stream)
        if success:
            print(c("✔ Codex 完成代码编写与修改。", COLOR_GREEN))
            coder_output = out
            coder_engine = "codex"
        else:
            print(c(f"⚠ Codex 亦不可用: {err}", COLOR_YELLOW))

    # Priority 3: Muse Code
    if coder_output is None and m_status not in ["limited", "needs_auth", "missing"]:
        print(c("→ 使用 Muse Code 作为备用编码引擎...", COLOR_PURPLE))
        success, out, err = execute_muse_task(full_prompt, cwd=cwd, timeout=timeout, tier=tier, model=model, stream=stream)
        if success:
            print(c("✔ Muse Code 完成代码编写与修改。", COLOR_GREEN))
            coder_output = out
            coder_engine = "muse"
        else:
            print(c(f"⚠ Muse Code 亦不可用: {err}", COLOR_YELLOW))

    # Priority 4: Antigravity (Google AI Pro, Conductor & Architect)
    if coder_output is None:
        print(c("→ 启用 Antigravity (Google AI Pro) 进行最终闭环实现...", COLOR_GREEN))
        success, out, err = execute_agy_task(full_prompt, cwd=cwd, timeout=timeout, tier=tier, model=model, stream=stream)
        if success:
            print(c("✔ Antigravity 完成代码编写与修改。", COLOR_GREEN))
            coder_output = out
            coder_engine = "agy"
        else:
            print(c(f"❌ 自动降级失败: {err}", COLOR_RED))
            sys.exit(1)

    if coder_output and not stream:
        print(c("【编码实现输出摘要】", COLOR_BOLD))
        print(coder_output.strip()[:500])
        print("...\n")

    # Step 3: Red-team review (Cross-model verification)
    print(c("\n▶ 阶段 2: 独立代码审计与质检 (Red-team Review - Tier: deep)", COLOR_BOLD + COLOR_CYAN))
    diff_out = get_git_diff(cwd)
    diff_snippet = diff_out[:4500] if diff_out else "无未提交的 git diff"

    review_prompt = (
        f"工作目录为: {cwd}。请审查以下代码改动（git diff），严查潜在并发死锁、内存泄露、空指针与边界用例漏洞。\n"
        f"若发现严重隐患，请标注 [P1] 或 [P2] 并给出明确修复建议；若逻辑严谨无严重漏洞，请明确回复'LGTM / 审核通过'：\n{diff_snippet}"
    )

    review_output = None
    reviewer_engine = None

    # Assign reviewer different from coder
    if coder_engine != "codex" and x_status != "limited":
        print(c("→ 派发给 Codex CLI 进行独立红队审查 (gpt-6-astra)...", COLOR_CYAN))
        success, out, err = execute_codex_task(review_prompt, cwd=cwd, timeout=timeout, tier="deep", stream=stream)
        if success:
            print(c("✔ Codex 独立红队审查完成。", COLOR_GREEN))
            review_output = out
            reviewer_engine = "codex"
        else:
            print(c(f"⚠ Codex 审查未成功: {err}", COLOR_YELLOW))

    if review_output is None:
        print(c("→ 派发给 Antigravity 进行独立跨模型审查 (Google AI Pro High Reasoning)...", COLOR_GREEN))
        success, out, err = execute_agy_task(review_prompt, cwd=cwd, timeout=timeout, tier="deep", stream=stream)
        if success:
            print(c("✔ Antigravity 审查完成。", COLOR_GREEN))
            review_output = out
            reviewer_engine = "agy"
        else:
            print(c("⚠ 审查步骤已跳过。", COLOR_YELLOW))

    # Step 4: Auto-Fix Loop
    if auto_fix and review_output and has_critical_defects(review_output):
        current_fix_iter = 0
        while current_fix_iter < max_fix and has_critical_defects(review_output):
            current_fix_iter += 1
            print(c(f"\n⚡ [Makewand Auto-Fix] 独立审计检测到高/中危缺陷，自动启动第 {current_fix_iter}/{max_fix} 轮修复闭环...", COLOR_YELLOW + COLOR_BOLD))

            fix_prompt = (
                f"目标工作目录绝对路径: {cwd}\n"
                f"独立红队审查针对上一轮提交的代码发现了以下真实缺陷，请针对性修复所有漏洞并确保单测全通：\n"
                f"{review_output}\n\n"
                f"请直接落盘修改对应代码文件。"
            )

            # Coder fixes
            fixed = False
            if coder_engine == "claude" and c_status != "limited":
                ok, _, _ = execute_claude_task(fix_prompt, cwd=cwd, timeout=timeout, tier=tier, stream=stream)
                if ok: fixed = True
            elif coder_engine == "codex" and x_status != "limited":
                ok, _, _ = execute_codex_task(fix_prompt, cwd=cwd, timeout=timeout, tier=tier, stream=stream)
                if ok: fixed = True

            if not fixed and x_status != "limited":
                ok, _, _ = execute_codex_task(fix_prompt, cwd=cwd, timeout=timeout, tier=tier, stream=stream)
                if ok: fixed = True
            if not fixed:
                ok, _, _ = execute_agy_task(fix_prompt, cwd=cwd, timeout=timeout, tier=tier, stream=stream)
                if ok: fixed = True

            if not fixed:
                print(c("⚠ 自动修复执行失败，终止后续轮次。", COLOR_RED))
                break

            # Re-review
            print(c(f"▶ [Makewand Auto-Fix] 修复已落盘，重新发起第 {current_fix_iter} 轮红队复审...", COLOR_CYAN))
            new_diff = get_git_diff(cwd)
            re_review_prompt = (
                f"工作目录为: {cwd}。经过上一轮缺陷修复后，请复审以下代码改动，检查上述缺陷是否已彻底解决，是否存在新隐患。\n"
                f"若发现严重隐患，请标注 [P1] 或 [P2] 并给出明确修复建议；若逻辑严谨无严重漏洞，请明确回复'LGTM / 审核通过'：\n{new_diff[:4500]}"
            )

            re_output = None
            if reviewer_engine == "codex" and x_status != "limited":
                ok, out, _ = execute_codex_task(re_review_prompt, cwd=cwd, timeout=timeout, tier="deep", stream=stream)
                if ok: re_output = out
            if not re_output:
                ok, out, _ = execute_agy_task(re_review_prompt, cwd=cwd, timeout=timeout, tier="deep", stream=stream)
                if ok: re_output = out

            if re_output:
                review_output = re_output
                if not has_critical_defects(re_output):
                    print(c("✔ [Makewand Auto-Fix] 经过自动修复，代码已通过红队复审！", COLOR_GREEN + COLOR_BOLD))
                    break

    print(c("\n============================================================", COLOR_BOLD))
    print(c("                   Makewand 联合调度完成报告", COLOR_BOLD + COLOR_GREEN))
    print(c("============================================================\n", COLOR_BOLD))
    if review_output and not stream:
        print(c("【最终审计意见与质量评估】", COLOR_BOLD))
        print(review_output.strip()[:1000])
        print("...\n")
    print(c("✔ 任务全链路自适应闭环完成。", COLOR_GREEN + COLOR_BOLD))

def run_review(cwd: Optional[str] = None, stream: bool = False, timeout: int = 300):
    if not cwd:
        cwd = os.getcwd()
    print(c("🔍 Makewand 代码审计工具", COLOR_BOLD + COLOR_CYAN))
    diff_out = get_git_diff(cwd)
    if not diff_out.strip():
        print("当前工作区没有检测到未提交的改动 (git diff 为空)。")
        return

    cache = get_or_update_status()
    x_status = cache.get("codex", {}).get("status")

    prompt = f"工作目录为: {cwd}。请详细审查当前仓库的修改（git diff），指出潜在隐患并给出修复建议：\n{diff_out[:6000]}"

    if x_status != "limited":
        print(c("派发给 Codex CLI 进行红队审计 (gpt-6-astra)...", COLOR_CYAN))
        success, out, err = execute_codex_task(prompt, cwd=cwd, tier="deep", stream=stream, timeout=timeout)
        if success:
            if not stream:
                print(out)
            return
        print(c(f"Codex 不可用 ({err})，转交 Antigravity...", COLOR_YELLOW))

    print(c("由 Antigravity 进行红队审计...", COLOR_GREEN))
    success, out, err = execute_agy_task(prompt, cwd=cwd, tier="deep", stream=stream, timeout=timeout)
    if success:
        if not stream:
            print(out)
    else:
        print(c(f"审查失败: {err}", COLOR_RED))

def run_race(prompt: str, cwd: Optional[str] = None, timeout: int = 300):
    if not cwd:
        cwd = os.getcwd()
    print(c(f"🏁 Makewand 双模型并发竞速模式启动: '{prompt}'", COLOR_BOLD + COLOR_CYAN))

    ensure_git_worktree(cwd)

    cache = get_or_update_status()
    c_ok = cache.get("claude", {}).get("status") == "healthy"
    x_ok = cache.get("codex", {}).get("status") == "healthy"
    m_ok = cache.get("muse", {}).get("status") == "healthy"

    wt_a = Path(tempfile.gettempdir()) / "makewand_race_agent_a"
    wt_b = Path(tempfile.gettempdir()) / "makewand_race_agent_b"

    import shutil
    shutil.rmtree(wt_a, ignore_errors=True)
    shutil.rmtree(wt_b, ignore_errors=True)

    clone_isolated_worktree(cwd, wt_a)
    clone_isolated_worktree(cwd, wt_b)

    # Pick Contestants
    name_a = "Codex (gpt-6-astra)" if x_ok else ("Muse Code" if m_ok else "Antigravity (Gemini Fast)")
    name_b = "Claude Code" if c_ok else "Antigravity (Gemini Deep)"

    print(c(f"  选手 A: {name_a} (独立工作区: {wt_a})", COLOR_CYAN + COLOR_BOLD))
    print(c(f"  选手 B: {name_b} (独立工作区: {wt_b})", COLOR_BLUE + COLOR_BOLD))
    print(c("并发执行中，请稍候...\n", COLOR_YELLOW))

    def run_agent_a():
        start = time.time()
        full_p = f"工作目录绝对路径: {wt_a}\n请在该目录下完成代码编写并直接落盘：\n{prompt}"
        if x_ok:
            ok, out, err = execute_codex_task(full_p, cwd=str(wt_a), timeout=timeout)
        elif m_ok:
            ok, out, err = execute_muse_task(full_p, cwd=str(wt_a), timeout=timeout)
        else:
            ok, out, err = execute_agy_task(full_p, cwd=str(wt_a), timeout=timeout, tier="fast")
        duration = round(time.time() - start, 2)
        return name_a, ok, out, duration, wt_a

    def run_agent_b():
        start = time.time()
        full_p = f"工作目录绝对路径: {wt_b}\n请在该目录下完成代码编写并直接落盘：\n{prompt}"
        if c_ok:
            ok, out, err = execute_claude_task(full_p, cwd=str(wt_b), timeout=timeout)
        else:
            ok, out, err = execute_agy_task(full_p, cwd=str(wt_b), timeout=timeout, tier="deep")
        duration = round(time.time() - start, 2)
        return name_b, ok, out, duration, wt_b

    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
        f_a = executor.submit(run_agent_a)
        f_b = executor.submit(run_agent_b)
        res_a = f_a.result()
        res_b = f_b.result()

    diff_a = get_git_diff(str(wt_a))
    diff_b = get_git_diff(str(wt_b))

    print(c("\n============================================================", COLOR_BOLD))
    print(c("                Makewand 竞速赛况与性能指标", COLOR_BOLD + COLOR_GREEN))
    print(c("============================================================\n", COLOR_BOLD))
    print(f"选手 A [{res_a[0]}]: 状态={'✔ 成功' if res_a[1] else '❌ 失败'}, 耗时={res_a[3]}s, 代码Diff大小={len(diff_a)} 字节")
    print(f"选手 B [{res_b[0]}]: 状态={'✔ 成功' if res_b[1] else '❌ 失败'}, 耗时={res_b[3]}s, 代码Diff大小={len(diff_b)} 字节\n")

    # Chief Referee evaluation with Antigravity
    judge_prompt = (
        f"请作为资深软件架构裁判，客观对比以下两位选手对同一任务的实现方案，指出各自优势与缺陷，并评定胜出者：\n\n"
        f"--- 原始任务 ---\n{prompt}\n\n"
        f"--- 选手 A ({res_a[0]}) 的改动 ---\n{diff_a[:3000] if diff_a else '无 diff'}\n\n"
        f"--- 选手 B ({res_b[0]}) 的改动 ---\n{diff_b[:3000] if diff_b else '无 diff'}\n\n"
        f"请给出：1. 方案对比分析 2. 最终裁决结果及推荐采纳理由。"
    )
    print(c("由 Antigravity (Google AI Pro) 担任主裁判进行方案综合评估...", COLOR_GREEN + COLOR_BOLD))
    ok, judge_report, _ = execute_agy_task(judge_prompt, cwd=cwd, tier="deep")
    if judge_report:
        print(c("\n【裁判裁决报告】", COLOR_BOLD))
        print(judge_report.strip())
