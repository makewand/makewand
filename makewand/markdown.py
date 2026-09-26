"""
Terminal Markdown Renderer for Makewand.
Provides clean ANSI syntax highlighting and structured typography for CLI output
without requiring heavy external dependencies.
Supports GitHub-style alerts and column-aligned tables.
"""

import sys
import re
import unicodedata
from typing import List, Set, Tuple, Optional

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

ALERT_STYLES = {
    "NOTE": (COLOR_CYAN, "💡 NOTE"),
    "TIP": (COLOR_GREEN, "💡 TIP"),
    "IMPORTANT": (COLOR_PURPLE, "📌 IMPORTANT"),
    "WARNING": (COLOR_YELLOW, "⚠️  WARNING"),
    "CAUTION": (COLOR_RED, "🚨 CAUTION"),
}

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

def strip_ansi(text: str) -> str:
    """Strips ANSI escape sequences from text for width calculation."""
    return re.sub(r'\033\[[0-9;]*[a-zA-Z]', '', text)

def display_width(text: str) -> int:
    """Calculates visible display width in terminal, taking East Asian wide characters into account."""
    clean = strip_ansi(text)
    w = 0
    for ch in clean:
        if unicodedata.east_asian_width(ch) in ('F', 'W'):
            w += 2
        else:
            w += 1
    return w

def pad_display(text: str, target_width: int, align: str = 'left') -> str:
    """Pads text to target display width respecting ANSI escapes and character width."""
    w = display_width(text)
    pad = max(0, target_width - w)
    if align == 'right':
        return (' ' * pad) + text
    elif align == 'center':
        left_pad = pad // 2
        right_pad = pad - left_pad
        return (' ' * left_pad) + text + (' ' * right_pad)
    else:  # left
        return text + (' ' * pad)

def format_inline(text: str) -> str:
    """Applies inline Markdown formatting (code, bold, italic)."""
    # Inline code: `code`
    formatted = re.sub(r'`([^`]+)`', rf'{COLOR_YELLOW}\1{COLOR_RESET}', text)
    # Bold: **text**
    formatted = re.sub(r'\*\*([^*]+)\*\*', rf'{COLOR_BOLD}\1{COLOR_RESET}', formatted)
    # Italic: *text* (when not part of **)
    formatted = re.sub(r'(?<!\*)\*([^*]+)\*(?!\*)', rf'{COLOR_DIM}\1{COLOR_RESET}', formatted)
    return formatted

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

def is_table_divider(line: str) -> bool:
    """Checks if a line matches markdown table divider (|---|:---:|---:|)."""
    stripped = line.strip()
    if not stripped or '|' not in stripped:
        return False
    parts = [p.strip() for p in stripped.strip('|').split('|')]
    if not parts:
        return False
    for p in parts:
        if not re.match(r'^:?-+:?$', p):
            return False
    return True

def get_column_align(div_cell: str) -> str:
    """Determines column alignment from divider cell syntax."""
    cell = div_cell.strip()
    if cell.startswith(':') and cell.endswith(':'):
        return 'center'
    elif cell.endswith(':'):
        return 'right'
    else:
        return 'left'

def render_table(header_line: str, divider_line: str, row_lines: List[str]) -> List[str]:
    """Renders a markdown table with unicode box drawing characters and aligned columns."""
    raw_headers = [c.strip() for c in header_line.strip().strip('|').split('|')]
    raw_divs = [c.strip() for c in divider_line.strip().strip('|').split('|')]
    aligns = [get_column_align(d) for d in raw_divs]

    raw_rows = []
    for r in row_lines:
        cells = [c.strip() for c in r.strip().strip('|').split('|')]
        raw_rows.append(cells)

    num_cols = max(len(raw_headers), len(aligns), max((len(r) for r in raw_rows), default=0))
    if num_cols == 0:
        return []

    while len(raw_headers) < num_cols:
        raw_headers.append("")
    while len(aligns) < num_cols:
        aligns.append("left")
    for r in raw_rows:
        while len(r) < num_cols:
            r.append("")

    col_widths = []
    for c_idx in range(num_cols):
        w = display_width(raw_headers[c_idx])
        for r in raw_rows:
            w = max(w, display_width(r[c_idx]))
        col_widths.append(max(w, 3))

    res: List[str] = []
    # Top border
    top_b = f"{COLOR_GRAY}┌" + "┬".join("─" * (w + 2) for w in col_widths) + f"┐{COLOR_RESET}"
    res.append(top_b)

    # Headers
    h_cells = []
    for idx, h in enumerate(raw_headers):
        formatted_h = format_inline(h)
        padded_h = pad_display(f"{COLOR_BOLD}{COLOR_CYAN}{formatted_h}{COLOR_RESET}", col_widths[idx], aligns[idx])
        h_cells.append(f" {padded_h} ")
    header_row = f"{COLOR_GRAY}│{COLOR_RESET}" + f"{COLOR_GRAY}│{COLOR_RESET}".join(h_cells) + f"{COLOR_GRAY}│{COLOR_RESET}"
    res.append(header_row)

    # Divider
    mid_b = f"{COLOR_GRAY}├" + "┼".join("─" * (w + 2) for w in col_widths) + f"┤{COLOR_RESET}"
    res.append(mid_b)

    # Rows
    for r in raw_rows:
        r_cells = []
        for idx, cell in enumerate(r):
            formatted_c = format_inline(cell)
            padded_c = pad_display(f"{formatted_c}{COLOR_RESET}", col_widths[idx], aligns[idx])
            r_cells.append(f" {padded_c} ")
        row_str = f"{COLOR_GRAY}│{COLOR_RESET}" + f"{COLOR_GRAY}│{COLOR_RESET}".join(r_cells) + f"{COLOR_GRAY}│{COLOR_RESET}"
        res.append(row_str)

    # Bottom border
    bot_b = f"{COLOR_GRAY}└" + "┴".join("─" * (w + 2) for w in col_widths) + f"┘{COLOR_RESET}"
    res.append(bot_b)

    return res

def render_alert(alert_type: str, body_lines: List[str]) -> List[str]:
    """Renders GitHub-style alert callout box with rounded corners and distinct color badge."""
    color, badge = ALERT_STYLES.get(alert_type, (COLOR_CYAN, f"ℹ️  {alert_type}"))
    res: List[str] = []
    box_width = 56
    res.append(f"{color}╭─ {badge} " + ("─" * max(2, box_width - display_width(badge) - 4)) + f"{COLOR_RESET}")
    for bl in body_lines:
        if bl.strip():
            res.append(f"{color}│{COLOR_RESET}  {format_inline(bl)}")
        else:
            res.append(f"{color}│{COLOR_RESET}")
    res.append(f"{color}╰" + ("─" * box_width) + f"{COLOR_RESET}")
    return res

def render_terminal_markdown(text: str) -> str:
    """
    Renders GitHub-flavored Markdown text with terminal-friendly ANSI formatting.
    Supports code blocks, headers, bullet/numbered lists, tables, and alerts.
    """
    if text is None:
        return None
    if not text:
        return ""

    lines = text.splitlines()
    output: List[str] = []
    in_code_block = False
    code_lang = ""

    i = 0
    num_lines = len(lines)

    while i < num_lines:
        line = lines[i]

        # Code block fence: ```lang
        if line.startswith("```"):
            if not in_code_block:
                in_code_block = True
                code_lang = line[3:].strip() or "code"
                output.append(f"{COLOR_GRAY}── [{COLOR_CYAN}{code_lang}{COLOR_RESET}{COLOR_GRAY}] " + ("─" * 45) + f"{COLOR_RESET}")
            else:
                in_code_block = False
                output.append(f"{COLOR_GRAY}" + ("─" * 55) + f"{COLOR_RESET}")
            i += 1
            continue

        if in_code_block:
            output.append("  " + highlight_code_line(line))
            i += 1
            continue

        # Headers
        if line.startswith("# "):
            output.append(f"\n{COLOR_CYAN}{COLOR_BOLD}=== {line[2:].strip()} ==={COLOR_RESET}")
            i += 1
            continue
        elif line.startswith("## "):
            output.append(f"\n{COLOR_PURPLE}{COLOR_BOLD}▶ {line[3:].strip()}{COLOR_RESET}")
            i += 1
            continue
        elif line.startswith("### "):
            output.append(f"\n{COLOR_YELLOW}{COLOR_BOLD}• {line[4:].strip()}{COLOR_RESET}")
            i += 1
            continue

        # Horizontal rule
        if line.strip() in ("---", "***", "___"):
            output.append(f"{COLOR_GRAY}" + ("─" * 50) + f"{COLOR_RESET}")
            i += 1
            continue

        # Markdown Table Detection
        if '|' in line and i + 1 < num_lines and is_table_divider(lines[i + 1]):
            header_line = line
            divider_line = lines[i + 1]
            table_rows = []
            curr = i + 2
            while (
                curr < num_lines and
                '|' in lines[curr] and
                lines[curr].strip() and
                not lines[curr].startswith("```") and
                not lines[curr].startswith("#")
            ):
                table_rows.append(lines[curr])
                curr += 1
            rendered_table = render_table(header_line, divider_line, table_rows)
            output.extend(rendered_table)
            i = curr
            continue

        # GitHub Alert Detection: > [!NOTE], > [!WARNING], etc.
        stripped_line = line.strip()
        alert_match = re.match(r'^>\s*\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]\s*(.*)$', stripped_line, flags=re.IGNORECASE)
        if alert_match:
            alert_type = alert_match.group(1).upper()
            inline_body = alert_match.group(2).strip()
            alert_lines = []
            if inline_body:
                alert_lines.append(inline_body)
            curr = i + 1
            while curr < num_lines:
                next_stripped = lines[curr].strip()
                if next_stripped.startswith(">"):
                    alert_lines.append(next_stripped[1:].strip())
                    curr += 1
                else:
                    break
            rendered_alert = render_alert(alert_type, alert_lines)
            output.extend(rendered_alert)
            i = curr
            continue

        # Standard Blockquote: > text
        if stripped_line.startswith(">"):
            quote_text = stripped_line[1:].strip()
            output.append(f"{COLOR_GRAY}│{COLOR_RESET} " + format_inline(quote_text))
            i += 1
            continue

        # Inline formatting for regular text
        formatted = format_inline(line)

        # Bullet points: - item or * item
        formatted = re.sub(r'^(\s*)[-*]\s+', rf'\1{COLOR_CYAN}• {COLOR_RESET}', formatted)

        # Numbered lists: 1. item
        formatted = re.sub(r'^(\s*)(\d+)\.\s+', rf'\1{COLOR_CYAN}\2.{COLOR_RESET} ', formatted)

        output.append(formatted)
        i += 1

    return "\n".join(output)
