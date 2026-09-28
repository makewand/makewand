"""
G2 regressions: yes/no and exploratory questions must stay read-only.

Covers py-orchestrator#3 and replay-0926-memory#6: questions containing coding words
(支持/添加/引入/add/patch/support/implement ...) used to be classified as 'code' and sent into the
writable pipeline (git init in non-git dirs, writable coder, cleanup that could delete ignored files).
"""

import contextlib
import io
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.orchestrator as orch
from makewand.orchestrator import classify_prompt_intent

# Every misrouted sample quoted in the findings (py-orchestrator#3 evidence, replay-0926-memory#6 evidence).
REPORTED_MISROUTED_QUESTIONS = [
    "这个项目支持 Windows 吗？",
    "Does this project support Windows?",
    "What does the patch command do?",
    "Should I add a lockfile?",
    "which files implement the router?",
    "这段代码为什么会引入内存泄漏",
    "能不能添加一个配置项？",
    "当前实现支持并发吗",
    "Does this add a new dependency?",
    "是否需要新建分支？",
    "Is it safe to develop on master?",
    "Should I add tests here?",
]

# Additional inquiry shapes required by the design (？/?, 吗/呢/是否/能否/能不能/可不可以/会不会/有没有/要不要,
# and does/do/is/are/can/could/should/would/will/what/which starters).
EXTRA_QUESTIONS = [
    "这个接口能否支持批量导入？",
    "可不可以引入 Redis 做缓存",
    "会不会引入新的依赖？",
    "有没有必要重构这个模块",
    "要不要添加单元测试？",
    "那它支持 ARM 呢",
    "Can this library support async?",
    "Could we implement this without a lock?",
    "Would it help to add an index",
    "Will this patch break the build?",
    "Are there tests that create temp files?",
    "Do we support Python 3.8?",
    "what would it take to add caching",
    "Is there a way to add caching",
    "请问这个项目支持 Windows 吗？",
    "能帮我看看这个项目支持 Windows 吗？",
    "你帮我添加了测试吗？",
    "这个库直接支持 async 吗？",
    "是先删除然后添加吗？",
    "更新日志已经写好了，还有什么要补充的吗？",
    "实现细节在 foo.py 里，这样做对吗？",
    "How do I add a flag?",
]

# Explicit imperative coding instructions must still reach the code pipeline, including polite questions
# that carry one ("能帮我添加…吗？", "Could you please add …?").
IMPERATIVE_CONTROLS = [
    "请添加一个配置项",
    "帮我实现一个 LRU 缓存",
    "add a --verbose flag to the CLI",
    "能帮我添加一个配置项吗？",
    "你能帮我实现登录接口吗？",
    "这个项目支持 Windows 吗？如果不支持，请添加支持。",
    "Does this support Windows? If not, add support for it.",
    "Could you please add a unit test for parse()?",
    "Implement a function that reverses a string",
    "Fix the null pointer bug in main.go",
    "这个函数有 bug 吗？修复它。",
    "解释一下这个函数，然后添加单测。",
    "What is wrong with parse()? Please fix it.",
    "Why does the build fail? Go ahead and fix it.",
    "给 README 添加安装说明",
    "实现一个LRU缓存并在当前目录落盘",
    "能不能实现一个 LRU 缓存并在当前目录落盘？",
    "修复这个bug并保证单测通过",
    "写一个登录页面",
    "Refactor the config loader to use pathlib",
    "Let's add retries to the HTTP client.",
    "Create a file named notes.md with a summary",
    "麻烦你加一个超时参数，可以吗？",
    "请把超时时间改成 30 秒，好吗？",
    "How does the router work? Add logging to it.",
    "introduce yourself, then run the tests",
]


class TestQuestionIntentIsReadOnly(unittest.TestCase):
    def test_reported_misrouted_questions_are_explain(self):
        for prompt in REPORTED_MISROUTED_QUESTIONS:
            with self.subTest(prompt=prompt):
                self.assertEqual(classify_prompt_intent(prompt), "explain")

    def test_additional_inquiries_are_read_only(self):
        for prompt in EXTRA_QUESTIONS:
            with self.subTest(prompt=prompt):
                self.assertIn(classify_prompt_intent(prompt), ("explain", "review", "identity"))

    def test_explicit_imperatives_still_route_to_code(self):
        self.assertGreaterEqual(len(IMPERATIVE_CONTROLS), 15)
        for prompt in IMPERATIVE_CONTROLS:
            with self.subTest(prompt=prompt):
                self.assertEqual(classify_prompt_intent(prompt), "code")

    def test_read_only_directive_beats_imperative(self):
        self.assertEqual(classify_prompt_intent("这个项目支持 Windows 吗？只分析，不要修改"), "explain")
        self.assertEqual(classify_prompt_intent("add a test for parse(), but do not modify any files"), "explain")

    def test_inquiry_helpers(self):
        self.assertTrue(orch.is_inquiry_prompt("Does this project support Windows?"))
        self.assertTrue(orch.is_inquiry_prompt("当前实现支持并发吗"))
        self.assertFalse(orch.is_inquiry_prompt("添加搜索功能"))
        self.assertTrue(orch.has_explicit_coding_imperative("能帮我添加一个配置项吗？"))
        self.assertFalse(orch.has_explicit_coding_imperative("能不能添加一个配置项？"))
        self.assertFalse(orch.has_explicit_coding_imperative("Should I add a lockfile?"))


class TestQuestionNeverEntersWritePipeline(unittest.TestCase):
    """End-to-end shape of the reported data-loss chain: a plain question in a non-git directory."""

    def test_question_in_non_git_dir_is_answered_read_only_and_keeps_ignored_files(self):
        with tempfile.TemporaryDirectory(prefix="g2-intent-") as tmp:
            work = Path(tmp)
            (work / ".gitignore").write_text(".env\n", encoding="utf-8")
            (work / ".env").write_text("SECRET=1\n", encoding="utf-8")
            (work / "app.py").write_text("print('hi')\n", encoding="utf-8")
            calls = []

            def dispatch(engine, prompt, cwd=None, readonly=False, **kwargs):
                calls.append({"engine": engine, "readonly": readonly})
                return True, "是的，支持。", None

            with contextlib.ExitStack() as stack:
                stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=False))
                stack.enter_context(patch.object(orch, "check_working_tree_isolation", return_value=(True, None)))
                stack.enter_context(patch.object(orch, "get_or_update_status", return_value={}))
                stack.enter_context(patch.object(orch, "select_optimal_engine_pair", return_value=(
                    ["claude"], ["codex"], {"primary_coder": "claude", "primary_reviewer": "codex", "reasons": []})))
                stack.enter_context(patch.object(orch, "dispatch_task", side_effect=dispatch))
                stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, None)))
                stack.enter_context(patch("makewand.memory.format_memory_hints_for_prompt", return_value=""))
                stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
                orch.run_pipeline("这个项目支持 Windows 吗？", cwd=str(work), force_code=False, auto_fix=True)

            self.assertTrue(calls, "the question should still be answered")
            self.assertTrue(all(call["readonly"] for call in calls), calls)
            self.assertFalse((work / ".git").exists(), "a question must not git-init the user's directory")
            self.assertTrue((work / ".env").exists(), "ignored files must survive a question")
            self.assertEqual(sorted(os.listdir(work)), [".env", ".gitignore", "app.py"])


if __name__ == "__main__":
    unittest.main()
