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
          "验收后记录成绩：makewand outcome <工具> <档> <类型> pass|fail")
CANDIDATE_LINE = re.compile(
    r"^  (\d+)\. makewand (\S+) (.*) -C <目录> -f <子任务文件> --max-model-calls 1   # (.+)$"
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

    @property
    def routing_dir(self) -> Path:
        return self.state_home / "makewand" / "routing"

    def run_cli(self, *args, quota=None, policy=None, worker=False):
        env = dict(os.environ)
        env.update({
            "HOME": str(self.home),
            "XDG_STATE_HOME": str(self.state_home),
            "MAKEWAND_CONFIG_DIR": str(self.base / "config"),
            "MAKEWAND_NO_DAEMON": "1",
            "MW_HUB_POLICY": str(self.policy_file if policy is not None else self.missing_policy),
        })
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
        result = self.run_cli("route", "implement", "mechanical", quota={})
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
        self.assertIn("验收不过就升一档：makewand route implement mechanical --tier 2", lines)
        self.assertEqual(lines[-1], FOOTER)
        # route 只读：不写成绩和标记文件。
        self.assertFalse((self.routing_dir / "outcomes.jsonl").exists())
        self.assertFalse((self.routing_dir / "marks.json").exists())

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
        result = self.run_cli("route", "implement", "mechanical", "--self", "agy", quota={})
        self.assertEqual(result.returncode, 0, result.stderr)
        candidates = [line for line in result.stdout.splitlines() if CANDIDATE_LINE.match(line)]
        self.assertIn(" agy ", candidates[-1])
        self.assertTrue(candidates[-1].endswith("（和你同一家，尽量不用）"))
        for line in candidates[:-1]:
            self.assertNotIn("和你同一家", line)
        payload = self.route_json("implement", "mechanical", "--self", "claude", quota={})
        self.assertEqual(payload["candidates"][-1]["provider"], "claude")
        self.assertTrue(payload["candidates"][-1]["is_self"])
        self.assertTrue(all(not item["is_self"] for item in payload["candidates"][:-1]))

    def test_route_not_skips_same_family(self):
        result = self.run_cli("route", "implement", "mechanical", "--not", "codex", quota={})
        self.assertEqual(result.returncode, 0, result.stderr)
        providers = [CANDIDATE_LINE.match(line).group(2)
                     for line in result.stdout.splitlines() if CANDIDATE_LINE.match(line)]
        self.assertNotIn("codex", providers)
        self.assertIn("跳过：codex（与 codex 同属 openai 家族）", result.stdout)
        payload = self.route_json("implement", "mechanical", "--not", "claude", quota={})
        self.assertEqual(payload["skipped"],
                         [{"provider": "claude", "reason": "与 claude 同属 anthropic 家族"}])

    # ---------- route 的 --json ----------

    def test_route_json_structure(self):
        payload = self.route_json("implement", "mechanical", quota={})
        self.assertEqual(payload["tier"], 1)
        self.assertEqual(payload["escalate_tier"], 2)
        self.assertEqual(payload["budget"], 3)
        self.assertEqual(payload["skipped"], [])
        self.assertEqual(len(payload["candidates"]), 4)
        for item in payload["candidates"]:
            self.assertIsInstance(item["args"], list)
            self.assertIsInstance(item["is_self"], bool)
            self.assertEqual(
                item["command"],
                f"makewand {item['provider']} {' '.join(item['args'])}"
                " -C <目录> -f <子任务文件> --max-model-calls 1",
            )
        strong = self.route_json("review", "hard", quota={})
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
        over = self.run_cli("route", "implement", "mechanical",
                            quota={"codex": [95, "10-20 15:04"], "claude": [10, ""]})
        self.assertEqual(over.returncode, 0, over.stderr)
        self.assertIn("跳过：codex（周额度已用 95%，达到阈值 90%（10-20 15:04））", over.stdout)
        under = self.run_cli("route", "implement", "mechanical",
                             quota={"codex": [10, ""], "claude": [84, ""]})
        self.assertNotIn("跳过：", under.stdout)
        payload = self.route_json("implement", "mechanical",
                                  quota={"claude": [91, "10-18 20:00"]})
        self.assertEqual([item["provider"] for item in payload["skipped"]], ["claude"])

    def test_route_missing_quota_file_means_no_data(self):
        result = self.run_cli("route", "implement", "mechanical")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("跳过：", result.stdout)

    def test_route_no_candidates(self):
        paused_all = 'PAUSED="agy:2099-01-01T00:00 codex:2099-01-01T00:00 grok:2099-01-01T00:00 claude:2099-01-01T00:00"\n'
        result = self.run_cli("route", "implement", "mechanical", quota={}, policy=paused_all)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("没有可用候选", result.stdout)
        self.assertIn("验收不过就升一档：makewand route implement mechanical --tier 2",
                      result.stdout.splitlines())
        top = 'PAUSED="codex:2099-01-01T00:00 agy:2099-01-01T00:00 claude:2099-01-01T00:00 grok:2099-01-01T00:00"\n'
        strong = self.run_cli("route", "review", "hard", quota={}, policy=top)
        self.assertIn("没有可用候选", strong.stdout)
        self.assertIn("后再试", strong.stdout)
        self.assertNotIn("验收不过就升一档", strong.stdout)

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

    # ---------- worker 拒绝与帮助 ----------

    def test_worker_environment_refuses_all_three(self):
        import makewand.cli as cli

        for name in ("route", "mark", "outcome"):
            self.assertNotIn(name, cli.WORKER_SAFE_SUBCOMMANDS)
        for args in (("route", "implement", "mechanical"),
                     ("mark", "codex", "6h"),
                     ("outcome", "codex", "1", "implement", "pass")):
            with self.subTest(args=args):
                result = self.run_cli(*args, quota={}, worker=True)
                self.assertEqual(result.returncode, 2, result.stdout)
                self.assertIn("递归", result.stderr)

    def test_help_lists_new_commands(self):
        result = self.run_cli("--help")
        self.assertEqual(result.returncode, 0, result.stderr)
        for name in ("route", "mark", "outcome"):
            self.assertIn(name, result.stdout)


if __name__ == "__main__":
    unittest.main()
