#!/usr/bin/env python3
"""Offline harness regressions. The fixture writers are stubs, never model evidence."""
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent
INTERVALS = '''def merge_intervals(intervals):
    result = []
    for start, end in sorted(intervals):
        if start > end:
            raise ValueError("reversed")
        if result and start <= result[-1][1]:
            result[-1] = (result[-1][0], max(end, result[-1][1]))
        else:
            result.append((start, end))
    return result
'''
SLUGS = '''import re
import unicodedata

def unique_slugs(titles):
    result, used = [], set()
    for title in titles:
        normalized = unicodedata.normalize("NFKD", title).encode("ascii", "ignore").decode().lower()
        base = re.sub("[^a-z0-9]+", "-", normalized).strip("-") or "item"
        slug, suffix = base, 2
        while slug in used:
            slug = f"{base}-{suffix}"
            suffix += 1
        result.append(slug)
        used.add(slug)
    return result
'''


class HarnessTests(unittest.TestCase):
    def test_independent_acceptance_rejects_successful_wrong_output(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            writer = root / "writer.py"
            writer.write_text(
                "from pathlib import Path\n"
                f"if Path('intervals.py').exists(): Path('intervals.py').write_text({INTERVALS!r})\n"
                f"if Path('slugs.py').exists(): Path('slugs.py').write_text({SLUGS!r})\n",
                encoding="utf-8",
            )
            arms = root / "arms.json"
            arms.write_text(json.dumps({"stub-good": [sys.executable, "-I", str(writer)],
                                       "stub-bad": [sys.executable, "-I", "-c", "print('all tests passed')"]}))
            output = root / "results"
            command = [sys.executable, "-I", str(ROOT / "runner.py"), "--arms", str(arms),
                       "--output", str(output), "--repeats", "1", "--timeout", "10"]
            plan = subprocess.run(command, check=True, capture_output=True, text=True)
            self.assertEqual(len(json.loads(plan.stdout)["schedule"]), 4)
            self.assertFalse(output.exists(), "dry run launched or created output")
            subprocess.run([*command, "--execute"], check=True, capture_output=True, text=True)
            summary = json.loads((output / "summary.json").read_text())
            self.assertEqual(summary["stub-good"]["accepted"], 2)
            self.assertEqual(summary["stub-bad"]["accepted"], 0)
            for result in output.glob("*/result.json"):
                self.assertIsNone(json.loads(result.read_text())["monetary_cost"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
