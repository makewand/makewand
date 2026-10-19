"""R: offline regressions for the remaining second-round review findings."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import re
import unittest
from unittest.mock import patch

from makewand import candidate, config, engine_selection, health, orchestrator as orch
from makewand.execution_contract import EXIT_FAILED, EXIT_UNVERIFIED
from test_fix_orch_dag_r3 import DagFixture


class ReviewJSONTests(DagFixture):
    def test_non_utf8_diff_error_round_trips_on_a_strict_utf8_stream(self):
        repo = self.make_repo()
        error = 'cannot read lat\udce9n.txt'
        raw = io.BytesIO()
        with io.TextIOWrapper(raw, encoding='utf-8', errors='strict') as out:
            with patch.object(orch, 'get_git_diff_status', return_value=('', error)), \
                    contextlib.redirect_stdout(out):
                code = orch.run_review(cwd=str(repo), output_json=True)
            out.flush()
            report = json.loads(raw.getvalue())
        self.assertEqual(code, EXIT_UNVERIFIED)
        self.assertEqual(report['error'], error)

    def test_non_utf8_path_in_a_fake_review_verdict_stays_valid_json(self):
        repo = self.make_repo()
        path = 'lat\udce9n.txt'

        def reviewer(engine, prompt, **kwargs):
            tag = re.search(r'MAKEWAND_VERDICT_[0-9a-f]+(?=:)', prompt).group()
            return True, tag + ': ' + json.dumps({'pass': False, 'defects': [path]}, ensure_ascii=False), None

        out = io.StringIO()
        with patch.object(orch, 'get_git_diff', return_value='diff --git a/app.py b/app.py\n+VALUE = 2\n'), \
                patch.object(orch, 'get_or_update_status', return_value={'codex': {'status': 'healthy'}}), \
                patch.object(orch, 'dispatch_task', side_effect=reviewer), \
                contextlib.redirect_stdout(out):
            code = orch.run_review(cwd=str(repo), output_json=True, engine='codex')
        report = json.loads(out.getvalue().encode('utf-8', 'strict'))
        self.assertEqual(code, EXIT_FAILED)
        self.assertEqual(report['defects'], [path])

    def test_saved_hybrid_json_escapes_non_utf8_race_id(self):
        race_id = 'rc_\udce9'
        out = io.StringIO()
        with patch.object(candidate.CandidateManager, 'get_race', return_value=None), contextlib.redirect_stdout(out):
            code = orch.review_saved_hybrid(race_id, output_json=True)
        self.assertEqual(code, EXIT_UNVERIFIED)
        self.assertEqual(json.loads(out.getvalue().encode('utf-8'))['race_id'], race_id)


class ForcedAndHealthTests(unittest.TestCase):
    def test_unknown_and_placeholder_engines_cannot_be_forced(self):
        for name in ('cursor', 'copilot', 'nonexistent'):
            with self.subTest(name=name):
                self.assertTrue(engine_selection.forced_engine_refusal(name, {}))

    def test_missing_cache_entry_needs_an_installed_cli_or_allowed_api(self):
        for subscribed, paid, key, allowed in ((False, False, False, False), (True, False, False, True),
                                               (False, True, True, True), (False, False, True, False)):
            with self.subTest(subscribed=subscribed, paid=paid, key=key), \
                    patch('makewand.config.has_subscription_configured', return_value=subscribed), \
                    patch('makewand.config.has_api_configured', return_value=key), \
                    patch.dict(os.environ, {'MAKEWAND_API_POLICY': 'allow_paid' if paid else 'subscription_only'}):
                self.assertEqual(engine_selection.forced_engine_refusal('grok', {}) is None, allowed)

    def test_api_only_health_never_looks_for_a_cli(self):
        for name in config.API_ONLY_ENGINES:
            for key, paid in ((False, False), (True, False), (True, True)):
                with self.subTest(name=name, key=key, paid=paid), \
                        patch('makewand.config.has_subscription_configured', side_effect=AssertionError('no CLI')), \
                        patch('makewand.config.has_api_configured', return_value=key), \
                        patch.dict(os.environ, {'MAKEWAND_API_POLICY': 'allow_paid' if paid else 'subscription_only'}):
                    entry = health.probe_model(name)
                self.assertEqual(entry['status'], 'healthy' if key and paid else 'missing')
                self.assertNotIn('PATH', entry['reason'])
                self.assertIn('API', entry['reason'])
                if key and not paid:
                    self.assertIn('subscription_only', entry['reason'])


class CatalogStreamTests(unittest.TestCase):
    def test_python_rejects_lines_that_would_read_answer_streams(self):
        valid = json.loads(health.CLI_MESSAGE_CATALOG_FILE.read_text(encoding='utf-8'))
        for mode, own, answer in (('merged', ['stderr'], ['stdout']),
                                   ('apart', ['stderr'], ['stderr'])):
            for explicit in (True, False):
                catalog = json.loads(json.dumps(valid))
                entry = catalog['providers']['codex']
                entry.update(own_streams=own, answer_streams=answer)
                if explicit:
                    entry['selection'][mode] = 'lines'
                else:
                    entry['selection'].pop(mode, None)
                with self.subTest(mode=mode, explicit=explicit), \
                        patch('builtins.open', return_value=io.StringIO(json.dumps(catalog))):
                    with self.assertRaisesRegex(ValueError, 'answer_streams'):
                        health._load_cli_catalog()


@unittest.skipIf(os.name == 'nt', 'host manifest cache uses POSIX file identity')
class RaceFreezeTests(DagFixture):
    def setUp(self):
        super().setUp()
        self.repo = self.make_repo(files={'app.py': 'VALUE = 1\n', '.gitignore': '*.log\ndata/\n'})
        (self.repo / 'server.log').write_text('before\n')
        (self.repo / 'data').mkdir()
        (self.repo / 'data' / 'big.bin').write_bytes(b'x' * 1024)

    def race(self):
        with patch.object(orch, 'check_load_backpressure'), \
                patch.object(orch, 'get_or_update_status', return_value={e: {'status': 'healthy'} for e in ('claude', 'codex')}), \
                patch('makewand.config.get_active_providers', return_value=['claude', 'codex']), \
                patch('makewand.sandbox.verify_writable_sandbox_or_authorized', return_value=(True, None, None)), \
                patch.object(orch, 'dispatch_task', return_value=(False, None, 'fixture refusal')) as dispatch, \
                patch('makewand.usage.record_engine_usage'), contextlib.redirect_stdout(io.StringIO()) as out:
            code = orch.run_race('Implement value 2', cwd=str(self.repo), timeout=60)
        return code, out.getvalue(), dispatch.call_count

    def test_ignored_log_changes_during_copy_do_not_abort_freezing(self):
        clone = orch.clone_isolated_worktree
        manifests = []

        def copy(source, target):
            clone(source, target)
            (self.repo / 'server.log').write_text('changed during copy\n')

        def manifest(path, **kwargs):
            result = candidate.build_manifest(path, **kwargs)
            if Path(path) == self.repo:
                manifests.append((result, kwargs))
            return result

        with patch.object(orch, 'clone_isolated_worktree', side_effect=copy), \
                patch.object(orch, 'build_manifest', side_effect=manifest):
            code, log, dispatched = self.race()
        self.assertEqual(code, EXIT_FAILED)  # both fake contestants refuse after freezing
        self.assertGreater(dispatched, 0, log)
        self.assertEqual(len(manifests), 2)
        self.assertEqual(manifests[0][0], manifests[1][0])
        self.assertNotIn('server.log', manifests[0][0])
        self.assertNotIn('data/big.bin', manifests[0][0])
        self.assertIs(manifests[0][1]['cache'], manifests[1][1]['cache'])
        for _, options in manifests:
            self.assertTrue(options['host'])
            self.assertIsInstance(options['budget'], candidate.ManifestBudget)

    def test_tracked_changes_still_abort_before_dispatch(self):
        clone = orch.clone_isolated_worktree

        def copy(source, target):
            clone(source, target)
            (self.repo / 'app.py').write_text('VALUE = 3\n')

        with patch.object(orch, 'clone_isolated_worktree', side_effect=copy):
            code, log, dispatched = self.race()
        self.assertEqual((code, dispatched), (EXIT_FAILED, 0))
        self.assertIn('host workspace changed', log)

    def test_hash_errors_and_snapshot_limits_abort_cleanly(self):
        with patch.object(orch, 'build_manifest', side_effect=OSError('fixture hash failure')):
            code, log, dispatched = self.race()
        self.assertEqual((code, dispatched), (EXIT_FAILED, 0))
        self.assertIn('fixture hash failure', log)
        with patch.dict(os.environ, {'MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES': '1'}):
            code, log, dispatched = self.race()
        self.assertEqual((code, dispatched), (EXIT_FAILED, 0))
        self.assertIn('MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES', log)
        self.assertFalse(self.candidates.exists() and list(self.candidates.iterdir()))
