"""
Unit tests for terminal markdown renderer.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import unittest
from makewand.markdown import (
    render_terminal_markdown,
    highlight_code_line,
    display_width,
    pad_display,
    strip_ansi
)

class TestMarkdownRenderer(unittest.TestCase):
    def test_render_headers(self):
        text = "# Header 1\n## Header 2\n### Header 3"
        rendered = render_terminal_markdown(text)
        self.assertIn("Header 1", rendered)
        self.assertIn("Header 2", rendered)
        self.assertIn("Header 3", rendered)

    def test_render_code_block(self):
        text = "```python\ndef hello():\n    return 'world'\n```"
        rendered = render_terminal_markdown(text)
        self.assertIn("python", rendered)
        self.assertIn("hello", rendered)
        self.assertIn("world", rendered)

    def test_inline_formatting(self):
        text = "This is `code` and **bold** and *item*"
        rendered = render_terminal_markdown(text)
        self.assertIn("code", rendered)
        self.assertIn("bold", rendered)

    def test_highlight_code_line(self):
        line = "def process_data(value: int) -> bool:"
        hl = highlight_code_line(line)
        self.assertIn("def", hl)

    def test_empty_text(self):
        self.assertEqual(render_terminal_markdown(""), "")
        self.assertIsNone(render_terminal_markdown(None))

    def test_table_rendering(self):
        table_md = (
            "| Model | Tier | Status |\n"
            "| :--- | :---: | ---: |\n"
            "| Claude | standard | Active |\n"
            "| Codex | deep | Healthy |\n"
            "| 谷歌模型 | auto | 正常 |"
        )
        rendered = render_terminal_markdown(table_md)
        self.assertIn("Claude", rendered)
        self.assertIn("Codex", rendered)
        self.assertIn("谷歌模型", rendered)
        self.assertIn("┌", rendered)
        self.assertIn("├", rendered)
        self.assertIn("└", rendered)

    def test_alerts_rendering(self):
        alert_md = (
            "> [!NOTE]\n"
            "> This is an important note.\n"
            "> Please take heed."
        )
        rendered = render_terminal_markdown(alert_md)
        self.assertIn("NOTE", rendered)
        self.assertIn("This is an important note.", rendered)
        self.assertIn("╭─", rendered)
        self.assertIn("╰─", rendered)

        warn_md = "> [!WARNING] Danger ahead!"
        rendered_warn = render_terminal_markdown(warn_md)
        self.assertIn("WARNING", rendered_warn)
        self.assertIn("Danger ahead!", rendered_warn)

    def test_standard_blockquote(self):
        quote_md = "> Normal quoted text line"
        rendered = render_terminal_markdown(quote_md)
        self.assertIn("Normal quoted text line", rendered)
        self.assertIn("│", rendered)

    def test_display_width_and_padding(self):
        self.assertEqual(display_width("hello"), 5)
        self.assertEqual(display_width("测试"), 4)  # 2 full-width chars = 4
        padded = pad_display("test", 8, align="left")
        self.assertEqual(len(padded), 8)
        self.assertEqual(padded, "test    ")

if __name__ == "__main__":
    unittest.main()
