"""
Unit tests for terminal markdown renderer.
"""

import unittest
from makewand.markdown import render_terminal_markdown, highlight_code_line

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

if __name__ == "__main__":
    unittest.main()
