"""
Unit tests for Makewand Universal Tool Adaptation & Dynamic Active Tool Pool.
Validates dynamic topology adaptation:
- N == 0 (no tools)
- N == 1 (single tool resilient self-critique mode)
- N >= 2 (cross-model joint orchestration)
- Mainstream ecosystem tools (Aider CLI, DeepSeek API, Aliyun Qwen API, etc.)
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import os
import sys
from pathlib import Path

# Ensure repo root is on sys.path
REPO_ROOT = Path(__file__).resolve().parent.parent
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

import unittest
from unittest.mock import patch, MagicMock

from makewand.config import (
    ALL_SUPPORTED_PROVIDERS,
    get_all_supported_providers,
    normalize_provider_name,
    get_api_config,
    has_api_configured,
    has_subscription_configured,
    get_provider_execution_mode,
    get_active_providers,
    is_provider_enabled,
    set_provider_enabled
)
from makewand.orchestrator import (
    select_optimal_engine_pair,
    dispatch_task
)

class TestUniversalToolAdaptation(unittest.TestCase):

    def setUp(self):
        # Clean environment overrides before each test
        self.orig_env = os.environ.copy()

    def tearDown(self):
        os.environ.clear()
        os.environ.update(self.orig_env)

    def test_all_supported_providers_inventory(self):
        """Ensures all mainstream AI tools and ecosystems are registered."""
        all_tools = get_all_supported_providers()
        expected = [
            "claude", "codex", "agy", "grok", "muse", "aider", "cursor", "copilot",
            "deepseek", "qwen", "glm", "kimi", "openrouter", "siliconflow", "local"
        ]
        for tool in expected:
            self.assertIn(tool, all_tools, f"Missing tool {tool} in supported inventory")

    def test_provider_alias_normalization(self):
        """Verifies aliases map to standard canonical names."""
        self.assertEqual(normalize_provider_name("dashscope"), "qwen")
        self.assertEqual(normalize_provider_name("aliyun"), "qwen")
        self.assertEqual(normalize_provider_name("gemini"), "agy")
        self.assertEqual(normalize_provider_name("google"), "agy")
        self.assertEqual(normalize_provider_name("anthropic"), "claude")
        self.assertEqual(normalize_provider_name("openai"), "codex")
        self.assertEqual(normalize_provider_name("xai"), "grok")
        self.assertEqual(normalize_provider_name("meta"), "muse")
        self.assertEqual(normalize_provider_name("ollama"), "local")
        self.assertEqual(normalize_provider_name("zhipu"), "glm")
        self.assertEqual(normalize_provider_name("moonshot"), "kimi")
        self.assertEqual(normalize_provider_name("silicon"), "siliconflow")

    def test_dynamic_active_providers_detection(self):
        """Tests dynamic active tool pool detection based on real-time environment."""
        # Paid API providers only count once billing is explicitly allowed. The test
        # used to inherit allow_paid from a config.json leaked by test_hybrid_routing.
        with patch("makewand.config.has_subscription_configured") as mock_sub, \
             patch("makewand.config.has_api_configured") as mock_api, \
             patch("makewand.config.is_provider_enabled") as mock_en, \
             patch.dict(os.environ, {"MAKEWAND_API_POLICY": "allow_paid"}), \
             patch("makewand.providers.local.is_local_model_available", return_value=(False, None, None)):

            mock_en.return_value = True

            # Case A: User only logged into Claude
            mock_sub.side_effect = lambda p: p == "claude"
            mock_api.side_effect = lambda p: False
            active = get_active_providers()
            self.assertEqual(active, ["claude"])

            # Case B: User logged into Claude and Codex, and provided DEEPSEEK_API_KEY
            mock_sub.side_effect = lambda p: p in ("claude", "codex")
            mock_api.side_effect = lambda p: p == "deepseek"
            active = get_active_providers()
            self.assertIn("claude", active)
            self.assertIn("codex", active)
            self.assertIn("deepseek", active)
            self.assertNotIn("agy", active)

            # Case C: A tool is disabled by user -> excluded even if subscription present
            mock_en.side_effect = lambda p: p != "claude"
            active = get_active_providers()
            self.assertNotIn("claude", active)

    def test_single_tool_mode_resilience(self):
        """Tests that N=1 triggers resilient single-tool mode (self-critique) without failing."""
        with patch("makewand.config.get_active_providers") as mock_active, \
             patch("makewand.orchestrator.get_or_update_status") as mock_status:

            mock_active.return_value = ["claude"]
            mock_status.return_value = {"claude": {"status": "healthy"}}

            coders, reviewers, meta = select_optimal_engine_pair("Write a quicksort function")
            self.assertEqual(coders[0], "claude")
            self.assertEqual(reviewers[0], "claude")
            self.assertTrue(meta["single_tool_mode"])
            self.assertIn("单工具实现 + 独立沙箱自审闭条模式" if "单工具实现 + 独立沙箱自审闭条模式" in " ".join(meta["reasons"]) else "单工具", " ".join(meta["reasons"]))

    def test_multi_tool_cross_model_orchestration(self):
        """Tests that N>=2 pairs different models for cross-model red-team review."""
        with patch("makewand.config.get_active_providers") as mock_active, \
             patch("makewand.orchestrator.get_or_update_status") as mock_status:

            mock_active.return_value = ["claude", "codex", "agy"]
            mock_status.return_value = {
                "claude": {"status": "healthy"},
                "codex": {"status": "healthy"},
                "agy": {"status": "healthy"}
            }

            coders, reviewers, meta = select_optimal_engine_pair("Refactor user model and write tests")
            self.assertFalse(meta["single_tool_mode"])
            self.assertNotEqual(coders[0], reviewers[0], "Coder and Reviewer must be different in multi-tool mode")
            self.assertIn(coders[0], ["claude", "codex", "agy"])
            self.assertIn(reviewers[0], ["claude", "codex", "agy"])

    def test_dispatch_task_aider(self):
        """Tests dispatching tasks to Aider CLI."""
        with patch("makewand.orchestrator.execute_aider_task") as mock_aider:
            mock_aider.return_value = (True, "Aider completed edits", None)
            ok, out, err = dispatch_task("aider", "Fix bug in app.py", cwd="/tmp")
            self.assertTrue(ok)
            self.assertEqual(out, "Aider completed edits")
            mock_aider.assert_called_once()

    def test_dispatch_task_deepseek_api(self):
        """Tests dispatching tasks to DeepSeek cloud API."""
        with patch("makewand.providers.api_client.call_api_chat") as mock_api:
            mock_api.return_value = (True, "DeepSeek analysis passed", None)
            ok, out, err = dispatch_task("deepseek", "Analyze complexity", cwd="/tmp")
            self.assertTrue(ok)
            self.assertEqual(out, "DeepSeek analysis passed")
            mock_api.assert_called_once()
            args, kwargs = mock_api.call_args
            provider = kwargs.get("provider") or (args[0] if args else None)
            self.assertEqual(provider, "deepseek")

    def test_dispatch_task_qwen_api(self):
        """Tests dispatching tasks to Aliyun Qwen cloud API."""
        with patch("makewand.providers.api_client.call_api_chat") as mock_api:
            mock_api.return_value = (True, "Qwen response generated", None)
            ok, out, err = dispatch_task("qwen", "Generate SQL query", cwd="/tmp")
            self.assertTrue(ok)
            self.assertEqual(out, "Qwen response generated")
            mock_api.assert_called_once()
            args, kwargs = mock_api.call_args
            provider = kwargs.get("provider") or (args[0] if args else None)
            self.assertEqual(provider, "qwen")

    def test_cli_enable_disable_universal(self):
        """Tests enabling and disabling any of the supported providers."""
        with patch("makewand.config.save_user_config") as mock_save, \
             patch("makewand.config.load_user_config") as mock_load:
            mock_load.return_value = {"enabled_providers": {}}
            mock_save.return_value = True

            # Test enable deepseek
            ok = set_provider_enabled("deepseek", True)
            self.assertTrue(ok)
            mock_save.assert_called()

            # Test disable qwen
            ok = set_provider_enabled("qwen", False)
            self.assertTrue(ok)

            # Test invalid tool name returns False
            ok = set_provider_enabled("invalid_tool_xyz", True)
            self.assertFalse(ok)

    def test_apply_agentic_code_output_file_blocks(self):
        """Tests extracting and writing files from LLM code block outputs."""
        import tempfile
        from makewand.providers.api_client import apply_agentic_code_output

        with tempfile.TemporaryDirectory() as tmp_dir:
            sample_output = (
                "Here is the implementation:\n\n"
                "```filepath: src/calc.py\n"
                "def add(a, b):\n"
                "    return a + b\n"
                "```\n\n"
                "And here is another file:\n\n"
                "### `tests/test_calc.py`\n"
                "```python\n"
                "from src.calc import add\n"
                "assert add(1, 2) == 3\n"
                "```\n"
            )
            modified = apply_agentic_code_output(sample_output, tmp_dir)
            self.assertEqual(len(modified), 2)
            self.assertIn("src/calc.py", modified)
            self.assertIn("tests/test_calc.py", modified)

            calc_path = Path(tmp_dir) / "src" / "calc.py"
            test_path = Path(tmp_dir) / "tests" / "test_calc.py"
            self.assertTrue(calc_path.exists())
            self.assertTrue(test_path.exists())
            self.assertIn("def add(a, b):", calc_path.read_text())
            self.assertIn("assert add(1, 2) == 3", test_path.read_text())

    def test_apply_agentic_code_output_security_traversal_blocked(self):
        """Ensures directory traversal and .git paths are strictly blocked."""
        import tempfile
        from makewand.providers.api_client import apply_agentic_code_output

        with tempfile.TemporaryDirectory() as tmp_dir:
            malicious_output = (
                "```filepath: ../../../escape.txt\nevil\n```\n"
                "```filepath: .git/config\nmalicious\n```\n"
                "```filepath: /etc/passwd\nroot\n```\n"
            )
            modified = apply_agentic_code_output(malicious_output, tmp_dir)
            self.assertEqual(len(modified), 0)
            self.assertFalse((Path(tmp_dir) / "escape.txt").exists())
            self.assertFalse((Path(tmp_dir) / ".git" / "config").exists())

    def test_apply_agentic_code_output_hardlink_and_directory_safety(self):
        """Ensures atomic write decouples hardlinks without truncating external inodes, and rejects directory overwrite."""
        import tempfile
        from makewand.providers.api_client import apply_agentic_code_output

        with tempfile.TemporaryDirectory() as tmp_dir:
            # 1. Test directory protection
            sub_dir = Path(tmp_dir) / "somedir"
            sub_dir.mkdir()
            dir_output = "```filepath: somedir\nnot allowed\n```\n"
            mod1 = apply_agentic_code_output(dir_output, tmp_dir)
            self.assertEqual(len(mod1), 0)
            self.assertTrue(sub_dir.is_dir())

            # 2. Test hardlink decoupling (defense against F04 in-place truncation)
            external_dir = tempfile.mkdtemp()
            try:
                external_file = Path(external_dir) / "important_host.conf"
                external_file.write_text("HOST_CONFIG_ORIGINAL")

                target_file = Path(tmp_dir) / "linked.conf"
                os.link(str(external_file), str(target_file))

                # Verify they share the same inode initially
                self.assertEqual(os.stat(external_file).st_ino, os.stat(target_file).st_ino)

                # Overwrite via apply_agentic_code_output
                hardlink_output = "```filepath: linked.conf\nNEW_AGENT_CODE\n```\n"
                mod2 = apply_agentic_code_output(hardlink_output, tmp_dir)
                self.assertIn("linked.conf", mod2)

                # The workspace file has the new content
                self.assertEqual(target_file.read_text(), "NEW_AGENT_CODE\n")
                # The external hardlinked file MUST remain untouched!
                self.assertEqual(external_file.read_text(), "HOST_CONFIG_ORIGINAL")
                # And their inodes must now be decoupled!
                self.assertNotEqual(os.stat(external_file).st_ino, os.stat(target_file).st_ino)
            finally:
                import shutil
                shutil.rmtree(external_dir, ignore_errors=True)

    def test_local_only_and_provider_override(self):
        """Tests local_only and forced_engine routing behavior."""
        from makewand.orchestrator import _run_pipeline_impl
        with patch("makewand.orchestrator.dispatch_task") as mock_dispatch, \
             patch("makewand.orchestrator.PipelineWorkspaceGuard.acquire_workspace_lock", return_value=None):
            mock_dispatch.return_value = (True, "mock explanation", None)
            ok = _run_pipeline_impl("只解释原理，不用修改文件", local_only=True)
            self.assertTrue(ok)
            mock_dispatch.assert_called()
            args, kwargs = mock_dispatch.call_args
            self.assertEqual(args[0], "local")

    def test_execute_agentic_tool_batch(self):
        """Tests executing a batch of tool actions (write_file, read_file, run_command)."""
        import tempfile
        from makewand.providers.api_client import execute_agentic_tool_batch

        with tempfile.TemporaryDirectory() as tmp_dir:
            actions = [
                {"action": "write_file", "path": "pkg/mod.py", "content": "x = 42\n"},
                {"action": "read_file", "path": "pkg/mod.py"},
                {"action": "run_command", "cmd": "echo batch_command_success"},
            ]
            results = execute_agentic_tool_batch(actions, tmp_dir)
            self.assertEqual(len(results), 3)

            # 1. write_file result
            self.assertEqual(results[0]["status"], "ok")
            self.assertEqual(results[0]["path"], "pkg/mod.py")
            self.assertTrue((Path(tmp_dir) / "pkg" / "mod.py").exists())

            # 2. read_file result
            self.assertEqual(results[1]["status"], "ok")
            self.assertEqual(results[1]["content"], "x = 42\n")

            # 3. run_command result
            self.assertEqual(results[2]["status"], "ok")
            self.assertIn("batch_command_success", results[2]["stdout"])

    def test_apply_agentic_code_output_tool_batch(self):
        """Tests parsing tool batch JSON block inside apply_agentic_code_output."""
        import tempfile
        from makewand.providers.api_client import apply_agentic_code_output

        with tempfile.TemporaryDirectory() as tmp_dir:
            batch_output = (
                "Here is the batch execution plan:\n\n"
                "```json:makewand-tools\n"
                "[\n"
                '  {"action": "write_file", "path": "services/user.py", "content": "class User: pass\\n"},\n'
                '  {"action": "write_file", "path": "services/auth.py", "content": "class Auth: pass\\n"}\n'
                "]\n"
                "```\n"
            )
            modified = apply_agentic_code_output(batch_output, tmp_dir)
            self.assertEqual(len(modified), 2)
            self.assertIn("services/user.py", modified)
            self.assertIn("services/auth.py", modified)
            self.assertTrue((Path(tmp_dir) / "services" / "user.py").exists())
            self.assertTrue((Path(tmp_dir) / "services" / "auth.py").exists())


if __name__ == "__main__":
    unittest.main()


