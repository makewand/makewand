"""route / mark / outcome 子命令：子进程跑 bin/makewand，只看公开行为。

每项测试用独立的临时 HOME、XDG_STATE_HOME、MW_HUB_POLICY 和
MAKEWAND_ROUTE_QUOTA_FILE，不碰真实账本、配置和 makewand-server，
也不调用任何模型。
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import re
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
LAUNCHER = REPO_ROOT / "bin" / "makewand"
WORKER_ENV = "MAKEWAND_PROVIDER_WORKER"
FOOTER = ("派活遇到 429 / 额度用完：makewand mark <工具> <剩余时长>；"
          "验收后记录成绩：makewand outcome <工具> <档> <类型> pass|fail"
          " --model <模型> --effort <推理深度>")
CANDIDATE_LINE = re.compile(
    r"^  (\d+)\. makewand (\S+) (.*) -C <目录> -f <子任务文件> --max-model-calls 1 --timeout \d+ --total-timeout \d+(?: --readonly)?   # (.+)$"
)


class RoutingCliTests(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-routing-cli-")
        self.addCleanup(self._tmp.cleanup)
        self.base = Path(self._tmp.name)
        self.home = self.base / "home"
        self.state_home = self.base / "state"
        self.work = self.base / "work"
        for path in (self.home, self.state_home, self.work):
            path.mkdir(parents=True)
        self.policy_file = self.base / "policy"
        self.missing_policy = self.base / "policy-absent"
        self.quota_file = self.base / "quota.json"
        self.missing_quota = self.base / "quota-absent.json"
        self.stub_log = self.base / "stub.log"

    @property
    def routing_dir(self) -> Path:
        return self.state_home / "makewand" / "routing"

    def test_request_context_survives_route_commands(self):
        payload = self.route_json("review", "normal", "--simple", "--request-id", "user-42",
                                  "--caller", "codex", "--max-model-calls", "5", quota={})
        self.assertEqual(payload["budget"], 5)
        self.assertTrue(payload["candidates"])
        for candidate in payload["candidates"]:
            self.assertIn("--request-id user-42", candidate["command"])
            self.assertIn("--caller codex", candidate["command"])
            self.assertIn("--max-model-calls 5", candidate["command"])

    def test_review_rejection_is_a_valid_review_and_has_separate_metadata(self):
        result = self.run_cli("outcome", "codex", "3", "review", "pass", "--candidate-verdict", "fail",
                              "--seconds", "12.5", "--calls", "1", "--request-id", "review-42",
                              "--caller", "agy", "--candidate-hash", "a" * 64)
        self.assertEqual(result.returncode, 0, result.stderr)
        rows = [json.loads(line) for line in (self.routing_dir / "outcomes.jsonl").read_text().splitlines()]
        row = rows[-1]
        self.assertTrue(row["passed"])
        self.assertTrue(row["review_validated"])
        self.assertFalse(row["candidate_approved"])
        self.assertEqual((row["seconds"], row["calls"], row["caller"], row["request_id"]), (12.5, 1, "agy", "review-42"))

    def test_recorded_request_failure_cools_route_without_affecting_other_requests(self):
        recorded = self.run_cli("outcome", "codex", "3", "review", "fail", "--failure-kind", "unknown",
                                "--request-id", "blocked-42", "--seconds", "1", "--calls", "1")
        self.assertEqual(recorded.returncode, 0, recorded.stderr)
        blocked = self.route_json("review", "hard", "--simple", "--request-id", "blocked-42", quota={})
        self.assertNotIn("codex", [c["provider"] for c in blocked["candidates"]])
        self.assertTrue(any("retry-approved" in s["reason"] for s in blocked["skipped"] if s["provider"] == "codex"))
        other = self.route_json("review", "hard", "--simple", "--request-id", "other-42", quota={})
        self.assertIn("codex", [c["provider"] for c in other["candidates"]])
        approved = self.route_json("review", "hard", "--simple", "--request-id", "blocked-42", "--retry-approved", quota={})
        self.assertIn("codex", [c["provider"] for c in approved["candidates"]])

    def test_inherited_request_cannot_be_replaced(self):
        result = self.run_cli("route", "review", "hard", "--request-id", "escape", "--json",
                              extra_env={"MAKEWAND_TASK_ID": "parent"})
        self.assertEqual(result.returncode, 2)
        self.assertIn("cannot replace", result.stderr)

    def test_empty_request_id_cannot_silently_start_a_new_budget(self):
        result = self.run_cli("route", "review", "normal", "--request-id", "", "--json")
        self.assertEqual(result.returncode, 2)
        self.assertIn("invalid request_id", result.stderr)

    def test_option_shaped_identifier_is_preserved_as_a_value(self):
        payload = self.route_json("review", "normal", "--simple", "--request-id=-user-42", quota={})
        self.assertTrue(payload["candidates"])
        self.assertTrue(all("--request-id=-user-42" in c["command"] for c in payload["candidates"]))

    def test_unverified_review_cannot_be_recorded_as_valid(self):
        result = self.run_cli("outcome", "codex", "3", "review", "pass", "--candidate-verdict", "unverified",
                              "--seconds", "1", "--calls", "1")
        self.assertEqual(result.returncode, 2)
        self.assertIn("cannot validate", result.stderr)

    def run_cli(self, *args, quota=None, policy=None, worker=False, extra_env=None):
        env = dict(os.environ)
        env.update({
            "HOME": str(self.home),
            "XDG_STATE_HOME": str(self.state_home),
            "XDG_CACHE_HOME": str(self.base / "xdg-cache"),
            "XDG_DATA_HOME": str(self.base / "xdg-data"),
            "XDG_CONFIG_HOME": str(self.base / "xdg-config"),
            "MAKEWAND_CONFIG_DIR": str(self.base / "config"),
            "MAKEWAND_NO_DAEMON": "1",
            "MW_HUB_POLICY": str(self.policy_file if policy is not None else self.missing_policy),
            "MAKEWAND_TEST_STUB_LOG": str(self.stub_log),
        })
        # 路由只认这次测试写进 HOME 的缓存，不读调用者或隔离根里的目录。
        for key in ("CODEX_HOME", "CLAUDE_CONFIG_DIR", "MUSE_DATA_DIR"):
            env.pop(key, None)
        if policy is not None:
            self.policy_file.write_text(policy, encoding="utf-8")
        if quota is None:
            env["MAKEWAND_ROUTE_QUOTA_FILE"] = str(self.missing_quota)
        else:
            self.quota_file.write_text(json.dumps(quota), encoding="utf-8")
            env["MAKEWAND_ROUTE_QUOTA_FILE"] = str(self.quota_file)
        env.pop(WORKER_ENV, None)
        if worker:
            env[WORKER_ENV] = "1"
        if extra_env:
            env.update(extra_env)
        return subprocess.run(
            [sys.executable, "-I", str(LAUNCHER), *args],
            env=env, cwd=self.work, capture_output=True, text=True,
            timeout=60, stdin=subprocess.DEVNULL,
        )

    def route_json(self, *args, **kwargs):
        result = self.run_cli("route", *args, "--json", **kwargs)
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    # ---------- route 文本输出 ----------

    def test_route_text_format(self):
        result = self.run_cli("route", "implement", "mechanical", "--simple", quota={})
        self.assertEqual(result.returncode, 0, result.stderr)
        lines = result.stdout.splitlines()
        self.assertEqual(
            lines[0],
            "子任务：implement / mechanical → 第 1 档（1 便宜快，3 强且推理深）；每个请求默认最多 3 次调用",
        )
        candidates = [line for line in lines if CANDIDATE_LINE.match(line)]
        self.assertEqual(len(candidates), 4)
        providers = [CANDIDATE_LINE.match(line).group(2) for line in candidates]
        self.assertEqual(set(providers), {"agy", "codex", "grok", "claude"})
        agy_line = next(line for line in candidates if " agy " in line)
        self.assertIn("--model gemini-3.8-flash-medium", agy_line)
        self.assertNotIn("跳过：", result.stdout)
        self.assertIn("验收不过就升一档：makewand route implement mechanical --tier 2 --simple", lines)
        self.assertEqual(lines[-1], FOOTER)
        self.assertNotIn("还有", result.stdout)
        # route 只读：不写成绩和标记文件。
        self.assertFalse((self.routing_dir / "outcomes.jsonl").exists())
        self.assertFalse((self.routing_dir / "marks.json").exists())

    def test_route_execution_limits_text_and_json(self):
        for task_type in ("review", "test", "research", "doc", "implement"):
            limits = (1200, 3600, 10800) if task_type == "implement" else (900, 1800, 5400)
            for difficulty, timeout in zip(("mechanical", "normal", "hard"), limits):
                with self.subTest(task_type=task_type, difficulty=difficulty):
                    text = self.run_cli("route", task_type, difficulty, "--simple", quota={})
                    self.assertEqual(text.returncode, 0, text.stderr)
                    commands = [line for line in text.stdout.splitlines()
                                if CANDIDATE_LINE.match(line)]
                    self.assertTrue(commands)
                    payload = self.route_json(task_type, difficulty, "--simple", quota={})
                    self.assertTrue(payload["candidates"])
                    for item in [payload, *payload["candidates"]]:
                        self.assertEqual(item["timeout"], timeout)
                        self.assertEqual(item["total_timeout"], timeout)
                        self.assertEqual(item["readonly"], task_type == "review")
                    commands += [item["command"] for item in payload["candidates"]]
                    for command in commands:
                        self.assertIn(f"--timeout {timeout} --total-timeout {timeout}", command)
                        self.assertEqual("--readonly" in command, task_type == "review")

    def test_route_timeout_tracks_tier_risk_and_explicit_total(self):
        for options in (("--tier", "3"), ("--risk", "high")):
            payload = self.route_json("implement", "mechanical", *options, quota={})
            self.assertEqual(payload["timeout"], 10800)
        overridden = self.route_json("review", "hard", "--total-timeout", "600", quota={})
        self.assertEqual(overridden["timeout"], 600)
        self.assertTrue(all(item["total_timeout"] == 600 for item in overridden["candidates"]))
        result = self.run_cli("route", "implement", "mechanical", "--total-timeout", "600", quota={})
        self.assertIn("--tier 2 --total-timeout 600", result.stdout)
        fractional = self.route_json("review", "hard", "--total-timeout", "0.5", quota={})
        self.assertEqual(fractional["timeout"], 0.5)
        self.assertTrue(fractional["candidates"])
        from makewand import cli
        import shlex
        from unittest.mock import patch
        class Parsed(Exception):
            pass
        resolve = cli._resolve_direct_timeout
        def capture(args):
            resolve(args)
            self.assertEqual(args.timeout, 0.5)
            raise Parsed()
        for item in fractional["candidates"]:
            self.assertIn("--timeout 1 --total-timeout 0.5", item["command"])
            with patch.object(sys, "argv", shlex.split(item["command"])), \
                    patch.object(cli, "_resolve_direct_timeout", capture):
                with self.assertRaises(Parsed):
                    cli._main()

    def test_route_starting_tiers_and_risk(self):
        pressed = self.run_cli("route", "test", "normal", quota={})
        self.assertIn("→ 第 1 档", pressed.stdout.splitlines()[0])
        middle = self.run_cli("route", "implement", "normal", quota={})
        self.assertIn("→ 第 2 档", middle.stdout.splitlines()[0])
        strong = self.run_cli("route", "review", "hard", quota={})
        self.assertIn("→ 第 3 档", strong.stdout.splitlines()[0])
        self.assertNotIn("验收不过就升一档", strong.stdout)
        risky = self.run_cli("route", "doc", "mechanical", "--risk", "high", quota={})
        self.assertEqual(risky.returncode, 0, risky.stderr)
        self.assertIn("→ 第 3 档", risky.stdout.splitlines()[0])

    def test_route_tier_override(self):
        result = self.run_cli("route", "implement", "hard", "--tier", "1", quota={})
        self.assertEqual(result.returncode, 0, result.stderr)
        lines = result.stdout.splitlines()
        self.assertIn("→ 第 1 档", lines[0])
        self.assertIn("验收不过就升一档：makewand route implement hard --tier 2", lines)

    def test_route_budget_from_policy(self):
        result = self.run_cli("route", "implement", "mechanical", quota={},
                              policy="REQUEST_BUDGET=5\n")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("每个请求默认最多 5 次调用", result.stdout.splitlines()[0])
        payload = self.route_json("implement", "mechanical", quota={},
                                  policy='REQUEST_BUDGET="7"\n')
        self.assertEqual(payload["budget"], 7)

    def test_route_self_goes_last(self):
        result = self.run_cli("route", "implement", "mechanical", "--self", "agy", "--simple", quota={})
        self.assertEqual(result.returncode, 0, result.stderr)
        candidates = [line for line in result.stdout.splitlines() if CANDIDATE_LINE.match(line)]
        self.assertIn(" agy ", candidates[-1])
        self.assertTrue(candidates[-1].endswith("（和你同一家，尽量不用）"))
        for line in candidates[:-1]:
            self.assertNotIn("和你同一家", line)
        payload = self.route_json("implement", "mechanical", "--self", "claude", "--simple", quota={})
        self.assertEqual(payload["candidates"][-1]["provider"], "claude")
        self.assertTrue(payload["candidates"][-1]["is_self"])
        self.assertTrue(all(not item["is_self"] for item in payload["candidates"][:-1]))

    def test_route_not_skips_same_family(self):
        result = self.run_cli("route", "implement", "mechanical", "--not", "codex", "--simple", quota={})
        self.assertEqual(result.returncode, 0, result.stderr)
        providers = [CANDIDATE_LINE.match(line).group(2)
                     for line in result.stdout.splitlines() if CANDIDATE_LINE.match(line)]
        self.assertNotIn("codex", providers)
        self.assertIn("跳过：codex（与 codex 同属 openai 家族）", result.stdout)
        payload = self.route_json("implement", "mechanical", "--not", "claude", "--simple", quota={})
        self.assertEqual(payload["skipped"],
                         [{"provider": "claude", "reason": "与 claude 同属 anthropic 家族"}])

    def test_route_not_and_self_cover_agy_proxied_models(self):
        # The owner's review rule: an agy-proxied Claude model must not review Claude's work.
        result = self.run_cli("route", "review", "hard", "--not", "claude", "--self", "claude", quota={})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("跳过：claude（与 claude 同属 anthropic 家族）；"
                      "agy（claude-opus-5-5-high 属于 claude 模型家族，与 claude 同家族）", result.stdout)
        self.assertNotIn("claude-opus", "\n".join(line for line in result.stdout.splitlines()
                                                  if CANDIDATE_LINE.match(line)))
        payload = self.route_json("review", "hard", "--not", "claude", quota={})
        self.assertEqual([item["candidate_id"] for item in payload["candidates"]
                          if item["model"].startswith("claude-")], [])
        self.assertIn({"provider": "agy", "reason": "claude-opus-5-5-high 属于 claude 模型家族，与 claude 同家族"},
                      payload["skipped"])
        demoted = self.route_json("review", "hard", "--self", "claude", quota={})
        flags = [item["is_self"] for item in demoted["candidates"]]
        self.assertEqual(flags, sorted(flags))
        self.assertTrue(next(item for item in demoted["candidates"]
                             if item["candidate_id"] == "agy/claude-opus-5-5-high/high")["is_self"])

    # ---------- route 的 --json ----------

    def test_route_json_structure(self):
        payload = self.route_json("implement", "mechanical", "--simple", quota={})
        self.assertEqual(payload["tier"], 1)
        self.assertEqual(payload["escalate_tier"], 2)
        self.assertEqual(payload["budget"], 3)
        self.assertEqual(payload["skipped"], [])
        self.assertEqual(len(payload["candidates"]), 4)
        for item in payload["candidates"]:
            self.assertIsInstance(item["args"], list)
            self.assertIsInstance(item["is_self"], bool)
            self.assertIsNone(item["model"])
            self.assertIsNone(item["effort"])
            self.assertIsNone(item["candidate_id"])
            self.assertEqual(
                item["command"],
                f"makewand {item['provider']} {' '.join(item['args'])}"
                " -C <目录> -f <子任务文件> --max-model-calls 1 --timeout 1200 --total-timeout 1200",
            )
        strong = self.route_json("review", "hard", "--simple", quota={})
        self.assertEqual(strong["tier"], 3)
        self.assertIsNone(strong["escalate_tier"])

    # ---------- 暂停、额度、mark 的跳过 ----------

    def test_route_paused_and_expired_pause(self):
        future = "2099-01-01T00:00"
        result = self.run_cli("route", "implement", "mechanical", quota={},
                              policy=f'PAUSED="codex:{future}"\n')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"跳过：codex（暂停到 {future}）", result.stdout)
        expired = self.run_cli("route", "implement", "mechanical", quota={},
                               policy='PAUSED="codex:2000-01-01T00:00"\n')
        self.assertNotIn("跳过：", expired.stdout)

    def test_route_quota_over_threshold(self):
        over = self.run_cli("route", "implement", "mechanical", "--simple",
                            quota={"codex": [95, "10-20 15:04"], "claude": [10, ""]})
        self.assertEqual(over.returncode, 0, over.stderr)
        self.assertIn("跳过：codex（周额度已用 95%，达到阈值 90%（10-20 15:04））", over.stdout)
        under = self.run_cli("route", "implement", "mechanical", "--simple",
                             quota={"codex": [10, ""], "claude": [84, ""]})
        self.assertNotIn("跳过：", under.stdout)
        payload = self.route_json("implement", "mechanical", "--simple",
                                  quota={"claude": [91, "10-18 20:00"]})
        self.assertEqual([item["provider"] for item in payload["skipped"]], ["claude"])

    def test_route_missing_quota_file_means_no_data(self):
        result = self.run_cli("route", "implement", "mechanical")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("跳过：", result.stdout)

    def test_route_no_candidates(self):
        paused_all = 'PAUSED="agy:2099-01-01T00:00 codex:2099-01-01T00:00 grok:2099-01-01T00:00 claude:2099-01-01T00:00"\n'
        result = self.run_cli("route", "implement", "mechanical", "--simple", quota={}, policy=paused_all)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("没有可用候选", result.stdout)
        self.assertIn("验收不过就升一档：makewand route implement mechanical --tier 2 --simple",
                      result.stdout.splitlines())
        top = 'PAUSED="codex:2099-01-01T00:00 agy:2099-01-01T00:00 claude:2099-01-01T00:00 grok:2099-01-01T00:00"\n'
        strong = self.run_cli("route", "review", "hard", "--simple", quota={}, policy=top)
        self.assertIn("没有可用候选", strong.stdout)
        self.assertIn("后再试", strong.stdout)
        self.assertNotIn("验收不过就升一档", strong.stdout)


    def test_route_full_skipped_list_is_complete_and_deduped(self):
        future = "2099-01-01T00:00"
        payload = self.route_json(
            "implement", "mechanical", quota={},
            policy=f'PAUSED="codex:{future} agy:{future}"\n')
        # Full catalog: codex/agy each have many model x effort combos, but
        # the skipped list holds exactly one entry per provider, in order.
        self.assertEqual(payload["skipped"], [
            {"provider": "agy", "reason": f"暂停到 {future}"},
            {"provider": "codex", "reason": f"暂停到 {future}"},
        ])
        self.assertTrue(all(item["provider"] not in ("codex", "agy")
                            for item in payload["candidates"]))


    def test_route_works_when_routing_dir_is_unusable(self):
        # State location is a regular file: the routing directory can never
        # be created there. route is read-only, so it must still answer
        # from an empty ledger instead of tracebacking on PermissionError.
        blocker = self.base / "state-file"
        blocker.write_text("not a directory", encoding="utf-8")
        result = self.run_cli(
            "route", "implement", "mechanical", "--simple", quota={},
            extra_env={"MAKEWAND_ROUTING_DIR": str(blocker / "routing")})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("子任务：implement / mechanical", result.stdout)
        self.assertNotIn("Traceback", result.stderr)

    def test_writes_to_an_unusable_routing_dir_fail_cleanly(self):
        blocker = self.base / "state-file"
        blocker.write_text("not a directory", encoding="utf-8")
        env = {"MAKEWAND_ROUTING_DIR": str(blocker / "routing")}
        for args, wanted in ((("mark", "codex", "6h"), "写入额度标记失败"),
                             (("outcome", "agy", "1", "review", "pass"), "写入成绩失败")):
            with self.subTest(command=args[0]):
                result = self.run_cli(*args, quota={}, extra_env=env)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(wanted, result.stderr)
                self.assertNotIn("Traceback", result.stderr)


    def test_escalate_hint_keeps_effective_constraints(self):
        result = self.run_cli(
            "route", "implement", "mechanical", "--tier", "1", "--risk", "high",
            "--self", "agy", "--not", "codex", "--legacy", quota={})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(
            "验收不过就升一档：makewand route implement mechanical --tier 2"
            " --risk high --self agy --not codex --legacy",
            result.stdout.splitlines())
        simple = self.run_cli(
            "route", "implement", "mechanical", "--simple", "--legacy", quota={})
        self.assertEqual(simple.returncode, 0, simple.stderr)
        # --legacy has no effect together with --simple, so only --simple is carried.
        self.assertIn(
            "验收不过就升一档：makewand route implement mechanical --tier 2 --simple",
            simple.stdout.splitlines())
        plain = self.run_cli("route", "implement", "mechanical", "--simple", quota={})
        self.assertIn(
            "验收不过就升一档：makewand route implement mechanical --tier 2 --simple",
            plain.stdout.splitlines())

    def test_prescan_only_treats_the_first_positional_as_a_subcommand(self):
        # A prompt's own options may carry a value that is also a subcommand
        # name (--provider agy); the auto-route must still insert "run".
        from unittest import mock
        sys.path.insert(0, str(REPO_ROOT))
        try:
            import makewand.cli as cli
        finally:
            sys.path.remove(str(REPO_ROOT))

        class Parsed(Exception):
            pass

        def fake_parse_args(parser, args=None, namespace=None):
            raise Parsed(list(sys.argv[1:]))

        cases = (
            (["fix the bug", "--provider", "agy"], ["run", "fix the bug", "--provider", "agy"]),
            # run options in front of the prompt, with values that are subcommand names
            (["--provider", "agy", "fix the bug"], ["run", "--provider", "agy", "fix the bug"]),
            (["-C", str(self.work), "--provider", "codex", "fix it"],
             ["run", "-C", str(self.work), "--provider", "codex", "fix it"]),
            (["--risk", "high", "mark", "codex", "6h"], ["--risk", "high", "mark", "codex", "6h"]),
            (["-C", str(self.work), "fix the route", "--provider", "codex"],
             ["run", "-C", str(self.work), "fix the route", "--provider", "codex"]),
            (["-C", str(self.work), "mark", "codex", "6h", "--reason", "x"],
             ["-C", str(self.work), "mark", "codex", "6h", "--reason", "x"]),
        )
        for argv, expected in cases:
            with self.subTest(argv=argv), \
                    mock.patch.object(sys, "argv", ["makewand", *argv]), \
                    mock.patch.dict(os.environ, {"MAKEWAND_NO_DAEMON": "1"}), \
                    mock.patch.object(cli.argparse.ArgumentParser, "parse_args", fake_parse_args):
                os.environ.pop(WORKER_ENV, None)
                with self.assertRaises(Parsed) as caught:
                    cli._main()
                self.assertEqual(caught.exception.args[0], expected)

    # ---------- mark ----------

    def test_mark_writes_and_route_skips(self):
        result = self.run_cli("mark", "codex", "6h", "--reason", "额度用完", quota={})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertRegex(result.stdout.strip(),
                         r"^已记录：codex 约 \d{2}-\d{2} \d{2}:\d{2} 前不再派活$")
        marks = json.loads((self.routing_dir / "marks.json").read_text(encoding="utf-8"))
        self.assertIn("codex", marks)
        routed = self.run_cli("route", "implement", "mechanical", quota={})
        self.assertIn("跳过：codex（额度标记未过期：额度用完）", routed.stdout)

    def test_mark_duration_formats(self):
        for provider, duration in (("agy", "93h17m"), ("grok", "2d"),
                                   ("muse", "45m"), ("claude", "30s"),
                                   ("codex", "1d2h3m4s")):
            with self.subTest(duration=duration):
                result = self.run_cli("mark", provider, duration, quota={})
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(f"已记录：{provider} 约 ", result.stdout)

    def test_mark_invalid(self):
        for args in (("mark", "bogus", "6h"), ("mark", "codex", "6x"),
                     ("mark", "codex", "10"), ("mark", "codex", "0m"),
                     ("mark", "codex", ""), ("mark", "codex", "1h2d")):
            with self.subTest(args=args):
                result = self.run_cli(*args, quota={})
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertTrue(result.stderr.strip())
        bad_provider = self.run_cli("mark", "bogus", "6h", quota={})
        self.assertIn("legal values", bad_provider.stderr)
        bad_duration = self.run_cli("mark", "codex", "6x", quota={})
        self.assertIn("d/h/m/s", bad_duration.stderr)


    def test_mark_huge_duration_fails_cleanly_without_traceback(self):
        for duration in ("999999999999999999999d", "366d", "10000d"):
            with self.subTest(duration=duration):
                result = self.run_cli("mark", "codex", duration, quota={})
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn("365", result.stderr)
                self.assertNotIn("Traceback", result.stderr)
        ok = self.run_cli("mark", "codex", "365d", quota={})
        self.assertEqual(ok.returncode, 0, ok.stderr)
        self.assertIn("已记录：codex 约 ", ok.stdout)

    # ---------- outcome ----------

    def test_outcome_records_and_stats_read_it(self):
        from makewand.outcome_ledger import OutcomeLedger

        result = self.run_cli("outcome", "codex", "1", "implement", "pass",
                              "--seconds", "12", "--calls", "1", quota={})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(result.stdout.strip().splitlines()), 1)
        self.assertIn("codex", result.stdout)
        self.assertIn("pass", result.stdout)
        ledger = OutcomeLedger(self.routing_dir)
        stats = ledger.stats("codex", 1, "implement")
        self.assertEqual(stats.count, 1)
        self.assertGreater(stats.alpha, 1.9)
        self.assertAlmostEqual(stats.avg_seconds, 12.0, delta=0.5)
        self.assertAlmostEqual(stats.avg_calls, 1.0, delta=0.1)
        failed = self.run_cli("outcome", "codex", "1", "implement", "fail", quota={})
        self.assertEqual(failed.returncode, 0, failed.stderr)
        stats = OutcomeLedger(self.routing_dir).stats("codex", 1, "implement")
        self.assertEqual(stats.count, 2)
        self.assertGreater(stats.beta, 1.9)
        rows = [json.loads(line) for line in
                (self.routing_dir / "outcomes.jsonl").read_text(encoding="utf-8").splitlines()]
        self.assertEqual([row["passed"] for row in rows], [True, False])
        routed = self.run_cli("route", "implement", "mechanical", quota={})
        self.assertEqual(routed.returncode, 0, routed.stderr)

    def test_outcome_invalid(self):
        for args in (("outcome", "codex", "9", "implement", "pass"),
                     ("outcome", "codex", "1", "bogus", "pass"),
                     ("outcome", "bogus", "1", "implement", "pass"),
                     ("outcome", "codex", "1", "implement", "maybe"),
                     ("outcome", "codex", "1", "implement", "pass", "--seconds", "-1"),
                     ("outcome", "codex", "1", "implement", "pass", "--calls", "-2")):
            with self.subTest(args=args):
                result = self.run_cli(*args, quota={})
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertTrue(result.stderr.strip())

    # ---------- route 非法参数 ----------

    def test_route_invalid_args(self):
        cases = (
            (("route", "bogus", "normal"), "review"),
            (("route", "implement", "bogus"), "mechanical"),
            (("route", "implement", "normal", "--tier", "4"), "legal values"),
            (("route", "implement", "normal", "--not", "bogus"), "legal values"),
            (("route", "implement", "normal", "--self", "bogus"), "legal values"),
            (("route", "implement", "normal", "--risk", "medium"), ""),
            (("route", "implement", "normal", "--tier", "abc"), ""),
            (("route",), ""),
        )
        for args, hint in cases:
            with self.subTest(args=args):
                result = self.run_cli(*args, quota={})
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertTrue(result.stderr.strip())
                if hint:
                    self.assertIn(hint, result.stderr)


    # ---------- 缩写解析（新命令拒绝，旧命令保持 master 行为） ----------

    def test_new_commands_reject_abbreviated_options(self):
        for args in (("route", "implement", "mechanical", "--jso"),
                     ("mark", "codex", "6h", "--reas", "x"),
                     ("outcome", "codex", "1", "implement", "pass", "--mod", "m")):
            with self.subTest(args=args):
                result = self.run_cli(*args, quota={})
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertTrue(result.stderr.strip())

    def test_old_commands_keep_abbreviated_options(self):
        # status is an old command: --jso still resolves to --json.
        result = self.run_cli("status", "--jso", quota={})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["engine"], "python")
        # Global options in front of a subcommand keep abbreviated form too.
        routed = self.run_cli("--repo-tr", "trusted", "route", "implement",
                              "mechanical", "--simple", quota={})
        self.assertEqual(routed.returncode, 0, routed.stderr)
        self.assertIn("子任务：implement / mechanical", routed.stdout)

    def write_discovery_cache(self):
        """把只有这次测试才有的模型写进 discovery 会读的本地缓存。"""
        codex = self.home / ".codex" / "models_cache.json"
        codex.parent.mkdir(parents=True)
        codex.write_text(json.dumps({"models": [
            {"slug": "gpt-6-luna-fixture", "visibility": "list",
             "supported_reasoning_levels": [{"effort": "low"}, {"effort": "high"}]},
            {"slug": "gpt-5.6", "visibility": "list",
             "supported_reasoning_levels": [{"effort": "low"}]},
        ]}), encoding="utf-8")
        agy = self.home / ".cache" / "makewand" / "agy_models_cache.json"
        agy.parent.mkdir(parents=True)
        agy.write_text(json.dumps({"updated_at": 0, "models": [
            {"id": "gemini-4-flash-low", "description": "Flash low"},
            {"id": "gemini-4-flash-high", "description": "Flash high"},
            {"id": "gemini-3.8-flash-medium", "description": "legacy flash"},
        ]}), encoding="utf-8")

    def stub_lines(self):
        if not self.stub_log.exists():
            return []
        return [line for line in self.stub_log.read_text(encoding="utf-8").splitlines() if line.strip()]

    def assert_full_candidate_line(self, line):
        match = CANDIDATE_LINE.match(line)
        self.assertIsNotNone(match, line)
        provider, args, label = match.group(2), match.group(3), match.group(4)
        self.assertIn(provider, label)
        self.assertIn("--model ", args)
        model = args.split("--model ", 1)[1].split()[0]
        self.assertIn(model, label)
        if provider == "agy":
            self.assertNotIn("--effort", args)
            self.assertIn("-", model)
            effort = model.rsplit("-", 1)[-1]
        else:
            self.assertIn("--effort ", args)
            effort = args.split("--effort ", 1)[1].split()[0]
        self.assertIn(effort, label)
        return provider, model, effort

    # ---------- 完整目录 ----------

    def test_route_default_uses_discovery_cache_not_the_network(self):
        self.write_discovery_cache()
        payload = self.route_json("implement", "mechanical", quota={})
        self.assertEqual(payload["tier"], 1)
        ids = {item["candidate_id"] for item in payload["candidates"]}
        self.assertIn("codex/gpt-6-luna-fixture/low", ids)
        self.assertIn("codex/gpt-6-luna-fixture/high", ids)
        self.assertNotIn("codex/gpt-6-luna/low", ids)
        self.assertNotIn("codex/gpt-5.6/low", ids)
        self.assertIn("agy/gemini-4-flash-low/low", ids)
        self.assertNotIn("agy/gemini-4-argon-low/low", ids)
        self.assertNotIn("agy/gemini-3.8-flash-medium/medium", ids)
        self.assertNotIn("agy/gemini-4-flash-high/high", ids)
        agy = next(item for item in payload["candidates"]
                   if item["candidate_id"] == "agy/gemini-4-flash-low/low")
        self.assertEqual(agy["provider"], "agy")
        self.assertEqual(agy["model"], "gemini-4-flash-low")
        self.assertEqual(agy["effort"], "low")
        self.assertEqual(agy["args"], ["--model", "gemini-4-flash-low"])
        codex = next(item for item in payload["candidates"]
                     if item["candidate_id"] == "codex/gpt-6-luna-fixture/low")
        self.assertEqual(codex["model"], "gpt-6-luna-fixture")
        self.assertEqual(codex["effort"], "low")
        self.assertEqual(codex["args"], ["--model", "gpt-6-luna-fixture", "--effort", "low"])
        for item in payload["candidates"]:
            self.assertEqual(item["candidate_id"], f"{item['provider']}/{item['model']}/{item['effort']}")
            self.assertIn("--model", item["command"])
        self.assertFalse(any(line.split()[0] == "agy" for line in self.stub_lines()), self.stub_lines())

    def test_route_simple_keeps_the_slim_table_when_a_cache_exists(self):
        self.write_discovery_cache()
        result = self.run_cli("route", "implement", "mechanical", "--simple", quota={})
        self.assertEqual(result.returncode, 0, result.stderr)
        candidates = [line for line in result.stdout.splitlines() if CANDIDATE_LINE.match(line)]
        self.assertEqual(len(candidates), 4)
        self.assertIn("--model gemini-3.8-flash-medium", result.stdout)
        self.assertNotIn("gpt-6-luna-fixture", result.stdout)
        self.assertNotIn("gemini-4-flash-low", result.stdout)
        self.assertNotIn("还有", result.stdout)
        self.assertEqual(result.stdout.splitlines()[-1], FOOTER)
        both = self.run_cli("route", "implement", "mechanical", "--simple", "--legacy", quota={})
        self.assertEqual(both.returncode, 0, both.stderr)

        def commands(stdout):
            found = set()
            for line in stdout.splitlines():
                match = CANDIDATE_LINE.match(line)
                if match:
                    found.add(match.group(2) + " " + match.group(3))
            return found

        self.assertEqual(commands(result.stdout), commands(both.stdout))

    def test_route_legacy_includes_old_models_from_the_cache(self):
        self.write_discovery_cache()
        payload = self.route_json("implement", "mechanical", "--legacy", quota={})
        ids = {item["candidate_id"] for item in payload["candidates"]}
        self.assertIn("codex/gpt-5.6/low", ids)
        self.assertIn("agy/gemini-3.8-flash-medium/medium", ids)
        old = next(item for item in payload["candidates"]
                   if item["candidate_id"] == "agy/gemini-3.8-flash-medium/medium")
        self.assertEqual(old["args"], ["--model", "gemini-3.8-flash-medium"])
        self.assertNotIn("--effort", old["args"])
        self.assertEqual(old["effort"], "medium")

    def test_route_text_lists_five_and_points_at_json_for_the_rest(self):
        result = self.run_cli("route", "implement", "mechanical", quota={})
        self.assertEqual(result.returncode, 0, result.stderr)
        shown = [line for line in result.stdout.splitlines() if CANDIDATE_LINE.match(line)]
        self.assertEqual(len(shown), 5)
        for line in shown:
            self.assert_full_candidate_line(line)
        payload = self.route_json("implement", "mechanical", quota={})
        hidden = len(payload["candidates"]) - 5
        self.assertGreater(hidden, 0)
        self.assertIn(f"还有 {hidden} 个，用 --json 看全部", result.stdout)
        self.assertEqual(result.stdout.count("还有"), 1)
        self.assertGreater(len(payload["candidates"]), 5)
        self.assertEqual(result.stdout.splitlines()[-1], FOOTER)
        self.assertFalse(any(line.split()[0] == "agy" for line in self.stub_lines()), self.stub_lines())

    def test_outcome_model_and_effort_are_visible_to_candidate_stats(self):
        from makewand.outcome_ledger import OutcomeLedger

        passed = self.run_cli(
            "outcome", "codex", "2", "implement", "pass",
            "--model", "gpt-6.1-sol", "--effort", "medium",
            "--seconds", "4", "--calls", "1", quota={},
        )
        self.assertEqual(passed.returncode, 0, passed.stderr)
        self.assertIn("model=gpt-6.1-sol", passed.stdout)
        self.assertIn("effort=medium", passed.stdout)
        failed = self.run_cli(
            "outcome", "codex", "2", "implement", "fail",
            "--model", "gpt-6-astra", "--effort", "high", quota={},
        )
        self.assertEqual(failed.returncode, 0, failed.stderr)
        ledger = OutcomeLedger(self.routing_dir)
        sol = ledger.stats("codex", 2, "implement", model="gpt-6.1-sol", effort="medium")
        self.assertEqual(sol.count, 1)
        self.assertGreater(sol.alpha, 1.9)
        self.assertAlmostEqual(sol.avg_seconds, 4.0, delta=0.5)
        astra = ledger.stats("codex", 2, "implement", model="gpt-6-astra", effort="high")
        self.assertEqual(astra.count, 1)
        self.assertGreater(astra.beta, 1.9)
        self.assertEqual(ledger.stats("codex", 2, "implement").count, 2)
        self.assertEqual(ledger.stats("codex", 2, "implement", model="gpt-6.1-sol").count, 1)
        rows = [json.loads(line) for line in
                (self.routing_dir / "outcomes.jsonl").read_text(encoding="utf-8").splitlines()]
        self.assertEqual(rows[0]["model"], "gpt-6.1-sol")
        self.assertEqual(rows[0]["effort"], "medium")
        self.assertEqual(rows[1]["model"], "gpt-6-astra")
        self.assertEqual(rows[1]["effort"], "high")

    def test_models_refresh_calls_explicit_refresh_with_fake_runner(self):
        import io
        import time
        from contextlib import redirect_stdout
        from types import SimpleNamespace
        from unittest.mock import patch

        import makewand.cli as cli
        import makewand.discovery as discovery

        cache = self.home / ".cache" / "makewand" / "agy_models_cache.json"
        cache.parent.mkdir(parents=True)
        cache.write_text(json.dumps({
            "updated_at": time.time(),
            "models": [{"id": "gemini-3-old", "description": "old"}],
        }), encoding="utf-8")
        self.assertGreater(cache.stat().st_size, 30)
        marker = cache.with_name(cache.name + ".attempt")
        state = discovery._agy_cache_state(cache)
        marker.write_text(state, encoding="utf-8")
        discovery._AGY_FAILED_REFRESHES[str(cache)] = (time.monotonic(), state)
        self.addCleanup(discovery._AGY_FAILED_REFRESHES.pop, str(cache), None)

        calls = []

        def runner(argv, **kwargs):
            calls.append((list(argv), kwargs.get("timeout")))
            return SimpleNamespace(
                returncode=0,
                stdout="gemini-4-argon-high\tGemini 4 Argon (High)\n",
            )

        real = discovery.refresh_agy_models_cache

        def spy(*args, **kwargs):
            self.assertIsNone(kwargs.get("runner"))
            self.assertEqual(kwargs.get("timeout"), discovery.AGY_MODELS_REFRESH_TIMEOUT_SECONDS)
            forwarded = dict(kwargs)
            forwarded["runner"] = runner
            return real(*args, **forwarded)

        env = {
            "HOME": str(self.home),
            "MAKEWAND_CONFIG_DIR": str(self.base / "config"),
            "XDG_DATA_HOME": str(self.base / "xdg-data"),
            "XDG_CACHE_HOME": str(self.base / "xdg-cache"),
            "MAKEWAND_TEST_STUB_LOG": str(self.stub_log),
        }
        with patch.dict(os.environ, env, clear=False), \
             patch("makewand.discovery.refresh_agy_models_cache", side_effect=spy) as refresh, \
             redirect_stdout(io.StringIO()) as captured:
            cli.cmd_models(SimpleNamespace(refresh=True))

        refresh.assert_called_once()
        self.assertEqual(calls, [(["agy", "models"], discovery.AGY_MODELS_REFRESH_TIMEOUT_SECONDS)])
        self.assertEqual(discovery.AGY_MODELS_REFRESH_TIMEOUT_SECONDS, 60.0)
        stored = json.loads(cache.read_text(encoding="utf-8"))
        self.assertEqual([item["id"] for item in stored["models"]], ["gemini-4-argon-high"])
        self.assertFalse(marker.exists())
        self.assertNotIn(str(cache), discovery._AGY_FAILED_REFRESHES)
        self.assertIn("gemini-4-argon-high", captured.getvalue())
        self.assertNotIn("gemini-3-old", captured.getvalue())
        self.assertFalse(any(line.split()[0] == "agy" for line in self.stub_lines()), self.stub_lines())

    # ---------- worker 拒绝与帮助 ----------

    def test_worker_environment_refuses_all_three(self):
        import makewand.cli as cli

        for name in ("route", "mark", "outcome", "models"):
            self.assertNotIn(name, cli.WORKER_SAFE_SUBCOMMANDS)
        for args in (("route", "implement", "mechanical"),
                     ("mark", "codex", "6h"),
                     ("outcome", "codex", "1", "implement", "pass"),
                     ("models", "--refresh")):
            with self.subTest(args=args):
                result = self.run_cli(*args, quota={}, worker=True)
                self.assertEqual(result.returncode, 2, result.stdout)
                self.assertIn("递归", result.stderr)

    def test_help_lists_new_commands(self):
        result = self.run_cli("--help")
        self.assertEqual(result.returncode, 0, result.stderr)
        for name in ("route", "mark", "outcome"):
            self.assertIn(name, result.stdout)
        route_help = self.run_cli("route", "--help")
        self.assertEqual(route_help.returncode, 0, route_help.stderr)
        self.assertIn("--simple", route_help.stdout)
        self.assertIn("--legacy", route_help.stdout)
        outcome_help = self.run_cli("outcome", "--help")
        self.assertEqual(outcome_help.returncode, 0, outcome_help.stderr)
        self.assertIn("--model", outcome_help.stdout)
        self.assertIn("--effort", outcome_help.stdout)
        models_help = self.run_cli("models", "--help")
        self.assertEqual(models_help.returncode, 0, models_help.stderr)
        self.assertIn("--refresh", models_help.stdout)


class RoutingCommandDocsTests(unittest.TestCase):
    def test_commands_are_documented_where_the_cli_is_listed(self):
        # #81 added route, mark and outcome; mark and outcome were documented nowhere.
        for path in (REPO_ROOT / "README.md", REPO_ROOT / "skills" / "makewand-orchestrator" / "SKILL.md",
                     REPO_ROOT / "site" / "docs.html"):
            text = path.read_text(encoding="utf-8")
            for command in ("makewand route", "makewand mark", "makewand outcome"):
                with self.subTest(doc=path.name, command=command):
                    self.assertIn(command, text)


if __name__ == "__main__":
    unittest.main()
