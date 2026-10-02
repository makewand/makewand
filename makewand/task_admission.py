"""
Makewand Task Admission & Intent Classification:
Task tiering, prompt intent routing, conversational detection, and backpressure control.
"""

import os
import re
from typing import List

from makewand.config import (
    c,
    COLOR_BOLD,
    COLOR_GREEN,
    COLOR_YELLOW,
    COLOR_BLUE,
    COLOR_CYAN,
    COLOR_PURPLE,
    COLOR_RED,
    COLOR_RESET,
)


def detect_task_tier(prompt: str) -> str:
    p_lower = prompt.lower()

    # 1. Fast inspect queries (probe, typo, format, simple inspect)
    fast_phrases = [
        "快速查看", "快速探测", "快速检查", "快速看下", "随便看看",
        "查看一下", "简单探测", "拼写检查", "probe", "quick check",
        "just check", "typo", "format only", "format json", "format code",
        "print version", "help info"
    ]
    if any(k in p_lower for k in fast_phrases):
        return "fast"

    # 2. Deep battle-tested / architectural / algorithmic domains
    deep_keywords = [
        "审查", "审计", "死锁", "并发", "竞态", "内存泄露", "内存泄漏", "漏洞",
        "全局架构", "底层架构", "重构", "无锁", "环形缓冲区", "内存序",
        "动态规划", "图论", "红队", "渗透", "安全漏洞", "高并发", "分布式共识",
        "review", "deadlock", "race condition", "memory leak", "lock-free",
        "ring buffer", "memory order", "memory model", "paxos", "raft",
        "consensus", "dynamic programming", "cross-module", "monorepo",
        "security audit", "vulnerability", "red-team", "battle-tested",
        "heavy refactor", "deep reasoning", "formal verification"
    ]
    if any(k in p_lower for k in deep_keywords):
        return "deep"

    # Complexity heuristic: very long prompts (> 250 words / 800 chars) typically involve complex tasks
    if len(prompt) > 800 or len(prompt.split()) > 250:
        return "deep"

    # 3. Fast individual keywords if not matched above
    fast_individual = ["简单", "探测", "快速", "拼写", "quick", "fast", "probe"]
    if any(k in p_lower for k in fast_individual):
        return "fast"

    return "standard"


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


# --- Intent classification: yes/no and exploratory questions are read-only unless an explicit
# imperative coding instruction is present. When unsure, prefer read-only.
_ZH_QUESTION_MARKERS = (
    "吗", "呢", "是否", "能否", "能不能", "可不可以", "会不会", "有没有", "要不要", "是不是",
    "对不对", "行不行", "好不好", "为什么", "为何", "怎么", "怎样", "如何", "什么", "哪些", "哪个",
    "哪里", "哪儿", "请问", "想知道", "问一下",
)
_ZH_FINAL_PARTICLES = ("吗", "呢", "么", "不", "没")
_ZH_A_NOT_A = re.compile(r"([\u4e00-\u9fa5]{1,2})(?:[^\u4e00-\u9fa5\n]{0,10}|[\u4e00-\u9fa5]{0,4})(?:不|没)\1")
_ZH_INQUIRY_STARTERS = re.compile(
    r"^(?:请(?:问)?|帮我|帮忙|麻烦|给我|替我|你来)?\s*(?:解释|介绍|说明|讲讲|描述|列出|告诉我|看看|分析|阐述|梳理|查看|检索|阅读|展示|总结)"
)
_EN_INQUIRY_STARTERS = re.compile(
    r"^(?:(?:please|kindly|can\s+you|could\s+you)\s+)?(?:explain|describe|tell\s+me|show\s+me|list|summarize|walk\s+me\s+through|detail|elaborate\s+on|inspect|outline)\b"
)
_EN_QUESTION_STARTERS = re.compile(
    r"^(?:does|do|did|is|are|was|were|am|can|could|should|would|will|shall|may|might|what|which|"
    r"who|whom|whose|when|where|why|how|any\s+plans?\s+to|isn't|aren't|doesn't|don't|didn't|can't|won't|wouldn't|"
    r"shouldn't|couldn't)\b"
)
_EN_QUESTION_PHRASES = re.compile(
    r"\b(?:how\s+(?:does|do|did|is|are|can|could|should|would|to)|what\s+(?:is|are|does|do)|"
    r"why\s+(?:does|do|is|are)|where\s+(?:we|do|does|is|are|can)|which\s+(?:files?|parts?|modules?)|"
    r"is\s+there|are\s+there|whether|i\s+wonder|wondering|any\s+plans?\s+to)\b"
)
_ZH_CODE_VERBS = (
    r"(?:添加|增加|加上|加入|加个|实现|修复|修改|修正|修一下|编写|创建|新建|生成|重构|补充|补上|补全|删除|删掉|"
    r"移除|去掉|更新|升级|替换|迁移|优化|引入|接入|对接|集成|改成|改为|改一下|写一个|写个|写一下|写|改|加|修|删)"
)
_ZH_POLITE_IMPERATIVE = re.compile(
    r"(?:请(?!问|求|教)|帮我|帮忙|麻烦|给我|替我|你来)"
    r"(?:你|您|帮我|帮忙|再|也|顺便|直接|给我|一起|先|尽快|马上|立即|务必)*\s*" + _ZH_CODE_VERBS + r"(?!了)"
    r"|(?:请(?!问|求|教)|帮我|帮忙|麻烦|给我|替我|你来)[^，,。！!？?；;\n]{0,4}把[^，,。！!？?；;\n]{1,30}?"
    r"(?:改成|改为|修改为|替换为|替换成|加上|加入|添加到|删掉|删除|移除|去掉)"
)
_ZH_SEQUENCE_IMPERATIVE = re.compile(r"^(?:然后|接着|之后|随后|最后|顺便|另外|同时|并且|再)\s*" + _ZH_CODE_VERBS + r"(?!了)")
_ZH_CLAUSE_IMPERATIVE = re.compile(
    r"^(?:添加|增加|加上|加入|实现|修复|修改|修正|编写|创建|新建|生成|重构|补充|补上|补全|删除|删掉|移除|去掉|"
    r"更新|升级|替换|迁移|优化|引入|接入|写)"
    r"(?:一个|一下|一些|个|下|它|这个|那个|这些|那些|该|对应|相应|新的|上|掉)"
)
_EN_CODE_VERBS = (
    r"(?:add|implement|create|write|fix|refactor|build|generate|patch|integrate|remove|delete|update|rename|"
    r"migrate|change|modify|introduce|extend|replace|convert|optimize|upgrade|rewrite|make|port|bump|move|support)"
)
_EN_POLITE_IMPERATIVE = re.compile(
    r"\b(?:please|kindly|go\s+ahead\s+and|i\s+want\s+you\s+to|i\s+need\s+you\s+to|i'd\s+like\s+you\s+to|"
    r"let's|let\s+us)\s+(?:(?:also|just|now|then|go\s+ahead\s+and)\s+)*" + _EN_CODE_VERBS + r"\b"
)
_EN_CLAUSE_IMPERATIVE = re.compile(
    r"^" + _EN_CODE_VERBS + r"\s+(?:a|an|the|this|that|these|those|it|them|some|all|any|new|missing|proper|"
    r"unit|tests?|support|logging|docs?|documentation|comments?|type|types|error|errors|retries|--?\w+|`)\b"
)
_EN_LEADING_CONNECTORS = re.compile(
    r"^(?:(?:and\s+then|and|then|also|so|next|finally|afterwards|after\s+that|if\s+so|if\s+not|otherwise|"
    r"just|now|please|kindly|go\s+ahead\s+and)\s+)+"
)
_EXPLICIT_WRITE_DIRECTIVES = ("并在当前目录落盘", "并落盘", "直接落盘", "落盘到", "写入文件并保存")
_CLAUSE_SPLIT_RE = re.compile(r"[。！!；;\n，,：:]|\.(?=\s|$)|(?<=[？?])")


def _split_prompt_clauses(lower: str) -> List[str]:
    clauses = []
    for raw in _CLAUSE_SPLIT_RE.split(lower):
        clause = raw.strip().lstrip("-*•>#\"'“”‘’`（）()[] \t")
        if clause:
            clauses.append(clause)
    return clauses


def _is_question_clause(clause: str) -> bool:
    if clause.endswith(("?", "？")):
        return True
    if any(clause.endswith(p) for p in _ZH_FINAL_PARTICLES):
        return True
    if any(m in clause for m in _ZH_QUESTION_MARKERS):
        return True
    if _ZH_A_NOT_A.search(clause):
        return True
    if _ZH_INQUIRY_STARTERS.match(clause):
        return True
    if _EN_INQUIRY_STARTERS.match(clause):
        return True
    if _EN_QUESTION_STARTERS.match(clause) or _EN_QUESTION_PHRASES.search(clause):
        return True
    return False


def is_inquiry_prompt(prompt: str) -> bool:
    """True for yes/no or exploratory questions (？/?, 吗/呢/是否/能否..., does/is/can/what/which...)."""
    lower = (prompt or "").lower().strip()
    if not lower:
        return False
    if lower.endswith(("?", "？")):
        return True
    return any(_is_question_clause(cl) for cl in _split_prompt_clauses(lower))


def has_explicit_coding_imperative(prompt: str) -> bool:
    """
    Detects an explicit imperative coding instruction such as '请添加…', '帮我实现…', 'please add …',
    'add a …' at the start of a non-question clause, or '…并在当前目录落盘'.
    """
    lower = (prompt or "").lower().strip()
    if not lower:
        return False
    if any(d in lower for d in _EXPLICIT_WRITE_DIRECTIVES):
        return True
    if _ZH_POLITE_IMPERATIVE.search(lower) or _EN_POLITE_IMPERATIVE.search(lower):
        return True
    for clause in _split_prompt_clauses(lower):
        if _is_question_clause(clause):
            continue
        if _ZH_SEQUENCE_IMPERATIVE.match(clause) or _ZH_CLAUSE_IMPERATIVE.match(clause):
            return True
        en_clause = _EN_LEADING_CONNECTORS.sub("", clause)
        if _EN_CLAUSE_IMPERATIVE.match(en_clause):
            return True
    return False


def classify_prompt_intent(prompt: str) -> str:
    """
    Classify user prompt into:
    - 'identity': questions about who makewand is or what it can do
    - 'explain': questions/explanations/chit-chat (strictly read-only execution)
    - 'review': code audit/review requests (strictly read-only execution)
    - 'code': code generation/refactoring/fixing tasks

    Yes/no and exploratory questions are read-only ('explain'/'review'/'identity') unless the prompt
    also carries an explicit imperative coding instruction; when unsure, prefer read-only.
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

    # Explicit read-only directives always win over any coding action.
    negation_patterns = [
        "不要修改", "不用修改", "别修改", "不要改", "别改", "不用改",
        "只看不改", "只解释", "无需修改", "不要写代码", "别写代码", "不用写代码",
        "只分析", "只做分析", "只读",
        "don't modify", "do not modify", "without modifying", "don't edit", "do not edit",
        "read only", "readonly", "explain only", "just explain"
    ]
    has_negation = any(n in lower for n in negation_patterns)

    has_chinese_coding = any(k in lower for k in chinese_coding_triggers)
    has_english_coding = any(re.search(pat, lower) for pat in english_coding_patterns)
    explicit_imperative = has_explicit_coding_imperative(lower)
    has_coding_action = (has_chinese_coding or has_english_coding or explicit_imperative) and not has_negation

    # Questions ("这个项目支持 Windows 吗？", "Should I add a lockfile?") stay read-only unless an explicit
    # imperative instruction is present ("…？如果不支持，请添加支持", "Could you please add …?").
    if has_coding_action and (explicit_imperative or not is_inquiry_prompt(lower)):
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
        f"{COLOR_BOLD}{COLOR_GREEN}✨ 我是 Makewand (v3.1) —— 零成本多模型 AI 订阅与全生态编程工具统一调度中枢。{COLOR_RESET}\n\n"
        "我统合调度本机主流 AI 订阅服务、云端 API 与本地大模型：\n"
        f"  {COLOR_GREEN}• Google AI Pro (Antigravity / AGY){COLOR_RESET}: 全局架构设计、复杂推理与闭环兜底\n"
        f"  {COLOR_BLUE}• Claude Code (Anthropic){COLOR_RESET}: 高敏捷代码编写、多文件重构与实现\n"
        f"  {COLOR_CYAN}• Codex CLI (OpenAI / gpt-6-astra){COLOR_RESET}: 独立红队代码审查与算法攻防\n"
        f"  {COLOR_RED}• Grok Build CLI (xAI / grok-4.7){COLOR_RESET}: 前沿深度推理、大上下文架构与快速原型开发\n"
        f"  {COLOR_PURPLE}• Muse Code (Meta / Muse Spark){COLOR_RESET}: 辅助生成、沙箱验证与备用编码\n"
        f"  {COLOR_GREEN}• Aider / Cursor / Copilot{COLOR_RESET}: 结对编程命令行与代码辅助生成\n"
        f"  {COLOR_CYAN}• DeepSeek / Qwen / GLM / Kimi{COLOR_RESET}: 主流商业云端 API 动态接入\n"
        f"  {COLOR_PURPLE}• Local Self-Hosted (Ollama / vLLM){COLOR_RESET}: 本地私有离线大模型 (0 成本/安全)\n\n"
        f"{COLOR_BOLD}核心机制：{COLOR_RESET}\n"
        "  1. 智能意图路由：精准区分闲聊/问答（直接响应）与工程开发任务（多模型流水线），杜绝误触发程序检查或缺陷修复\n"
        "  2. 跨模型联合流水线：自动规划、编码实现、红队盲审与 Auto-Fix 缺陷自愈\n"
        "  3. 双模型沙箱竞速 (/race)：临时工作区并发派发比拼与主裁判评定\n"
        "  4. 订阅配额健康监控 (/status, /quota)：按订阅额度与显式 API 费用策略切换\n"
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


def _explicit_readonly_request(prompt: str) -> bool:
    # Reuse the pipeline's existing negative-instruction boundary when an
    # explicit workflow is selected; workflow selection cannot grant writes.
    triggers = (
        "不要修改", "不用修改", "别修改", "不要改", "别改", "不用改",
        "只看不改", "只解释", "无需修改", "不要写代码", "别写代码", "不用写代码",
        "只分析", "只做分析", "只读", "don't modify", "do not modify", "without modifying",
        "don't edit", "do not edit", "read only", "readonly", "explain only", "just explain",
    )
    return any(value in prompt.lower() for value in triggers)
