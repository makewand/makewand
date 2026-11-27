"""Follow-up to "local records remind, they do not gate" (2026-11-28).

Four places still refused on a local record or on a value the adapter
translates: automatic engine selection (`makewand review`, the status
recommendation), the Muse catalog snapshot of a high-risk review, route for
re-review rounds (it never listed Grok), and a high-risk dispatch whose
effort the provider's CLI does not accept as given.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import os
import tempfile
import unittest
from contextlib import contextmanager
from datetime import datetime
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock, patch

import makewand.config as config
import makewand.health as health
import makewand.orchestrator as orch
import makewand.usage as usage
from makewand.pacing import calculate_dynamic_pacing

try:
    from _verdicts import answer_with_nonce
except ImportError:  # python3 -m unittest tests.<module>
    from tests._verdicts import answer_with_nonce

CORE = ["claude", "codex", "grok", "agy", "muse"]
PASS_LINE = 'MAKEWAND_VERDICT: {"pass": true, "defects": []}'
USED_UP = "周额度已用 100%，达到阈值 100%"


def entry(status="healthy", **fields):
    return dict({"status": status, "reason": "", "resets_at": None, "updated_at": datetime.now().isoformat()}, **fields)


def limited(**fields):
    return entry("limited", reason="上次派发被拒：额度用完", **fields)


def official_used_up(*providers):
    """The reading a launch checks shows these providers used up; the others have no reading."""
    return patch("makewand.model_quota.official_quota_reason",
                 side_effect=lambda provider, *args, **kwargs: USED_UP if provider in providers else None)


class _IsolatedState(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-record-gates-")
        self.addCleanup(self._tmp.cleanup)
        self.root = Path(self._tmp.name)
        for guard in (
            patch.object(config, "CONFIG_DIR", self.root / "cfg"),
            patch.object(config, "CANDIDATES_DIR", self.root / "cfg" / "candidates"),
            patch.object(config, "BACKUPS_DIR", self.root / "cfg" / "backups"),
            patch.object(config, "LEGACY_TRIO_CACHE", self.root / "nolegacy" / "trio.json"),
            patch.object(health, "STATUS_CACHE_FILE", self.root / "cfg" / "status.json"),
            patch.object(health, "LEGACY_TRIO_CACHE", self.root / "nolegacy" / "trio.json"),
            patch.object(usage, "USAGE_WINDOW_FILE", self.root / "usage_window.json"),
            patch("makewand.config.get_active_providers", return_value=list(CORE)),
            patch("makewand.config.has_api_configured", return_value=False),
            patch("makewand.config.has_subscription_configured", return_value=True),
            patch("makewand.config.is_provider_enabled", return_value=True),
        ):
            guard.start()
            self.addCleanup(guard.stop)

    @staticmethod
    def cache(**statuses):
        return {engine: statuses.get(engine) or entry() for engine in CORE}


class RecordedLimitInEngineSelectionTests(_IsolatedState):
    def test_a_recorded_limit_leaves_the_engine_usable(self):
        from makewand.engine_selection import _engine_usable
        self.assertEqual(_engine_usable("agy", {"agy": limited()}), (True, ""))
        self.assertEqual(_engine_usable("codex", {"codex": limited(resets_at="in 3 hours")}), (True, ""))

    def test_a_current_official_reading_still_keeps_the_engine_out(self):
        from makewand.engine_selection import _engine_usable
        with official_used_up("codex"):
            usable, why = _engine_usable("codex", {"codex": limited()})
            self.assertEqual(_engine_usable("agy", {"agy": limited()}), (True, ""))
        self.assertFalse(usable)
        self.assertIn("limited", why)

    def test_other_failure_verdicts_exclude_as_before(self):
        from makewand.engine_selection import _engine_usable
        for status in ("needs_auth", "error", "missing", "disabled"):
            with self.subTest(status=status):
                self.assertFalse(_engine_usable("agy", {"agy": entry(status)})[0])

    def test_a_forced_engine_with_a_recorded_limit_is_not_refused(self):
        from makewand.engine_selection import forced_engine_refusal
        self.assertIsNone(forced_engine_refusal("agy", {"agy": limited()}))
        with official_used_up("codex"):
            self.assertIn("limited", forced_engine_refusal("codex", {"codex": limited()}))

    def test_selection_keeps_the_engine_and_ranks_it_after_the_others(self):
        from makewand.engine_selection import select_optimal_engine_pair
        # Claude has the highest coder score and Codex the highest reviewer score.
        coders, _, meta = select_optimal_engine_pair("实现导出功能", tier="standard", cache=self.cache(claude=limited()))
        self.assertGreater(len(coders), 1)
        self.assertEqual(coders[-1], "claude")
        self.assertTrue(any("claude 有额度受限的记录" in reason for reason in meta["reasons"]), meta["reasons"])
        coders, reviewers, _ = select_optimal_engine_pair("实现导出功能", tier="standard", cache=self.cache(codex=limited()))
        self.assertEqual(coders[0], "claude")
        self.assertGreater(len(reviewers), 1)
        self.assertEqual(reviewers[-1], "codex")

    def test_selection_drops_an_engine_the_official_reading_shows_used_up(self):
        from makewand.engine_selection import select_optimal_engine_pair
        with official_used_up("codex"):
            coders, reviewers, _ = select_optimal_engine_pair("实现导出功能", tier="standard",
                                                              cache=self.cache(codex=limited()))
        self.assertNotIn("codex", coders)
        self.assertNotIn("codex", reviewers)

    def test_every_engine_with_a_recorded_limit_still_yields_a_selection(self):
        from makewand.engine_selection import select_optimal_engine_pair
        coders, _, meta = select_optimal_engine_pair(
            "实现导出功能", tier="standard", cache={engine: limited() for engine in CORE})
        self.assertTrue(coders)
        self.assertFalse(meta.get("no_usable_engines"))

    def test_pacing_is_neutral_for_a_record_and_hard_for_an_official_reading(self):
        verdict = calculate_dynamic_pacing("claude", limited(resets_at="tomorrow"))
        self.assertEqual((verdict["routing_boost"], verdict["signal"]), (0.0, "record"))
        self.assertNotEqual(verdict["pacing_state"], "limited")
        self.assertIn("只提醒、不排除", verdict["reason"])
        with official_used_up("claude"):
            verdict = calculate_dynamic_pacing("claude", limited(resets_at="tomorrow"))
        self.assertEqual((verdict["routing_boost"], verdict["pacing_state"]), (-999.0, "limited"))


class RecordedLimitInReviewLadderTests(_IsolatedState):
    def run_review(self, cache, engine=None):
        mocks = {name: MagicMock(side_effect=answer_with_nonce((True, PASS_LINE, None)))
                 for name in ("execute_codex_task", "execute_grok_task", "execute_agy_task")}
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "get_git_diff", return_value="diff --git a/a b/a\n+x"))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value=cache))
            for name, mock in mocks.items():
                stack.enter_context(patch.object(orch, name, mock))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            code = orch.run_review(cwd="/tmp", output_json=True, engine=engine)
        return code, {name.split("_")[1]: mock.call_count for name, mock in mocks.items()}

    def test_the_engine_without_a_record_reviews_first(self):
        code, calls = self.run_review({"codex": limited(), "agy": entry()})
        self.assertEqual(code, orch.EXIT_PASSED)
        self.assertEqual(calls, {"codex": 0, "grok": 0, "agy": 1})

    def test_a_recorded_limit_engine_reviews_when_no_other_can(self):
        code, calls = self.run_review({"codex": limited(), "agy": entry("error", reason="probe timeout")})
        self.assertEqual(code, orch.EXIT_PASSED)
        self.assertEqual(calls, {"codex": 1, "grok": 0, "agy": 0})
        # Both recorded: the ladder keeps its order.
        code, calls = self.run_review({"codex": limited(), "agy": limited()})
        self.assertEqual(calls, {"codex": 1, "grok": 0, "agy": 0})

    def test_an_engine_the_official_reading_shows_used_up_is_skipped(self):
        with official_used_up("codex"):
            code, calls = self.run_review({"codex": limited(), "agy": limited()})
        self.assertEqual(code, orch.EXIT_PASSED)
        self.assertEqual(calls, {"codex": 0, "grok": 0, "agy": 1})

    def test_a_forced_reviewer_with_a_recorded_limit_runs(self):
        code, calls = self.run_review({"codex": limited(), "agy": entry()}, engine="codex")
        self.assertEqual(code, orch.EXIT_PASSED)
        self.assertEqual(calls, {"codex": 1, "grok": 0, "agy": 0})


class RereviewRouteTests(unittest.TestCase):
    def select(self, task_type="review", **options):
        from makewand.outcome_ledger import OutcomeLedger
        from makewand.route_select import Policy, select
        with tempfile.TemporaryDirectory(prefix="mw-rereview-") as root:
            return select(task_type, "normal", policy=Policy(), quota_reader=lambda: {},
                          ledger=OutcomeLedger(Path(root) / "routing"), **options)

    def test_a_first_review_still_leaves_grok_out(self):
        result = self.select()
        self.assertNotIn("grok", {row.provider for row in result.candidates})
        self.assertIn("不派审查", dict(result.skipped)["grok"])

    def test_a_rereview_round_may_use_grok(self):
        result = self.select(rereview=True)
        self.assertIn("grok", {row.provider for row in result.candidates})
        self.assertNotIn("grok", dict(result.skipped))
        full = self.select(rereview=True, full=True)
        self.assertIn("grok", {row.provider for row in full.candidates})

    def test_other_task_types_are_unchanged_by_the_option(self):
        plain = self.select("implement")
        marked = self.select("implement", rereview=True)
        self.assertEqual({row.provider for row in plain.candidates}, {row.provider for row in marked.candidates})
        self.assertEqual(plain.skipped, marked.skipped)

    def test_a_pause_still_holds_in_a_rereview_round(self):
        from makewand.route_select import Pause, Policy, select
        from makewand.outcome_ledger import OutcomeLedger
        with tempfile.TemporaryDirectory(prefix="mw-rereview-") as root:
            result = select("review", "normal", policy=Policy(paused=(Pause("grok", 4102444800.0, "2100-01-01T00:00"),)),
                            quota_reader=lambda: {}, ledger=OutcomeLedger(Path(root) / "routing"), rereview=True)
        self.assertNotIn("grok", {row.provider for row in result.candidates})
        self.assertEqual(dict(result.skipped)["grok"], "暂停到 2100-01-01T00:00")


class SkippedReasonWordingTests(unittest.TestCase):
    def test_a_reason_for_some_combinations_does_not_read_as_a_skipped_provider(self):
        from makewand.outcome_ledger import OutcomeLedger
        from makewand.review_policy import ReviewPolicy
        from makewand.route_select import Policy, select
        discovered = {"agy": {"gemini-4-argon-high": ["high"], "gemini-4-argon-xhigh": ["xhigh"]},
                      "codex": {"gpt-6.2-astra": ["high"]}}
        verified = {("agy", "gemini-4-argon-high", "high"), ("codex", "gpt-6.2-astra", "high")}
        with tempfile.TemporaryDirectory(prefix="mw-skipped-") as root, \
                patch("makewand.review_policy.review_models", return_value=verified), \
                patch("makewand.config.has_subscription_configured", return_value=True), \
                patch("makewand.config.is_provider_enabled", return_value=True):
            result = select("review", "hard", risk="high", full=True, policy=Policy(), quota_reader=lambda: {},
                            status_reader=lambda: {}, ledger=OutcomeLedger(Path(root) / "routing"),
                            discovered=discovered, review_policy=ReviewPolicy.from_options("codex", (), False))
        offered = {row.provider for row in result.candidates}
        skipped = dict(result.skipped)
        self.assertIn("agy", offered)
        # agy is offered: the reason belongs to its other combinations only.
        self.assertTrue(skipped["agy"].startswith("部分型号："), skipped)
        # codex is not offered at all: its reason stands as it is.
        self.assertNotIn("codex", offered)
        self.assertEqual(skipped["codex"], "与实现方同模型家族，需要明确批准回退")


class TranslatedEffortTests(unittest.TestCase):
    def test_validation_returns_the_effort_the_adapter_launches(self):
        from makewand.review_policy import ReviewPolicy, validate_review_dispatch
        by_codex = ReviewPolicy.from_options("codex", (), False)
        by_claude = ReviewPolicy.from_options("claude", (), False)
        self.assertEqual(validate_review_dispatch(by_codex, "claude", "claude-fable-5-2", "ultra", {}), "max")
        self.assertEqual(validate_review_dispatch(by_codex, "claude", "claude-fable-5-2", "xhigh", {}), "xhigh")
        self.assertEqual(validate_review_dispatch(by_codex, "muse", "muse-wonder-1.0", "ultra", {}), "ultra")
        with patch("makewand.discovery._codex_model_efforts", return_value=["low", "medium", "high", "xhigh"]):
            self.assertEqual(validate_review_dispatch(by_claude, "codex", "gpt-6.2-astra", "ultra", {}), "xhigh")
            self.assertEqual(validate_review_dispatch(by_claude, "codex", "gpt-6.2-astra", "high", {}), "high")
        # AGY carries the effort in the model ID and is launched without --effort.
        self.assertEqual(validate_review_dispatch(by_codex, "agy", "gemini-4-argon-high", "high", {}), "high")

    def test_a_high_effort_that_would_launch_below_high_is_refused_with_the_reason(self):
        from makewand.review_policy import ReviewPolicy, validate_review_dispatch
        policy = ReviewPolicy.from_options("claude", (), False)
        with patch("makewand.discovery._codex_model_efforts", return_value=["low", "medium"]):
            with self.assertRaisesRegex(ValueError, "codex 的 gpt-6.2-astra 最高只提供推理档 medium，达不到所要求的 high"):
                validate_review_dispatch(policy, "codex", "gpt-6.2-astra", "high", {})
            # An effort the caller names below high is the caller's choice, as before.
            self.assertEqual(validate_review_dispatch(policy, "codex", "gpt-6.2-astra", "medium", {}), "medium")

    def test_a_missing_model_is_named_as_the_problem(self):
        from makewand.review_policy import ReviewPolicy, validate_review_dispatch
        policy = ReviewPolicy.from_options("codex", (), False)
        for provider in ("claude", "muse", "agy"):
            with self.subTest(provider=provider), self.assertRaisesRegex(ValueError, "需要用 --model 指定精确型号"):
                validate_review_dispatch(policy, provider, None, "high", {})
        with self.assertRaisesRegex(ValueError, "排除 Grok"):
            validate_review_dispatch(policy, "grok", None, "high", {})

    def test_the_translated_effort_passes_the_launch_check(self):
        from makewand.execution_runtime import execution_context
        from makewand.providers.claude import execute_claude_task
        from makewand.review_policy import ReviewPolicy, validate_review_dispatch

        model = "claude-fable-5-2"
        launched = validate_review_dispatch(ReviewPolicy.from_options("codex", (), False), "claude", model, "ultra", {})

        @contextmanager
        def prepare_sandbox(*args, **kwargs):
            yield SimpleNamespace(cmd=list(args[1]), pass_fds=(), env={})

        with tempfile.TemporaryDirectory(prefix="mw-effort-") as root, \
                execution_context(review_dispatch=("claude", model, launched)), \
                patch("makewand.config.has_subscription_configured", return_value=True), \
                patch("makewand.sandbox.provider_sandbox_launch", side_effect=prepare_sandbox), \
                patch("makewand.execution_runtime.mark_provider_invocation") as started, \
                patch("makewand.providers.claude.run_subprocess",
                      return_value=(0, "fixture answer", "", None)) as transport:
            ok, _, error = execute_claude_task("fixture", cwd=root, model=model, effort="ultra", readonly=True)
        self.assertTrue(ok, error)
        started.assert_called_once()
        command = transport.call_args.args[0]
        self.assertEqual(command[command.index("--effort") + 1], "max")


class MuseCatalogReminderTests(unittest.TestCase):
    MODEL = "muse-wonder-1.0"

    def dispatch(self, launch, env=None):
        from makewand.execution_runtime import execution_context
        from makewand.providers.muse import execute_muse_task

        @contextmanager
        def prepare_sandbox(*args, **kwargs):
            yield launch

        warnings = io.StringIO()
        with tempfile.TemporaryDirectory(prefix="mw-muse-") as root, \
                execution_context(review_dispatch=("muse", self.MODEL, "high")), \
                patch.dict(os.environ, env or {}), \
                patch("makewand.config.has_subscription_configured", return_value=True), \
                patch("makewand.providers.muse.detect_muse_guard", return_value=(False, None)), \
                patch("makewand.sandbox.is_bwrap_available", return_value=True), \
                patch("makewand.sandbox.provider_sandbox_launch", side_effect=prepare_sandbox), \
                patch("makewand.execution_runtime.mark_provider_invocation") as started, \
                patch("makewand.providers.muse.run_subprocess",
                      return_value=(0, "fixture answer", "", None)) as transport, \
                contextlib.redirect_stderr(warnings):
            if env is None:
                os.environ.pop("MUSE_DATA_DIR", None)
                os.environ.pop("XDG_DATA_HOME", None)
            ok, out, error = execute_muse_task("fixture", cwd=root, model=self.MODEL, effort="high", readonly=True)
        return ok, error, started, transport, warnings.getvalue()

    def launch(self, current):
        home = SimpleNamespace(muse_review_snapshot_is_current=lambda: current)
        return SimpleNamespace(sandboxed=True, home=home, cmd=["muse"], pass_fds=(), env={})

    def test_a_changed_catalog_snapshot_reminds_and_the_review_runs(self):
        ok, error, started, transport, warnings = self.dispatch(self.launch(current=False))
        self.assertTrue(ok, error)
        started.assert_called_once()
        transport.assert_called_once()
        self.assertIn("[Makewand -> muse] 提醒（不拦截）：沙箱里的模型目录快照和本机当前目录不一致", warnings)

    def test_a_missing_private_home_reminds_too(self):
        launch = SimpleNamespace(sandboxed=True, home=None, cmd=["muse"], pass_fds=(), env={})
        ok, error, started, _, warnings = self.dispatch(launch)
        self.assertTrue(ok, error)
        started.assert_called_once()
        self.assertIn("提醒（不拦截）：沙箱里的模型目录快照", warnings)

    def test_a_current_snapshot_gives_no_reminder(self):
        ok, error, started, _, warnings = self.dispatch(self.launch(current=True))
        self.assertTrue(ok, error)
        started.assert_called_once()
        self.assertNotIn("提醒（不拦截）", warnings)

    def test_a_custom_data_directory_reminds_and_the_review_runs(self):
        with tempfile.TemporaryDirectory(prefix="mw-muse-data-") as custom:
            ok, error, started, transport, warnings = self.dispatch(self.launch(current=True),
                                                                    env={"MUSE_DATA_DIR": custom})
        self.assertTrue(ok, error)
        started.assert_called_once()
        transport.assert_called_once()
        self.assertIn("提醒（不拦截）：沙箱使用默认数据目录", warnings)

    def test_another_model_or_effort_is_still_refused_at_launch(self):
        from makewand.execution_runtime import execution_context
        from makewand.providers.muse import execute_muse_task

        @contextmanager
        def prepare_sandbox(*args, **kwargs):
            yield self.launch(current=True)

        with tempfile.TemporaryDirectory(prefix="mw-muse-") as root, \
                execution_context(review_dispatch=("muse", self.MODEL, "high")), \
                patch("makewand.config.has_subscription_configured", return_value=True), \
                patch("makewand.providers.muse.detect_muse_guard", return_value=(False, None)), \
                patch("makewand.sandbox.is_bwrap_available", return_value=True), \
                patch("makewand.sandbox.provider_sandbox_launch", side_effect=prepare_sandbox), \
                patch("makewand.execution_runtime.mark_provider_invocation") as started, \
                patch("makewand.providers.muse.run_subprocess") as transport, \
                contextlib.redirect_stderr(io.StringIO()):
            ok, _, error = execute_muse_task("fixture", cwd=root, model=self.MODEL, effort="xhigh", readonly=True)
        self.assertFalse(ok)
        self.assertIn("型号或推理档已变化", error)
        started.assert_not_called()
        transport.assert_not_called()


if __name__ == "__main__":
    unittest.main()
