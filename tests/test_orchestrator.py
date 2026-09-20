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

        # Clean passes
        self.assertFalse(has_critical_defects("经审查，代码未发现严重漏洞，LGTM，建议直接合并"))
        self.assertFalse(has_critical_defects("所有用例均通过且无安全漏洞，审核通过"))
        self.assertFalse(has_critical_defects("没有发现明显缺陷，无需修改"))

if __name__ == "__main__":
    unittest.main()
