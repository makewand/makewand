"""
Unit tests for orchestrator logic: task tiering and defect detection.
"""

import unittest
import tempfile
import makewand.orchestrator
from pathlib import Path
from unittest.mock import patch
from makewand.orchestrator import (
    detect_task_tier,
    has_critical_defects,
    classify_prompt_intent,
    is_identity_or_chit_chat,
    run_pipeline,
    extract_review_verdict_dict,
    run_review,
    EXIT_PASSED,
    EXIT_FAILED,
    EXIT_UNVERIFIED
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

        # All candidate reviewers (codex, agy, grok, muse) fail or return None
        with patch("makewand.orchestrator.execute_codex_task", return_value=(False, None, "error")), \
             patch("makewand.orchestrator.execute_agy_task", return_value=(False, None, "error")), \
             patch("makewand.orchestrator.execute_grok_task", return_value=(False, None, "error")), \
             patch("makewand.orchestrator.execute_muse_task", return_value=(False, None, "error")):
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

        with patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)):
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

            # 5. False positive keyword protection:
            # "building" contains "ui", "capital" contains "api", "artifacts" contains "ts"
            # Should NOT trigger refactor keywords or Claude affinity boost
            from makewand.orchestrator import _match_domain_keywords
            matched = _match_domain_keywords(["ui", "api", "ts", "rest"], "building capital artifacts interest")
            self.assertEqual(matched, [])

            matched_valid = _match_domain_keywords(["ui", "api", "ts", "rest"], "fix the UI component and REST API in ts")
            self.assertEqual(sorted(matched_valid), sorted(["ui", "api", "ts", "rest"]))

    def test_extract_review_verdict_dict(self):
        # Case 1: Structured JSON pass
        res_pass = (
            "Review analysis...\n"
            "MAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}"
        )
        dict_pass = extract_review_verdict_dict(res_pass)
        self.assertTrue(dict_pass["pass"])
        self.assertEqual(dict_pass["defects"], [])

        # Case 2: Structured JSON defect
        res_defect = (
            "Review analysis...\n"
            "MAKEWAND_VERDICT: {\"pass\": false, \"defects\": [\"[P1] Deadlock in mutex.Lock()\"]}"
        )
        dict_defect = extract_review_verdict_dict(res_defect)
        self.assertFalse(dict_defect["pass"])
        self.assertEqual(dict_defect["defects"], ["[P1] Deadlock in mutex.Lock()"])

    @patch("makewand.orchestrator.get_git_diff")
    @patch("makewand.orchestrator.get_or_update_status")
    @patch("makewand.orchestrator.execute_codex_task")
    def test_run_review_json_output(self, mock_codex, mock_status, mock_diff):
        import io
        import json
        from contextlib import redirect_stdout

        mock_status.return_value = {"codex": {"status": "healthy"}}
        mock_diff.return_value = "diff --git a/test.py b/test.py\n+x = 1"
        mock_codex.return_value = (True, "LGTM\nMAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}", None)

        buf = io.StringIO()
        with redirect_stdout(buf):
            code = run_review(cwd="/tmp", output_json=True)

        self.assertEqual(code, EXIT_PASSED)
        payload = json.loads(buf.getvalue().strip())
        self.assertTrue(payload["pass"])
        self.assertEqual(payload["engine"], "codex")
        self.assertEqual(payload["exit_code"], 0)

    def test_extract_verdict_json_prompt_example_and_malformed_verdict(self):
        from makewand.orchestrator import extract_verdict_json, is_review_passed

        # 1. Output quotes the prompt example, but then emits a failed verdict:
        text_with_prompt_quote = (
            "Here is the format you asked for: MAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}\n"
            "Now my actual analysis:\n"
            "We found a severe race condition in the worker pool.\n"
            "MAKEWAND_VERDICT: {\"pass\": false, \"defects\": [\"[P1] Race condition in worker pool\"]}"
        )
        v1 = extract_verdict_json(text_with_prompt_quote)
        self.assertIsNotNone(v1)
        self.assertFalse(v1["pass"])
        self.assertEqual(v1["defects"], ["[P1] Race condition in worker pool"])
        self.assertFalse(is_review_passed(text_with_prompt_quote))

        # 2. Output quotes prompt example, but the final verdict has a syntax error:
        text_malformed_final = (
            "Quoting example: MAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}\n"
            "Analysis: found memory leak.\n"
            "MAKEWAND_VERDICT: {\"pass\": false, \"defects\": [unclosed quote}"
        )
        v2 = extract_verdict_json(text_malformed_final)
        self.assertIsNotNone(v2)
        self.assertTrue(v2.get("parse_error"))
        self.assertFalse(v2["pass"])
        self.assertFalse(is_review_passed(text_malformed_final))  # MUST FAIL CLOSED!

        # 3. Lenient parsing of trailing commas before closing braces
        text_trailing_comma = (
            "Code looks good.\n"
            "MAKEWAND_VERDICT: {\"pass\": true, \"defects\": [],}"
        )
        v3 = extract_verdict_json(text_trailing_comma)
        self.assertIsNotNone(v3)
        self.assertTrue(v3["pass"])
        self.assertEqual(v3["defects"], [])
        self.assertTrue(is_review_passed(text_trailing_comma))

        # 4. Contradiction: pass=True but defects list is non-empty
        text_contradiction = (
            "MAKEWAND_VERDICT: {\"pass\": true, \"defects\": [\"[P1] critical bug\"]}"
        )
        v4 = extract_verdict_json(text_contradiction)
        self.assertIsNotNone(v4)
        self.assertFalse(v4["pass"])
        self.assertFalse(is_review_passed(text_contradiction))

        # 5. Contradiction: body text contains [P1] defect but final JSON claimed pass=True with empty defects
        text_body_p1_contradiction = (
            "Review Summary:\n"
            "1. [P1] External symlink write-through vulnerability detected.\n"
            "MAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}"
        )
        self.assertFalse(is_review_passed(text_body_p1_contradiction))

    def test_shadow_delivery_artifact_isolation(self):
        from makewand.git_helper import run_git_cmd, ShadowWorktreeResult
        from makewand.orchestrator import run_pipeline

        with tempfile.TemporaryDirectory() as base_tmp:
            run_git_cmd(["git", "init"], cwd=base_tmp)
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=base_tmp)
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=base_tmp)
            (Path(base_tmp) / "init.txt").write_text("hello")
            run_git_cmd(["git", "add", "-A"], cwd=base_tmp)
            run_git_cmd(["git", "commit", "-m", "init"], cwd=base_tmp)
            _, base_head, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=base_tmp)
            base_head = base_head.strip()

            with tempfile.TemporaryDirectory() as shadow_dir:
                run_git_cmd(["git", "clone", base_tmp, shadow_dir], cwd=base_tmp)
                run_git_cmd(["git", "config", "user.name", "Tester"], cwd=shadow_dir)
                run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=shadow_dir)
                run_git_cmd(["git", "checkout", "-b", "makewand/test_branch"], cwd=shadow_dir)

                shadow_res = ShadowWorktreeResult(
                    shadow_dir,
                    "makewand/test_branch",
                    lambda: None,
                    baseline_commit=base_head,
                    repo_head=base_head,
                    repo_root=base_tmp,
                    worktree_root=shadow_dir
                )

                def mock_coder(prompt, cwd=None, **kwargs):
                    (Path(cwd) / "file.py").write_text("print('task code')")
                    return True, "Code written", None

                with patch("makewand.orchestrator.check_working_tree_isolation", return_value=(False, "Active tmux session")), \
                     patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)), \
                     patch("makewand.orchestrator.create_ephemeral_shadow_worktree", return_value=shadow_res), \
                     patch("makewand.orchestrator.get_or_update_status", return_value={"codex": {"status": "healthy"}, "claude": {"status": "healthy"}}), \
                     patch("makewand.orchestrator.execute_codex_task", return_value=(True, "LGTM\nMAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}", None)), \
                     patch("makewand.orchestrator.execute_claude_task", side_effect=mock_coder):
                    res = run_pipeline("实现测试任务", cwd=base_tmp, stream=False)
                    self.assertTrue(res)

                    # Shadow worktree must have 0 uncommitted or untracked files
                    _, st_out, _ = run_git_cmd(["git", "status", "--porcelain"], cwd=shadow_dir)
                    self.assertEqual(st_out.strip(), "")

    def test_patch_export_failure_blocks_delivery(self):
        from makewand.git_helper import run_git_cmd, ShadowWorktreeResult
        from makewand.orchestrator import run_pipeline

        with tempfile.TemporaryDirectory() as base_tmp:
            run_git_cmd(["git", "init"], cwd=base_tmp)
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=base_tmp)
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=base_tmp)
            (Path(base_tmp) / "init.txt").write_text("hello")
            run_git_cmd(["git", "add", "-A"], cwd=base_tmp)
            run_git_cmd(["git", "commit", "-m", "init"], cwd=base_tmp)
            _, base_head, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=base_tmp)
            base_head = base_head.strip()

            with tempfile.TemporaryDirectory() as shadow_dir:
                run_git_cmd(["git", "clone", base_tmp, shadow_dir], cwd=base_tmp)
                run_git_cmd(["git", "config", "user.name", "Tester"], cwd=shadow_dir)
                run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=shadow_dir)
                run_git_cmd(["git", "checkout", "-b", "makewand/test_branch"], cwd=shadow_dir)

                shadow_res = ShadowWorktreeResult(
                    shadow_dir,
                    "makewand/test_branch",
                    lambda: None,
                    baseline_commit=base_head,
                    repo_head=base_head,
                    repo_root=base_tmp,
                    worktree_root=shadow_dir
                )

                def mock_coder(prompt, cwd=None, **kwargs):
                    (Path(cwd) / "file.py").write_text("print('task code')")
                    return True, "Code written", None

                # Injected patch export failure
                orig_run_git_cmd = makewand.orchestrator.run_git_cmd
                def mock_git_cmd(cmd, cwd=None, **kwargs):
                    if isinstance(cmd, list) and "diff" in cmd and "--binary" in cmd and "--full-index" in cmd:
                        return 1, b"", "Simulated patch export fatal error"
                    return orig_run_git_cmd(cmd, cwd=cwd, **kwargs)

                with patch("makewand.orchestrator.check_working_tree_isolation", return_value=(False, "Active tmux session")), \
                     patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)), \
                     patch("makewand.orchestrator.create_ephemeral_shadow_worktree", return_value=shadow_res), \
                     patch("makewand.orchestrator.get_or_update_status", return_value={"codex": {"status": "healthy"}, "claude": {"status": "healthy"}}), \
                     patch("makewand.orchestrator.execute_codex_task", return_value=(True, "LGTM\nMAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}", None)), \
                     patch("makewand.orchestrator.execute_claude_task", side_effect=mock_coder), \
                     patch("makewand.orchestrator.run_git_cmd", side_effect=mock_git_cmd):
                    res = run_pipeline("实现测试任务", cwd=base_tmp, stream=False)
                    # Must fail-closed!
                    self.assertFalse(res)

    def test_normal_workspace_captures_intermediate_commits(self):
        from makewand.git_helper import run_git_cmd
        from makewand.orchestrator import run_pipeline

        with tempfile.TemporaryDirectory() as base_tmp:
            run_git_cmd(["git", "init"], cwd=base_tmp)
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=base_tmp)
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=base_tmp)
            (Path(base_tmp) / "init.txt").write_text("hello")
            run_git_cmd(["git", "add", "-A"], cwd=base_tmp)
            run_git_cmd(["git", "commit", "-m", "init"], cwd=base_tmp)

            def mock_coder_commit(prompt, cwd=None, **kwargs):
                (Path(cwd) / "committed.py").write_text("print('committed in task')")
                run_git_cmd(["git", "add", "-A"], cwd=cwd)
                run_git_cmd(["git", "commit", "-m", "task commit"], cwd=cwd)
                return True, "Code committed", None

            recorded_diff = []
            def mock_reviewer(prompt, **kwargs):
                recorded_diff.append(prompt)
                return True, "LGTM\nMAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}", None

            with patch("makewand.orchestrator.check_working_tree_isolation", return_value=(True, None)), \
                 patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)), \
                 patch("makewand.orchestrator.get_or_update_status", return_value={"codex": {"status": "healthy"}, "claude": {"status": "healthy"}}), \
                 patch("makewand.orchestrator.execute_claude_task", side_effect=mock_coder_commit), \
                 patch("makewand.orchestrator.execute_codex_task", side_effect=mock_reviewer):
                res = run_pipeline("实现并提交任务", cwd=base_tmp, stream=False)
                self.assertTrue(res)
                self.assertTrue(len(recorded_diff) > 0)
                # Verify that prompt sent to reviewer contains the committed file
                self.assertIn("committed.py", recorded_diff[0])

    def test_delivery_script_injection_and_spaces(self):
        """
        Verify that apply_delivery.sh handles paths with spaces and prevents $() command injection.
        """
        import subprocess
        from makewand.git_helper import ShadowWorktreeResult, run_git_cmd
        from makewand.orchestrator import run_pipeline

        with tempfile.TemporaryDirectory() as base_tmp:
            # Create repo path with spaces and command substitution syntax
            injection_dir = Path(base_tmp) / "repo $(printf INJECTED) dir"
            injection_dir.mkdir(parents=True)
            run_git_cmd(["git", "init"], cwd=str(injection_dir))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(injection_dir))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(injection_dir))
            (injection_dir / "file.txt").write_text("initial")
            run_git_cmd(["git", "add", "-A"], cwd=str(injection_dir))
            run_git_cmd(["git", "commit", "-m", "init"], cwd=str(injection_dir))
            _, head_out, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(injection_dir))

            shadow_dir = Path(base_tmp) / "shadow dir with space"
            shadow_dir.mkdir()
            run_git_cmd(["git", "init"], cwd=str(shadow_dir))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(shadow_dir))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(shadow_dir))
            (shadow_dir / "file.txt").write_text("initial")
            run_git_cmd(["git", "add", "-A"], cwd=str(shadow_dir))
            run_git_cmd(["git", "commit", "-m", "baseline"], cwd=str(shadow_dir))
            _, base_commit, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(shadow_dir))

            shadow_res = ShadowWorktreeResult(
                str(shadow_dir),
                "makewand/test_space_branch",
                lambda: None,
                baseline_commit=base_commit.strip(),
                repo_head=head_out.strip(),
                repo_root=str(injection_dir),
                worktree_root=str(shadow_dir)
            )

            def mock_coder(prompt, cwd=None, **kwargs):
                (Path(cwd) / "file.txt").write_text("updated by task")
                return True, "Code updated", None

            art_base = Path("/tmp/makewand-artifacts")
            pre_dirs = set(art_base.glob("delivery_*")) if art_base.exists() else set()

            with patch("makewand.orchestrator.check_working_tree_isolation", return_value=(False, "Active session")), \
                 patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)), \
                 patch("makewand.orchestrator.create_ephemeral_shadow_worktree", return_value=shadow_res), \
                 patch("makewand.orchestrator.get_or_update_status", return_value={"codex": {"status": "healthy"}, "claude": {"status": "healthy"}}), \
                 patch("makewand.orchestrator.execute_claude_task", side_effect=mock_coder), \
                 patch("makewand.orchestrator.execute_codex_task", return_value=(True, "LGTM\nMAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}", None)):
                res = run_pipeline("修改代码更新文件并落盘", cwd=str(injection_dir), stream=False)
                self.assertTrue(res)

                # Locate the exact delivery artifact generated by this specific task
                new_dirs = [d for d in art_base.glob("delivery_*") if d not in pre_dirs]
                self.assertTrue(len(new_dirs) > 0)
                latest_art = sorted(new_dirs, key=lambda p: p.stat().st_mtime, reverse=True)[0]
                script = latest_art / "apply_delivery.sh"
                self.assertTrue(script.exists())

                # Execute apply_delivery.sh and verify it applies cleanly without command injection
                import os
                env = dict(os.environ)
                run_res = subprocess.run(["bash", str(script)], capture_output=True, text=True, env=env)
                self.assertEqual(run_res.returncode, 0, f"apply_delivery.sh failed: {run_res.stderr}")
                self.assertNotIn("INJECTED", run_res.stdout)
                # Verify that the target repo received the modification
                self.assertEqual((injection_dir / "file.txt").read_text(), "updated by task")

    def test_autofix_captures_actual_defects(self):
        """
        Verify that record_autofix_lesson captures the actual defect descriptions from the failed review.
        """
        from makewand.orchestrator import run_pipeline

        recorded_lessons = []
        def mock_record_lesson(keywords=None, issue=None, lesson=None):
            recorded_lessons.append({"keywords": keywords, "issue": issue, "lesson": lesson})

        call_count = [0]
        def mock_reviewer(prompt, **kwargs):
            call_count[0] += 1
            if call_count[0] == 1:
                return True, "发现严重并发死锁缺陷\nMAKEWAND_VERDICT: {\"pass\": false, \"defects\": [\"并发死锁与channel泄漏\"]}", None
            return True, "已修复\nMAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}", None

        with tempfile.TemporaryDirectory() as base_tmp:
            (Path(base_tmp) / "main.py").write_text("print('hello')")

            with patch("makewand.orchestrator.check_working_tree_isolation", return_value=(True, None)), \
                 patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)), \
                 patch("makewand.orchestrator.get_or_update_status", return_value={"codex": {"status": "healthy"}, "claude": {"status": "healthy"}}), \
                 patch("makewand.orchestrator.execute_claude_task", return_value=(True, "code written", None)), \
                 patch("makewand.orchestrator.execute_codex_task", side_effect=mock_reviewer), \
                 patch("makewand.orchestrator.get_git_diff", return_value="diff --git a/main.py b/main.py\n+print('fix')"), \
                 patch("makewand.memory.record_autofix_lesson", side_effect=mock_record_lesson):
                res = run_pipeline("实现用户数据转换", cwd=base_tmp, auto_fix=True, stream=False)
                self.assertTrue(res)
                self.assertEqual(len(recorded_lessons), 1)
                # Defect must contain the actual defect description flagged in round 1!
                self.assertIn("并发死锁与channel泄漏", recorded_lessons[0]["issue"])

    def test_delivery_submodule_rollback_handles_colons_and_reports_status(self):
        """
        Verify that apply_delivery.sh rollback handles submodule paths containing colons
        without corrupted splitting and reports rollback failure when reverse apply fails.
        """
        import subprocess
        from makewand.git_helper import ShadowWorktreeResult, run_git_cmd
        from makewand.orchestrator import run_pipeline

        with tempfile.TemporaryDirectory() as base_tmp:
            # Main repo
            main_repo = Path(base_tmp) / "main_repo"
            main_repo.mkdir()
            run_git_cmd(["git", "init"], cwd=str(main_repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(main_repo))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(main_repo))
            (main_repo / "main.txt").write_text("main v1\n")
            run_git_cmd(["git", "add", "-A"], cwd=str(main_repo))
            run_git_cmd(["git", "commit", "-m", "init"], cwd=str(main_repo))
            _, head_out, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(main_repo))

            # Shadow worktree
            shadow_dir = Path(base_tmp) / "shadow"
            shadow_dir.mkdir()
            run_git_cmd(["git", "init"], cwd=str(shadow_dir))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(shadow_dir))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(shadow_dir))
            (shadow_dir / "main.txt").write_text("main v1\n")
            run_git_cmd(["git", "add", "-A"], cwd=str(shadow_dir))
            run_git_cmd(["git", "commit", "-m", "baseline"], cwd=str(shadow_dir))
            _, base_commit, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(shadow_dir))

            # Create submodule with colon in directory name in both main repo and shadow worktree
            src_sub = main_repo / "lib:colon_dir"
            src_sub.mkdir(parents=True)
            run_git_cmd(["git", "init"], cwd=str(src_sub))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(src_sub))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(src_sub))
            (src_sub / "sub.txt").write_text("sub v1\n")
            run_git_cmd(["git", "add", "-A"], cwd=str(src_sub))
            run_git_cmd(["git", "commit", "-m", "init sub"], cwd=str(src_sub))
            _, sub_base_hash, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(src_sub))

            (main_repo / ".gitmodules").write_text('[submodule "lib:colon_dir"]\n\tpath = lib:colon_dir\n\turl = ./lib:colon_dir\n')
            run_git_cmd(["git", "add", "-A"], cwd=str(main_repo))
            run_git_cmd(["git", "commit", "-m", "add gitmodules"], cwd=str(main_repo))
            _, head_out, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(main_repo))

            dst_sub = shadow_dir / "lib:colon_dir"
            dst_sub.mkdir(parents=True)
            run_git_cmd(["git", "init"], cwd=str(dst_sub))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(dst_sub))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(dst_sub))
            (dst_sub / "sub.txt").write_text("sub v1\n")
            run_git_cmd(["git", "add", "-A"], cwd=str(dst_sub))
            run_git_cmd(["git", "commit", "-m", "init sub"], cwd=str(dst_sub))

            (shadow_dir / ".gitmodules").write_text('[submodule "lib:colon_dir"]\n\tpath = lib:colon_dir\n\turl = ./lib:colon_dir\n')
            run_git_cmd(["git", "add", "-A"], cwd=str(shadow_dir))
            run_git_cmd(["git", "commit", "-m", "add gitmodules"], cwd=str(shadow_dir))
            _, base_commit, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(shadow_dir))

            shadow_res = ShadowWorktreeResult(
                str(shadow_dir),
                "makewand/test_colon_branch",
                lambda: None,
                baseline_commit=base_commit.strip(),
                repo_head=head_out.strip(),
                repo_root=str(main_repo),
                worktree_root=str(shadow_dir),
                sub_baselines={"lib:colon_dir": sub_base_hash.strip()}
            )

            art_base = Path("/tmp/makewand-artifacts")
            pre_dirs = set(art_base.glob("delivery_*")) if art_base.exists() else set()

            def mock_coder(prompt, cwd=None, **kwargs):
                (Path(cwd) / "main.txt").write_text("main v2\n")
                (Path(cwd) / "lib:colon_dir" / "sub.txt").write_text("sub v2\n")
                return True, "Code updated", None

            with patch("makewand.orchestrator.check_working_tree_isolation", return_value=(False, "Active session")), \
                 patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)), \
                 patch("makewand.orchestrator.create_ephemeral_shadow_worktree", return_value=shadow_res), \
                 patch("makewand.orchestrator.get_or_update_status", return_value={"codex": {"status": "healthy"}, "claude": {"status": "healthy"}, "grok": {"status": "limited"}, "muse": {"status": "limited"}, "agy": {"status": "limited"}}), \
                 patch("makewand.orchestrator.execute_claude_task", side_effect=mock_coder), \
                 patch("makewand.orchestrator.execute_codex_task", return_value=(True, "LGTM\nMAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}", None)):
                res = run_pipeline("修改代码并实现功能提交交付", cwd=str(main_repo), stream=False)
                self.assertTrue(res)

                new_dirs = [d for d in art_base.glob("delivery_*") if d not in pre_dirs]
                self.assertTrue(len(new_dirs) > 0)
                latest_art = sorted(new_dirs, key=lambda p: p.stat().st_mtime, reverse=True)[0]
                script = latest_art / "apply_delivery.sh"
                self.assertTrue(script.exists())
                script_content = script.read_text(encoding="utf-8")
                # Must use index tracking rather than colon splitting!
                self.assertIn("APPLIED_SUB_INDICES=()", script_content)
                self.assertNotIn("IFS=\":\"", script_content)
                self.assertIn("ROLLBACK_FAILED=0", script_content)

if __name__ == "__main__":
    unittest.main()
