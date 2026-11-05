"""
py-rel-19 (round 2): selection never names an engine that is not installed or
usable, and every consumer reads failure verdicts the same way.

With every active provider in a live failure state, select_optimal_engine_pair
fell back to a hard-coded ["agy"] in single-tool mode even when agy was not
installed; an error verdict with an API key slipped back in through the
fallbacks although the adapters only relay limited / needs_auth / missing work
to the API; a failure entry with a missing or unparseable timestamp stayed
excluded in _engine_usable while load_status_cache decayed it; and the ensemble
let error and needs_auth engines through.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import tempfile
import unittest
from datetime import datetime, timedelta
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
from makewand.engine_selection import _engine_usable, forced_engine_refusal, select_optimal_engine_pair

REGION = "Antigravity 在当前地区/账号不可用 (真实派发返回地区限制)"


def ago(**kwargs):
    return (datetime.now() - timedelta(**kwargs)).isoformat()


def live(status, **extra):
    return dict({"status": status, "reason": f"{status} reason", "updated_at": ago(minutes=1)}, **extra)


class TestTimestampRuleIsShared(unittest.TestCase):
    """load_status_cache and selection read a failure verdict's age the same way."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-fix-usable-r2-")
        root = Path(self._tmp.name)
        for p in (patch.object(config, "CONFIG_DIR", root / "cfg"),
                  patch.object(health, "STATUS_CACHE_FILE", root / "cfg" / "status.json"),
                  patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json"),
                  patch("makewand.config.is_provider_enabled", return_value=True)):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        (root / "cfg").mkdir(parents=True)

    def entry(self, status, stamp):
        entry = {"status": status, "reason": REGION, "source": "dispatch", "ttl_seconds": 6 * 3600}
        if stamp is not None:
            entry["updated_at"] = stamp
        return entry

    def both(self, entry):
        health.STATUS_CACHE_FILE.write_text(json.dumps({"agy": entry}), encoding="utf-8")
        return health.load_status_cache()["agy"]["status"], _engine_usable("agy", {"agy": dict(entry)})[0]

    def test_unparseable_timestamp_is_neutral_in_both(self):
        for status in ("error", "needs_auth"):
            with self.subTest(status=status):
                self.assertEqual(self.both(self.entry(status, "garbage")), ("unknown", True))

    def test_verdict_without_timestamp_is_neutral_in_both(self):
        # Round 3: like master, a verdict without a timestamp decays at once (it
        # used to be stamped on every load and so stayed excluded until a save).
        for stamp in (None, ""):
            for status in ("error", "needs_auth"):
                with self.subTest(stamp=stamp, status=status):
                    self.assertEqual(self.both(self.entry(status, stamp)), ("unknown", True))

    def test_verdict_without_timestamp_is_saved_back_as_neutral(self):
        health.STATUS_CACHE_FILE.write_text(json.dumps({"agy": self.entry("error", None)}), encoding="utf-8")
        health.record_engine_limit("codex", "limit", "in 3 hours")  # any save
        saved = json.loads(health.STATUS_CACHE_FILE.read_text(encoding="utf-8"))["agy"]
        self.assertEqual((saved["status"], saved["expired_from"]), ("unknown", "error"))
        self.assertTrue(saved.get("updated_at"))
        self.assertEqual(self.both(saved), ("unknown", True))

    def test_fresh_timestamp_excludes_in_both(self):
        self.assertEqual(self.both(live("error", ttl_seconds=6 * 3600, source="dispatch")), ("error", False))


if __name__ == "__main__":
    unittest.main()
