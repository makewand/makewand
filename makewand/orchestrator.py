"""
Makewand Orchestrator: Multi-model pipeline, task tiering, auto-fix loop, and race engine.
"""

import os
import sys
import re
import json
import time
import uuid
import shlex
import hashlib
import tempfile
import concurrent.futures
from datetime import datetime
from pathlib import Path
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
    COLOR_RESET,
    CANDIDATES_DIR,
    ensure_config_dir,
)
from makewand.git_helper import (
    ensure_git_worktree,
    get_git_diff,
    clone_isolated_worktree,
    run_git_cmd,
    check_working_tree_isolation,
    create_ephemeral_shadow_worktree,
    get_submodule_paths,
)
from makewand.candidate import CandidateManager
from makewand.health import get_or_update_status
from makewand.providers.agy import execute_agy_task
from makewand.providers.claude import execute_claude_task
from makewand.providers.codex import execute_codex_task
from makewand.providers.muse import execute_muse_task

# Standardized Exit Codes
EXIT_PASSED = 0
EXIT_INTERNAL_ERROR = 1
EXIT_USAGE_ERROR = 2
EXIT_FAILED = 10
EXIT_UNVERIFIED = 11
EXIT_CANCELLED = 12
EXIT_BUDGET_EXHAUSTED = 13
EXIT_APPLY_CONFLICT = 14
EXIT_SANDBOX_UNAVAILABLE = 15

def detect_task_tier(prompt: str) -> str:
    p_lower = prompt.lower()
    deep_keywords = ["审查", "审计", "review", "死锁", "并发", "安全", "漏洞", "架构", "设计", "deep", "complex", "formal", "重构"]
    fast_keywords = ["简单", "探测", "查看", "快速", "拼写", "probe", "quick", "fast", "typo", "format"]

    if any(k in p_lower for k in deep_keywords):
        return "deep"
    if any(k in p_lower for k in fast_keywords):
        return "fast"
    return "standard"

def _normalize_verdict_dict(d: Dict[str, Any]) -> Dict[str, Any]:
    res = dict(d)
    raw_pass = res.get("pass")
    pass_val = False
    if isinstance(raw_pass, bool):
        pass_val = raw_pass
    elif isinstance(raw_pass, str):
        pass_val = raw_pass.strip().lower() in ["true", "1", "yes", "pass", "lgtm"]
    elif isinstance(raw_pass, (int, float)):
        # Strictly 1 is True; values like 2, -1, 0 must NOT be treated as True
        pass_val = (raw_pass == 1)

    raw_defects = res.get("defects", [])
    if isinstance(raw_defects, str):
        defects_list = [raw_defects.strip()] if raw_defects.strip() else []
    elif isinstance(raw_defects, list):
        defects_list = [str(x).strip() for x in raw_defects if str(x).strip()]
    elif isinstance(raw_defects, dict):
        items = raw_defects.get("items") or raw_defects.get("defects") or list(raw_defects.values())
        if isinstance(items, list):
            defects_list = [str(x).strip() for x in items if str(x).strip()]
        else:
            defects_list = [str(raw_defects)]
    elif raw_defects:
        defects_list = [str(raw_defects).strip()]
    else:
        defects_list = []

    # Contradiction guard: non-empty defects MUST force pass to False
    if defects_list:
        pass_val = False

    res["pass"] = pass_val
    res["defects"] = defects_list
    return res

def extract_verdict_json(text: str) -> Optional[Dict[str, Any]]:
    """
    Robust extraction of MAKEWAND_VERDICT JSON payload from review text.
    Finds the LAST occurrence of MAKEWAND_VERDICT: to avoid prompt template quotes.
    Uses raw_decode and lenient trailing-comma cleaning.
    If the last occurrence cannot be parsed, returns a fail-closed dict with parse_error=True,
    preventing any fallback to earlier examples.
    """
    if not text:
        return None
    tag = "MAKEWAND_VERDICT:"
    pos = text.rfind(tag)
    if pos == -1:
        return None

    snippet = text[pos + len(tag):].lstrip()
    decoder = json.JSONDecoder()

    # Attempt 1: direct raw_decode
    try:
        obj, _ = decoder.raw_decode(snippet)
        if isinstance(obj, dict):
            return _normalize_verdict_dict(obj)
    except Exception:
        pass

    # Attempt 2: sanitize trailing commas before } or ] and retry
    try:
        cleaned = re.sub(r",\s*([}\]])", r"\1", snippet)
        obj, _ = decoder.raw_decode(cleaned)
        if isinstance(obj, dict):
            return _normalize_verdict_dict(obj)
    except Exception:
        pass

    # Fail-closed: the model emitted MAKEWAND_VERDICT: but the JSON is corrupted/unparseable.
    first_line = snippet.splitlines()[0] if snippet.splitlines() else snippet
    return {
        "pass": False,
        "defects": [f"末尾评审判定 JSON 格式解析失败 (Syntax/Decode Error): {first_line[:120]}"],
        "parse_error": True,
    }

def is_review_passed(review_text: str) -> bool:
    """
    Returns True if and only if review explicitly passes quality gate without defects.
    Any unverified text, empty output, contradiction, or failure to produce explicit approval returns False (Fail-Closed).
    """
    if not review_text or not review_text.strip():
        return False

    lower = review_text.lower().strip()

    # Reject unverified or failure outputs immediately
    unverified_signals = [
        "unable to review", "cannot review", "failed to review",
        "unverified", "do not approve", "not approve", "not lgtm", "disapprove",
        "不通过", "未通过", "拒绝合并", "建议不要合并"
    ]
    if any(sig in lower for sig in unverified_signals):
        return False

    # 1. Structural JSON verdict check (from end of output to skip template quotes)
    verdict_data = extract_verdict_json(review_text)
    if verdict_data:
        if verdict_data.get("parse_error"):
            return False
        if verdict_data.get("defects"):
            return False
        if not verdict_data.get("pass", False):
            return False
        if has_critical_defects(review_text):
            return False
        return True

    # If MAKEWAND_VERDICT tag is present in review_text but extract_verdict_json returned None
    if "MAKEWAND_VERDICT:" in review_text:
        return False

    if has_critical_defects(review_text):
        return False

    # Positive confirmation check
    pass_signals = [
        "没有发现明显缺陷", "无需修改", "建议直接合并", "审核通过",
        "所有用例均通过且无安全漏洞", "未发现严重漏洞", "无安全漏洞", "未发现安全漏洞",
        "looks good to me", "all tests pass", "表现良好"
    ]
    if any(sig in lower for sig in pass_signals):
        return True

    # Standalone lgtm (guard against "not lgtm" / "isn't lgtm")
    if "lgtm" in lower and not any(neg in lower for neg in ["not lgtm", "no lgtm", "isn't lgtm"]):
        return True

    return False

def format_review_diff(diff: str, max_chars: int = 15000) -> str:
    """
    Formats git diff for review prompts without silent full-truncation.
    For small-to-medium diffs, preserves full content.
    For large diffs (>max_chars), retains head & tail and emits clear truncation notice.
    """
    if not diff:
        return ""
    if len(diff) <= max_chars:
        return diff
    head_len = 10000
    tail_len = 4000
    head = diff[:head_len]
    tail = diff[-tail_len:]
    return (
        f"{head}\n\n"
        f"=== [Makewand Diff Truncated: 变更总长度为 {len(diff)} 字符，已展示前 {head_len} 字符及后 {tail_len} 字符核心片段。请审查模型结合只读文件读取工具审阅全貌] ===\n\n"
        f"{tail}"
    )

def run_local_tests(cwd: str, timeout: int = 60) -> Tuple[bool, Optional[str]]:
    """
    Deterministically detects and runs local unit test suites in cwd inside Bubblewrap sandbox.
    Returns (passed: bool, details: Optional[str]).
    If no tests exist in project, returns (True, None).
    """
    import shutil
    from makewand.sandbox import run_in_sandbox
    p = Path(cwd)

    test_cmd = None
    extra_env = {"PYTHONPATH": f"{cwd}:{os.environ.get('PYTHONPATH', '')}"}

    # 1. Python test suites
    if (p / "pytest.ini").exists() or (p / "pyproject.toml").exists() or (p / "tests").is_dir() or list(p.glob("test_*.py")):
        py_bin = sys.executable or "python3"
        test_target = ["tests"] if (p / "tests").is_dir() else []
        try:
            import pytest
            test_cmd = [py_bin, "-m", "pytest", "-q", "-p", "no:langsmith", "-p", "no:django"] + test_target
        except ImportError:
            if shutil.which("pytest"):
                test_cmd = ["pytest", "-q", "-p", "no:langsmith", "-p", "no:django"] + test_target
            else:
                test_cmd = [py_bin, "-m", "unittest", "discover", "-q"]

    # 2. Go test suites
    elif (p / "go.mod").exists():
        test_cmd = ["go", "test", "./..."]

    # 3. Node / npm test suites
    elif (p / "package.json").exists():
        try:
            with open(p / "package.json", "r", encoding="utf-8") as f:
                pkg_data = json.load(f)
                if "test" in pkg_data.get("scripts", {}):
                    test_cmd = ["npm", "test", "--", "--passWithNoTests"]
        except Exception:
            pass

    # 4. Cargo / Rust
    elif (p / "Cargo.toml").exists():
        test_cmd = ["cargo", "test"]

    if not test_cmd:
        return True, None

    # Execute tests strictly inside isolated sandbox:
    # allow_network=False, readonly=False (allows test artifacts inside workspace),
    # is_provider=False (tmpfs HOME, masks credentials, drops host environment)
    code, stdout, stderr, err_category = run_in_sandbox(
        cmd=test_cmd,
        workspace=cwd,
        timeout=timeout,
        allow_network=False,
        readonly=False,
        is_provider=False,
        extra_env=extra_env
    )
    if code == 0:
        return True, stdout.strip()
    else:
        output = (stdout + "\n" + stderr).strip()
        if err_category == "SandboxUnavailable":
            return False, f"Bubblewrap 沙箱不可用，根据安全防御原则阻断本地测试执行: {stderr}"
        elif err_category:
            return False, f"本地单元测试执行异常 ({err_category}):\n{output}"
        return False, output

def has_critical_defects(review_text: str) -> bool:
    """
    Returns True if the review text explicitly indicates critical defects.
    Hard defect markers (e.g. [P1], reject recommendation) always override contradictory JSON verdicts.
    """
    if not review_text or not review_text.strip():
        return True

    lower = review_text.lower()

    # Hard defect signals and rejections in text always count as defects (override contradictory JSON)
    hard_rejections = [
        "do not approve", "not approve", "not lgtm", "disapprove",
        "不通过", "未通过", "拒绝合并", "建议不要合并", "不建议合并",
        "[p0]", "[p1]", "p0:", "p1:"
    ]
    if any(sig in lower for sig in hard_rejections):
        return True

    # 1. Structural JSON verdict check (from end of output)
    verdict_data = extract_verdict_json(review_text)
    if verdict_data:
        if verdict_data.get("parse_error"):
            return True
        defects = verdict_data.get("defects")
        if defects and isinstance(defects, list) and len(defects) > 0:
            return True
        if "pass" in verdict_data:
            val = verdict_data["pass"]
            if not bool(val):
                return True

    # 2. Negation phrase stripping to avoid false positives
    cleaned = lower
    item_pat = r"(?:并发死锁|死锁|内存泄露|内存泄漏|数据竞态(?:隐患)?|竞态(?:隐患)?|race\s+condition|安全漏洞|安全隐患|缺陷|漏洞|隐患|bug|问题)"
    prefix_pat = r"(?:未发现|没有发现|未见|不存在|没有|无|亦无|并无|且无|毫无)\s*(?:明显|严重|任何|潜在|可疑)?"
    compound_negation = rf"{prefix_pat}\s*{item_pat}(?:\s*(?:与|和|及|以及|或)\s*{item_pat})*"

    negation_patterns = [
        compound_negation,
        r"\bno\s+(?:deadlock|race\s+condition|memory\s+leak|defects?|vulnerabilit(?:y|ies))\b(?:\s+(?:or|and)\s+(?:deadlock|race\s+condition|memory\s+leak|defects?|vulnerabilit(?:y|ies)))*",
        r"\bwithout\s+(?:any\s+)?(?:deadlock|defect|bug|vulnerability|race\s+condition)\b",
        r"\bfree\s+of\s+(?:deadlocks?|defects?|vulnerabilit(?:y|ies))\b"
    ]
    for pat in negation_patterns:
        cleaned = re.sub(pat, " ", cleaned)

    unambiguous_defects = [
        "[p0]", "[p1]", "[p2]",
        "p0:", "p1:", "p2:",
        "致命缺陷", "建议修改后再合并", "需要整改", "未通过",
        "并发死锁", "内存泄露", "内存泄漏", "数据竞态", "race condition",
        "arbitrary host command"
    ]
    if any(p in cleaned for p in unambiguous_defects):
        return True

    # If JSON explicitly passed and no unnegated defects were found in cleaned text
    if verdict_data and verdict_data.get("pass") is True:
        return False

    pass_signals = [
        "没有发现明显缺陷", "无需修改", "建议直接合并", "审核通过", "lgtm",
        "所有用例均通过且无安全漏洞", "未发现严重漏洞", "无安全漏洞", "未发现安全漏洞",
        "looks good to me", "all tests pass"
    ]
    if any(sig in lower for sig in pass_signals) and not any(neg in lower for neg in ["not lgtm", "do not approve"]):
        return False

    defect_patterns = ["缺陷", "漏洞", "隐患", "死锁", "竞态", "泄露", "泄漏", "overflowerror"]
    return any(p in cleaned for p in defect_patterns)

def extract_review_verdict_dict(review_text: str) -> Dict[str, Any]:
    """
    Extract structured review verdict and defects list from review output.
    """
    passed = is_review_passed(review_text)
    defects: List[str] = []
    verdict_data = extract_verdict_json(review_text)
    if verdict_data:
        raw_defects = verdict_data.get("defects", [])
        if isinstance(raw_defects, list):
            defects = [str(d).strip() for d in raw_defects if str(d).strip()]

    if not passed and not defects and review_text:
        for line in review_text.splitlines():
            l_strip = line.strip()
            if any(tag in l_strip.upper() for tag in ["[P0]", "[P1]", "[P2]", "P0:", "P1:", "P2:", "CRITICAL", "DEFECT"]):
                defects.append(l_strip[:200])
                if len(defects) >= 5:
                    break

    return {
        "pass": passed,
        "defects": defects,
    }

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
    - 'explain': questions/explanations/chit-chat (strictly read-only execution)
    - 'review': code audit/review requests (strictly read-only execution)
    - 'code': code generation/refactoring/fixing tasks
    """
    lower = prompt.lower().strip()

    # 1. Action verbs for Chinese (expanded to include incremental creation words)
    chinese_coding_triggers = [
        "写代码", "写一个", "写个", "写段", "帮我写", "编写", "实现", "创建文件",
        "生成代码", "重构", "修改代码", "改写代码", "落盘", "修bug", "修复",
        "解决bug", "补丁", "优化代码", "写单测", "编写测试", "写脚本", "生成脚本",
        "运行测试", "跑测试", "跑单测", "执行测试",
        "添加", "增加", "支持", "接入", "对接", "开发", "引入", "新建", "加上",
        "增加功能", "添加功能", "支持功能"
    ]

    # Action verbs for English with word boundary regex
    english_coding_patterns = [
        r"\bwrite\s+code\b", r"\bwrite\s+a\b", r"\bimplement\b", r"\bbuild\s+a\b",
        r"\bcreate\s+(?:a\s+)?file\b", r"\bfix\s+bug\b", r"\bpatch\b", r"\brefactor\b",
        r"\bgenerate\s+code\b", r"\bwrite\s+a\s+test\b", r"\bcode\s+a\b",
        r"\brun\s+(?:the\s+)?tests?\b", r"\badd\b", r"\bcreate\b", r"\bdevelop\b",
        r"\bsupport\b", r"\bintegrate\b"
    ]

    # Check for explicit read-only or negation patterns first
    negation_patterns = [
        "不要修改", "不用修改", "别修改", "不要改", "别改", "不用改",
        "只看不改", "只解释", "无需修改", "不要写代码", "别写代码", "不用写代码",
        "只分析", "只做分析", "只读", "规范是什么", "是如何实现", "是怎么实现", "原理是什么",
        "don't modify", "do not modify", "without modifying", "don't edit", "do not edit",
        "read only", "readonly", "explain only", "just explain", "how does", "how do i",
        "what is", "why does", "what are"
    ]
    has_negation = any(n in lower for n in negation_patterns)

    # Has explicit coding action?
    has_chinese_coding = any(k in lower for k in chinese_coding_triggers)
    has_english_coding = any(re.search(pat, lower) for pat in english_coding_patterns)
    has_coding_action = (has_chinese_coding or has_english_coding) and not has_negation

    # If user explicitly asked for code modifications (even if they also asked to review, e.g. "实现一个登录接口并审查代码")
    if has_coding_action:
        # Avoid pure informational questions like "如何添加搜索功能？"
        if not any(q in lower for q in ["如何", "怎么", "规范是什么", "是什么", "有哪些", "why", "how"]):
            return "code"
        elif any(act in lower for act in ["并在当前目录落盘", "保存到", "写入文件", "修改文件", "并落盘"]):
            return "code"

    # If user asked for review without coding actions (or with explicit read-only negation)
    review_keywords = ["审查", "审计", "review", "检查代码", "看下diff", "看下代码改动", "质检", "代码审计", "diff check"]
    if any(k in lower for k in review_keywords):
        return "review"

    if has_negation:
        return "explain"

    if is_identity_or_chit_chat(prompt):
        return "identity"

    # Default to explain mode for general questions/explanations/conversations
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

def check_load_backpressure(load_threshold: float = 24.0) -> bool:
    """
    Monitors system 1-minute load average. When load exceeds load_threshold,
    yields process priority and logs throttled concurrency notice.
    """
    try:
        load_1m = os.getloadavg()[0]
        if load_1m > load_threshold:
            print(c(f"⏳ [Makewand Backpressure] 检测到主机负载偏高 (1m load: {load_1m:.1f} > {load_threshold})，自适应降低调度优先级...", COLOR_YELLOW))
            try:
                os.nice(5)
            except Exception:
                pass
            return True
    except Exception:
        pass
    return False

def dispatch_task(
    engine: str,
    prompt: str,
    cwd: Optional[str] = None,
    timeout: int = 300,
    tier: str = "standard",
    model: Optional[str] = None,
    stream: bool = False,
    readonly: bool = False,
    repo_root: Optional[str] = None
) -> Tuple[bool, Optional[str], Optional[str]]:
    """Generic multi-model task dispatcher wrapping provider adapters."""
    res = None
    if engine == "claude":
        res = execute_claude_task(prompt, cwd=cwd, timeout=timeout, tier=tier, model=model, stream=stream, readonly=readonly, repo_root=repo_root)
    elif engine == "codex":
        p = f"目标工作目录绝对路径: {cwd}\n请在该目录下创建/修改对应代码文件并落盘：\n{prompt}" if not readonly and cwd else prompt
        res = execute_codex_task(p, cwd=cwd, timeout=timeout, tier=tier, model=model, stream=stream, readonly=readonly, repo_root=repo_root)
    elif engine == "muse":
        p = f"目标工作目录绝对路径: {cwd}\n请在该目录下创建/修改对应代码文件并落盘：\n{prompt}" if not readonly and cwd else prompt
        res = execute_muse_task(p, cwd=cwd, timeout=timeout, tier=tier, model=model, stream=stream, readonly=readonly, repo_root=repo_root)
    elif engine == "agy":
        p = f"目标工作目录绝对路径: {cwd}\n请在该目录下创建/修改对应代码文件并落盘：\n{prompt}" if not readonly and cwd else prompt
        res = execute_agy_task(p, cwd=cwd, timeout=timeout, tier=tier, model=model, stream=stream, readonly=readonly, repo_root=repo_root)
    else:
        return False, None, f"未知或不支持的模型引擎: {engine}"

    if isinstance(res, (tuple, list)) and len(res) == 3:
        try:
            from makewand.usage import record_engine_usage
            record_engine_usage(engine, tier=tier, success=res[0], task=prompt)
        except Exception:
            pass
        return res[0], res[1], res[2]

    try:
        from makewand.usage import record_engine_usage
        record_engine_usage(engine, tier=tier, success=False, task=prompt)
    except Exception:
        pass
    return False, None, f"引擎 {engine} 适配器返回了异常或非预期格式: {type(res).__name__}"

def _match_domain_keywords(keywords: List[str], text: str) -> List[str]:
    """
    Matches keywords against text.
    For ASCII/English keywords (containing alphanumeric/hyphen/underscore/plus), enforces word boundaries.
    For non-ASCII / CJK keywords, uses substring inclusion.
    """
    matched = []
    for k in keywords:
        if re.match(r'^[a-zA-Z0-9_\-\+]+$', k):
            pattern = r'(?<![a-zA-Z0-9_])' + re.escape(k) + r'(?![a-zA-Z0-9_])'
            if re.search(pattern, text, re.IGNORECASE):
                matched.append(k)
        else:
            if k in text:
                matched.append(k)
    return matched

def select_optimal_engine_pair(
    prompt: str,
    tier: str = "standard",
    cache: Optional[Dict[str, Any]] = None
) -> Tuple[List[str], List[str], Dict[str, Any]]:
    """
    Intelligently scores and pairs engines for (Implementation, Red-team Review)
    based on task domain affinity and quota window dynamics.
    Returns: (ordered_coders, ordered_reviewers, meta_info)
    """
    if cache is None:
        cache = get_or_update_status(force_probe=False)

    p_lower = prompt.lower()

    # Base scores:
    # Claude: primary general software development & engineering (2.0)
    # Codex: short rolling window (3-4h resets) & red-team specialist (1.8)
    # Antigravity: continuous high-capacity reasoning anchor (1.4)
    # Muse: secondary alternative (0.8)
    scores = {
        "claude": 2.0,
        "codex": 1.8,
        "agy": 1.4,
        "muse": 0.8
    }

    reasons = []

    # 1. Semantic Domain Keywords
    # Algorithmic, Concurrency, Low-level, Security -> Codex affinity
    algo_keywords = [
        "算法", "algorithm", "leetcode", "二叉树", "binary tree", "动态规划", "dynamic programming",
        "排序", "sort", "图论", "graph", "hash", "哈希", "并发", "concurrency", "死锁", "deadlock",
        "race condition", "竞态", "mutex", "channel", "goroutine", "thread", "asyncio", "锁",
        "性能", "benchmark", "优化", "optimize", "内存", "memory leak", "底层", "kernel",
        "protocol", "协议", "socket", "tcp", "udp", "汇编", "assembly", "c++", "rust",
        "unsafe", "位运算", "bitwise", "逆向", "reverse engineering"
    ]
    matched_algo = _match_domain_keywords(algo_keywords, p_lower)
    if matched_algo:
        scores["codex"] += 2.5
        reasons.append(f"命中算法与底层并发特征 ({', '.join(matched_algo[:3])}) -> Codex 专精大幅加权")

    # Refactoring, UI/Frontend, Web, Docs, Types -> Claude affinity
    refactor_keywords = [
        "重构", "refactor", "整理", "clean code", "rename", "拆分", "前端", "frontend",
        "react", "vue", "svelte", "nextjs", "component", "组件", "css", "html", "tailwind",
        "ui", "ux", "web", "django", "fastapi", "flask", "express", "spring", "crud",
        "rest", "api", "endpoint", "controller", "view", "typescript", "ts", "文档",
        "docstring", "readme", "markdown", "unittest", "pytest", "mock", "测试用例"
    ]
    matched_refactor = _match_domain_keywords(refactor_keywords, p_lower)
    if matched_refactor:
        scores["claude"] += 2.5
        reasons.append(f"命中工程重构/前端/框架特征 ({', '.join(matched_refactor[:3])}) -> Claude 专精大幅加权")

    # Architecture, Full repo, Monorepo, Global Plan -> Antigravity affinity
    arch_keywords = [
        "全仓", "跨项目", "全局架构", "architecture", "跨模块", "system design", "总体设计",
        "全链路", "全工程", "monorepo", "超长上下文", "long context", "综合分析", "技术选型",
        "方案对比", "tradeoff", "可行性"
    ]
    matched_arch = _match_domain_keywords(arch_keywords, p_lower)
    if matched_arch:
        scores["agy"] += 3.0
        reasons.append(f"命中全局架构/跨模块/全仓设计特征 ({', '.join(matched_arch[:3])}) -> Antigravity 架构师加权")

    # Tier adjustments
    if tier == "deep":
        scores["codex"] += 0.8
        scores["agy"] += 0.8
    elif tier == "fast":
        scores["claude"] += 0.8

    # 2. Sliding Window Quota Burn-Rate Adjustment
    try:
        from makewand.usage import get_burn_rate_penalty
        for model_name in list(scores.keys()):
            if scores[model_name] > 0:
                pen, pen_reason = get_burn_rate_penalty(model_name)
                if pen != 0.0:
                    scores[model_name] += pen
                    if pen_reason:
                        reasons.append(pen_reason)
    except Exception:
        pass

    # 3. Quota Health Filter
    for model_name in list(scores.keys()):
        status = cache.get(model_name, {}).get("status", "unknown")
        if status == "limited":
            scores[model_name] = -999.0
            reasons.append(f"{model_name} 当前额度受限 (limited)")
        elif status in ("needs_auth", "missing"):
            scores[model_name] = -999.0

    # Sort coder candidates
    available_coders = [m for m, sc in sorted(scores.items(), key=lambda x: x[1], reverse=True) if sc > 0]
    if not available_coders:
        available_coders = ["agy"]

    primary_coder = available_coders[0]

    # Reviewer candidates: strictly different from coder, with Codex / AGY / Claude preferred
    reviewer_base_scores = {
        "codex": 2.2,   # exceptional red-team adversarial tester
        "agy": 2.0,     # deep high reasoning judge
        "claude": 1.6,  # great for readability, lint, and test validation
        "muse": 0.5
    }
    reviewer_base_scores.pop(primary_coder, None)
    for model_name in list(reviewer_base_scores.keys()):
        status = cache.get(model_name, {}).get("status", "unknown")
        if status in ("limited", "needs_auth", "missing"):
            reviewer_base_scores[model_name] = -999.0

    available_reviewers = [m for m, sc in sorted(reviewer_base_scores.items(), key=lambda x: x[1], reverse=True) if sc > 0]
    if not available_reviewers:
        available_reviewers = ["agy"]

    meta_info = {
        "scores": scores,
        "reasons": reasons,
        "primary_coder": primary_coder,
        "primary_reviewer": available_reviewers[0] if available_reviewers else "agy"
    }

    return available_coders, available_reviewers, meta_info

def run_pipeline(
    prompt: str,
    cwd: Optional[str] = None,
    tier: str = "auto",
    model: Optional[str] = None,
    stream: bool = False,
    auto_fix: bool = True,
    max_fix: int = 2,
    timeout: int = 300,
    total_budget: Optional[int] = None,
    force_code: bool = False
) -> bool:
    check_load_backpressure()
    if not cwd:
        cwd = os.getcwd()
    if tier == "auto":
        tier = detect_task_tier(prompt)

    if total_budget is None:
        total_budget = timeout
    else:
        total_budget = min(total_budget, timeout)

    pipeline_start_time = time.time()

    def get_remaining_timeout(requested: int) -> int:
        elapsed = time.time() - pipeline_start_time
        left = int(total_budget - elapsed)
        if left <= 0:
            return 0
        return min(requested, left)

    # Explicit read-only / negative patterns strictly override force_code
    explicit_readonly_triggers = [
        "不要修改", "不用修改", "别修改", "不要改", "别改", "不用改",
        "只看不改", "只解释", "无需修改", "不要写代码", "别写代码", "不用写代码",
        "只分析", "只做分析", "只读", "don't modify", "do not modify", "without modifying",
        "don't edit", "do not edit", "read only", "readonly", "explain only", "just explain"
    ]
    has_explicit_readonly = any(n in prompt.lower() for n in explicit_readonly_triggers)

    if has_explicit_readonly:
        intent = classify_prompt_intent(prompt)
        if intent == "code":
            intent = "explain"
    elif force_code:
        intent = "code"
    else:
        intent = classify_prompt_intent(prompt)

    if intent == "identity":
        print(c("💡 Makewand 意图识别: 身份/能力问答 (无需执行代码修改或程序检查)", COLOR_BOLD + COLOR_GREEN))
        print(get_identity_message())
        return True

    # Check multi-session working tree isolation guard for engineering tasks
    is_shadow_active = False
    shadow_res = None
    shadow_worktree_dir = None
    shadow_branch = None
    cleanup_shadow = None

    if intent not in ("identity", "explain", "review"):
        try:
            is_safe, conflict_msg = check_working_tree_isolation(cwd)
        except Exception:
            is_safe, conflict_msg = True, None

        if not is_safe:
            print(c(f"🛡️ [Makewand Multi-Session Guard] {conflict_msg}！", COLOR_YELLOW + COLOR_BOLD))
            print(c("   依从 P920 工作树隔离铁律，自动切换为独立影子工作树进行开发与审查...", COLOR_YELLOW))
            try:
                shadow_res = create_ephemeral_shadow_worktree(cwd, prefix="guard")
                shadow_worktree_dir, shadow_branch, cleanup_shadow = shadow_res[0], shadow_res[1], shadow_res[2]
                if not shadow_worktree_dir or not Path(shadow_worktree_dir).exists():
                    raise RuntimeError("Shadow worktree directory could not be established")
                cwd = shadow_worktree_dir
                is_shadow_active = True
                branch_label = shadow_branch if shadow_branch else "独立隔离副本"
                print(c(f"   ✔ 已自动建立影子工作树: {shadow_worktree_dir} (分支: {branch_label})", COLOR_GREEN))
            except Exception as e:
                print(c(f"❌ [Makewand Multi-Session Guard] 无法为活跃冲突会话建立安全影子工作树 ({e})，终止任务以防踩踏。", COLOR_RED + COLOR_BOLD))
                return False

    def fail_and_cleanup(msg: str) -> bool:
        if is_shadow_active and cleanup_shadow:
            try:
                cleanup_shadow()
            except Exception:
                pass
        print(c(msg, COLOR_RED + COLOR_BOLD))
        return False

    if intent == "explain":
        print(c(f"💡 Makewand 意图识别: 技术问答/解释模式 '{prompt}' (推理档位: {tier}, 只读安全隔离)", COLOR_BOLD + COLOR_GREEN))
        cache = get_or_update_status(force_probe=False)
        c_status = cache.get("claude", {}).get("status")
        x_status = cache.get("codex", {}).get("status")
        m_status = cache.get("muse", {}).get("status")

        step_timeout = get_remaining_timeout(timeout)
        if step_timeout <= 0:
            print(c("❌ [Makewand Budget] 全局流水线预算已耗尽，终止问答执行。", COLOR_RED + COLOR_BOLD))
            return False

        qa_output = None
        if c_status != "limited":
            success, out, err = execute_claude_task(prompt, cwd=cwd, timeout=step_timeout, tier=tier, model=model, stream=stream, readonly=True)
            if success:
                qa_output = out
        if qa_output is None and x_status != "limited":
            step_timeout = get_remaining_timeout(timeout)
            if step_timeout > 0:
                success, out, err = execute_codex_task(prompt, cwd=cwd, timeout=step_timeout, tier=tier, model=model, stream=stream, readonly=True)
                if success:
                    qa_output = out
        if qa_output is None and m_status not in ["limited", "needs_auth", "missing"]:
            step_timeout = get_remaining_timeout(timeout)
            if step_timeout > 0:
                success, out, err = execute_muse_task(prompt, cwd=cwd, timeout=step_timeout, tier=tier, model=model, stream=stream, readonly=True)
                if success:
                    qa_output = out
        if qa_output is None:
            step_timeout = get_remaining_timeout(timeout)
            if step_timeout > 0:
                success, out, err = execute_agy_task(prompt, cwd=cwd, timeout=step_timeout, tier=tier, model=model, stream=stream, readonly=True)
                if success:
                    qa_output = out
        if qa_output and not stream:
            print(qa_output)

        return qa_output is not None

    if intent == "review":
        print(c(f"💡 Makewand 意图识别: 独立代码审计/审查模式 '{prompt}' (只读安全隔离)", COLOR_BOLD + COLOR_CYAN))
        exit_code = run_review(cwd=cwd, stream=stream, timeout=timeout, user_prompt=prompt)
        return exit_code == EXIT_PASSED

    print(c(f"🚀 Makewand 流水线启动: '{prompt}' (自适应模型档位: {tier})", COLOR_BOLD))
    print(f"工作目录: {cwd}\n")

    # Step 1: Health inspection
    cache = get_or_update_status(force_probe=False)

    # Ensure git tracking in non-git directories
    ensure_git_worktree(cwd)

    # Step 2: Intelligent Multi-Model Routing & Implementation
    coder_candidates, reviewer_candidates, route_meta = select_optimal_engine_pair(prompt, tier=tier, cache=cache)
    primary_c = route_meta["primary_coder"]
    primary_r = route_meta["primary_reviewer"]

    print(c("🎯 [Makewand Smart Routing] 智能专精匹配与配额削峰决策:", COLOR_BOLD + COLOR_GREEN))
    if route_meta["reasons"]:
        for r_item in route_meta["reasons"]:
            print(c(f"  • {r_item}", COLOR_CYAN))
    print(c(f"  • 主力实现引擎: {primary_c.upper()} (候选梯队: {' -> '.join([c.upper() for c in coder_candidates])})", COLOR_BOLD + COLOR_BLUE))
    print(c(f"  • 独立盲审引擎: {primary_r.upper()} (候选梯队: {' -> '.join([r.upper() for r in reviewer_candidates])})\n", COLOR_BOLD + COLOR_PURPLE))

    # Record task baseline commit before dispatching implementation
    # For shadow worktrees, baseline_commit preserves forwarded active session dirty state.
    # For normal worktrees, recording current HEAD captures intermediate commits + uncommitted modifications.
    shadow_repo_root = getattr(shadow_res, "repo_root", None) if is_shadow_active else None
    if is_shadow_active:
        task_baseline = getattr(shadow_res, "baseline_commit", None)
        active_sub_baselines = getattr(shadow_res, "sub_baselines", {}) or {}
    else:
        _, cur_head, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=cwd)
        task_baseline = cur_head.strip() if cur_head else None
        active_sub_baselines = {}
        if (Path(cwd) / ".gitmodules").exists():
            sorted_subs = get_submodule_paths(cwd)
            for s_rel in sorted_subs:
                s_p = Path(cwd) / s_rel
                if s_p.exists():
                    _, s_head, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(s_p))
                    if s_head and s_head.strip():
                        active_sub_baselines[s_rel] = s_head.strip()

    print(c(f"▶ 阶段 1: 代码编写与实现 (Implementation - Tier: {tier})", COLOR_BOLD + COLOR_BLUE))
    # Retrieve past quality lessons and failure patterns
    memory_hints = ""
    try:
        from makewand.memory import format_memory_hints_for_prompt
        memory_hints = format_memory_hints_for_prompt(prompt)
        if memory_hints:
            print(c("🧠 [Makewand Memory] 匹配并注入历史避坑与工程质量准则...", COLOR_PURPLE))
    except Exception:
        pass
    coder_prompt = f"{prompt}\n{memory_hints}" if memory_hints else prompt

    coder_output = None
    coder_engine = None

    for eng in coder_candidates:
        step_timeout = get_remaining_timeout(timeout)
        if step_timeout <= 0:
            return fail_and_cleanup("❌ [Makewand Budget] 全局流水线预算已耗尽，终止任务执行。")

        print(c(f"→ 派发代码编写与实现任务给 {eng.upper()} (Tier: {tier})...", COLOR_BLUE + COLOR_BOLD))
        success, out, err = dispatch_task(eng, coder_prompt, cwd=cwd, timeout=step_timeout, tier=tier, model=model, stream=stream, readonly=False, repo_root=shadow_repo_root)
        if success:
            print(c(f"✔ {eng.upper()} 完成代码编写与修改。", COLOR_GREEN))
            coder_output = out
            coder_engine = eng
            break
        else:
            print(c(f"⚠ {eng.upper()} 遇到限制或故障: {err}", COLOR_YELLOW))
            print(c("→ 自动切换下一顺位备用引擎接管实现...", COLOR_YELLOW))

    if coder_output is None:
        return fail_and_cleanup("❌ 所有可用模型均无法完成编码任务，流水线终止。")

    if coder_output and not stream:
        print(c("【编码实现输出摘要】", COLOR_BOLD))
        print(coder_output.strip()[:500])
        print("...\n")

    # Step 3: Red-team review (Cross-model verification)
    worktree_for_diff = getattr(shadow_res, "worktree_root", cwd) if is_shadow_active else cwd
    diff_out = get_git_diff(worktree_for_diff, base_rev=task_baseline, sub_baselines=active_sub_baselines)
    if not diff_out or not diff_out.strip():
        print(c("ℹ 本次任务未产生相对于基线的有效代码改动 (git diff 为空)，无需启动红队复审与自愈流水线。", COLOR_CYAN))
        return fail_and_cleanup("❌ [Makewand Quality Gate] 任务未产生任何有效代码改动，终止交付。")

    # Run deterministic local test suite before review
    print(c("🧪 [Makewand Test Gate] 正在执行本地确定性测试验证...", COLOR_CYAN))
    test_ok, test_err = run_local_tests(cwd)
    if not test_ok:
        print(c(f"❌ [Makewand Test Gate] 发现单元测试失败：\n{test_err[:400]}", COLOR_RED + COLOR_BOLD))
    else:
        print(c("✔ [Makewand Test Gate] 本地测试套件校验通过 (或无单测需执行)。", COLOR_GREEN))

    print(c("\n▶ 阶段 2: 独立代码审计与质检 (Red-team Review - Tier: deep, 只读安全隔离)", COLOR_BOLD + COLOR_CYAN))
    diff_snippet = format_review_diff(diff_out)
    test_warning = f"\n【重要：本地测试运行失败】代码改动后本地单元测试报错如下：\n{test_err[:1500]}\n" if not test_ok else ""

    review_prompt = (
        f"工作目录为: {cwd}。请审查以下代码改动（git diff），严查潜在并发死锁、内存泄露、空指针与边界用例漏洞。{test_warning}\n"
        f"若发现严重隐患或单测报错未解决，请标注 [P1] 或 [P2] 并给出明确修复建议；若逻辑严谨无严重漏洞且单测全通，请明确回复'LGTM / 审核通过'。\n"
        f"【重要输出规范】请在回答最后一行务必输出且仅输出一行 JSON 判定：\n"
        f"MAKEWAND_VERDICT: {{\"pass\": true, \"defects\": []}} (若无严重缺陷且单测通过)\n"
        f"或 MAKEWAND_VERDICT: {{\"pass\": false, \"defects\": [\"缺陷简要描述\"]}} (若存在严重隐患或单测失败)\n"
        f"--- 代码改动 (git diff) ---\n{diff_snippet}"
    )

    actual_reviewers = [r for r in reviewer_candidates if r != coder_engine]
    if not actual_reviewers:
        fallback_r = "agy" if coder_engine != "agy" else ("codex" if cache.get("codex", {}).get("status") != "limited" else "claude")
        actual_reviewers = [fallback_r]

    review_output = None
    reviewer_engine = None
    for r_eng in actual_reviewers:
        step_timeout = get_remaining_timeout(timeout)
        if step_timeout <= 0:
            break
        print(c(f"→ 派发给 {r_eng.upper()} 进行独立跨模型红队审查 (Tier: deep, 只读隔离)...", COLOR_CYAN + COLOR_BOLD))
        res = dispatch_task(r_eng, review_prompt, cwd=cwd, timeout=step_timeout, tier="deep", stream=stream, readonly=True, repo_root=shadow_repo_root)
        success, out, err = (res[0], res[1], res[2]) if isinstance(res, (tuple, list)) and len(res) == 3 else (True, "LGTM", None)
        if success and out and out.strip():
            print(c(f"✔ {r_eng.upper()} 独立红队审查完成。", COLOR_GREEN))
            review_output = out
            reviewer_engine = r_eng
            break
        else:
            print(c(f"⚠ {r_eng.upper()} 审查未产生有效响应: {err}", COLOR_YELLOW))

    # Deterministic test gate override: if local tests failed, pass CANNOT be True under any circumstances,
    # regardless of whether the reviewer returned structured JSON or free-form text ("LGTM").
    if not test_ok:
        err_snippet = (test_err or "Unknown test failure")[:200].replace('"', '\\"')
        review_output = (
            f"MAKEWAND_VERDICT: {{\"pass\": false, \"defects\": [\"本地单元测试执行失败: {err_snippet}\"]}}\n\n"
            f"本地单测报错详情如下：\n{(test_err or '')[:2000]}\n\n"
            f"=== 原始审查意见 (已被单元测试硬防线否决) ===\n{review_output or ''}"
        )

    # Fail-Closed Quality Gate: If code has changes but review fails completely or is empty, reject delivery
    if not review_output or not review_output.strip():
        return fail_and_cleanup("❌ [Makewand Quality Gate] 独立审查服务未能完成代码审计 (UNVERIFIED)，出于安全防御原则阻断合并，拒绝交付。")

    # Step 4: Auto-Fix Loop
    if auto_fix and review_output and has_critical_defects(review_output):
        current_fix_iter = 0
        while current_fix_iter < max_fix and has_critical_defects(review_output):
            current_fix_iter += 1
            step_timeout = get_remaining_timeout(timeout)
            if step_timeout <= 0:
                print(c("❌ [Makewand Budget] 全局流水线预算耗尽，终止 Auto-Fix 自愈轮次。", COLOR_RED + COLOR_BOLD))
                break

            print(c(f"\n⚡ [Makewand Auto-Fix] 独立审计检测到高/中危缺陷，自动启动第 {current_fix_iter}/{max_fix} 轮修复闭环...", COLOR_YELLOW + COLOR_BOLD))

            fix_prompt = (
                f"目标工作目录绝对路径: {cwd}\n"
                f"独立红队审查针对上一轮提交的代码发现了以下真实缺陷，请针对性修复所有漏洞并确保单测全通：\n"
                f"{review_output}\n\n"
                f"请直接落盘修改对应代码文件。"
            )

            # Coder fixes
            fixed = False
            actual_fix_engine = None
            step_timeout = get_remaining_timeout(timeout)
            if coder_engine and step_timeout > 0:
                print(c(f"→ 由主力编码引擎 {coder_engine.upper()} 执行缺陷修复...", COLOR_YELLOW))
                ok, _, _ = dispatch_task(coder_engine, fix_prompt, cwd=cwd, timeout=step_timeout, tier=tier, stream=stream, readonly=False, repo_root=shadow_repo_root)
                if ok:
                    fixed = True
                    actual_fix_engine = coder_engine

            if not fixed:
                for alt_c in coder_candidates:
                    if alt_c != coder_engine:
                        step_timeout = get_remaining_timeout(timeout)
                        if step_timeout <= 0:
                            break
                        print(c(f"→ 自动切换备用引擎 {alt_c.upper()} 执行修复...", COLOR_YELLOW))
                        ok, _, _ = dispatch_task(alt_c, fix_prompt, cwd=cwd, timeout=step_timeout, tier=tier, stream=stream, readonly=False, repo_root=shadow_repo_root)
                        if ok:
                            fixed = True
                            actual_fix_engine = alt_c
                            break

            if not fixed:
                print(c("⚠ 缺陷自动修复未产生有效更新，维持当前审查结论。", COLOR_YELLOW))
                break

            # Re-run deterministic local tests after fix
            test_ok, test_err = run_local_tests(cwd)
            if not test_ok:
                print(c(f"❌ [Makewand Test Gate] 修复后本地单元测试仍未通过：\n{test_err[:400]}", COLOR_RED))
            else:
                print(c("✔ [Makewand Test Gate] 修复后本地单元测试执行全通！", COLOR_GREEN))

            step_timeout = get_remaining_timeout(timeout)
            if step_timeout <= 0:
                print(c("❌ [Makewand Budget] 预算已耗尽，终止复审。", COLOR_RED + COLOR_BOLD))
                break

            print(c(f"▶ [Makewand Auto-Fix] 修复已落盘，重新发起第 {current_fix_iter} 轮红队复审 (只读安全隔离)...", COLOR_CYAN))
            new_diff = get_git_diff(worktree_for_diff, base_rev=task_baseline, sub_baselines=active_sub_baselines)
            new_diff_snippet = format_review_diff(new_diff)
            re_test_warning = f"\n【重要：本地测试仍未通过】报错如下：\n{test_err[:1500]}\n" if not test_ok else ""

            re_review_prompt = (
                f"工作目录为: {cwd}。经过上一轮缺陷修复后，请复审以下代码改动，检查上述缺陷是否已彻底解决，是否存在新隐患。{re_test_warning}\n"
                f"若发现严重隐患或单测报错未解决，请标注 [P1] 或 [P2] 并给出明确修复建议；若逻辑严谨无严重漏洞且单测全通，请明确回复'LGTM / 审核通过'。\n"
                f"【重要输出规范】请在回答最后一行务必输出且仅输出一行 JSON 判定：\n"
                f"MAKEWAND_VERDICT: {{\"pass\": true, \"defects\": []}} (若已修复且无严重缺陷且测试通过)\n"
                f"或 MAKEWAND_VERDICT: {{\"pass\": false, \"defects\": [\"新缺陷描述\"]}} (若仍存在严重隐患或单测失败)\n"
                f"--- 最新代码改动 (git diff) ---\n{new_diff_snippet}"
            )

            # Strictly exclude actual_fix_engine from reviewers to preserve cross-model independence
            candidate_re_reviewers = [r for r in actual_reviewers if r != actual_fix_engine]
            if not candidate_re_reviewers:
                healthy_alts = [
                    e for e in ["codex", "claude", "agy", "muse"]
                    if e != actual_fix_engine and cache.get(e, {}).get("status") not in ["limited", "needs_auth", "missing"]
                ]
                candidate_re_reviewers = healthy_alts if healthy_alts else [e for e in ["codex", "claude", "agy", "muse"] if e != actual_fix_engine]

            re_output = None
            for alt_r in candidate_re_reviewers:
                step_timeout = get_remaining_timeout(timeout)
                if step_timeout <= 0:
                    break
                res = dispatch_task(alt_r, re_review_prompt, cwd=cwd, timeout=step_timeout, tier="deep", stream=stream, readonly=True, repo_root=shadow_repo_root)
                ok, out, _ = (res[0], res[1], res[2]) if isinstance(res, (tuple, list)) and len(res) == 3 else (True, "LGTM", None)
                if ok and out and out.strip():
                    re_output = out
                    break

            # Deterministic test gate override: if local tests failed, pass CANNOT be True under any circumstances
            if not test_ok:
                err_snippet = (test_err or "Unknown test failure")[:200].replace('"', '\\"')
                re_output = (
                    f"MAKEWAND_VERDICT: {{\"pass\": false, \"defects\": [\"本地单元测试执行失败: {err_snippet}\"]}}\n\n"
                    f"本地单测报错详情如下：\n{(test_err or '')[:2000]}\n\n"
                    f"=== 原始审查意见 (已被单元测试硬防线否决) ===\n{re_output or ''}"
                )

            if re_output:
                # Capture the flagged defects from the prior round BEFORE overwriting review_output
                last_verdict = extract_verdict_json(review_output)
                last_defects = last_verdict.get("defects", []) if last_verdict else []
                review_output = re_output
                if not has_critical_defects(re_output):
                    print(c("✔ [Makewand Auto-Fix] 经过自动修复，代码已通过红队复审！", COLOR_GREEN + COLOR_BOLD))
                    try:
                        from makewand.memory import record_autofix_lesson
                        defect_desc = "; ".join(last_defects[:3]) if last_defects else prompt[:120]
                        tokens = [w for w in re.findall(r"\b[a-zA-Z0-9_-]{4,}\b", prompt.lower()) if w not in ["this", "that", "with", "from", "have", "code", "file", "make", "task"]]
                        if not tokens:
                            tokens = [Path(cwd).name.lower()]
                        record_autofix_lesson(
                            keywords=tokens[:5],
                            issue=f"Defect flagged: {defect_desc}",
                            lesson=f"Remediated successfully in auto-fix iteration {current_fix_iter}."
                        )
                        print(c("🧠 [Makewand Memory] 已自动固化避坑修复经验到模式记忆库。", COLOR_PURPLE))
                    except Exception:
                        pass
                    break

    print(c("\n============================================================", COLOR_BOLD))
    print(c("                   Makewand 联合调度完成报告", COLOR_BOLD + COLOR_GREEN))
    print(c("============================================================\n", COLOR_BOLD))
    if review_output and not stream:
        print(c("【最终审计意见与质量评估】", COLOR_BOLD))
        print(review_output.strip()[:1000])
        print("...\n")

    # Non-bypassable Quality Gate: Local deterministic unit tests MUST pass
    if not test_ok:
        return fail_and_cleanup(
            f"❌ [Makewand Quality Gate] 本地确定性单元测试未通过 (Tests Failing)，阻断交付。\n"
            f"报错详情：\n{(test_err or '')[:1000]}"
        )

    if not is_review_passed(review_output):
        return fail_and_cleanup("❌ [Makewand Quality Gate] 代码未能通过独立红队审查 (未获批准或存在缺陷)，拒绝交付。")

    if is_shadow_active:
        if shadow_branch:
            delivered_branch = shadow_branch
            has_baseline_conflict = False
            try:
                baseline_commit = getattr(shadow_res, "baseline_commit", None)
                repo_head = getattr(shadow_res, "repo_head", None)
                repo_root = getattr(shadow_res, "repo_root", None)
                sub_baselines = getattr(shadow_res, "sub_baselines", {}) or {}
                worktree_root = getattr(shadow_res, "worktree_root", shadow_worktree_dir)

                art_ts = datetime.now().strftime("%Y%m%d_%H%M%S")
                art_id = uuid.uuid4().hex[:6]
                artifacts_dir = Path("/tmp/makewand-artifacts") / f"delivery_{art_ts}_{art_id}"
                artifacts_dir.mkdir(parents=True, exist_ok=True)
                patch_file = artifacts_dir / "makewand_delivery.patch"
                sub_patches = []

                # 0. Commit any changes inside submodules first so gitlinks can be staged
                # Crucial: Use get_submodule_paths to correctly handle paths with spaces and descending depth
                if (Path(worktree_root) / ".gitmodules").exists():
                    sorted_subs = get_submodule_paths(worktree_root)
                    for sub_rel in sorted_subs:
                        dst_sub = Path(worktree_root) / sub_rel
                        if dst_sub.exists():
                            _, s_out, _ = run_git_cmd(["git", "status", "--porcelain"], cwd=str(dst_sub))
                            if s_out.strip():
                                a_sub_code, _, a_sub_err = run_git_cmd(["git", "add", "-A"], cwd=str(dst_sub))
                                if a_sub_code != 0:
                                    return fail_and_cleanup(f"❌ [Makewand Quality Gate] 子模块 {sub_rel} 暂存失败 ({a_sub_err})，拒绝交付。")
                                c_sub_code, _, c_sub_err = run_git_cmd([
                                    "git",
                                    "-c", "user.name=Makewand",
                                    "-c", "user.email=makewand@local",
                                    "commit", "--no-verify", "-m", f"makewand: submodule {prompt[:50]}"
                                ], cwd=str(dst_sub))
                                if c_sub_code != 0:
                                    return fail_and_cleanup(f"❌ [Makewand Quality Gate] 子模块 {sub_rel} 提交失败 ({c_sub_err})，拒绝交付。")

                            # Generate binary-safe submodule patch if sub_base is known
                            sub_base = sub_baselines.get(sub_rel)
                            if sub_base:
                                p_sub_code, p_sub_b, p_sub_err = run_git_cmd(["git", "diff", "--binary", "--full-index", sub_base, "HEAD"], cwd=str(dst_sub), binary=True)
                                if p_sub_code != 0:
                                    return fail_and_cleanup(f"❌ [Makewand Quality Gate] 子模块 {sub_rel} 交付补丁导出失败 ({p_sub_err})，阻断交付。")
                                if p_sub_b and p_sub_b.strip():
                                    sub_hash = hashlib.sha256(sub_rel.encode("utf-8")).hexdigest()[:8]
                                    sub_patch_p = artifacts_dir / f"sub_{len(sub_patches):03d}_{sub_hash}.patch"
                                    if sub_patch_p.exists():
                                        return fail_and_cleanup(f"❌ [Makewand Quality Gate] 子模块 {sub_rel} 补丁文件已存在冲突，阻断交付。")
                                    try:
                                        sub_patch_p.write_bytes(p_sub_b)
                                    except Exception as swe:
                                        return fail_and_cleanup(f"❌ [Makewand Quality Gate] 子模块 {sub_rel} 补丁写入磁盘失败 ({swe})，阻断交付。")
                                    sub_patches.append({
                                        "rel_path": sub_rel,
                                        "patch_file": str(sub_patch_p),
                                        "sha256": hashlib.sha256(p_sub_b).hexdigest()
                                    })

                            # Sync submodule commit object to src_sub so host can inspect/merge
                            if repo_root:
                                src_sub = Path(repo_root) / sub_rel
                                if src_sub.exists():
                                    push_code, _, push_err = run_git_cmd(["git", "push", str(src_sub.resolve()), f"HEAD:refs/heads/{delivered_branch}"], cwd=str(dst_sub))
                                    if push_code != 0:
                                        return fail_and_cleanup(f"❌ [Makewand Quality Gate] 子模块 {sub_rel} 分支推送同步失败 ({push_err})，阻断交付。")

                # 1. Stage changes and verify staging success
                add_code, _, add_err = run_git_cmd(["git", "add", "-A"], cwd=worktree_root)
                if add_code != 0:
                    return fail_and_cleanup(f"❌ [Makewand Quality Gate] 影子分支代码暂存失败 ({add_err})，拒绝交付。")

                # 2. Check whether uncommitted changes exist in working tree to commit
                diff_staged_code, staged_names, _ = run_git_cmd(["git", "diff", "--cached", "--name-only"], cwd=worktree_root)
                if staged_names.strip():
                    c_code, _, c_err = run_git_cmd([
                        "git",
                        "-c", "user.name=Makewand",
                        "-c", "user.email=makewand@local",
                        "commit", "--no-verify", "-m", f"makewand: implement {prompt[:80]}"
                    ], cwd=worktree_root)
                    if c_code != 0:
                        return fail_and_cleanup(f"❌ [Makewand Quality Gate] 影子分支代码提交失败 ({c_err})，拒绝交付。")

                # Verify that working tree is 100% clean and matches the committed state
                _, clean_check, _ = run_git_cmd(["git", "status", "--porcelain"], cwd=worktree_root)
                if clean_check and clean_check.strip():
                    return fail_and_cleanup("❌ [Makewand Quality Gate] 交付提交后工作区残留未审查改动，拒绝交付未验证内容。")

                # 3. Verify that the task produced actual net changes compared to baseline
                impl_commit = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=worktree_root)[1].strip()
                if baseline_commit and impl_commit == baseline_commit:
                    return fail_and_cleanup("❌ [Makewand Quality Gate] 影子分支没有检测到任何已落盘的代码修改，拒绝交付空提交。")

                # Synchronize main repository delivery branch to repo_root so host can directly inspect/merge
                if repo_root and delivered_branch:
                    push_code, _, push_err = run_git_cmd(
                        ["git", "push", str(repo_root), f"HEAD:refs/heads/{delivered_branch}"],
                        cwd=worktree_root
                    )
                    if push_code != 0:
                        return fail_and_cleanup(f"❌ [Makewand Quality Gate] 主仓库交付分支同步失败 ({push_err})，阻断交付。")

                # 4. Generate binary-safe, full-index patch covering the entire task range (baseline_commit -> HEAD)
                # Saved outside the repository to prevent artifact leakage or uncommitted file pollution
                if baseline_commit:
                    p_code, p_diff_b, p_err = run_git_cmd([
                        "git", "diff", "--binary", "--full-index", baseline_commit, "HEAD"
                    ], cwd=worktree_root, binary=True)
                    if p_code != 0 or not p_diff_b or len(p_diff_b.strip()) == 0:
                        return fail_and_cleanup(f"❌ [Makewand Quality Gate] 交付补丁导出失败或内容为空 (code: {p_code}, err: {p_err})，阻断交付。")
                    try:
                        patch_file.write_bytes(p_diff_b)
                    except Exception as we:
                        return fail_and_cleanup(f"❌ [Makewand Quality Gate] 交付补丁写入磁盘失败 ({we})，阻断交付。")

                    if not patch_file.exists() or patch_file.stat().st_size == 0:
                        return fail_and_cleanup("❌ [Makewand Quality Gate] 交付补丁文件校验失败 (文件不存在或大小为0)，阻断交付。")

                # 5. Post-delivery integrity check: shadow worktree must be 100% clean
                _, dirty_check, _ = run_git_cmd(["git", "status", "--porcelain"], cwd=worktree_root)
                if dirty_check.strip():
                    return fail_and_cleanup(f"❌ [Makewand Quality Gate] 影子工作区交付后存在未受控改动或脏文件 ({dirty_check.strip()[:120]})，阻断交付。")

                # Tree-based dirty baseline detection: compare tree hashes to avoid false conflict on clean repo
                tree_b_code, tree_b, _ = run_git_cmd(["git", "rev-parse", f"{baseline_commit}^{{tree}}"], cwd=worktree_root) if baseline_commit else (1, "", "")
                tree_h_code, tree_h, _ = run_git_cmd(["git", "rev-parse", f"{repo_head}^{{tree}}"], cwd=worktree_root) if repo_head else (1, "", "")
                has_baseline_conflict = bool(tree_b_code == 0 and tree_h_code == 0 and tree_b.strip() != tree_h.strip())

                # Generate apply_delivery.sh and delivery_manifest.json
                repo_apply_root = str(repo_root) if repo_root else worktree_root
                manifest_data = {
                    "timestamp": art_ts,
                    "delivered_branch": delivered_branch,
                    "repo_root": repo_apply_root,
                    "baseline_commit": baseline_commit,
                    "repo_head": repo_head,
                    "has_baseline_conflict": has_baseline_conflict,
                    "main_patch": str(patch_file),
                    "submodule_patches": sub_patches
                }
                manifest_file = artifacts_dir / "delivery_manifest.json"
                manifest_file.write_text(json.dumps(manifest_data, indent=2, ensure_ascii=False), encoding="utf-8")

                script_lines = [
                    "#!/usr/bin/env bash",
                    "# Auto-generated by Makewand Quality Gate Delivery (Transactional)",
                    "set -euo pipefail",
                    f'REPO_ROOT={shlex.quote(repo_apply_root)}',
                    'echo "============================================================"',
                    'echo "      📦 [Makewand Delivery Applier] 开始应用代码改动"',
                    'echo "============================================================"',
                    "",
                    "# 1. Pre-flight verification (atomic test without modifying files)",
                    'echo "→ [阶段 1/2] 补丁完整性与冲突预检 (Pre-flight check)..."'
                ]
                main_patch_esc = shlex.quote(str(patch_file))
                main_sha = hashlib.sha256(p_diff_b).hexdigest()
                script_lines.append(f'MAIN_PATCH={main_patch_esc}')
                script_lines.append(f'MAIN_SHA="{main_sha}"')
                script_lines.append('if [ "$(sha256sum "$MAIN_PATCH" | cut -d" " -f1)" != "$MAIN_SHA" ]; then echo "❌ 主仓库补丁校验和不匹配，拒绝应用" >&2; exit 1; fi')

                for idx, sp in enumerate(sub_patches):
                    sub_p_esc = shlex.quote(sp["patch_file"])
                    sub_r_esc = shlex.quote(sp["rel_path"])
                    sub_sha = shlex.quote(sp.get("sha256", ""))
                    script_lines.append(f'SUB_REL_{idx}={sub_r_esc}')
                    script_lines.append(f'SUB_PATCH_{idx}={sub_p_esc}')
                    script_lines.append(f'SUB_SHA_{idx}={sub_sha}')
                    if sp.get("sha256"):
                        script_lines.append(f'if [ "$(sha256sum "$SUB_PATCH_{idx}" | cut -d" " -f1)" != "$SUB_SHA_{idx}" ]; then echo "❌ 子模块补丁校验和不匹配 ($SUB_REL_{idx})，拒绝应用" >&2; exit 1; fi')
                    script_lines.append(f'git -C "$REPO_ROOT/$SUB_REL_{idx}" apply --check --binary "$SUB_PATCH_{idx}"')

                script_lines.append('git -C "$REPO_ROOT" apply --check --binary "$MAIN_PATCH"')
                script_lines.append('echo "✔ 预检通过，未检测到补丁冲突。"')
                script_lines.append("")
                script_lines.append("# 2. Transactional application with auto-rollback on error")
                script_lines.append('echo "→ [阶段 2/2] 执行事务性应用..."')
                script_lines.append("APPLIED_SUB_INDICES=()")
                script_lines.append("MAIN_APPLIED=0")
                script_lines.append("")
                script_lines.append("rollback() {")
                script_lines.append("    set +e")
                script_lines.append('    echo "❌ 补丁应用遭遇错误，触发原子回滚..." >&2')
                script_lines.append("    ROLLBACK_FAILED=0")
                script_lines.append('    if [ "$MAIN_APPLIED" -eq 1 ]; then')
                script_lines.append('        echo "  → 正在回滚主仓库改动..." >&2')
                script_lines.append('        if ! git -C "$REPO_ROOT" apply --reverse --binary "$MAIN_PATCH"; then')
                script_lines.append('            echo "  ❌ 主仓库回滚失败！" >&2')
                script_lines.append('            ROLLBACK_FAILED=1')
                script_lines.append('        fi')
                script_lines.append('    fi')
                script_lines.append('    for (( i=${#APPLIED_SUB_INDICES[@]}-1 ; i>=0 ; i-- )) ; do')
                script_lines.append('        sub_idx="${APPLIED_SUB_INDICES[i]}"')
                script_lines.append('        eval "sub_rel=\\$SUB_REL_${sub_idx}"')
                script_lines.append('        eval "sub_patch=\\$SUB_PATCH_${sub_idx}"')
                script_lines.append('        echo "  → 正在回滚子模块改动: $sub_rel..." >&2')
                script_lines.append('        if ! git -C "$REPO_ROOT/$sub_rel" apply --reverse --binary "$sub_patch"; then')
                script_lines.append('            echo "  ❌ 子模块 ($sub_rel) 回滚失败！" >&2')
                script_lines.append('            ROLLBACK_FAILED=1')
                script_lines.append('        fi')
                script_lines.append('    done')
                script_lines.append('    if [ "$ROLLBACK_FAILED" -eq 0 ]; then')
                script_lines.append('        echo "✔ 目标仓库已安全回滚至未修改状态。" >&2')
                script_lines.append('    else')
                script_lines.append('        echo "⚠️ 回滚过程中遇到错误，目标仓库存在未完全回滚的残留修改！请执行 git status 检查。" >&2')
                script_lines.append('    fi')
                script_lines.append('    exit 1')
                script_lines.append("}")
                script_lines.append("trap rollback ERR")
                script_lines.append("")

                for idx, sp in enumerate(sub_patches):
                    script_lines.append(f'printf "→ 应用子模块改动: %s...\\n" "$SUB_REL_{idx}"')
                    script_lines.append(f'git -C "$REPO_ROOT/$SUB_REL_{idx}" apply --binary "$SUB_PATCH_{idx}"')
                    script_lines.append(f'APPLIED_SUB_INDICES+=({idx})')

                script_lines.append('printf "→ 应用主仓库改动...\\n"')
                script_lines.append('git -C "$REPO_ROOT" apply --binary "$MAIN_PATCH"')
                script_lines.append('MAIN_APPLIED=1')
                script_lines.append('trap - ERR')
                script_lines.append('printf "✔ 所有补丁已原子应用成功，目标仓库改动就绪。\\n"')

                apply_script_file = artifacts_dir / "apply_delivery.sh"
                apply_script_file.write_text("\n".join(script_lines) + "\n", encoding="utf-8")
                os.chmod(apply_script_file, 0o755)

            except Exception as e:
                return fail_and_cleanup(f"❌ [Makewand Quality Gate] 影子分支交付发生异常 ({e})，拒绝交付。")

            repo_apply_root = str(repo_root) if repo_root else worktree_root
            apply_root_esc = shlex.quote(repo_apply_root)
            patch_file_esc = shlex.quote(str(patch_file))
            apply_script_esc = shlex.quote(str(apply_script_file))

            print(c("\n============================================================", COLOR_BOLD))
            print(c("       🛡️ [Makewand Multi-Session Guard] 隔离交付报告", COLOR_BOLD + COLOR_GREEN))
            print(c("============================================================\n", COLOR_BOLD))
            print(c("✔ 任务在独立工作树完成，零污染当前会话工作区！", COLOR_GREEN + COLOR_BOLD))
            print(f"  工作树路径: {c(worktree_root, COLOR_CYAN)}")
            if delivered_branch:
                print(f"  交付分支: {c(delivered_branch, COLOR_YELLOW)}")
                if has_baseline_conflict:
                    print(c("  ⚠ 注意：本任务基于当前会话未提交快照开发并完成独立审查。交付分支保留该上下文以保证可运行性。", COLOR_YELLOW))
                    print(f"  独立补丁文件 (仅包含本轮任务改动，二进制安全，零仓库污染): {c(str(patch_file), COLOR_CYAN)}")
                    if sub_patches:
                        print(f"  子模块补丁数量: {len(sub_patches)} (清单存放在 {c(str(manifest_file), COLOR_CYAN)})")
                    print(f"  一键应用交付补丁 (推荐): {c(apply_script_esc, COLOR_GREEN + COLOR_BOLD)}")
                    print(f"  手动应用: git -C {apply_root_esc} apply {patch_file_esc}\n")
                else:
                    print(f"  宿主机仓库可直接合并独立审查通过的改动: git -C {apply_root_esc} merge {delivered_branch}")
                    print(f"  独立补丁备用存档: {c(str(patch_file), COLOR_CYAN)}")
                    print(f"  一键应用脚本备用: {c(apply_script_esc, COLOR_CYAN)}\n")
            else:
                print("  已在独立隔离副本保存所有产物，原工作区未受任何修改污染。\n")

    print(c("✔ 任务全链路自适应闭环完成并通过红队审查。", COLOR_GREEN + COLOR_BOLD))
    return True

def run_review(cwd: Optional[str] = None, stream: bool = False, timeout: int = 300, user_prompt: Optional[str] = None, output_json: bool = False) -> int:
    if not cwd:
        cwd = os.getcwd()
    if not output_json:
        print(c("🔍 Makewand 代码审计工具", COLOR_BOLD + COLOR_CYAN))
    diff_out = get_git_diff(cwd)
    if not diff_out.strip():
        if output_json:
            print(json.dumps({
                "pass": True,
                "exit_code": EXIT_PASSED,
                "engine": None,
                "defects": [],
                "message": "当前工作区没有检测到未提交的改动 (git diff 为空)"
            }, ensure_ascii=False, indent=2))
        else:
            print("当前工作区没有检测到未提交的改动 (git diff 为空)。")
        return EXIT_PASSED

    cache = get_or_update_status()
    x_status = cache.get("codex", {}).get("status")

    focus = f" 特别关注要求: {user_prompt}。" if user_prompt else ""
    prompt = (
        f"工作目录为: {cwd}。请详细审查当前仓库的修改（git diff），{focus}指出潜在隐患并给出修复建议。\n"
        f"【重要输出规范】请在回答最后一行务必输出且仅输出一行 JSON 判定：\n"
        f"MAKEWAND_VERDICT: {{\"pass\": true, \"defects\": []}} (若无严重缺陷)\n"
        f"或 MAKEWAND_VERDICT: {{\"pass\": false, \"defects\": [\"缺陷描述\"]}} (若存在严重隐患)\n"
        f"--- 代码改动 (git diff) ---\n{diff_out[:6000]}"
    )

    review_res = None
    reviewer_engine = None
    if x_status != "limited":
        if not output_json:
            print(c("派发给 Codex CLI 进行红队审计 (gpt-6-astra, 只读隔离)...", COLOR_CYAN))
        success, out, err = execute_codex_task(prompt, cwd=cwd, tier="deep", stream=stream and not output_json, timeout=timeout, readonly=True)
        if success and out and out.strip():
            review_res = out
            reviewer_engine = "codex"
        elif not output_json:
            print(c(f"Codex 不可用 ({err or '输出内容为空'})，转交 Antigravity...", COLOR_YELLOW))

    if review_res is None:
        if not output_json:
            print(c("由 Antigravity 进行红队审计 (只读隔离)...", COLOR_GREEN))
        success, out, err = execute_agy_task(prompt, cwd=cwd, tier="deep", stream=stream and not output_json, timeout=timeout, readonly=True)
        if success and out and out.strip():
            review_res = out
            reviewer_engine = "agy"
        elif not output_json:
            print(c(f"审查失败: {err or '输出内容为空'}", COLOR_RED))

    if not review_res:
        if output_json:
            print(json.dumps({
                "pass": False,
                "exit_code": EXIT_UNVERIFIED,
                "engine": None,
                "defects": ["独立审查服务未能产生有效输出 (UNVERIFIED)"],
                "error": "Independent review engine failed to produce valid output"
            }, ensure_ascii=False, indent=2))
        else:
            print(c("❌ [Makewand Quality Gate] 独立审查服务未能产生有效输出 (UNVERIFIED)，拒绝交付。", COLOR_RED + COLOR_BOLD))
        return EXIT_UNVERIFIED

    passed = is_review_passed(review_res)
    exit_code = EXIT_PASSED if passed else EXIT_FAILED

    if output_json:
        v_dict = extract_review_verdict_dict(review_res)
        v_dict["exit_code"] = exit_code
        v_dict["engine"] = reviewer_engine
        v_dict["raw_summary"] = review_res.strip()
        print(json.dumps(v_dict, ensure_ascii=False, indent=2))
        return exit_code

    if not stream:
        print(review_res)

    if passed:
        print(c("✔ 代码审计通过，未发现严重缺陷 (PASSED)。", COLOR_GREEN + COLOR_BOLD))
        return EXIT_PASSED
    else:
        print(c("❌ 代码审计检测到严重隐患，未达合并标准 (FAILED)。", COLOR_RED + COLOR_BOLD))
        return EXIT_FAILED

def run_race(prompt: str, cwd: Optional[str] = None, timeout: int = 300):
    check_load_backpressure()
    if not cwd:
        cwd = os.getcwd()
    print(c(f"🏁 Makewand 双模型并发竞速模式启动: '{prompt}'", COLOR_BOLD + COLOR_CYAN))

    ensure_git_worktree(cwd)

    cache = get_or_update_status()
    c_ok = cache.get("claude", {}).get("status") == "healthy"
    x_ok = cache.get("codex", {}).get("status") == "healthy"
    m_ok = cache.get("muse", {}).get("status") == "healthy"

    ensure_config_dir()
    race_id = f"rc_{uuid.uuid4().hex[:8]}"
    session_dir = CANDIDATES_DIR / race_id
    wt_a = session_dir / "agent_a"
    wt_b = session_dir / "agent_b"

    saved_successfully = False
    try:
        wt_a.mkdir(parents=True, exist_ok=True)
        wt_b.mkdir(parents=True, exist_ok=True)

        clone_isolated_worktree(cwd, wt_a)
        clone_isolated_worktree(cwd, wt_b)

        # Record baseline commit of host workspace
        code, b_commit, _ = run_git_cmd("git rev-parse HEAD", cwd=cwd)
        # Record baseline commit of candidate worktrees
        _, base_a_commit, _ = run_git_cmd("git rev-parse HEAD", cwd=str(wt_a))
        _, base_b_commit, _ = run_git_cmd("git rev-parse HEAD", cwd=str(wt_b))

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
                ok, out, err = execute_codex_task(full_p, cwd=str(wt_a), timeout=timeout, repo_root=cwd)
            elif m_ok:
                ok, out, err = execute_muse_task(full_p, cwd=str(wt_a), timeout=timeout, repo_root=cwd)
            else:
                ok, out, err = execute_agy_task(full_p, cwd=str(wt_a), timeout=timeout, tier="fast", repo_root=cwd)
            duration = round(time.time() - start, 2)
            return name_a, ok, out, duration, wt_a

        def run_agent_b():
            start = time.time()
            full_p = f"工作目录绝对路径: {wt_b}\n请在该目录下完成代码编写并直接落盘：\n{prompt}"
            if c_ok:
                ok, out, err = execute_claude_task(full_p, cwd=str(wt_b), timeout=timeout, repo_root=cwd)
            else:
                ok, out, err = execute_agy_task(full_p, cwd=str(wt_b), timeout=timeout, tier="deep", repo_root=cwd)
            duration = round(time.time() - start, 2)
            return name_b, ok, out, duration, wt_b

        try:
            high_load = os.getloadavg()[0] > 24.0
        except Exception:
            high_load = False

        if high_load:
            print(c("⏳ [Makewand Backpressure] 主机负载偏高，动态降为串行分时执行以避免竞争系统资源...", COLOR_YELLOW))
            res_a = run_agent_a()
            res_b = run_agent_b()
        else:
            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
                f_a = executor.submit(run_agent_a)
                f_b = executor.submit(run_agent_b)
                res_a = f_a.result()
                res_b = f_b.result()

        diff_a = get_git_diff(str(wt_a), base_rev=base_a_commit.strip() if base_a_commit else None)
        diff_b = get_git_diff(str(wt_b), base_rev=base_b_commit.strip() if base_b_commit else None)

        print(c("\n============================================================", COLOR_BOLD))
        print(c("                Makewand 竞速赛况与性能指标", COLOR_BOLD + COLOR_GREEN))
        print(c("============================================================\n", COLOR_BOLD))
        print(f"选手 A [{res_a[0]}]: 状态={'✔ 成功' if res_a[1] else '❌ 失败'}, 耗时={res_a[3]}s, 代码Diff大小={len(diff_a)} 字节")
        print(f"选手 B [{res_b[0]}]: 状态={'✔ 成功' if res_b[1] else '❌ 失败'}, 耗时={res_b[3]}s, 代码Diff大小={len(diff_b)} 字节\n")

        # Chief Referee evaluation with Antigravity (strictly read-only)
        judge_prompt = (
            f"请作为资深软件架构裁判，客观对比以下两位选手对同一任务的实现方案，指出各自优势与缺陷，并评定胜出者：\n\n"
            f"--- 原始任务 ---\n{prompt}\n\n"
            f"--- 选手 A ({res_a[0]}) 的改动 ---\n{diff_a[:3000] if diff_a else '无 diff'}\n\n"
            f"--- 选手 B ({res_b[0]}) 的改动 ---\n{diff_b[:3000] if diff_b else '无 diff'}\n\n"
            f"请给出：1. 方案对比分析 2. 最终裁决结果及推荐采纳理由。"
        )
        print(c("由 Antigravity (Google AI Pro) 担任主裁判进行方案综合评估 (只读安全隔离)...", COLOR_GREEN + COLOR_BOLD))
        ok, judge_report, _ = execute_agy_task(judge_prompt, cwd=cwd, tier="deep", timeout=timeout, readonly=True)
        if judge_report:
            print(c("\n【裁判裁决报告】", COLOR_BOLD))
            print(judge_report.strip())

        # Determine winner
        winner = None
        if ok and judge_report:
            m_a = re.search(r"(?:推荐(?:采纳)?|采纳|胜出者|获胜|胜者|winner|prefer|recommend)\s*[:：]?\s*(?:选手|agent|candidate|方案)?\s*[Aa]", judge_report, re.IGNORECASE)
            m_b = re.search(r"(?:推荐(?:采纳)?|采纳|胜出者|获胜|胜者|winner|prefer|recommend)\s*[:：]?\s*(?:选手|agent|candidate|方案)?\s*[Bb]", judge_report, re.IGNORECASE)
            if m_a and not m_b:
                if res_a[1]:
                    winner = "A"
            elif m_b and not m_a:
                if res_b[1]:
                    winner = "B"
            elif not m_a and not m_b:
                if res_a[1] and not res_b[1]:
                    winner = "A"
                elif res_b[1] and not res_a[1]:
                    winner = "B"
        else:
            if res_a[1] and not res_b[1]:
                winner = "A"
            elif res_b[1] and not res_a[1]:
                winner = "B"

        CandidateManager.save_race(
            race_id=race_id,
            prompt=prompt,
            base_cwd=cwd,
            baseline_commit=b_commit.strip() if (code == 0 and b_commit) else "",
            agent_a={
                "model": res_a[0],
                "path": str(wt_a),
                "duration": res_a[3],
                "success": res_a[1],
                "diff": diff_a,
                "baseline_commit": base_a_commit.strip() if base_a_commit else "",
            },
            agent_b={
                "model": res_b[0],
                "path": str(wt_b),
                "duration": res_b[3],
                "success": res_b[1],
                "diff": diff_b,
                "baseline_commit": base_b_commit.strip() if base_b_commit else "",
            },
            judge_report=judge_report or "",
            winner=winner,
        )
        saved_successfully = True

        print(c(f"\n💾 候选工作区已妥善封存 (Race ID: {race_id})", COLOR_GREEN + COLOR_BOLD))
        if winner:
            print(c(f"  ★ 主裁推荐胜出方案: 选手 {winner}", COLOR_GREEN + COLOR_BOLD))
            print(f"  • 审查改动差异: makewand inspect {race_id} --candidate {winner}")
            print(f"  • 安全应用方案: makewand apply {race_id} --candidate {winner}")
        else:
            print(c("  ⚠ 未决出唯一胜出方案，请审查后显式指定方案:", COLOR_YELLOW))
            print(f"  • 审查方案差异: makewand inspect {race_id} --candidate A|B")
            print(f"  • 安全应用方案: makewand apply {race_id} --candidate A|B")
        print(f"  • 丢弃废弃候选: makewand discard {race_id}\n")

        if not res_a[1] and not res_b[1]:
            print(c("❌ 两位选手均未能成功完成任务。", COLOR_RED + COLOR_BOLD))
            return EXIT_FAILED
        if not ok and winner is None:
            return EXIT_UNVERIFIED
        return EXIT_PASSED
    finally:
        if not saved_successfully and session_dir.exists():
            import shutil
            shutil.rmtree(session_dir, ignore_errors=True)
