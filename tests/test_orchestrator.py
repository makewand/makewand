"""
Unit tests for orchestrator logic: task tiering and defect detection.
"""

import unittest
from makewand.orchestrator import detect_task_tier, has_critical_defects

class TestOrchestrator(unittest.TestCase):
    def test_detect_task_tier(self):
        self.assertEqual(detect_task_tier("深度审查并发死锁"), "deep")
        self.assertEqual(detect_task_tier("review PR security architecture"), "deep")
        self.assertEqual(detect_task_tier("快速拼写检查"), "fast")
        self.assertEqual(detect_task_tier("编写一个简单的脚本"), "fast")
        self.assertEqual(detect_task_tier("实现一个二叉搜索树类并在当前目录落盘"), "standard")

    def test_has_critical_defects(self):
        # Critical defects
        self.assertTrue(has_critical_defects("发现重大隐患: [P1] 互斥锁存在死锁漏洞"))
        self.assertTrue(has_critical_defects("存在 [P2] 资源泄露风险，建议修改后再合并"))
        self.assertTrue(has_critical_defects("检测到 race condition 和数据竞态"))

        # Clean passes
        self.assertFalse(has_critical_defects("经审查，代码未发现严重漏洞，LGTM，建议直接合并"))
        self.assertFalse(has_critical_defects("所有用例均通过且无安全漏洞，审核通过"))
        self.assertFalse(has_critical_defects("没有发现明显缺陷，无需修改"))

if __name__ == "__main__":
    unittest.main()
