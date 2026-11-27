"""Claude Code's catalog cache holds several files; the one that describes the models is read.

Seen 2026-11-27: next to "<org>-cc.json" Claude Code wrote "<org>-cc.badges.json"
with the same modification time and bare model ids. The reader took the newest
file alone, got no effort levels, and every Claude model failed the high-risk
review check until the times stopped tying.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import discovery

EFFORTS = [{"id": "low", "name": "Low"}, {"id": "medium", "name": "Medium"}, {"id": "high", "name": "High"},
           {"id": "xhigh", "name": "Extra"}, {"id": "max", "name": "Max"}]


def described(model, name, **extra):
    return {"id": model, "name": name, "section": "main", "thinking": {"type": "effort", "effort_options": EFFORTS},
            **extra}


def catalog(*models):
    return {"version": 2, "catalog": {"surface": "cc", "config": {"id": "cc", "models": list(models)}}}


class ClaudeCatalogFilesTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.directory = self.home / ".claude" / "cache" / "model-catalog"
        self.directory.mkdir(parents=True)
        environment = patch.dict(os.environ, {"HOME": str(self.home), "CLAUDE_CONFIG_DIR": ""})
        environment.start()
        self.addCleanup(environment.stop)
        self.clock = 1_800_000_000

    def write(self, name, data, age=0):
        path = self.directory / name
        path.write_text(data if isinstance(data, str) else json.dumps(data), encoding="utf-8")
        os.utime(path, (self.clock - age, self.clock - age))
        return path

    def bare(self, *models):
        return catalog(*({"id": model, "section": "main"} for model in models))

    def test_bare_sibling_with_the_same_time_does_not_hide_the_efforts(self):
        full = catalog(described("claude-fable-5-2", "Fable 5.2"), described("claude-opus-5-5", "Opus 5.5"))
        # Both orders of the tie: the bare file sorts after or before the catalog.
        for sibling in ("org-cc.badges.json", "org-cc.aaa.json"):
            with self.subTest(sibling=sibling):
                for path in self.directory.iterdir():
                    path.unlink()
                self.write("org-cc.json", full)
                self.write(sibling, self.bare("claude-fable-5-2", "claude-opus-5-5"))
                self.assertEqual(discovery._claude_effort_map(), {
                    "claude-fable-5-2": ["low", "medium", "high", "xhigh", "max"],
                    "claude-opus-5-5": ["low", "medium", "high", "xhigh", "max"]})
                efforts = discovery.discover_model_efforts(strict_catalogs=True)["claude"]
                self.assertEqual(efforts["claude-fable-5-2"], ["low", "medium", "high", "xhigh", "max"])

    def test_newer_bare_file_does_not_hide_the_catalog(self):
        self.write("org-cc.json", catalog(described("claude-fable-5-2", "Fable 5.2")), age=3600)
        self.write("org-cc.badges.json", self.bare("claude-fable-5-2"))
        self.assertEqual(discovery._claude_effort_map()["claude-fable-5-2"], ["low", "medium", "high", "xhigh", "max"])

    def test_newest_describing_file_is_the_only_one_read(self):
        self.write("old-cc.json", catalog(described("claude-fable-5-1", "Fable 5.1")), age=86400)
        self.write("new-cc.json", catalog(described("claude-fable-5-2", "Fable 5.2")))
        # Models of an older catalog (another account, a retired model) are not merged in.
        self.assertEqual(list(discovery._claude_effort_map()), ["claude-fable-5-2"])

    def test_only_bare_files_still_list_their_models(self):
        self.write("org-cc.badges.json", self.bare("claude-fable-5-2"))
        self.assertEqual(discovery._claude_effort_map(), {"claude-fable-5-2": []})

    def test_runtime_levels_stand_in_for_missing_options(self):
        entry = {"id": "claude-fable-5-2", "name": "Fable 5.2", "runtime": {"effort_levels": ["low", "high", "odd"]}}
        self.write("org-cc.json", catalog(entry))
        self.assertEqual(discovery._claude_effort_map(), {"claude-fable-5-2": ["low", "high"]})

    def test_unreadable_and_malformed_files_are_skipped(self):
        self.write("broken.json", "{not json")
        self.write("list.json", "[]")
        self.write("shape.json", {"catalog": {"config": {"models": "x"}}})
        self.write("ids.json", catalog({"id": "bad id"}, "x", {"name": "no id"}))
        self.write("org-cc.json", catalog(described("claude-fable-5-2", "Fable 5.2")), age=60)
        self.assertEqual(list(discovery._claude_effort_map()), ["claude-fable-5-2"])

    def test_missing_directory_or_no_files_gives_nothing(self):
        self.assertEqual(discovery._claude_effort_map(), {})
        self.directory.rmdir()
        self.assertEqual(discovery._claude_effort_map(), {})
        self.assertEqual(discovery._claude_catalog_models(), [])

    def test_model_list_and_tier_resolution_read_the_same_catalog(self):
        self.write("org-cc.json", catalog(described("claude-fable-5-2", "Fable 5.2"), described("claude-haiku-5-5", "Haiku 5.5")))
        self.write("org-cc.badges.json", self.bare("claude-fable-5-2", "claude-haiku-5-5"))
        models = discovery.discover_available_models(refresh=True)["claude"]
        self.assertEqual(models["source"], "detected")
        self.assertIn("claude-fable-5-2 (Fable 5.2)", models["available"])
        resolved = discovery.get_provider_model_tier("claude", "deep")
        self.assertTrue(resolved["is_dynamic"])
        self.assertEqual(resolved["effort"], "max")


if __name__ == "__main__":
    unittest.main()
