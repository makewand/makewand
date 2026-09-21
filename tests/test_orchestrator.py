"""
Unit tests for orchestrator logic: task tiering and defect detection.
"""

import unittest
from unittest.mock import patch
from makewand.orchestrator import (
    detect_task_tier,
    has_critical_defects,
    classify_prompt_intent,
    is_identity_or_chit_chat,
    run_pipeline
)

class TestOrchestrator(unittest.TestCase):
    def test_detect_task_tier(self):
        self.assertEqual(detect_task_tier("深度审查并发死锁"), "deep")
        self.assertEqual(detect_task_tier("review PR security architecture"), "deep")
        self.assertEqual(detect_task_tier("快速拼写检查"), "fast")
        self.assertEqual(detect_task_tier("编写一个简单的脚本"), "fast")
        self.assertEqual(detect_task_tier("实现一个二叉搜索树类并在当前目录落盘"), "standard")

    def test_classify_prompt_intent(self):
        self.assertEqual(classify_prompt_intent("你是谁"), "identity")
        self.assertEqual(classify_prompt_intent("Who are you?"), "identity")
        self.assertEqual(classify_prompt_intent("介绍一下自己"), "identity")
        self.assertEqual(classify_prompt_intent("你好"), "identity")
        self.assertEqual(classify_prompt_intent("hello there"), "identity")
        self.assertEqual(classify_prompt_intent("你是人类还是AI"), "identity")
        self.assertEqual(classify_prompt_intent("你是什么类型的助手"), "identity")
        self.assertEqual(classify_prompt_intent("在这个页面你能干什么"), "identity")
        self.assertEqual(classify_prompt_intent("谁创建了你"), "identity")
        self.assertEqual(classify_prompt_intent("你叫啥"), "identity")
        self.assertEqual(classify_prompt_intent("介绍下你自己"), "identity")
        self.assertEqual(classify_prompt_intent("你到底是谁"), "identity")

        # Explain queries & general questions
        self.assertEqual(classify_prompt_intent("解释一下Go语言的channel底层原理"), "explain")
        self.assertEqual(classify_prompt_intent("what is a goroutine?"), "explain")
        self.assertEqual(classify_prompt_intent("Python和Go哪个更好"), "explain")
        self.assertEqual(classify_prompt_intent("协程和线程的区别"), "explain")

        # Compound requests (must NOT be swallowed into identity)
        self.assertEqual(classify_prompt_intent("你能做什么？顺便分析这份日志"), "explain")
        self.assertEqual(classify_prompt_intent("你好，解释一下Go语言channel"), "explain")
        self.assertEqual(classify_prompt_intent("introduce yourself, then run the tests"), "code")

        # Review & Code
        self.assertEqual(classify_prompt_intent("审查当前git diff中的并发死锁"), "review")
        self.assertEqual(classify_prompt_intent("实现一个LRU缓存并在当前目录落盘"), "code")
        self.assertEqual(classify_prompt_intent("修复这个bug并保证单测通过"), "code")
        self.assertEqual(classify_prompt_intent("写一个登录页面并在当前目录落盘"), "code")

    def test_is_identity_or_chit_chat(self):
        self.assertTrue(is_identity_or_chit_chat("你是谁"))
        self.assertTrue(is_identity_or_chit_chat("你是什么AI"))
        self.assertTrue(is_identity_or_chit_chat("你能做什么"))
        self.assertTrue(is_identity_or_chit_chat("introduce yourself"))
        self.assertTrue(is_identity_or_chit_chat("你好！"))
        self.assertTrue(is_identity_or_chit_chat("你是人类还是AI"))
        self.assertTrue(is_identity_or_chit_chat("谁创建了你"))
        self.assertTrue(is_identity_or_chit_chat("在这个页面你能做什么"))

        # Must be False for compound requests and coding requests
        self.assertFalse(is_identity_or_chit_chat("你能做什么？顺便分析这份日志"))
        self.assertFalse(is_identity_or_chit_chat("introduce yourself, then run the tests"))
        self.assertFalse(is_identity_or_chit_chat("你好，解释一下Go语言channel"))
        self.assertFalse(is_identity_or_chit_chat("写一个排序算法并保存到sort.py"))
        self.assertFalse(is_identity_or_chit_chat("修复这个bug"))

    def test_run_pipeline_identity_query(self):
        # Must return True immediately without triggering code generation or auto-fix loop
        self.assertTrue(run_pipeline("你是谁", auto_fix=True))
        self.assertTrue(run_pipeline("who are you", auto_fix=True))

    @patch("makewand.orchestrator.run_review")
    def test_run_pipeline_review_query(self, mock_review):
        mock_review.return_value = 0
        # Review request must call run_review and NOT trigger code file writing
        res = run_pipeline("审查当前git diff中的并发死锁", auto_fix=True)
        self.assertTrue(res)
        mock_review.assert_called_once()

    def test_has_critical_defects(self):
        # Critical defects
        self.assertTrue(has_critical_defects("发现重大隐患: [P0] arbitrary host command execution"))
        self.assertTrue(has_critical_defects("发现重大隐患: [P1] 互斥锁存在死锁漏洞"))
        self.assertTrue(has_critical_defects("存在 [P2] 资源泄露风险，建议修改后再合并"))
        self.assertTrue(has_critical_defects("检测到 race condition 和数据竞态"))
        self.assertTrue(has_critical_defects("LGTM; race condition in worker"))

        # Clean passes with natural language
        self.assertFalse(has_critical_defects("经审查，代码未发现严重漏洞，LGTM，建议直接合并"))
        self.assertFalse(has_critical_defects("所有用例均通过且无安全漏洞，审核通过"))
        self.assertFalse(has_critical_defects("没有发现明显缺陷，无需修改"))

        # Crucial: Negation phrases must NOT trigger false defects!
        self.assertFalse(has_critical_defects("经检查，未发现并发死锁，没有发现内存泄漏，亦无数据竞态隐患，表现良好。"))
        self.assertFalse(has_critical_defects("Review result: no deadlock, no race condition, without any defect. Looks good!"))
        self.assertFalse(has_critical_defects("经过分析，代码中不存在死锁，没有发现缺陷，建议合并。"))

        # Structural JSON verdicts
        self.assertFalse(has_critical_defects(
            "审查意见详情...\n"
            "MAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}"
        ))
        self.assertTrue(has_critical_defects(
            "审查意见详情...\n"
            "MAKEWAND_VERDICT: {\"pass\": false, \"defects\": [\"空指针解引用\"]}"
        ))

        # JSON string booleans: must not fall into bool("false") == True trap!
        self.assertTrue(has_critical_defects(
            "审查意见详情...\n"
            "MAKEWAND_VERDICT: {\"pass\": \"false\", \"defects\": [\"逻辑死锁\"]}"
        ))
        self.assertFalse(has_critical_defects(
            "审查意见详情...\n"
            "MAKEWAND_VERDICT: {\"pass\": \"true\", \"defects\": []}"
        ))

        # Empty or unverified text must fail closed
        self.assertTrue(has_critical_defects(""))
        self.assertTrue(has_critical_defects(None))
        self.assertTrue(has_critical_defects("   "))

    @patch("makewand.orchestrator.get_or_update_status")
    @patch("makewand.orchestrator.execute_claude_task")
    @patch("makewand.orchestrator.get_git_diff")
    def test_fail_closed_on_unverified_review(self, mock_diff, mock_claude, mock_status):
        mock_status.return_value = {"claude": {"status": "healthy"}, "codex": {"status": "limited"}}
        mock_claude.return_value = (True, "Code written", None)
        mock_diff.return_value = "diff --git a/main.py b/main.py\n+print('hello')"

        # Both reviewer codex and agy fail or return None
        with patch("makewand.orchestrator.execute_codex_task", return_value=(False, None, "error")), \
             patch("makewand.orchestrator.execute_agy_task", return_value=(False, None, "error")):
            res = run_pipeline("实现测试功能", cwd="/tmp", auto_fix=False)
            # Must FAIL-CLOSED (return False, rejecting delivery)
            self.assertFalse(res)

    @patch("makewand.orchestrator.get_or_update_status")
    @patch("makewand.orchestrator.execute_claude_task")
    def test_explain_readonly_flag(self, mock_claude, mock_status):
        mock_status.return_value = {"claude": {"status": "healthy"}}
        mock_claude.return_value = (True, "Go channel explanation", None)

        res = run_pipeline("解释一下Go语言channel原理", cwd="/tmp")
        self.assertTrue(res)
        # Must have passed readonly=True
        mock_claude.assert_called_once()
        _, kwargs = mock_claude.call_args
        self.assertTrue(kwargs.get("readonly"))

    def test_select_optimal_engine_pair(self):
        from makewand.orchestrator import select_optimal_engine_pair

        all_healthy = {
            "codex": {"status": "healthy"},
            "claude": {"status": "healthy"},
            "agy": {"status": "healthy"},
            "muse": {"status": "healthy"}
        }

        # 1. Algorithmic / Concurrency task -> Codex primary coder, Claude reviewer
        coders, reviewers, meta = select_optimal_engine_pair(
            "实现无锁并发环形缓冲区 lock-free ring buffer 并排查死锁与竞态",
            tier="deep",
            cache=all_healthy
        )
        self.assertEqual(meta["primary_coder"], "codex")
        self.assertIn(meta["primary_reviewer"], ["claude", "agy"])
        self.assertNotEqual(meta["primary_coder"], meta["primary_reviewer"])

        # 2. Refactoring / Frontend task -> Claude primary coder, Codex reviewer
        coders, reviewers, meta = select_optimal_engine_pair(
            "重构用户管理模块的前端 React 组件与页面样式，补充单测",
            tier="standard",
            cache=all_healthy
        )
        self.assertEqual(meta["primary_coder"], "claude")
        self.assertEqual(meta["primary_reviewer"], "codex")
        self.assertNotEqual(meta["primary_coder"], meta["primary_reviewer"])

        # 3. Global Architecture / Monorepo task -> Antigravity primary coder
        coders, reviewers, meta = select_optimal_engine_pair(
            "总体设计全仓跨仓库微服务拆分架构与长文档方案对比",
            tier="deep",
            cache=all_healthy
        )
        self.assertEqual(meta["primary_coder"], "agy")
        self.assertIn(meta["primary_reviewer"], ["codex", "claude"])
        self.assertNotEqual(meta["primary_coder"], meta["primary_reviewer"])

        # 4. Quota Limited fallback: When Codex is limited, algorithm falls back to Claude/AGY
        codex_limited = {
            "codex": {"status": "limited"},
            "claude": {"status": "healthy"},
            "agy": {"status": "healthy"}
        }
        coders, reviewers, meta = select_optimal_engine_pair(
            "实现快速排序算法",
            tier="standard",
            cache=codex_limited
        )
        self.assertEqual(meta["primary_coder"], "claude")
        self.assertEqual(meta["primary_reviewer"], "agy")

if __name__ == "__main__":
    unittest.main()
