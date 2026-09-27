"""
Unit tests for Makewand Interactive Console / REPL.
Verifies REPL slash commands (/help, /status, /quota, /model, /provider, /clear, /compact, /tier),
display width formatting (pad_display), and conversation history sliding window truncation.
"""

import io
import os
import unittest
from pathlib import Path
from unittest.mock import patch, MagicMock

from makewand.interactive import (
    render_welcome_card,
    get_git_branch,
    format_short_path,
    get_model_status_badges,
    handle_conversational_turn,
    start_interactive_session,
    SLASH_COMMANDS,
    pad_display,
)
from makewand.markdown import display_width


class TestInteractiveConsole(unittest.TestCase):
    def test_render_welcome_card(self):
        cwd = os.getcwd()
        card = render_welcome_card(cwd, width=66)
        self.assertIn("╭", card)
        self.assertIn("╰", card)
        self.assertIn("│", card)
        self.assertIn("Makewand (v3.1.0)", card)
        self.assertIn("/help", card)

    def test_get_git_branch(self):
        cwd = os.getcwd()
        branch = get_git_branch(cwd)
        # Should be master on this repo
        self.assertIsNotNone(branch)
        self.assertEqual(branch, "master")

    def test_format_short_path(self):
        home = str(Path.home())
        test_p = os.path.join(home, "dev", "project")
        short = format_short_path(test_p)
        self.assertEqual(short, "~/dev/project")

        non_home = "/var/log/test"
        self.assertEqual(format_short_path(non_home), "/var/log/test")

    def test_slash_commands_presence(self):
        expected = ["/help", "/status", "/diff", "/review", "/compact", "/clear", "/exit"]
        for cmd in expected:
            self.assertIn(cmd, SLASH_COMMANDS)

    def test_model_status_badges(self):
        mock_cache = {
            "agy": {"status": "healthy"},
            "claude": {"status": "limited"},
            "codex": {"status": "limited"},
        }
        badges = get_model_status_badges(mock_cache)
        self.assertIn("AGY", badges)
        self.assertIn("Claude", badges)
        self.assertIn("Codex", badges)


class TestPadDisplay(unittest.TestCase):
    """Tests for visual display formatting and character alignment."""

    def test_pad_display_left_ascii(self):
        text = "hello"
        padded = pad_display(text, 10, align="left")
        self.assertEqual(len(padded), 10)
        self.assertEqual(padded, "hello     ")

    def test_pad_display_right(self):
        text = "hello"
        padded = pad_display(text, 10, align="right")
        self.assertEqual(len(padded), 10)
        self.assertEqual(padded, "     hello")

    def test_pad_display_center(self):
        text = "hello"
        padded = pad_display(text, 11, align="center")
        self.assertEqual(len(padded), 11)
        self.assertEqual(padded, "   hello   ")

    def test_pad_display_east_asian_characters(self):
        text = "你好世界"
        # 4 full-width chars = 8 display units
        self.assertEqual(display_width(text), 8)
        padded = pad_display(text, 12, align="left")
        self.assertEqual(display_width(padded), 12)
        self.assertEqual(padded, "你好世界    ")

    def test_pad_display_with_ansi_codes(self):
        text = "\033[32mstatus\033[0m"
        # ANSI sequence has 0 display width, "status" has 6 display width
        self.assertEqual(display_width(text), 6)
        padded = pad_display(text, 10, align="left")
        self.assertEqual(display_width(padded), 10)
        self.assertTrue(padded.endswith("    "))

    def test_pad_display_overflow_no_truncate(self):
        text = "longer_than_target_width"
        padded = pad_display(text, 5, align="left")
        self.assertEqual(padded, text)

    def test_pad_display_zero_and_negative_width(self):
        text = "abc"
        self.assertEqual(pad_display(text, 0), "abc")
        self.assertEqual(pad_display(text, -3), "abc")
        self.assertEqual(pad_display("", 5), "     ")


class TestConversationHistorySlidingWindow(unittest.TestCase):
    """Tests for conversation history sliding window truncation."""

    @patch("makewand.interactive.dispatch_task")
    @patch("makewand.interactive.select_optimal_engine_pair")
    @patch("makewand.interactive.get_or_update_status")
    def test_sliding_window_caps_at_6_turns(self, mock_status, mock_select, mock_dispatch):
        mock_status.return_value = {}
        mock_select.return_value = (["codex"], ["agy"], {"primary_coder": "codex"})
        mock_dispatch.return_value = (True, "Answer for turn 11", None)

        # Create history with 10 turns (20 messages)
        history = []
        for i in range(1, 11):
            history.append({"role": "user", "content": f"Turn {i} question"})
            history.append({"role": "assistant", "content": f"Turn {i} answer"})

        handle_conversational_turn(
            user_input="Turn 11 question",
            conversation_history=history,
            cwd="/tmp",
        )

        dispatched_prompt = mock_dispatch.call_args[0][1]
        self.assertIn("【前序会话上下文】", dispatched_prompt)
        # Turns 1 to 7 should have been truncated out (only last 6 messages: turns 8-10)
        self.assertNotIn("Turn 1 question", dispatched_prompt)
        self.assertNotIn("Turn 5 answer", dispatched_prompt)
        self.assertNotIn("Turn 7 question", dispatched_prompt)
        self.assertIn("Turn 8 question", dispatched_prompt)
        self.assertIn("Turn 10 answer", dispatched_prompt)
        self.assertIn("【用户最新问题】\nTurn 11 question", dispatched_prompt)

        # Verify new turn was appended to history
        self.assertEqual(history[-2]["content"], "Turn 11 question")
        self.assertEqual(history[-1]["content"], "Answer for turn 11")

    @patch("makewand.interactive.dispatch_task")
    @patch("makewand.interactive.select_optimal_engine_pair")
    @patch("makewand.interactive.get_or_update_status")
    def test_sliding_window_caps_at_1500_chars(self, mock_status, mock_select, mock_dispatch):
        mock_status.return_value = {}
        mock_select.return_value = (["codex"], ["agy"], {"primary_coder": "codex"})
        mock_dispatch.return_value = (True, "Answer", None)

        long_text = "A" * 2000
        history = [
            {"role": "user", "content": long_text},
            {"role": "assistant", "content": "short answer"},
        ]

        handle_conversational_turn(
            user_input="Next question",
            conversation_history=history,
            cwd="/tmp",
        )

        dispatched_prompt = mock_dispatch.call_args[0][1]
        self.assertIn("A" * 1500 + "... [历史截断]", dispatched_prompt)
        self.assertNotIn("A" * 1501, dispatched_prompt)

    @patch("makewand.interactive.dispatch_task")
    @patch("makewand.interactive.select_optimal_engine_pair")
    @patch("makewand.interactive.get_or_update_status")
    def test_sliding_window_empty_history(self, mock_status, mock_select, mock_dispatch):
        mock_status.return_value = {}
        mock_select.return_value = (["codex"], ["agy"], {"primary_coder": "codex"})
        mock_dispatch.return_value = (True, "Answer", None)

        history = []
        handle_conversational_turn(
            user_input="First question",
            conversation_history=history,
            cwd="/tmp",
        )

        dispatched_prompt = mock_dispatch.call_args[0][1]
        self.assertNotIn("【前序会话上下文】", dispatched_prompt)
        self.assertEqual(dispatched_prompt, "First question")

    @patch("makewand.interactive.dispatch_task")
    @patch("makewand.interactive.select_optimal_engine_pair")
    @patch("makewand.interactive.get_or_update_status")
    def test_sliding_window_1500_char_boundary(self, mock_status, mock_select, mock_dispatch):
        mock_status.return_value = {}
        mock_select.return_value = (["codex"], ["agy"], {"primary_coder": "codex"})
        mock_dispatch.return_value = (True, "Answer", None)

        # Exactly 1500 chars -> should not append truncation marker
        exact_1500 = "B" * 1500
        history_exact = [{"role": "user", "content": exact_1500}]
        handle_conversational_turn(user_input="q", conversation_history=history_exact, cwd="/tmp")
        prompt_exact = mock_dispatch.call_args[0][1]
        self.assertNotIn("[历史截断]", prompt_exact)
        self.assertIn(exact_1500, prompt_exact)

        # 1501 chars -> should append truncation marker
        over_1500 = "C" * 1501
        history_over = [{"role": "user", "content": over_1500}]
        handle_conversational_turn(user_input="q", conversation_history=history_over, cwd="/tmp")
        prompt_over = mock_dispatch.call_args[0][1]
        self.assertIn("C" * 1500 + "... [历史截断]", prompt_over)


class TestSlashCommandHandling(unittest.TestCase):
    """Tests for REPL slash command routing and execution."""

    def setUp(self):
        # Slash routing must neither inspect live provider state nor register
        # history writes against the developer's real home directory.
        for target, value in (("render_welcome_card", "Makewand"), ("setup_readline", None)):
            replacement = patch("makewand.interactive." + target, return_value=value)
            replacement.start()
            self.addCleanup(replacement.stop)

    @patch("sys.stdout", new_callable=io.StringIO)
    @patch("builtins.input", side_effect=["/help", "/?", "/exit"])
    def test_help_slash_commands(self, mock_input, mock_stdout):
        start_interactive_session()
        out = mock_stdout.getvalue()
        self.assertIn("Commands:", out)
        self.assertIn("/help, /?", out)
        self.assertIn("/diff", out)

    @patch("makewand.cli.cmd_status")
    @patch("builtins.input", side_effect=["/status", "/quota", "/exit"])
    def test_status_and_quota_commands(self, mock_input, mock_status):
        start_interactive_session()
        self.assertEqual(mock_status.call_count, 2)
        for call_arg in mock_status.call_args_list:
            self.assertFalse(call_arg[0][0].probe)

    @patch("sys.stdout", new_callable=io.StringIO)
    @patch("builtins.input", side_effect=["/model", "/model codex", "/provider grok", "/provider invalid_eng", "/exit"])
    def test_model_and_provider_commands(self, mock_input, mock_stdout):
        start_interactive_session()
        out = mock_stdout.getvalue()
        self.assertIn("当前锁定引擎: 自动智能路由 (auto)", out)
        self.assertIn("交互会话已指定锁定引擎: CODEX", out)
        self.assertIn("交互会话已指定锁定引擎: GROK", out)
        self.assertIn("未知引擎 'invalid_eng'", out)

    @patch("os.system")
    @patch("builtins.input", side_effect=["/clear", "/exit"])
    def test_clear_command(self, mock_input, mock_system):
        start_interactive_session()
        mock_system.assert_called_once()
        cmd_arg = mock_system.call_args[0][0]
        self.assertIn(cmd_arg, ("clear", "cls"))

    @patch("sys.stdout", new_callable=io.StringIO)
    @patch("builtins.input", side_effect=["/compact", "/tier fast", "/tier balanced", "/tier power", "/tier invalid", "/exit"])
    def test_compact_and_tier_commands(self, mock_input, mock_stdout):
        start_interactive_session()
        out = mock_stdout.getvalue()
        self.assertIn("当前会话暂无历史上下文，无需压缩。", out)
        self.assertIn("推理档位已切换为: fast", out)
        self.assertIn("推理档位已切换为: standard", out)
        self.assertIn("推理档位已切换为: deep", out)
        self.assertIn("当前推理档位: deep (可选: auto, fast, standard, deep)", out)

    @patch("sys.stdout", new_callable=io.StringIO)
    @patch("builtins.input", side_effect=["/mode", "/mode fast", "/mode power", "/mode balanced", "/exit"])
    def test_mode_slash_commands(self, mock_input, mock_stdout):
        start_interactive_session()
        out = mock_stdout.getvalue()
        self.assertIn("当前推理档位: auto (可选: auto, fast, standard, deep)", out)
        self.assertIn("推理档位已切换为: fast", out)
        self.assertIn("推理档位已切换为: deep", out)
        self.assertIn("推理档位已切换为: standard", out)

    @patch("makewand.cli.cmd_models")
    @patch("sys.stdout", new_callable=io.StringIO)
    @patch("builtins.input", side_effect=["/models", "/model", "/exit"])
    def test_model_vs_models_distinction(self, mock_input, mock_stdout, mock_cmd_models):
        start_interactive_session()
        mock_cmd_models.assert_called_once()
        out = mock_stdout.getvalue()
        self.assertIn("当前锁定引擎: 自动智能路由 (auto)", out)


class TestModeContract(unittest.TestCase):
    """Tests for bi-directional mode and tier normalization in Python."""

    def test_normalize_tier_and_tier_to_go_mode(self):
        from makewand.config import normalize_tier, tier_to_go_mode
        self.assertEqual(normalize_tier("fast"), "fast")
        self.assertEqual(normalize_tier("standard"), "standard")
        self.assertEqual(normalize_tier("deep"), "deep")
        self.assertEqual(normalize_tier("balanced"), "standard")
        self.assertEqual(normalize_tier("power"), "deep")
        self.assertEqual(normalize_tier("auto"), "auto")
        self.assertEqual(normalize_tier("unknown"), "standard")
        self.assertEqual(normalize_tier(None), "standard")

        self.assertEqual(tier_to_go_mode("fast"), "fast")
        self.assertEqual(tier_to_go_mode("standard"), "balanced")
        self.assertEqual(tier_to_go_mode("deep"), "power")
        self.assertEqual(tier_to_go_mode("balanced"), "balanced")
        self.assertEqual(tier_to_go_mode("power"), "power")


if __name__ == "__main__":
    unittest.main()
