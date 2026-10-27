"""Transient Windows denials of accounting I/O are retried for a bounded time.

The retry helper is platform-neutral: these tests raise OSError subclasses
carrying a Windows error code, as ctypes.WinError does on Windows.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import unittest

from makewand import accounting_files


def winerror(code):
    class WinOSError(OSError):
        pass
    error = WinOSError(f"winerror {code}")
    error.winerror = code
    return error


class FakeClock:
    def __init__(self):
        self.now = 0.0
        self.sleeps = []

    def clock(self):
        return self.now

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.now += seconds


class TransientRetryTests(unittest.TestCase):
    def run_with(self, failures, final=None):
        clock = FakeClock()
        calls = []

        def operation():
            calls.append(clock.now)
            if len(calls) <= len(failures):
                raise failures[len(calls) - 1]
            return final
        result = accounting_files._retry_transient_windows(operation, sleep=clock.sleep, clock=clock.clock)
        return result, calls, clock

    def test_access_denied_and_sharing_violation_are_retried_until_success(self):
        result, calls, clock = self.run_with([winerror(5), winerror(32), winerror(5)], final="handle")
        self.assertEqual(result, "handle")
        self.assertEqual(len(calls), 4)
        self.assertEqual(clock.sleeps, [0.005, 0.01, 0.02])

    def test_persistent_denial_fails_closed_with_the_original_error_within_the_bound(self):
        clock = FakeClock()
        denial = winerror(5)
        attempts = []

        def operation():
            attempts.append(clock.now)
            raise denial
        with self.assertRaises(OSError) as raised:
            accounting_files._retry_transient_windows(operation, sleep=clock.sleep, clock=clock.clock)
        self.assertIs(raised.exception, denial)
        self.assertLessEqual(clock.now, accounting_files._TRANSIENT_RETRY_SECONDS)
        self.assertGreater(len(attempts), 3)
        self.assertLessEqual(max(clock.sleeps), 0.25)

    def test_other_errors_are_not_retried(self):
        for error in (PermissionError("Accounting state is owned by another account"),
                      FileNotFoundError(2, "missing"), winerror(2), ValueError("identity changed")):
            with self.subTest(error=error):
                calls = []

                def operation():
                    calls.append(1)
                    raise error
                with self.assertRaises(type(error)):
                    accounting_files._retry_transient_windows(operation, sleep=lambda _: self.fail("slept"))
                self.assertEqual(len(calls), 1)


if __name__ == "__main__":
    unittest.main()
