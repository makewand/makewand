"""Offline negative protocols; these tests do not certify a Windows desktop."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import copy
import json
import os
import shutil
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'scripts'))
from windows_test_report import ResultError, WINDOWS_SKIPS, python_inventory, summarize_go, summarize_python
from windows_verification_env import PROVIDERS, clean_checkout, isolated_env, scrub_env
from verify_windows_desktop import CASES, final_state


def go_events(*rows):
    return ('\n'.join(json.dumps(row) for row in rows) + '\n').encode()


class WindowsReleaseProtocolTests(unittest.TestCase):
    def setUp(self):
        self.expected = python_inventory(ROOT)

    def python_output(self, changes=None, omit=None, duplicate=None, warning=None):
        lines = []
        for name in sorted(self.expected):
            if name == omit:
                continue
            status = 'skipped ' + repr(WINDOWS_SKIPS[name]) if name in WINDOWS_SKIPS else 'ok'
            status = (changes or {}).get(name, status)
            line = name.rsplit('.', 1)[1] + ' (' + name + ') ... '
            if name == warning:
                lines.extend((line + 'warning: fixture audit unavailable', 'ok'))
            else:
                lines.append(line + status)
            if name == duplicate:
                lines.append(line + status)
        lines.extend(('', '-' * 70, 'Ran %d tests in 1.000s' % len(self.expected), '', 'OK (skipped=8)', ''))
        return '\n'.join(lines).encode()

    def test_python_complete_inventory_and_exact_platform_skips(self):
        result = summarize_python(self.python_output(), self.expected, 'utf-8')
        self.assertEqual(result['ran'], len(self.expected))
        self.assertEqual(result['pass'], len(self.expected) - 8)
        self.assertEqual({r['test']: r['reason'] for r in result['skips']}, WINDOWS_SKIPS)

    def test_python_inventory_rejects_fake_footer_missing_duplicate_and_unknown_skip(self):
        native = next(name for name in sorted(self.expected) if name not in WINDOWS_SKIPS)
        cases = [b'Ran 75 tests in 1.000s\n\nOK (skipped=8)\n',
                 self.python_output(omit=native), self.python_output(duplicate=native),
                 self.python_output({native: "skipped 'missingAPI'"}),
                 self.python_output({next(iter(WINDOWS_SKIPS)): "skipped 'wrong platform reason'"})]
        for data in cases:
            with self.subTest(output_length=len(data)), self.assertRaises(ResultError):
                summarize_python(data, self.expected, 'utf-8')

    def test_python_warning_then_standalone_completion_is_preserved(self):
        name = next(n for n in self.expected if n not in WINDOWS_SKIPS)
        self.assertEqual(summarize_python(self.python_output(warning=name), self.expected, 'utf-8')['pass'], len(self.expected) - 8)

    def test_python_failure_and_invalid_encoding_fail_closed(self):
        name = next(n for n in self.expected if n not in WINDOWS_SKIPS)
        with self.assertRaises(ResultError):
            summarize_python(self.python_output({name: 'FAIL'}), self.expected, 'utf-8')
        with self.assertRaises(UnicodeDecodeError):
            summarize_python(b'\xff', self.expected, 'utf-8')

    def go_row(self, action, test=None, package='example/native', **extra):
        return dict(Package=package, Action=action, **({'Test': test} if test else {}), **extra)

    def test_go_exact_packages_and_actual_run_completion(self):
        data = go_events(self.go_row('run', 'TestWindowsReal'), self.go_row('pass', 'TestWindowsReal'),
                         self.go_row('pass'), self.go_row('skip', package='example/commands'))
        report = summarize_go(data, {'example/native': True, 'example/commands': False}, full_race=True)
        self.assertEqual(report['pass'], 1)
        self.assertTrue(report['full_race'])
        self.assertEqual(report['no_test_file_packages'], ['example/commands'])

    def test_go_unfinished_native_run_cannot_hide_behind_child_and_package_pass(self):
        data = go_events(self.go_row('run', 'TestCLIProcessChild'), self.go_row('pass', 'TestCLIProcessChild'),
                         self.go_row('run', 'TestCLIProcessStartFailureHonorsContextAndBudget'), self.go_row('pass'))
        with self.assertRaisesRegex(ResultError, 'RUN/completion'):
            summarize_go(data, {'example/native': True})

    def test_go_duplicate_failure_or_missing_package_cannot_turn_green(self):
        valid = [self.go_row('run', 'TestReal'), self.go_row('pass', 'TestReal'), self.go_row('pass')]
        cases = [valid + [self.go_row('pass', 'TestReal')],
                 [self.go_row('run', 'TestReal'), self.go_row('fail', 'TestReal'), self.go_row('pass', 'TestReal'), self.go_row('pass')],
                 valid[:-1], [self.go_row('pass')], [self.go_row('pass', 'TestReal'), self.go_row('pass')]]
        for rows in cases:
            with self.subTest(rows=rows), self.assertRaises(ResultError):
                summarize_go(go_events(*rows), {'example/native': True})
        with self.assertRaises(ResultError):
            summarize_go(go_events(*valid), {'example/native': True, 'example/missing': True})

    def test_go_foreign_package_events_cannot_hide_without_package_completion(self):
        valid = [self.go_row('run', 'TestReal'), self.go_row('pass', 'TestReal'), self.go_row('pass')]
        foreign = [self.go_row('run', 'TestForeign', package='example/foreign'),
                   self.go_row('pass', 'TestForeign', package='example/foreign')]
        for extra in (foreign, [self.go_row('output', package='example/foreign', Output='foreign diagnostic\n')]):
            with self.subTest(extra=extra), self.assertRaisesRegex(ResultError, 'unexpected package'):
                summarize_go(go_events(*(valid + extra)), {'example/native': True})

    def test_go_required_native_skip_and_entirely_skipped_package_rejected(self):
        for name in ('TestWindowsProcessJobMemoryLimitConfigured', 'TestPendingApprovalWindowsPrivateDirectoryBindsCurrentUser',
                     'TestOutputCaptureRejectsTruncatedSuccessfulCommand', 'TestMixedGoPythonProcessesShareHardLimit', 'TestStoreRejectsSymlinkLockWithoutChangingTarget'):
            data = go_events(self.go_row('run', 'TestChild'), self.go_row('pass', 'TestChild'),
                             self.go_row('run', name), self.go_row('output', name, Output='missing capability\n'),
                             self.go_row('skip', name), self.go_row('pass'))
            with self.subTest(name=name), self.assertRaisesRegex(ResultError, 'required native test skipped'):
                summarize_go(data, {'example/native': True})
        with self.assertRaises(ResultError):
            summarize_go(go_events(self.go_row('run', 'TestOptional'), self.go_row('skip', 'TestOptional'), self.go_row('pass')), {'example/native': True})

    def test_go_explicit_optional_skip_is_retained_and_invalid_json_rejected(self):
        rows = [self.go_row('run', 'TestReal'), self.go_row('pass', 'TestReal'),
                self.go_row('run', 'TestStoreLockNormalizesPermissionsAndSpecialBits'),
                self.go_row('output', 'TestStoreLockNormalizesPermissionsAndSpecialBits', Output='POSIX file modes\n'),
                self.go_row('skip', 'TestStoreLockNormalizesPermissionsAndSpecialBits'), self.go_row('pass')]
        report = summarize_go(go_events(*rows), {'example/native': True})
        self.assertIn('POSIX file modes', report['skips'][0]['reason'])
        with self.assertRaises(ValueError):
            summarize_go(b'not json\n', {'example/native': True})


class WindowsVerificationBoundaryTests(unittest.TestCase):
    def test_environment_scrubs_accounts_endpoints_git_redirects_and_policy(self):
        dangerous = {'CODEX_HOME': 'real-account', 'GIT_WORK_TREE': 'other-repo', 'git_config_count': '1',
                     'GOOGLE_APPLICATION_CREDENTIALS': 'account.json', 'AWS_PROFILE': 'real-profile',
                     'OPENAI_BASE_URL': 'remote', 'LOCAL_API_KEY': 'secret', 'PYTHONPATH': 'injection',
                     'MAKEWAND_REMOTE_URL': 'remote', 'MAKEWAND_API_POLICY': 'paid', 'GORACE': 'halt_on_error=0'}
        self.assertEqual(scrub_env(dangerous), {})
        with tempfile.TemporaryDirectory() as directory:
            env = isolated_env(Path(directory), {**dangerous, 'PATH': 'tools', 'SystemRoot': 'system', 'SYSTEMTEMP': 'old-system-temp', 'tMp': 'old-tmp', 'home': 'old-home', 'userprofile': 'old-user-profile', 'appData': 'old-appdata', 'localappdata': 'old-local-appdata', 'homepath': 'old-home-path'})
            self.assertEqual(env['PATH'], 'tools')
            self.assertEqual(env['SystemRoot'], 'system')
            self.assertEqual(len(env), len({key.upper() for key in env}))
            self.assertFalse(any(value.startswith('old-') for value in env.values()))
            for key in ('HOME', 'USERPROFILE', 'APPDATA', 'LOCALAPPDATA', 'CODEX_HOME', 'MAKEWAND_CONFIG_DIR', 'TMP', 'TEMP', 'SystemTemp', 'HOMEPATH'):
                self.assertEqual(sum(name.upper() == key.upper() for name in env), 1)
                self.assertTrue(Path(env[key]).is_relative_to(Path(directory)))
                self.assertTrue(Path(env[key]).is_dir())
            self.assertEqual(env['CODEX_HOME'], str(Path(directory) / 'codex'))
            self.assertEqual(env['MAKEWAND_API_POLICY'], 'subscription_only')
            cfg = json.loads((Path(directory) / 'config/config.json').read_text())
            self.assertEqual(cfg['enabled_providers'], dict.fromkeys(PROVIDERS, False))
            self.assertFalse(cfg['allow_paid'])
            cfg = json.loads((Path(directory) / 'config/config.json').read_text())
            self.assertEqual(cfg['api_policy'], 'subscription_only')

    @unittest.skipUnless(shutil.which('git'), 'Git required for checkout-bound verification')
    def test_checkout_rejects_dirty_untracked_and_wrong_commit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            repo = root / 'repo'; repo.mkdir()
            env = isolated_env(root / 'isolated')
            def git(*args):
                return subprocess.check_output(['git', *args], cwd=repo, env=env, stderr=subprocess.PIPE).decode().strip()
            git('init')
            (repo / 'module.py').write_text('answer = 1\n')
            git('add', 'module.py')
            git('-c', 'user.name=Offline QA', '-c', 'user.email=qa@example.invalid', 'commit', '-m', 'fixture')
            commit = git('rev-parse', 'HEAD')
            self.assertEqual(clean_checkout(repo, commit, env), commit)
            with self.assertRaises(ValueError): clean_checkout(repo, '0' * 40, env)
            (repo / 'module.py').write_text('answer = 2\n')
            with self.assertRaises(ValueError): clean_checkout(repo, commit, env)
            (repo / 'module.py').write_text('answer = 1\n')
            (repo / 'extra.py').write_text('answer = 3\n')
            with self.assertRaises(ValueError): clean_checkout(repo, commit, env)

    def report(self):
        return {'manual_cases': {name: {'result': 'pass', 'observation': 'Actually observed in the terminal'} for name in CASES},
                'ui_runs': [{'exit': 0, 'elapsed_seconds': 1}]}

    def test_desktop_prepared_or_corrupt_record_never_implies_manual_pass(self):
        good = self.report()
        self.assertEqual(final_state(good, ['version']), 'manual_desktop_passed')
        reports = []
        for bad in ({'result': 'pending', 'observation': 'no'}, {'result': 'pass', 'observation': ''}, {'result': 'pass', 'observation': 'x' * 2001}, {'result': 'fail', 'observation': 'failure'}, {'result': 'pass'}):
            row = copy.deepcopy(good); row['manual_cases'][CASES[0]] = bad; reports.append(row)
        row = copy.deepcopy(good); del row['manual_cases'][CASES[0]]; reports.append(row)
        row = copy.deepcopy(good); row['manual_cases']['extra'] = {'result': 'pass', 'observation': 'no'}; reports.append(row)
        row = copy.deepcopy(good); row['ui_runs'][0]['exit'] = False; reports.append(row)
        row = copy.deepcopy(good); row['ui_runs'] = []; reports.append(row)
        for report in reports:
            with self.subTest(report=report): self.assertEqual(final_state(report, ['version']), 'failed')
        pending = self.report(); pending['manual_cases'] = dict.fromkeys(CASES)
        self.assertEqual(final_state(pending, ['version']), 'pending_manual')
        self.assertEqual(final_state(good, ['version', 'blocked_invocation']), 'failed')

    @unittest.skipIf(os.name == 'nt', 'non-Windows refusal contract')
    def test_native_and_desktop_entrypoints_refuse_this_platform(self):
        with tempfile.TemporaryDirectory() as directory:
            out = Path(directory)
            gate = subprocess.run([sys.executable, '-I', str(ROOT / 'scripts/windows_test_gate.py'), '--output', str(out / 'gate'), '--expected-sha', '0' * 40], capture_output=True, timeout=10)
            self.assertEqual(gate.returncode, 1)
            self.assertEqual(json.loads((out / 'gate/summary.json').read_text())['state'], 'failed')
            desktop = subprocess.run([sys.executable, '-I', str(ROOT / 'scripts/verify_windows_desktop.py'), 'prepare', '--output', str(out / 'desktop')], capture_output=True, timeout=10)
            self.assertEqual(desktop.returncode, 1)
            self.assertIn(b'actual Windows desktop', desktop.stderr)
            self.assertFalse((out / 'desktop').exists())


if __name__ == '__main__':
    unittest.main()
