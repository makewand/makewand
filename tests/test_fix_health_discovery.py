"""
py-rel-23: the agy model cache is refreshed once older than its max_age (it used
to be written only when missing). A refresh runs only after its attempt marker
could be written (an unwritable cache directory used to rerun `agy models` on
every lookup), at most once per process and back-off window, never replaces the
catalog with output that names no Gemini model, and a deleted cache or a marker
dated in the future does not hold a refresh back.
py-rel-24: tier selection does not misread price/variant descriptions: grok's
standard tier picked "grok-4.7-build-fast" ("Fast variant. 2x the price.") and
the word "frontier" in grok-4.7's description counted like a deep model family.
The real Grok CLI cache nests descriptions under "info"; ties between equally
scored models no longer depend on catalog order.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import json
import os
import shutil
import subprocess
import sys
import tempfile
import textwrap
import time
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock, patch

import makewand.discovery as discovery
from makewand.discovery import (_discover_local_agy_models, _price_signal, discover_available_models,
                                export_routing_overrides, get_provider_model_tier, rank_models_for_tier,
                                score_model_for_tier)

REPO_ROOT = Path(__file__).resolve().parent.parent

GROK_CATALOG = {"models": {
    "grok-4.7": {"description": "SpaceXAI's latest frontier model"},
    "grok-4.7-build-fast": {"description": "Fast variant. 2x the price."},
    "grok-4.6": {"description": None},
    "grok-4.5": {"description": None},
}}

GROK_ENTRIES = {
    "grok-4.7": ("Grok 4.7", "SpaceXAI's latest frontier model"),
    "grok-4.7-build-fast": ("Grok 4.7 Fast", "Fast variant. 2x the price."),
    "grok-4.6": ("Grok 4.6", None),
    "grok-4.5": ("Grok 4.5", None),
}
GROK_CLI_ORDER = ["grok-4.7", "grok-4.7-build-fast", "grok-4.6", "grok-4.5"]
GROK_FAST_FIRST = ["grok-4.7-build-fast", "grok-4.7", "grok-4.6", "grok-4.5"]


def grok_cli_cache(order=GROK_CLI_ORDER, with_descriptions=True):
    """~/.grok/models_cache.json as grok CLI 1.0.x writes it: each entry's metadata
    nested under "info" (invented values, no credentials). The effort descriptions
    carry tier words ("complex", "Quick") that must not be read as the model's."""
    efforts = [
        {"id": "high", "value": "high", "label": "High", "description": "Thorough reasoning for complex tasks.",
         "default": True},
        {"id": "low", "value": "low", "label": "Low", "description": "Quick answers with light reasoning.",
         "default": False},
    ]
    models = {}
    for model_id in order:
        name, desc = GROK_ENTRIES[model_id]
        models[model_id] = {
            "info": {"id": model_id, "model": model_id, "model_family": "xai", "name": name,
                     "description": desc if with_descriptions else None, "hidden": False,
                     "supported_in_api": True, "reasoning_effort": "high", "supports_reasoning_effort": True,
                     "reasoning_efforts": efforts, "context_window": 256000},
            "api_key": None, "env_key": None, "api_base_url": None,
        }
    return {"fetched_at": "2026-10-01T08:00:00Z", "grok_version": "1.0.50", "models": models}


class _TempHome(unittest.TestCase):
    # Tests of the agy refresh itself drop the isolation root, which otherwise
    # disables every `agy models` run (subprocess.run is mocked there).
    KEEP_ISOLATION_ROOT = False

    def setUp(self):
        self.home = Path(tempfile.mkdtemp(prefix="mw-fix-discovery-"))
        self.addCleanup(shutil.rmtree, self.home, True)
        dropped = {"GEMINI_CONFIG_DIR", "AGY_CONFIG_DIR", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "MUSE_DATA_DIR"}
        if not self.KEEP_ISOLATION_ROOT:
            dropped.add("MAKEWAND_TEST_ISOLATION_ROOT")
        env = {k: v for k, v in os.environ.items() if k not in dropped}
        env["HOME"] = str(self.home)
        env["XDG_DATA_HOME"] = str(self.home / ".local" / "share")
        for p in (patch.dict(os.environ, env, clear=True),
                  patch("pathlib.Path.home", return_value=self.home),  # USERPROFILE on Windows
                  patch("makewand.config.is_provider_enabled", return_value=True),
                  patch.dict(discovery._AGY_FAILED_REFRESHES, clear=True)):
            p.start()
            self.addCleanup(p.stop)

    def write(self, rel, data, age_seconds=0):
        path = self.home / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(data), encoding="utf-8")
        if age_seconds:
            stamp = time.time() - age_seconds
            os.utime(path, (stamp, stamp))
        return path


class TestAgyModelCacheRefresh(_TempHome):
    CACHE = ".cache/makewand/agy_models_cache.json"
    OLD = {"updated_at": 0, "models": [{"id": "gemini-1.0-old", "description": "Gemini 1.0 Old"}]}

    @property
    def marker(self):
        return self.home / (self.CACHE + ".attempt")

    def discover(self, returncode=0, stdout="gemini-9.9-pro  Gemini 9.9 Pro (High)\n", side_effect=None):
        with patch("shutil.which", return_value="/usr/local/bin/agy"), \
             patch("subprocess.run", return_value=MagicMock(returncode=returncode, stdout=stdout),
                   side_effect=side_effect) as run:
            models = [slug for slug, _ in _discover_local_agy_models().models]
            self.standard = get_provider_model_tier("agy", "standard")["model"]
        return models, run

    def test_stale_cache_is_refreshed(self):
        cache = self.write(self.CACHE, self.OLD, age_seconds=3 * 86400)
        models, run = self.discover()
        run.assert_called_once()
        self.assertEqual(run.call_args[0][0], ["agy", "models"])
        self.assertEqual(models, ["gemini-9.9-pro"])
        self.assertLess(time.time() - cache.stat().st_mtime, 60)

    def test_fresh_cache_is_not_refreshed(self):
        self.write(self.CACHE, self.OLD, age_seconds=60)
        models, run = self.discover()
        run.assert_not_called()
        self.assertEqual(models, ["gemini-1.0-old"])

    def test_failed_refresh_keeps_old_catalog_and_backs_off(self):
        self.write(self.CACHE, self.OLD, age_seconds=3 * 86400)
        models, run = self.discover(returncode=1, stdout="")
        run.assert_called_once()
        self.assertEqual(models, ["gemini-1.0-old"])
        models, run = self.discover()
        run.assert_not_called()  # retried only after the back-off window
        self.assertEqual(models, ["gemini-1.0-old"])

    def test_hanging_agy_is_waited_for_once(self):
        self.write(self.CACHE, self.OLD, age_seconds=3 * 86400)
        models, run = self.discover(side_effect=subprocess.TimeoutExpired(["agy", "models"], 4.0))
        run.assert_called_once()
        self.assertEqual(models, ["gemini-1.0-old"])
        models, run = self.discover()
        run.assert_not_called()

    @unittest.skipIf(os.name == "nt" or os.geteuid() == 0, "needs POSIX directory permissions that bind this user")
    def test_unwritable_cache_directory_never_runs_agy(self):
        # Inside an agent sandbox with a read-only HOME: a refresh could not be
        # stored, so `agy models` must not run (it used to run on every lookup).
        cache = self.write(self.CACHE, self.OLD, age_seconds=3 * 86400)
        cache.parent.chmod(0o555)
        self.addCleanup(cache.parent.chmod, 0o755)
        for _ in range(3):
            models, run = self.discover()
            run.assert_not_called()
            self.assertEqual(models, ["gemini-1.0-old"])
        self.assertFalse(self.marker.exists())

    def test_one_attempt_per_process_even_when_the_marker_disappears(self):
        self.write(self.CACHE, self.OLD, age_seconds=3 * 86400)
        self.discover(returncode=1, stdout="")
        self.assertTrue(self.marker.exists())
        self.marker.unlink()  # e.g. removed by a cache cleaner, or a file system that loses it
        models, run = self.discover()
        run.assert_not_called()
        self.assertEqual(models, ["gemini-1.0-old"])

    def test_output_without_a_gemini_model_keeps_the_previous_catalog(self):
        good = {"updated_at": 0, "models": [{"id": "gemini-3.8-flash-high", "description": "Gemini 3.8 Flash (High)"},
                                            {"id": "gemini-3.1-pro-high", "description": "Gemini 3.1 Pro (High)"}]}
        outputs = (
            "No models available for this account.\n",
            "Error: Gemini Code Assist is not currently available in your location.\n",
            "Please sign in to view available models. Launch the CLI without arguments to sign in.\n",
            "claude-sonnet-4-6  Claude Sonnet 4.6 (Thinking)\ngpt-oss-120b-medium  GPT-OSS 120B (Medium)\n",
        )
        for stdout in outputs:
            with self.subTest(stdout=stdout):
                discovery._AGY_FAILED_REFRESHES.clear()
                if self.marker.exists():
                    self.marker.unlink()
                cache = self.write(self.CACHE, good, age_seconds=25 * 3600)
                before = cache.read_text(encoding="utf-8")
                models, run = self.discover(stdout=stdout)
                run.assert_called_once()
                self.assertEqual(cache.read_text(encoding="utf-8"), before)
                self.assertEqual(sorted(set(models)), ["gemini-3.1-pro-high", "gemini-3.8-flash-high"])
                self.assertIn(self.standard, ("gemini-3.1-pro", "gemini-3.8-flash"))
                self.assertTrue(self.marker.exists())  # a failed refresh: back off
                models, run = self.discover()
                run.assert_not_called()

    def test_deleting_the_cache_after_a_failed_refresh_forces_a_new_one(self):
        cache = self.write(self.CACHE, self.OLD, age_seconds=3 * 86400)
        self.discover(returncode=1, stdout="")
        cache.unlink()
        models, run = self.discover()
        run.assert_called_once()
        self.assertEqual(models, ["gemini-9.9-pro"])
        self.assertFalse(self.marker.exists())

    def test_marker_dated_in_the_future_does_not_block_a_refresh(self):
        self.write(self.CACHE, self.OLD, age_seconds=3 * 86400)
        self.marker.write_text("", encoding="utf-8")
        future = time.time() + 2 * 86400
        os.utime(self.marker, (future, future))
        models, run = self.discover()
        run.assert_called_once()
        self.assertEqual(models, ["gemini-9.9-pro"])
        self.assertFalse(self.marker.exists())

    def test_successful_refresh_clears_an_expired_marker_and_leaves_no_temporary_file(self):
        cache = self.write(self.CACHE, self.OLD, age_seconds=3 * 86400)
        self.marker.write_text("missing", encoding="utf-8")
        expired = time.time() - 2 * 3600
        os.utime(self.marker, (expired, expired))
        models, run = self.discover()
        run.assert_called_once()
        self.assertEqual(models, ["gemini-9.9-pro"])
        self.assertEqual(sorted(p.name for p in cache.parent.iterdir()), [cache.name])

    def test_slow_runner_times_out_implicitly_and_explicit_refresh_waits(self):
        # A networked `agy models` is slower than the 4s implicit budget. The
        # fake runner raises TimeoutExpired below 30s and succeeds at 60s, so
        # the test does not actually sleep.
        cache = self.write(self.CACHE, self.OLD, age_seconds=3 * 86400)
        cache_file = self.home / self.CACHE
        calls = []

        def runner(argv, **kwargs):
            timeout = kwargs["timeout"]
            calls.append(timeout)
            if timeout < 30:
                raise subprocess.TimeoutExpired(argv, timeout)
            return SimpleNamespace(
                returncode=0,
                stdout="gemini-4-argon-high\tGemini 4 Argon (High)\n",
            )

        self.assertIsNone(discovery._sync_agy_models_cache(cache_file=cache_file, runner=runner))
        self.assertEqual(calls, [discovery.AGY_MODELS_TIMEOUT_SECONDS])
        self.assertEqual(cache.read_text(encoding="utf-8"), json.dumps(self.OLD))
        self.assertTrue(self.marker.exists())
        # The .attempt marker and the in-process back-off must hold the next
        # implicit lookup, and must not hold an explicit refresh.
        self.assertIsNone(discovery._sync_agy_models_cache(cache_file=cache_file, runner=runner))
        self.assertEqual(calls, [discovery.AGY_MODELS_TIMEOUT_SECONDS])

        refreshed = discovery.refresh_agy_models_cache(cache_file=cache_file, runner=runner)
        self.assertEqual(refreshed, cache_file)
        self.assertEqual(calls, [discovery.AGY_MODELS_TIMEOUT_SECONDS, discovery.AGY_MODELS_REFRESH_TIMEOUT_SECONDS])
        stored = json.loads(cache.read_text(encoding="utf-8"))
        self.assertEqual([item["id"] for item in stored["models"]], ["gemini-4-argon-high"])
        self.assertFalse(self.marker.exists())
        self.assertNotIn(str(cache_file), discovery._AGY_FAILED_REFRESHES)

    def test_explicit_refresh_ignores_a_fresh_cache_and_a_young_marker(self):
        cache = self.write(self.CACHE, self.OLD, age_seconds=60)
        cache_file = self.home / self.CACHE
        calls = []

        def runner(argv, **kwargs):
            calls.append(kwargs["timeout"])
            return SimpleNamespace(
                returncode=0,
                stdout="gemini-4-argon-high\tGemini 4 Argon (High)\n",
            )

        self.assertEqual(discovery._sync_agy_models_cache(cache_file=cache_file, runner=runner), cache_file)
        self.assertEqual(calls, [])
        self.marker.write_text(discovery._agy_cache_state(cache_file), encoding="utf-8")
        discovery._AGY_FAILED_REFRESHES[str(cache_file)] = (
            time.monotonic(),
            discovery._agy_cache_state(cache_file),
        )
        refreshed = discovery.refresh_agy_models_cache(cache_file=cache_file, runner=runner)
        self.assertEqual(refreshed, cache_file)
        self.assertEqual(calls, [discovery.AGY_MODELS_REFRESH_TIMEOUT_SECONDS])
        stored = json.loads(cache.read_text(encoding="utf-8"))
        self.assertEqual(stored["models"][0]["id"], "gemini-4-argon-high")
        self.assertFalse(self.marker.exists())

    def test_subprocess_timeout_is_4s_implicit_and_60s_explicit(self):
        self.write(self.CACHE, self.OLD, age_seconds=3 * 86400)
        cache_file = self.home / self.CACHE
        with patch("shutil.which", return_value="/usr/local/bin/agy"), \
             patch("subprocess.run", return_value=MagicMock(
                 returncode=0, stdout="gemini-9.9-pro  Gemini 9.9 Pro (High)\n")) as run:
            discovery._discover_local_agy_models()
        run.assert_called_once()
        self.assertEqual(run.call_args[1]["timeout"], discovery.AGY_MODELS_TIMEOUT_SECONDS)
        self.marker.write_text("", encoding="utf-8")
        with patch("shutil.which", return_value="/usr/local/bin/agy"), \
             patch("subprocess.run", return_value=MagicMock(
                 returncode=0, stdout="gemini-4-argon-high\tGemini 4 Argon (High)\n")) as run:
            discovery.refresh_agy_models_cache(cache_file=cache_file)
        run.assert_called_once()
        self.assertEqual(run.call_args[1]["timeout"], discovery.AGY_MODELS_REFRESH_TIMEOUT_SECONDS)
        self.assertEqual(run.call_args[1]["cwd"], tempfile.gettempdir())
        stored = json.loads(cache_file.read_text(encoding="utf-8"))
        self.assertEqual(stored["models"][0]["id"], "gemini-4-argon-high")

    def test_discover_model_efforts_reads_a_stale_cache_without_running_agy(self):
        cache = self.write(self.CACHE, {
            "updated_at": 0,
            "models": [
                {"id": "gemini-4-argon-high", "description": "Gemini 4 Argon (High)"},
                {"id": "gemini-3.8-flash-low", "description": "Gemini 3.8 Flash (Low)"},
            ],
        }, age_seconds=3 * 86400)
        before = cache.read_text(encoding="utf-8")
        with patch("shutil.which", return_value="/usr/local/bin/agy"), \
             patch("subprocess.run") as run:
            found = discovery.discover_model_efforts()
        run.assert_not_called()
        self.assertEqual(found["agy"]["gemini-4-argon-high"], ["high"])
        self.assertEqual(found["agy"]["gemini-3.8-flash-low"], ["low"])
        self.assertEqual(cache.read_text(encoding="utf-8"), before)
        self.assertFalse(self.marker.exists())


@unittest.skipIf(os.name == "nt", "uses a POSIX shell script as the fake agy")
class TestAgyModelCacheConcurrentRefresh(unittest.TestCase):
    """Separate makewand processes looking up a stale catalog at the same time."""

    PROBE = textwrap.dedent("""
        import os, shutil, sys
        sys.path.insert(0, sys.argv[1])
        if os.path.dirname(shutil.which("agy") or "") != sys.argv[2]:
            sys.exit("fake agy is not the one on PATH")
        from makewand.discovery import _discover_local_agy_models
        print(",".join(slug for slug, _ in _discover_local_agy_models().models))
    """)

    def test_marker_age_uses_time_after_metadata_observation(self):
        from makewand.discovery import _claim_agy_refresh
        with tempfile.TemporaryDirectory() as directory:
            marker = Path(directory) / "catalog.attempt"
            marker.write_text("catalog-state")
            observed = time.time()
            os.utime(marker, (observed, observed))
            def clock():
                # Another owner publishes its marker between reading the
                # clock and stat. Its mtime must not be misread as a future
                # stale marker and stolen while that owner refreshes.
                os.utime(marker, (observed + .01, observed + .01))
                return observed + .001
            with patch("time.time", side_effect=clock):
                self.assertFalse(_claim_agy_refresh(marker, "catalog-state", 60))
            self.assertEqual(marker.read_text(), "catalog-state")

    def test_concurrent_lookups_run_agy_once_and_leave_no_marker(self):
        root = Path(tempfile.mkdtemp(prefix="mw-fix-agy-race-"))
        self.addCleanup(shutil.rmtree, root, True)
        home, fake_bin = root / "home", root / "bin"
        fake_bin.mkdir()
        agy = fake_bin / "agy"
        agy.write_text('#!/bin/sh\necho "$$" >> "$(dirname "$0")/calls.log"\n'
                       '[ "$1" = "models" ] || exit 1\nsleep 0.7\n'
                       'echo "gemini-9.9-pro  Gemini 9.9 Pro (High)"\n', encoding="utf-8")
        agy.chmod(0o755)
        cache = home / ".cache" / "makewand" / "agy_models_cache.json"
        cache.parent.mkdir(parents=True)
        cache.write_text(json.dumps(TestAgyModelCacheRefresh.OLD), encoding="utf-8")
        stale = time.time() - 3 * 86400
        os.utime(cache, (stale, stale))
        env = {"HOME": str(home), "PATH": f"{fake_bin}{os.pathsep}/usr/bin{os.pathsep}/bin", "LANG": "C.UTF-8",
               "MAKEWAND_CONFIG_DIR": str(root / "cfg"), "XDG_CONFIG_HOME": str(root / "xdg-config"),
               "XDG_CACHE_HOME": str(root / "xdg-cache"), "XDG_DATA_HOME": str(root / "xdg-data")}
        procs = [subprocess.Popen([sys.executable, "-I", "-c", self.PROBE, str(REPO_ROOT), str(fake_bin)], env=env,
                                  cwd=str(root), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                 for _ in range(6)]
        results = []
        for proc in procs:
            out, err = proc.communicate(timeout=120)
            self.assertEqual(proc.returncode, 0, err)
            results.append(out.strip())
        calls = (fake_bin / "calls.log").read_text(encoding="utf-8").split()
        self.assertEqual(len(calls), 1, results)
        self.assertEqual([m["id"] for m in json.loads(cache.read_text(encoding="utf-8"))["models"]],
                         ["gemini-9.9-pro"])
        self.assertEqual(sorted(p.name for p in cache.parent.iterdir()), [cache.name])  # no marker, no temp file
        self.assertTrue(set(results) <= {"gemini-1.0-old", "gemini-9.9-pro"}, results)


class TestTierSelectionReadsPriceAndVariants(_TempHome):
    def test_grok_standard_and_deep_pick_the_base_model(self):
        self.write(".grok/models_cache.json", GROK_CATALOG)
        self.assertEqual(get_provider_model_tier("grok", "standard")["model"], "grok-4.7")
        self.assertEqual(get_provider_model_tier("grok", "deep")["model"], "grok-4.7")

    def test_surcharged_variant_is_not_the_cheap_pick(self):
        self.write(".grok/models_cache.json", GROK_CATALOG)
        self.assertNotEqual(get_provider_model_tier("grok", "fast")["model"], "grok-4.7-build-fast")
        two = [("grok-4.7", "SpaceXAI's latest frontier model"), ("grok-4.7-build-fast", "Fast variant. 2x the price.")]
        self.assertEqual(rank_models_for_tier(two, "fast")[0][1], "grok-4.7")
        self.assertEqual(rank_models_for_tier(two, "standard")[0][1], "grok-4.7")

    def test_speed_variant_without_surcharge_is_a_fast_pick_only(self):
        models = [("grok-4.7", "Latest model"), ("grok-4.7-build-fast", "Fast variant.")]
        self.assertEqual(rank_models_for_tier(models, "fast")[0][1], "grok-4.7-build-fast")
        self.assertEqual(rank_models_for_tier(models, "standard")[0][1], "grok-4.7")
        self.assertEqual(rank_models_for_tier(models, "deep")[0][1], "grok-4.7")

    def test_frontier_wording_does_not_push_a_model_out_of_standard(self):
        prose = score_model_for_tier("grok-4.7", "SpaceXAI's latest frontier model", "standard")
        plain = score_model_for_tier("grok-4.7", "", "standard")
        self.assertEqual(prose, plain)
        self.assertGreater(score_model_for_tier("grok-4.7", "SpaceXAI's latest frontier model", "deep"),
                           score_model_for_tier("grok-4.7", "", "deep"))


class TestGrokCliCacheLayout(_TempHome):
    """The cache grok CLI 1.0.x writes keeps descriptions under models[id].info."""

    KEEP_ISOLATION_ROOT = True

    def test_tiers_read_descriptions_nested_under_info_in_any_order(self):
        for order in (GROK_CLI_ORDER, GROK_FAST_FIRST, list(reversed(GROK_CLI_ORDER))):
            with self.subTest(order=order):
                self.write(".grok/models_cache.json", grok_cli_cache(order))
                picks = {tier: get_provider_model_tier("grok", tier) for tier in ("fast", "standard", "deep")}
                self.assertEqual(picks["fast"]["model"], "grok-4.6")
                self.assertEqual(picks["standard"]["model"], "grok-4.7")
                self.assertEqual(picks["deep"]["model"], "grok-4.7")
                self.assertTrue(all(p["is_dynamic"] for p in picks.values()))
                self.assertEqual(discover_available_models()["grok"]["current_default"], "grok-4.7")

    def test_models_command_default_does_not_depend_on_catalog_order(self):
        # Equal scores (no descriptions) used to be settled by dict order.
        for order in (GROK_CLI_ORDER, GROK_FAST_FIRST):
            with self.subTest(order=order):
                self.write(".grok/models_cache.json", grok_cli_cache(order, with_descriptions=False))
                self.assertEqual(get_provider_model_tier("grok", "standard")["model"], "grok-4.7")
                models = discover_available_models()["grok"]
                self.assertEqual(models["current_default"], "grok-4.7")
                self.assertEqual(models["default_source"], "detected")

    def test_routing_export_does_not_make_the_surcharged_variant_cheap(self):
        self.write(".grok/models_cache.json", grok_cli_cache(GROK_FAST_FIRST))
        with patch("makewand.config.is_provider_enabled", side_effect=lambda name: name == "grok"):
            out = export_routing_overrides(config_dir=self.home / "cfg")
        grok = json.loads(out.read_text(encoding="utf-8"))["models"]["grok"]
        self.assertEqual(grok, {"cheap": "grok-4.6", "mid": "grok-4.7", "premium": "grok-4.7"})

    def test_fast_dispatch_does_not_pass_the_surcharged_variant(self):
        from makewand.providers import grok as grok_provider
        (self.home / ".grok" / "bin").mkdir(parents=True)
        (self.home / ".grok" / "bin" / "grok").write_text("#!/bin/sh\n", encoding="utf-8")
        (self.home / ".grok" / "config.toml").write_text("", encoding="utf-8")
        self.write(".grok/models_cache.json", grok_cli_cache())
        workspace = Path(tempfile.mkdtemp(prefix="mw-fix-grok-ws-"))
        self.addCleanup(shutil.rmtree, workspace, True)
        with patch("shutil.which", return_value="/opt/grok/bin/grok"), \
             patch("makewand.sandbox.is_bwrap_available", return_value=True), \
             patch.object(grok_provider, "run_subprocess", return_value=(0, "ok", "", None)) as run:
            grok_provider.execute_grok_task("analyze problem", cwd=str(workspace), tier="fast", readonly=True)
        cmd = run.call_args[0][0]
        self.assertEqual(cmd[cmd.index("--model") + 1], "grok-4.6")

    def test_no_cache_fallback_is_not_the_surcharged_variant(self):
        fast = get_provider_model_tier("grok", "fast")
        self.assertEqual((fast["model"], fast["effort"], fast["is_dynamic"]), ("grok-4.7", "low", False))


class TestPriceWording(unittest.TestCase):
    SURCHARGES = (
        "Fast variant. 2x the price.", "Fast variant. 2x price.", "Fast variant, 2× the price",
        "Fast variant. Double the price.", "Fast variant. Twice the price.", "Fast variant. 2x pricing.",
        "Fast variant. Costs 2x.", "Fast variant. Costs twice as much.", "Fast variant. 2x more expensive.",
        "Faster output at 2x the cost.", "Fast variant (2x usage).", "Fast variant. Priced at 2x.",
        "Fast variant. 200% price.", "Fast variant. +100% price.", "Fast variant. 2x token cost.",
        "Fast variant; uses credits 2x faster.", "Fast variant. 2x per-token price.", "Fast. Premium pricing.",
        "Speed-optimised. Costs more.", "Fast variant. Expensive.", "Lower latency, higher cost.",
        "Fast variant. 2X the price.", "Fast variant. 2.0x the price.", "Fast variant. x2 price.",
        "Consumes credits at 2x the rate.", "Fable is the most capable model and draws down usage much faster than Opus",
        "Price: 2x.", "2x the standard price.", "Uses 2x the credits.", "Counts as 2x.", "Costlier than grok-4.7.",
        "Higher per-token price.", "2x speed, increased usage.",
    )
    ALLOWANCES = (
        "Fast and affordable model for easier tasks. About 4x the usage of sol on the same plan.",
        "Fast and affordable. 2x the rate limits of sol.", "5x more usage than Plus.", "2x usage limits.",
        "Higher rate limits.", "Higher usage limits.", "Increased usage limits.", "Gives 4x the usage of sol.",
        "2x speed, same price.", "2x faster at the same price.", "Uses 2x fewer tokens.",
        "Latest workhorse model for coding and everyday work.", "Supports fast mode.",
    )
    DISCOUNTS = ("Half the price of Opus.", "0.5x the price.", "Cheaper than Sonnet.", "Less expensive.",
                 "50% off.", "Costs less.", "Costs 50% less.", "Uses half the tokens.")

    def test_price_statements(self):
        for group, expected in ((self.SURCHARGES, 1), (self.ALLOWANCES, 0), (self.DISCOUNTS, -1)):
            for text in group:
                with self.subTest(text=text):
                    self.assertEqual(_price_signal(text.lower()), expected)

    def test_surcharged_speed_variant_is_never_the_cheap_or_everyday_pick(self):
        for wording in self.SURCHARGES:
            with self.subTest(wording=wording):
                models = [("grok-4.7", "SpaceXAI's latest frontier model"), ("grok-4.7-build-fast", wording),
                          ("grok-4.6", None)]
                self.assertEqual(rank_models_for_tier(models, "fast")[0][1], "grok-4.6")
                self.assertEqual(rank_models_for_tier(models, "standard")[0][1], "grok-4.7")

    def test_allowance_wording_does_not_move_codex_fast(self):
        for wording in ("About 4x the usage of sol on the same plan.", "2x the rate limits of sol."):
            with self.subTest(wording=wording):
                models = [("gpt-6.1-sol", "Latest workhorse model for coding and everyday work."),
                          ("gpt-6-astra", "Frontier intelligence for the most demanding work."),
                          ("gpt-6-luna", "Fast and affordable model for easier tasks. " + wording)]
                self.assertEqual(rank_models_for_tier(models, "fast")[0][1], "gpt-6-luna")
                self.assertEqual(rank_models_for_tier(models, "standard")[0][1], "gpt-6.1-sol")


class TestPremiumModelsInTheCheapTier(unittest.TestCase):
    def test_supports_fast_mode_does_not_make_opus_a_fast_family(self):
        models = [("claude-opus-5-5", "Opus 5.5 Our most capable model. Supports fast mode."),
                  ("claude-sonnet-5-5", "Sonnet 5.5 Most efficient for simpler tasks"),
                  ("claude-haiku-5-5", "Haiku 5.5 Fastest for quick answers")]
        self.assertEqual(rank_models_for_tier(models, "deep")[0][1], "claude-opus-5-5")
        self.assertEqual(rank_models_for_tier(models, "standard")[0][1], "claude-sonnet-5-5")
        self.assertEqual(rank_models_for_tier(models, "fast")[0][1], "claude-haiku-5-5")

    def test_newer_flagship_never_beats_an_unmarked_model_for_fast(self):
        for flagship in ("grok-5.0", "grok-6.2", "grok-9.0"):
            with self.subTest(flagship=flagship):
                models = [(flagship, "SpaceXAI's latest frontier model"), ("grok-4.6", None), ("grok-4.5", None)]
                self.assertEqual(rank_models_for_tier(models, "fast")[0][1], "grok-4.6")
                self.assertEqual(rank_models_for_tier(models, "standard")[0][1], flagship)
                self.assertEqual(rank_models_for_tier(models, "deep")[0][1], flagship)
        models = [("gpt-9-astra", "Frontier intelligence for the most demanding work."),
                  ("gpt-6.1-sol", "Latest workhorse model for coding and everyday work.")]
        self.assertEqual(rank_models_for_tier(models, "fast")[0][1], "gpt-6.1-sol")

    def test_a_fast_family_stays_a_fast_pick_when_called_most_capable(self):
        models = [("gemini-4.0-flash", "Our most capable Flash model"), ("gemini-3.8-flash", ""),
                  ("gemini-4.0-pro", "Frontier reasoning")]
        self.assertEqual(rank_models_for_tier(models, "fast")[0][1], "gemini-4.0-flash")

    def test_ties_are_settled_by_the_model_id_not_catalog_order(self):
        names = [("grok-4.7-build-fast", ""), ("grok-4.7", ""), ("grok-4.6", "")]
        for tier in ("fast", "standard", "deep"):
            with self.subTest(tier=tier):
                self.assertEqual(rank_models_for_tier(names, tier)[0][1],
                                 rank_models_for_tier(list(reversed(names)), tier)[0][1])
        self.assertEqual(rank_models_for_tier(names, "standard")[0][1], "grok-4.7")

    def test_equally_scored_local_models_tie_break_on_their_size(self):
        # Parameter counts do not score, so same-family sizes tie: the larger is
        # the deep pick and the smaller the fast one, in any listing order.
        for models in (["llama3.1:8b", "llama3.1:70b"], ["llama3.1:70b", "llama3.1:8b"]):
            with self.subTest(models=models):
                self.assertEqual(rank_models_for_tier(models, "deep")[0][1], "llama3.1:70b")
                self.assertEqual(rank_models_for_tier(models, "fast")[0][1], "llama3.1:8b")
        listed = ["gemma4:31b", "gemma4:26b", "gemma4:e4b", "qwen2.5-coder:7b"]
        self.assertEqual(rank_models_for_tier(listed, "deep")[0][1], "gemma4:31b")
        self.assertEqual(rank_models_for_tier(list(reversed(listed)), "deep")[0][1], "gemma4:31b")


class TestRealCatalogPicksUnchanged(_TempHome):
    """Catalogs shaped like the ones the provider CLIs write (invented values, no
    credentials): the claude, codex, muse and agy picks stay what they were."""

    KEEP_ISOLATION_ROOT = True  # never refresh the agy catalog here

    def setUp(self):
        super().setUp()
        efforts = [{"id": e, "name": e.title()} for e in ("low", "medium", "high", "xhigh", "max")]
        claude = [
            ("claude-opus-5-5", "Opus 5.5", "For complex work and everyday tasks", None),
            ("claude-fable-5-1", "Fable 5.1", "For your toughest challenges",
             "Fable is the most capable model and draws down usage much faster than Opus"),
            ("claude-sonnet-5-5", "Sonnet 5.5", "Most efficient for simpler tasks", None),
            ("claude-haiku-5-5", "Haiku 5.5", "Fastest for quick answers", None),
            ("claude-haiku-4-5-20251001", "Haiku 4.5", None, None),
            ("claude-sonnet-5", "Sonnet 5", None, None),
            ("claude-opus-5", "Opus 5", None, None),
            ("claude-fable-5", "Fable 5", None,
             "Fable is the most capable model and draws down usage much faster than Opus"),
        ]
        self.write(".claude/cache/model-catalog/catalog.json", {"version": 1, "catalog": {"config": {"models": [
            {"id": mid, "name": name, "short_name": name.split()[0], "description": desc, "section": "models",
             "notice": {"text": notice} if notice else None,
             "thinking": {"type": "adaptive", "description": "Thinking", "effort_options": efforts},
             "fast_mode": {"type": "toggle", "name": "Fast mode", "options": [{"id": "on", "name": "On"}]}}
            for mid, name, desc, notice in claude]}}})
        levels = [{"effort": e, "description": e} for e in ("low", "medium", "high", "xhigh", "max")]
        codex = [
            ("gpt-6.1-sol", "Latest workhorse model for coding and everyday work.", "list"),
            ("gpt-6-astra", "Frontier intelligence for the most demanding work.", "list"),
            ("gpt-6-sol", "Previous generation workhorse model.", "list"),
            ("gpt-6-luna", "Fast and affordable model for easier tasks.", "list"),
            ("gpt-reserve", "Fast and affordable agentic coding model.", "hide"),
            ("gpt-5.6-terra", "Older balanced model for straightforward work.", "list"),
            ("gpt-5.6-luna", "Older fast and efficient model.", "list"),
            ("codex-auto-review", "Automatic approval review model for Codex.", "hide"),
        ]
        self.write(".codex/models_cache.json", {"client_version": "0.0.0", "models": [
            {"slug": slug, "display_name": slug.upper(), "description": desc, "visibility": vis,
             "supported_reasoning_levels": levels,
             "service_tiers": [{"id": "priority", "name": "Fast", "description": "2x speed, increased usage"}]}
            for slug, desc, vis in codex]})
        (self.home / ".codex" / "config.toml").write_text('model = "gpt-6.1-sol"\nmodel_reasoning_effort = "max"\n',
                                                          encoding="utf-8")
        contributor = "Your content, including inter-session messages, may be used for product improvement."
        self.write(".local/share/muse/model-catalog/catalog.json", {"schema_version": 1, "rows": [
            {"model_id": "muse-spark-1.3", "display_label": "muse-spark-1.3", "visibility": "visible",
             "is_default": True, "is_current": False, "description": None},
            {"model_id": "muse-spark-1.3-contributor", "display_label": "muse-spark-1.3-contributor",
             "visibility": "hidden", "is_default": False, "is_current": True, "description": contributor},
            {"model_id": "muse-spark-1.2", "display_label": "muse-spark-1.2", "visibility": "visible",
             "is_default": False, "is_current": False, "description": None},
        ]})
        self.write(".config/muse/settings.json", {"model": "muse-spark-1.3-contributor", "reasoning_effort": "max"})
        self.write(".gemini/antigravity-cli/settings.json", {"model": "Gemini 3.8 Flash (High)"})
        agy = [(f"gemini-{v}-flash-{e}", f"Gemini {v} Flash ({e.title()})")
               for v in ("3.8", "3.7") for e in ("high", "medium", "low")]
        agy += [("gemini-3.1-pro-high", "Gemini 3.1 Pro (High)"), ("gemini-3.1-pro-low", "Gemini 3.1 Pro (Low)"),
                ("claude-opus-4-6-thinking", "Claude Opus 4.6 (Thinking)"),
                ("gpt-oss-120b-medium", "GPT-OSS 120B (Medium)")]
        self.write(".cache/makewand/agy_models_cache.json",
                   {"updated_at": time.time(), "models": [{"id": i, "description": d} for i, d in agy]})

    def test_picks(self):
        expected = {
            "claude": ("claude-haiku-5-5", "claude-sonnet-5-5", "claude-fable-5-1"),
            "codex": ("gpt-6-luna", "gpt-6.1-sol", "gpt-6-astra"),
            "muse": ("muse-spark-1.3", "muse-spark-1.3-contributor", "muse-spark-1.3"),
            "agy": ("gemini-3.8-flash", "gemini-3.8-flash", "gemini-3.1-pro"),
        }
        for provider, picks in expected.items():
            with self.subTest(provider=provider):
                got = tuple(get_provider_model_tier(provider, tier)["model"] for tier in ("fast", "standard", "deep"))
                self.assertEqual(got, picks)
        defaults = discover_available_models()
        self.assertEqual({p: defaults[p]["current_default"] for p in expected},
                         {"claude": "claude-sonnet-5-5", "codex": "gpt-6.1-sol",
                          "muse": "muse-spark-1.3-contributor", "agy": "gemini-3.8-flash"})



class TestAgyProbeSandbox(unittest.TestCase):
    """P1: the real agy probe goes through provider_sandbox_launch with a
    pinned native Gemini model, and fails closed without a sandbox."""

    def test_probe_command_pins_native_gemini_model(self):
        import makewand.health as health
        captured = {}

        def fake_probe(engine, command, timeout, repo_trust="trusted", cwd=None):
            captured["command"] = list(command)
            return (0, "ok\n", "", None)

        with patch.object(health, "_run_model_probe", side_effect=fake_probe), \
             patch("makewand.config.api_relay_available", return_value=False):
            entry = health._probe_agy_for_real()
        self.assertEqual(entry["status"], "healthy")
        cmd = captured["command"]
        self.assertIn("--model", cmd)
        self.assertEqual(cmd[cmd.index("--model") + 1], "gemini-3.8-flash-low")
        self.assertNotIn("opus", " ".join(cmd).lower())
        self.assertNotIn("sonnet", " ".join(cmd).lower())

    def test_probe_runs_through_sandbox_launch_not_bare_host(self):
        import makewand.health as health
        from contextlib import contextmanager
        from types import SimpleNamespace
        calls = {}

        @contextmanager
        def fake_launch(provider, cmd, cwd, **kwargs):
            calls["provider"] = provider
            calls["cmd"] = list(cmd)
            calls["kwargs"] = kwargs
            yield SimpleNamespace(cmd=["wrapped"] + list(cmd), pass_fds=(), env={})

        def fake_run(cmd, **kwargs):
            calls["run_cmd"] = list(cmd)
            return (0, "ok\n", "", None)

        with patch("makewand.sandbox.provider_sandbox_launch", fake_launch), \
             patch.object(health, "run_subprocess", side_effect=fake_run), \
             patch("makewand.config.api_relay_available", return_value=False):
            entry = health._probe_agy_for_real()
        self.assertEqual(entry["status"], "healthy")
        self.assertEqual(calls["provider"], "agy")
        self.assertTrue(calls["kwargs"]["readonly"])
        self.assertEqual(calls["run_cmd"][0], "wrapped")
        self.assertIn("gemini-3.8-flash-low", calls["run_cmd"])

    def test_probe_without_sandbox_fails_closed_and_never_runs_host_agy(self):
        import makewand.health as health
        from makewand.sandbox import SandboxUnavailableError
        ran = []
        with patch("makewand.sandbox.provider_sandbox_launch",
                   side_effect=SandboxUnavailableError(
                       "❌ [Agy 沙箱不可用] useG1Credits 未能证明均为 false，可能扣 credits")), \
             patch.object(health, "run_subprocess",
                          side_effect=lambda *a, **k: ran.append(a) or (0, "ok", "", None)), \
             patch("makewand.config.api_relay_available", return_value=False):
            entry = health._probe_agy_for_real()
        self.assertEqual(ran, [])
        self.assertEqual(entry["status"], "warning")
        self.assertIs(entry["verified"], False)
        self.assertIn("useG1Credits", entry["reason"])


if __name__ == "__main__":
    unittest.main()
