"""Any two different healthy racers form a pair, and a refusal names every engine truthfully (race-pairing-msg).

run_race picked contestant A from codex, grok, muse, local, agy and contestant B
from claude, grok, local, agy only, so muse (a racer in slot A) never raced
codex: with codex and muse healthy and the rest missing the race refused, and
its message said 'muse: 健康，但没有另一位可与它组成竞速的不同引擎' right after
'(可用: CODEX)'. A forced contestant B could also make slot A pick the same
engine. Each slot now falls back to the other slot's racers, so any two
different usable racers race; the refusal lists disabled engines too and words
an engine the race never picks by itself truthfully. Providers are fakes.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import tempfile
import unittest
from datetime import datetime, timedelta
from unittest.mock import patch

from makewand import config, orchestrator as orch
from makewand.execution_contract import EXIT_UNVERIFIED
from makewand.git_helper import run_git_cmd


def live(status, reason=""):
    return {"status": status, "reason": reason, "updated_at": (datetime.now() - timedelta(minutes=1)).isoformat()}


class RacePairingTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-race-pairing-")
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        self.repo = root / "repo"
        self.repo.mkdir()
        (self.repo / "app.py").write_text("VALUE = 1\n")
        (self.repo / "test_app.py").write_text("def test_value():\n    assert True\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(self.repo))
            self.assertEqual(code, 0, error)
        self.candidates = root / "candidates"
        self.calls = []

    def dispatch(self, engine, prompt, cwd=None, readonly=False, **kwargs):
        self.calls.append((engine, readonly))
        if readonly:
            return False, None, "no judge in this fixture"
        (Path(cwd) / "app.py").write_text(f"VALUE = 2  # {engine}\n")
        return True, "implemented", None

    def race(self, cache, enabled, active, **options):
        out = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value=cache))
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch.os, "getloadavg", return_value=(30, 30, 30)))
            stack.enter_context(patch("makewand.config.is_provider_enabled", side_effect=lambda name: name in enabled))
            stack.enter_context(patch("makewand.config.get_active_providers", return_value=list(active)))
            stack.enter_context(patch("makewand.config.has_api_configured", return_value=False))
            stack.enter_context(patch("makewand.sandbox.verify_writable_sandbox_or_authorized", return_value=(True, None, None)))
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, "fixture tests passed")))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=self.dispatch))
            stack.enter_context(patch("makewand.usage.record_engine_usage"))
            stack.enter_context(patch.object(config, "CANDIDATES_DIR", self.candidates))
            stack.enter_context(patch.object(orch, "CANDIDATES_DIR", self.candidates))
            stack.enter_context(contextlib.redirect_stdout(out))
            code = orch.run_race("Implement value 2", cwd=str(self.repo), timeout=60, **options)
        return code, out.getvalue()

    def contestants(self):
        return sorted(engine for engine, readonly in self.calls if not readonly)

    def test_codex_and_muse_race_each_other(self):
        cache = {"codex": live("healthy"), "muse": live("healthy"), "claude": live("missing", "not installed"),
                 "grok": live("missing", "not installed"), "agy": live("missing", "not installed")}
        code, log = self.race(cache, enabled=("codex", "muse", "claude", "grok", "agy"),
                              active=("codex", "muse"))
        self.assertNotIn("没有两个不同", log, log)
        self.assertEqual(self.contestants(), ["codex", "muse"])

    def test_grok_and_muse_race_each_other(self):
        cache = {"grok": live("healthy"), "muse": live("healthy")}
        self.race(cache, enabled=("grok", "muse"), active=("grok", "muse"))
        self.assertEqual(self.contestants(), ["grok", "muse"])

    def test_a_forced_contestant_b_never_meets_itself(self):
        cache = {"claude": live("healthy"), "codex": live("healthy")}
        code, log = self.race(cache, enabled=("claude", "codex"), active=("claude", "codex"), engine_b="codex")
        self.assertNotIn("竞速需要两个不同的已启用提供者", log)
        self.assertEqual(self.contestants(), ["claude", "codex"])

    def test_refusal_names_every_engine_truthfully(self):
        cache = {"codex": live("healthy"), "claude": live("needs_auth", "login expired"),
                 "grok": live("missing", "not installed"), "deepseek": live("healthy")}
        code, log = self.race(cache, enabled=("codex", "claude", "grok", "deepseek"),
                              active=("codex", "claude", "grok", "deepseek"))
        self.assertEqual(code, EXIT_UNVERIFIED)
        self.assertEqual(self.calls, [])
        self.assertIn("(可用: CODEX)", log)
        self.assertIn("• claude: 健康状态为 needs_auth：login expired", log)
        self.assertIn("• muse: 已被用户禁用", log)  # disabled engines are listed as well
        self.assertIn("• local: 已被用户禁用", log)
        self.assertIn("• deepseek: 竞速不会自动选用该引擎", log)
        self.assertNotIn("没有另一位可与它组成竞速的不同引擎", log)
        self.assertNotIn("• codex:", log)


if __name__ == "__main__":
    unittest.main()
