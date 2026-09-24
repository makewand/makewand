"""
Terminal Markdown Renderer for Makewand.
Provides clean ANSI syntax highlighting and structured typography for CLI output
without requiring heavy external dependencies.
"""

import sys
import re
from typing import List, Set

from makewand.config import (
    c,
    supports_color,
    COLOR_BOLD,
    COLOR_CYAN,
    COLOR_GREEN,
    COLOR_YELLOW,
    COLOR_RED,
    COLOR_BLUE,
    COLOR_PURPLE,
    COLOR_RESET
)

COLOR_DIM = "\033[2m"
COLOR_GRAY = "\033[90m"

COMMON_KEYWORDS: Set[str] = {
    # Python
    "def", "class", "import", "from", "return", "if", "elif", "else", "for", "while",
    "try", "except", "finally", "with", "as", "lambda", "yield", "async", "await", "pass", "raise",
    # JavaScript / TypeScript
    "function", "const", "let", "var", "export", "default", "new", "this", "throw", "typeof",
    # Go / Rust / C / Java
    "func", "package", "type", "struct", "interface", "public", "private", "protected",
    "fn", "mut", "impl", "pub", "trait", "switch", "case", "break", "continue",
    # Literals
    "true", "false", "True", "False", "None", "nil", "null", "undefined"
}

def highlight_code_line(line: str) -> str:
    """Highlights syntax in a single code line."""
    stripped = line.strip()
    # Comments: Python / Shell (#) or C / JS / Go (//)
    if stripped.startswith("#") or stripped.startswith("//"):
        return f"{COLOR_GRAY}{line}{COLOR_RESET}"

    # Strings: double or single quoted
    def replace_str(m):
        return f"{COLOR_GREEN}{m.group(0)}{COLOR_RESET}"
    line = re.sub(r'("[^"]*"|\'[^\']*\')', replace_str, line)

    # Keywords
    def replace_kw(m):
        word = m.group(0)
        if word in COMMON_KEYWORDS:
            return f"{COLOR_PURPLE}{COLOR_BOLD}{word}{COLOR_RESET}"
        return word
    line = re.sub(r'\b[a-zA-Z_][a-zA-Z0-9_]*\b', replace_kw, line)

    # Numbers
    def replace_num(m):
        return f"{COLOR_YELLOW}{m.group(0)}{COLOR_RESET}"
    line = re.sub(r'\b\d+(?:\.\d+)?\b', replace_num, line)

    return line

def render_terminal_markdown(text: str) -> str:
    """
    Renders GitHub-flavored Markdown text with terminal-friendly ANSI formatting.
    """
    if not text or not supports_color():
        return text

    lines = text.splitlines()
    output: List[str] = []
    in_code_block = False
    code_lang = ""

    for line in lines:
        # Code block fence: ```lang
        if line.startswith("```"):
            if not in_code_block:
                in_code_block = True
                code_lang = line[3:].strip() or "code"
                output.append(f"{COLOR_GRAY}── [{COLOR_CYAN}{code_lang}{COLOR_RESET}{COLOR_GRAY}] " + ("─" * 45) + f"{COLOR_RESET}")
            else:
                in_code_block = False
                output.append(f"{COLOR_GRAY}" + ("─" * 55) + f"{COLOR_RESET}")
            continue

        if in_code_block:
            output.append("  " + highlight_code_line(line))
            continue

        # Headers
        if line.startswith("# "):
            output.append(f"\n{COLOR_CYAN}{COLOR_BOLD}=== {line[2:].strip()} ==={COLOR_RESET}")
            continue
        elif line.startswith("## "):
            output.append(f"\n{COLOR_PURPLE}{COLOR_BOLD}▶ {line[3:].strip()}{COLOR_RESET}")
            continue
        elif line.startswith("### "):
            output.append(f"\n{COLOR_YELLOW}{COLOR_BOLD}• {line[4:].strip()}{COLOR_RESET}")
            continue

        # Horizontal rule
        if line.strip() in ("---", "***", "___"):
            output.append(f"{COLOR_GRAY}" + ("─" * 50) + f"{COLOR_RESET}")
            continue

        # Inline formatting for regular text
        formatted = line

        # Inline code: `code`
        formatted = re.sub(r'`([^`]+)`', rf'{COLOR_YELLOW}\1{COLOR_RESET}', formatted)

        # Bold: **text**
        formatted = re.sub(r'\*\*([^*]+)\*\*', rf'{COLOR_BOLD}\1{COLOR_RESET}', formatted)

        # Bullet points: - item or * item
        formatted = re.sub(r'^(\s*)[-*]\s+', rf'\1{COLOR_CYAN}• {COLOR_RESET}', formatted)

        # Numbered lists: 1. item
        formatted = re.sub(r'^(\s*)(\d+)\.\s+', rf'\1{COLOR_CYAN}\2.{COLOR_RESET} ', formatted)

        output.append(formatted)

    return "\n".join(output)
