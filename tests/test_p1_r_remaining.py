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

from makewand import config, engine_selection, health, orchestrator as orch
from makewand.execution_contract import EXIT_FAILED, EXIT_UNVERIFIED


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


