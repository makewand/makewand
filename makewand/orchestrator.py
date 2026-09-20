"""
Makewand Orchestrator: Multi-model pipeline, task tiering, auto-fix loop, and race engine.
"""

import os
import sys
import time
import uuid
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
    COLOR_PURPLE,
    COLOR_RESET
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
    unambiguous_defects = [
        "[p0]", "[p1]", "[p2]",
        "p0:", "p1:", "p2:",
        "致命缺陷", "建议修改后再合并", "需要整改", "未通过",
        "并发死锁", "内存泄露", "数据竞态", "race condition",
        "arbitrary host command"
    ]
    # If explicit defect tags exist, it is ALWAYS a defect (even if pass words appear)
    if any(p in lower for p in unambiguous_defects):
        return True

    pass_signals = [
        "没有发现明显缺陷", "无需修改", "建议直接合并", "审核通过", "lgtm",
        "所有用例均通过且无安全漏洞", "未发现严重漏洞", "无安全漏洞", "未发现安全漏洞"
    ]
    if any(sig in lower for sig in pass_signals):
        return False

    defect_patterns = ["缺陷", "漏洞", "隐患", "死锁", "竞态", "泄露", "overflowerror"]
    return any(p in lower for p in defect_patterns)

def is_identity_or_chit_chat(prompt: str) -> bool:
    lower = prompt.lower().strip()
    # Explicit action triggers take precedence: only if user explicitly asks to write/fix/build code
    coding_action_triggers = [
        "写代码", "写一个", "写个", "写段", "帮我写", "编写", "实现", "创建文件",
        "生成代码", "重构", "修改代码", "改写代码", "落盘", "修bug", "修复",
        "解决bug", "补丁", "优化代码", "写单测", "编写测试", "写脚本", "生成脚本",
        "运行测试", "跑测试", "跑单测", "执行测试",
        "write code", "write a", "implement", "build a", "create a file", "fix bug",
        "patch", "refactor", "generate code", "write a test", "code a",
        "run test", "run tests", "run the test", "run the tests"
    ]
    if any(t in lower for t in coding_action_triggers):
        return False

    # Check for compound follow-up indicators (e.g. "顺便", "然后", "接着", "并", "then", "and then", "also")
    compound_connectors = [
        "顺便", "然后", "接着", "顺带", "并且", "同时", "再帮我", "帮我", "顺便帮我",
        "then ", "and then", "after that", "also "
    ]
    if any(c in lower for c in compound_connectors):
        return False

    stripped = "".join(ch for ch in lower if ch.isalnum() or '\u4e00' <= ch <= '\u9fff')

    # Greetings: MUST be standalone greetings
    chinese_greetings = ["你好", "您好", "早上好", "下午好", "晚上好", "哈喽", "嗨", "打扰一下", "请问"]
    if stripped in chinese_greetings:
        return True

    english_greetings = ["hi", "hello", "hey", "hithere", "hellothere", "goodmorning", "goodafternoon", "goodevening"]
    if stripped in english_greetings:
        return True

    # Check if input starts with greeting and has substantial remainder
    for g in ["你好", "您好", "哈喽", "嗨", "hello", "hi"]:
        if lower.startswith(g):
            rem = lower[len(g):].strip(" ,，!！?？;；\t\n")
            if rem:
                return False

    identity_patterns = [
        "你是谁", "你是什么", "你叫什么", "你叫啥", "你到底是", "你究竟是", "你何方神圣",
        "介绍一下自己", "介绍自己", "介绍一下你自己", "介绍下自己", "介绍下你自己",
        "自我介绍", "做个自我介绍", "做一下自我介绍",
        "你能做什么", "你能干什么", "你能干啥", "你有什么功能", "你有哪些功能", "你有什么用", "你主要用来做",
        "谁开发了你", "谁创造了你", "谁创建了你", "谁写了你", "你的作者是谁", "你的开发者是谁",
        "你是人类还是", "你是什么类型", "你属于哪种", "你是什么ai", "你是什么模型", "你是什么智能",
        "whoareyou", "whatareyou", "whatisyourname", "whatsyourname",
        "introduceyourself", "tellmeaboutyourself", "whatcanyoudo", "whatdoyoudo",
        "whocreatedyou", "whomadeyou", "whoisyourauthor"
    ]

    task_verbs = ["分析", "审查", "解释", "说明", "排查", "测试", "执行", "运行", "run", "test", "analyze", "check", "explain"]
    for q in identity_patterns:
        if q in stripped:
            if any(v in lower for v in task_verbs):
                return False
            return True

    return False

def classify_prompt_intent(prompt: str) -> str:
    """
    Classify user prompt into:
    - 'identity': questions about who makewand is or what it can do
    - 'explain': questions/explanations/chit-chat that do not need file modifications or code review
    - 'review': code audit/review requests
    - 'code': code generation/refactoring/fixing tasks
    """
    lower = prompt.lower().strip()

    # Code action keywords take first priority ONLY if action verbs are present
    coding_action_triggers = [
        "写代码", "写一个", "写个", "写段", "帮我写", "编写", "实现", "创建文件",
        "生成代码", "重构", "修改代码", "改写代码", "落盘", "修bug", "修复",
        "解决bug", "补丁", "优化代码", "写单测", "编写测试", "写脚本", "生成脚本",
        "运行测试", "跑测试", "跑单测", "执行测试",
        "write code", "write a", "implement", "build a", "create a file", "fix bug",
        "patch", "refactor", "generate code", "write a test", "code a",
        "run test", "run tests", "run the test", "run the tests"
    ]
    if any(k in lower for k in coding_action_triggers):
        return "code"

    if is_identity_or_chit_chat(prompt):
        return "identity"

    if any(k in lower for k in ["审查", "审计", "review", "检查代码", "看下diff", "看下代码改动", "质检", "代码审计", "diff check"]):
        return "review"

    # Default to explain mode for general questions/explanations/conversations
    # so that questions do not accidentally trigger code file changes and red-team review loops.
    return "explain"

def get_identity_message() -> str:
    return (
        f"{COLOR_BOLD}{COLOR_GREEN}✨ 我是 Makewand (v3.0) —— 零成本多模型 AI 订阅统一调度中枢。{COLOR_RESET}\n\n"
        "我统合调度本机四大主流 AI 订阅服务：\n"
        f"  {COLOR_GREEN}• Google AI Pro (Antigravity / AGY){COLOR_RESET}: 全局架构设计、复杂推理与闭环兜底\n"
        f"  {COLOR_BLUE}• Claude Code (Anthropic){COLOR_RESET}: 高敏捷代码编写、多文件重构与实现\n"
        f"  {COLOR_CYAN}• Codex CLI (OpenAI / gpt-6-astra){COLOR_RESET}: 独立红队代码审查与算法攻防\n"
        f"  {COLOR_PURPLE}• Muse Code (Meta / Llama){COLOR_RESET}: 辅助生成、沙箱验证与备用编码\n\n"
        f"{COLOR_BOLD}核心机制：{COLOR_RESET}\n"
        "  1. 智能意图路由：精准区分闲聊/问答（直接响应）与工程开发任务（多模型流水线），杜绝误触发程序检查或缺陷修复\n"
        "  2. 跨模型联合流水线：自动规划、编码实现、红队盲审与 Auto-Fix 缺陷自愈\n"
        "  3. 双模型沙箱竞速 (/race)：临时工作区并发派发比拼与主裁判评定\n"
        "  4. 订阅配额健康监控 (/status, /quota)：零 Token 额外成本自适应容灾降级\n"
        "  5. 安全搜索与物理沙箱 (/search, /sandbox)：护栏搜索与 Bubblewrap 进程隔离"
    )

def run_pipeline(
    prompt: str,
    cwd: Optional[str] = None,
    tier: str = "auto",
    model: Optional[str] = None,
    stream: bool = False,
    auto_fix: bool = True,
    max_fix: int = 2,
    timeout: int = 300
) -> bool:
    if not cwd:
        cwd = os.getcwd()
    if tier == "auto":
        tier = detect_task_tier(prompt)

    intent = classify_prompt_intent(prompt)
    if intent == "identity":
        print(c("💡 Makewand 意图识别: 身份/能力问答 (无需执行代码修改或程序检查)", COLOR_BOLD + COLOR_GREEN))
        print(get_identity_message())
        return True

    if intent == "explain":
        print(c(f"💡 Makewand 意图识别: 技术问答/解释模式 '{prompt}' (推理档位: {tier})", COLOR_BOLD + COLOR_GREEN))
        cache = get_or_update_status(force_probe=False)
        c_status = cache.get("claude", {}).get("status")
        x_status = cache.get("codex", {}).get("status")
        m_status = cache.get("muse", {}).get("status")

        qa_output = None
        if c_status != "limited":
            success, out, err = execute_claude_task(prompt, cwd=cwd, timeout=timeout, tier=tier, model=model, stream=stream)
            if success:
                qa_output = out
        if qa_output is None and x_status != "limited":
            success, out, err = execute_codex_task(prompt, cwd=cwd, timeout=timeout, tier=tier, model=model, stream=stream)
            if success:
                qa_output = out
        if qa_output is None and m_status not in ["limited", "needs_auth", "missing"]:
            success, out, err = execute_muse_task(prompt, cwd=cwd, timeout=timeout, tier=tier, model=model, stream=stream)
            if success:
                qa_output = out
        if qa_output is None:
            success, out, err = execute_agy_task(prompt, cwd=cwd, timeout=timeout, tier=tier, model=model, stream=stream)
            if success:
                qa_output = out
        if qa_output and not stream:
            print(qa_output)

        return qa_output is not None

    if intent == "review":
        print(c(f"💡 Makewand 意图识别: 独立代码审计/审查模式 '{prompt}'", COLOR_BOLD + COLOR_CYAN))
        run_review(cwd=cwd, stream=stream, timeout=timeout, user_prompt=prompt)
        return True

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
            return False

    if coder_output is None:
        print(c("❌ 所有可用模型均无法完成编码任务，流水线终止。", COLOR_RED + COLOR_BOLD))
        return False

    if coder_output and not stream:
        print(c("【编码实现输出摘要】", COLOR_BOLD))
        print(coder_output.strip()[:500])
        print("...\n")

    # Step 3: Red-team review (Cross-model verification)
    diff_out = get_git_diff(cwd)
    if not diff_out or not diff_out.strip():
        print(c("ℹ 本次任务未产生未提交的代码改动 (git diff 为空)，无需启动红队复审与自愈流水线。", COLOR_CYAN))
        print(c("✔ 任务完成。", COLOR_GREEN + COLOR_BOLD))
        return True

    print(c("\n▶ 阶段 2: 独立代码审计与质检 (Red-team Review - Tier: deep)", COLOR_BOLD + COLOR_CYAN))
    diff_snippet = diff_out[:4500]

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

    if review_output and has_critical_defects(review_output):
        print(c("❌ [Makewand Quality Gate] 经修复轮次后代码仍存在未通过的缺陷，未达交付标准。", COLOR_RED + COLOR_BOLD))
        return False

    print(c("✔ 任务全链路自适应闭环完成并通过红队审查。", COLOR_GREEN + COLOR_BOLD))
    return True

def run_review(cwd: Optional[str] = None, stream: bool = False, timeout: int = 300, user_prompt: Optional[str] = None):
    if not cwd:
        cwd = os.getcwd()
    print(c("🔍 Makewand 代码审计工具", COLOR_BOLD + COLOR_CYAN))
    diff_out = get_git_diff(cwd)
    if not diff_out.strip():
        print("当前工作区没有检测到未提交的改动 (git diff 为空)。")
        return

    cache = get_or_update_status()
    x_status = cache.get("codex", {}).get("status")

    focus = f" 特别关注要求: {user_prompt}。" if user_prompt else ""
    prompt = f"工作目录为: {cwd}。请详细审查当前仓库的修改（git diff），{focus}指出潜在隐患并给出修复建议：\n{diff_out[:6000]}"

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

    import shutil
    race_id = uuid.uuid4().hex[:8]
    tmp_parent = Path(tempfile.gettempdir()) / f"makewand_race_{race_id}"
    wt_a = tmp_parent / "agent_a"
    wt_b = tmp_parent / "agent_b"

    try:
        wt_a.mkdir(parents=True, exist_ok=True)
        wt_b.mkdir(parents=True, exist_ok=True)

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
        ok, judge_report, _ = execute_agy_task(judge_prompt, cwd=cwd, tier="deep", timeout=timeout)
        if judge_report:
            print(c("\n【裁判裁决报告】", COLOR_BOLD))
            print(judge_report.strip())
    finally:
        shutil.rmtree(tmp_parent, ignore_errors=True)
