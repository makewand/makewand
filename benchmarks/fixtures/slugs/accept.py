"""Assertions execute outside the candidate process."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from acceptance import check

check(sys.argv[1], "slugs.py", "unique_slugs", [([], []), (["  Hello, World!  "], ["hello-world"]),
                         (["Café", "Cafe", "Ｃａｆｅ"], ["cafe", "cafe-2", "cafe-3"]),
                         (["中文", "!!!", "item"], ["item", "item-2", "item-3"]),
                         (["a", "a-2", "a", "a-2"], ["a", "a-2", "a-3", "a-2-2"]),
                         (["A__B", "a b", "  a  b  "], ["a-b", "a-b-2", "a-b-3"])])
