"""P0 rebase regressions: temporary credentials, synthetic process observations."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import argparse
import contextlib
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

from makewand import account_leases, call_budget, cli, discovery, execution_evidence, lease_process
from makewand.account_identity import account_ref
from makewand.execution_contract import ExecutionRequest
from makewand.execution_runtime import current_context, execute
from makewand.sandbox import ProviderHome, provider_sandbox_launch


class FinalFixTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.home = self.root / 'home'
        self.home.mkdir()
        env = patch.dict(os.environ, HOME=str(self.home), CODEX_HOME='', CLAUDE_CONFIG_DIR='',
                         MAKEWAND_CALL_BUDGET_FILE=str(self.root / 'calls.json'),
                         MAKEWAND_MAX_MODEL_CALLS='100', MAKEWAND_LAYERED_BUDGET='1',
                         MAKEWAND_PROJECT_DAILY_CALLS='100', MAKEWAND_MACHINE_DAILY_CALLS='100',
                         MAKEWAND_ACCOUNT_CONCURRENCY='1', MAKEWAND_POOL_CONCURRENCY='1',
                         MAKEWAND_LEASE_WAIT_SECONDS='0',
                         MAKEWAND_EXECUTION_EVIDENCE_DIR=str(self.root / 'evidence'))
        env.start()
        self.addCleanup(env.stop)

    def test_api_aliases_compete_but_subscription_stays_separate(self):
        expected = ['agy:api:provider', 'agy:api:unknown', 'agy:api:unknown:subscription']
        for engine in ('agy', 'gemini', 'antigravity', 'agy:api', 'gemini:api', 'antigravity:api'):
            for source in ('agy', 'gemini', 'antigravity'):
                self.assertEqual(account_leases.keys(engine, source + ':api:fixture-key'), expected)
        first = call_budget.reserve('agy', 'fast', account_ref='agy:api:fixture-key')
        for alias in ('agy', 'gemini', 'antigravity'):
            with self.subTest(alias=alias), self.assertRaisesRegex(call_budget.BudgetError, 'concurrency'):
                call_budget.reserve(alias, 'fast', account_ref=alias + ':api:fixture-key')
        subscription = call_budget.reserve('gemini', 'fast')
        call_budget.complete(first, False, 0)
        call_budget.complete(subscription, False, 0)

    def test_deleted_credentials_or_root_during_copy_refuse_launch(self):
        for provider, filename, variable in (('codex', 'auth.json', 'CODEX_HOME'),
                                              ('claude', '.credentials.json', 'CLAUDE_CONFIG_DIR')):
            for remove_root in (False, True):
                with self.subTest(provider=provider, remove_root=remove_root):
                    login = self.home / provider
                    login.mkdir()
                    (login / filename).write_text('{}')
                    original = ProviderHome._build
                    launched = []
                    private = []
                    def deleted(home, *args):
                        if remove_root:
                            shutil.rmtree(login)
                        else:
                            (login / filename).unlink()
                        return original(home, *args)
                    def callback(timeout):
                        home = ProviderHome(provider)
                        private.append(home)
                        try:
                            with patch.object(ProviderHome, '_build', deleted):
                                home.materialize()
                            launched.append(True)
                            return True, 'forbidden', None
                        finally:
                            home.finalize(write_back=False)
                    with patch.dict(os.environ, {variable: str(login)}):
                        result = execute(ExecutionRequest(task_id='deleted', engine=provider, stage='fixture'), callback)
                    self.assertFalse(result.success)
                    self.assertEqual(launched, [])
                    self.assertIsNone(private[0].private_dir)
                    shutil.rmtree(login, ignore_errors=True)

    def test_default_claude_metadata_deleted_after_admission_refuses_launch(self):
        login = self.home / '.claude'
        login.mkdir()
        (login / '.credentials.json').write_text('{}')
        metadata = self.home / '.claude.json'
        metadata.write_text('{"oauthAccount":{"accountUuid":"A"}}')
        original = ProviderHome._build
        launched = []
        prepared = []
        def deleted(home, *args):
            metadata.unlink()
            return original(home, *args)
        def callback(timeout):
            home = ProviderHome('claude')
            prepared.append(home)
            try:
                with patch.object(ProviderHome, '_build', deleted):
                    home.materialize()
                launched.append(True)
                return True, 'forbidden', None
            finally:
                home.finalize(write_back=False)
        result = execute(ExecutionRequest(task_id='deleted-metadata', engine='claude', stage='fixture'), callback)
        self.assertFalse(result.success)
        self.assertEqual(launched, [])
        self.assertIsNone(prepared[0].private_dir)

    def test_claude_evidence_uses_private_identity_and_invalidates_missing_or_symlink(self):
        for explicit in (False, True):
            for mutation in ('host_switch', 'private_switch', 'deleted', 'symlink', 'invalid'):
                with self.subTest(explicit=explicit, mutation=mutation):
                    login = self.home / '.claude'
                    login.mkdir(exist_ok=True)
                    (login / '.credentials.json').write_text('{"access_token":"fixture"}')
                    metadata = self.home / '.claude.json'
                    metadata.write_text('{"oauthAccount":{"accountUuid":"A"}}')
                    def callback(timeout):
                        home = ProviderHome('claude')
                        home.materialize()
                        identity = Path(home.identity_file[0]) / home.identity_file[1]
                        if mutation != 'host_switch':
                            current_context()['_evidence']['windows'] = [{
                                'window_key': '|300', 'window_minutes': 300,
                                'used_percent': 12, 'model': '',
                                'observed_at_ms': 1, 'reset_at_ms': 0}]
                        if explicit:
                            self.assertEqual(identity.parent, Path(home.credentials[0].private_root))
                        if mutation == 'host_switch':
                            metadata.write_text('{"oauthAccount":{"accountUuid":"B"}}')
                        elif mutation == 'private_switch':
                            identity.write_text('{"oauthAccount":{"accountUuid":"B"}}')
                        elif mutation == 'invalid':
                            identity.write_text('{"oauthAccount":{"accountUuid":123}}')
                        else:
                            identity.unlink()
                            if mutation == 'symlink':
                                identity.symlink_to(metadata)
                        home.finalize(write_back=False)
                        return True, 'ok', None
                    with patch.dict(os.environ, CLAUDE_CONFIG_DIR=str(login) if explicit else ''):
                        result = execute(ExecutionRequest(task_id='identity', engine='claude', stage='fixture'), callback)
                    self.assertTrue(result.success, result.error)
                    record = json.loads((self.root / 'evidence' / (result.attempt_id + '.json')).read_text())
                    expected = {'host_switch': 'A', 'private_switch': 'B'}.get(mutation)
                    self.assertEqual(record['account_ref'], account_ref('claude', expected) or '')
                    self.assertEqual(record['identity_known'], bool(expected))
                    self.assertEqual(record['pool_ref'], account_ref('claude', expected) + ':subscription' if expected else '')
                    self.assertEqual(record['windows'], [])

    def test_explicit_default_claude_direct_launch_preserves_config_directory(self):
        login = self.home / '.claude'
        login.mkdir()
        (login / '.credentials.json').write_text('{}')
        metadata = self.home / '.claude.json'
        metadata.write_text('{"oauthAccount":{"accountUuid":"A"}}')
        with patch.dict(os.environ, CLAUDE_CONFIG_DIR=str(login)), \
                patch('makewand.sandbox.is_bwrap_available', return_value=False), \
                patch('makewand.sandbox.authorize_unsandboxed_launch', return_value='fixture'):
            with provider_sandbox_launch('claude', ['fake-unused'], str(self.root / 'work'), readonly=True) as launch:
                self.assertNotIn('CLAUDE_CONFIG_DIR', launch.env)
                self.assertEqual(os.environ['CLAUDE_CONFIG_DIR'], str(login))

    def test_private_evidence_keeps_selected_snapshot_after_host_root_change(self):
        login = self.home / 'login'
        login.mkdir()
        (login / 'auth.json').write_text('{"tokens":{"account_id":"A"}}')
        replacement = self.home / 'replacement'
        replacement.mkdir()
        (replacement / 'auth.json').write_text('{"tokens":{"account_id":"B"}}')
        alias = self.home / 'selected'
        alias.symlink_to(login, target_is_directory=True)
        def callback(timeout):
            home = ProviderHome('codex')
            home.materialize()
            try:
                private = Path(home.credentials[0].private_root)
                (private / 'sessions').mkdir()
                (private / 'sessions/run.jsonl').write_text(json.dumps({
                    'type': 'turn_context', 'payload': {'model': 'frozen-fixture-model'}}))
                alias.unlink()
                alias.symlink_to(replacement, target_is_directory=True)
            finally:
                home.finalize(write_back=False)
            return True, 'ok', None
        with patch.dict(os.environ, CODEX_HOME=str(alias)):
            result = execute(ExecutionRequest(task_id='frozen-root', stage='fixture', engine='codex'), callback)
        self.assertTrue(result.success, result.error)
        record = json.loads((self.root / 'evidence' / (result.attempt_id + '.json')).read_text())
        self.assertEqual(record['account_ref'], account_ref('codex', 'A'))
        self.assertEqual(record.get('actual_model'), 'frozen-fixture-model')

    def test_new_codex_login_in_absent_root_is_collected_without_host_writeback(self):
        def callback(timeout):
            home = ProviderHome('codex')
            home.materialize()
            try:
                private = Path(home.credentials[0].private_root)
                (private / 'auth.json').write_text('{"tokens":{"account_id":"new-login"}}')
            finally:
                home.finalize(write_back=False)
            return True, 'ok', None
        result = execute(ExecutionRequest(task_id='new-login', stage='fixture', engine='codex'), callback)
        self.assertTrue(result.success, result.error)
        record = json.loads((self.root / 'evidence' / (result.attempt_id + '.json')).read_text())
        self.assertEqual(record['account_ref'], account_ref('codex', 'new-login'))
        self.assertFalse((self.home / '.codex').exists())

    def test_refresh_json_preserves_full_ids_and_provenance(self):
        cache = self.home / '.cache/makewand/agy_models_cache.json'
        cache.parent.mkdir(parents=True)
        cache.write_text(json.dumps({'models': ['gemini-4-argon-high']}))
        with patch.object(discovery, 'refresh_agy_models_cache') as refresh, \
                patch('makewand.providers.local.is_local_model_available', return_value=(False, None, [])), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            cli.cmd_models(argparse.Namespace(refresh=True, json=True))
        refresh.assert_called_once_with(timeout=discovery.AGY_MODELS_REFRESH_TIMEOUT_SECONDS)
        candidate = next(row for row in json.loads(output.getvalue())['agy']['candidates'] if row['model'] == 'gemini-4-argon-high')
        self.assertEqual(candidate['source'], str(cache))
        self.assertIsNotNone(candidate['cached_at'])


class PortableLeaseTests(unittest.TestCase):
    def test_nonlinux_dead_owner_reclaims_workerless_launch_fence(self):
        for platform in ('win32', 'darwin'):
            for fence, workers, reclaimed in ((False, [], True), (True, [], True),
                                               (False, [dict(pid=456, token=None, boot=None, group=None)], False)):
                with self.subTest(platform=platform, fence=fence, workers=bool(workers)):
                    data = dict(attempts=[{'id': 'charged'}], leases={'old': dict(
                        keys=account_leases.keys('agy', None),
                        owner=dict(pid=123, token=None, boot=None, group=None),
                        workers=workers, launch_pending=fence, expires_at=0)})
                    with patch.object(sys, 'platform', platform), \
                            patch.object(lease_process, '_windows_snapshot', return_value=(False, None)), \
                            patch.object(lease_process, '_darwin_rows', return_value={}):
                        if reclaimed:
                            account_leases.admission(data, 'gemini', None)
                            self.assertEqual(data['leases'], {})
                        else:
                            with self.assertRaisesRegex(call_budget.BudgetError, 'concurrency'):
                                account_leases.admission(data, 'gemini', None)
                    self.assertEqual(data['attempts'], [{'id': 'charged'}])

    def test_windows_handle_observation_distinguishes_death_from_access_denial(self):
        import ctypes
        kernel = Mock()
        kernel.OpenProcess.return_value = 123
        kernel.WaitForSingleObject.return_value = 258
        def times(handle, created, exited, kernel_time, user_time):
            created._obj.dwHighDateTime = 1
            created._obj.dwLowDateTime = 2
            return True
        kernel.GetProcessTimes.side_effect = times
        with patch.object(ctypes, 'WinDLL', return_value=kernel, create=True), \
                patch.object(ctypes, 'get_last_error', return_value=87, create=True) as error:
            self.assertEqual(lease_process._windows_snapshot(123), (True, '4294967298'))
            kernel.CloseHandle.assert_called_once_with(123)
            kernel.WaitForSingleObject.return_value = 0
            self.assertEqual(lease_process._windows_snapshot(123), (False, None))
            kernel.WaitForSingleObject.return_value = 0xffffffff
            self.assertEqual(lease_process._windows_snapshot(123), (None, None))
            self.assertEqual(kernel.CloseHandle.call_count, 3)
            kernel.OpenProcess.return_value = None
            self.assertEqual(lease_process._windows_snapshot(123), (False, None))
            error.return_value = 5  # ERROR_ACCESS_DENIED, not proof of death
            self.assertEqual(lease_process._windows_snapshot(123), (None, None))
            self.assertEqual(kernel.CloseHandle.call_count, 3)

    @unittest.skipUnless(sys.platform in ('win32', 'darwin'), 'native Windows/macOS process identity')
    def test_native_process_exit_and_birth_token(self):
        child = subprocess.Popen([sys.executable, '-I', '-c', 'import time; time.sleep(30)'],
                                 start_new_session=sys.platform != 'win32')
        try:
            ref = account_leases.process_ref(child.pid)
            self.assertIsNotNone(ref['token'])
            self.assertTrue(account_leases.alive(ref))
            self.assertFalse(account_leases.alive(dict(ref, token='different-birth')))
            child.kill()
            child.wait(timeout=5)
            self.assertFalse(account_leases.alive(ref))
        finally:
            if child.poll() is None:
                child.kill()
            child.wait(timeout=5)

    def test_windows_exit_pid_reuse_and_access_denied(self):
        ref = dict(pid=123, token='100', boot='windows', group=None)
        for snapshot, expected in (((False, None), False), ((True, '200'), False),
                                   ((True, '100'), True), ((None, None), True), ((True, None), True)):
            with self.subTest(snapshot=snapshot), patch.object(sys, 'platform', 'win32'), \
                    patch.object(lease_process, '_windows_snapshot', return_value=snapshot):
                self.assertEqual(account_leases.alive(ref), expected)
                self.assertTrue(account_leases.alive(ref, True))
                self.assertEqual(account_leases.process_ref(123)['token'], snapshot[1])

    def test_darwin_start_time_pid_reuse_zombie_and_surviving_group(self):
        ref = dict(pid=123, token='Sun Oct 18 01:02:03 2026', boot='darwin', group=123)
        for rows, owner, group in (({123: (123, 'S', ref['token'])}, True, True),
                                   ({123: (456, 'S', 'other-start')}, False, False),
                                   ({123: (123, 'Z', ref['token'])}, False, False),
                                   ({456: (123, 'S', 'child-start')}, False, True)):
            with self.subTest(rows=rows), patch.object(sys, 'platform', 'darwin'), \
                    patch.object(lease_process, '_darwin_rows', return_value=rows):
                self.assertEqual(account_leases.alive(ref), owner)
                self.assertEqual(account_leases.alive(ref, True), group)

    def test_darwin_no_start_time_falls_back_conservatively(self):
        ref = dict(pid=123, token=None, boot=None, group=123)
        for error, expected in ((None, True), (PermissionError(), True), (ProcessLookupError(), False)):
            with self.subTest(error=error), patch.object(sys, 'platform', 'darwin'), \
                    patch.object(lease_process, '_darwin_rows', side_effect=OSError()), \
                    patch.object(os, 'kill', side_effect=error):
                self.assertEqual(account_leases.alive(ref), expected)
                self.assertEqual(account_leases.alive(ref, True), expected)

    def test_darwin_parser_normalizes_time_and_rejects_bad_output(self):
        result = subprocess.CompletedProcess([], 0, '  123 123 S Sun Oct 18 01:02:03 2026\n', '')
        with patch.object(subprocess, 'run', return_value=result) as run:
            rows = lease_process._darwin_rows(123)
        self.assertEqual(rows[123], (123, 'S', 'Sun Oct 18 01:02:03 2026'))
        self.assertEqual(run.call_args.kwargs['env']['TZ'], 'UTC')
        result.stdout = 'unexpected output'
        with patch.object(subprocess, 'run', return_value=result), self.assertRaises(ValueError):
            lease_process._darwin_rows(123)
