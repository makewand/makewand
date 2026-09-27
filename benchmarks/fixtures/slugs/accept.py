import importlib.util
import sys
from pathlib import Path

spec = importlib.util.spec_from_file_location("candidate", Path(sys.argv[1]) / "slugs.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
for titles, expected in [([], []), (["  Hello, World!  "], ["hello-world"]),
                         (["Café", "Cafe", "Ｃａｆｅ"], ["cafe", "cafe-2", "cafe-3"]),
                         (["中文", "!!!", "item"], ["item", "item-2", "item-3"]),
                         (["a", "a-2", "a", "a-2"], ["a", "a-2", "a-3", "a-2-2"]),
                         (["A__B", "a b", "  a  b  "], ["a-b", "a-b-2", "a-b-3"])]:
    original = list(titles)
    actual = module.unique_slugs(titles)
    assert actual == expected, (titles, actual, expected)
    assert titles == original, "input mutated"
print("independent acceptance passed")
