"""Concurrency floors follow the leases that are running, not the key.

Seen 2026-11-27: capacity-policy.json was raised from 2 to 8 slots, yet new
leases were still admitted with 2. The floor stored per key was inherited by
every later lease while any lease held the key, and Claude was never idle.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import itertools
import os
from unittest.mock import patch

import pytest

from makewand import account_leases
from makewand.call_budget import ConcurrencyBusyError

ACCOUNT = "claude:account:" + "a" * 32
KEYS = ["claude:provider", ACCOUNT, ACCOUNT + ":subscription"]
_ids = itertools.count()


def policy(slots):
    return {"providers": {"claude": slots}}


def admit(data, slots, **env):
    clean = {name: "" for name in ("MAKEWAND_PROVIDER_CONCURRENCY", "MAKEWAND_ACCOUNT_CONCURRENCY",
                                    "MAKEWAND_POOL_CONCURRENCY")}
    with patch.dict(os.environ, {**clean, **env}):
        lease = account_leases.admission(data, "claude", ACCOUNT, policy(slots))
    name = f"attempt-{next(_ids)}"
    data["leases"][name] = lease
    return name


def busy(data, slots, **env):
    with pytest.raises(ConcurrencyBusyError):
        admit(data, slots, **env)


def ledger():
    return {"attempts": [], "leases": {}}


def test_raised_policy_applies_as_soon_as_the_older_leases_end():
    data = ledger()
    first, second = admit(data, 2), admit(data, 2)
    busy(data, 2)
    busy(data, 8)  # the two running leases were admitted with 2
    del data["leases"][first]
    third = admit(data, 8)
    assert data["leases"][third]["limits"] == [8, 8, 8]
    busy(data, 8)  # one lease admitted with 2 is still running
    del data["leases"][second]
    # The key was never free, and the stored floor no longer outlives its leases.
    more = [admit(data, 8) for _ in range(7)]
    assert data["lease_limits"] == dict.fromkeys(KEYS, 8)
    busy(data, 8)
    assert len(data["leases"]) == 8 and third in data["leases"] and len(more) == 7


def test_lowered_policy_applies_at_once():
    data = ledger()
    admit(data, 8), admit(data, 8)
    busy(data, 2)
    assert data["lease_limits"]["claude:provider"] == 2


def test_caller_that_asked_for_less_is_not_overrun_while_it_runs():
    data = ledger()
    alone = admit(data, 8, MAKEWAND_PROVIDER_CONCURRENCY="1")
    assert data["leases"][alone]["limits"] == [1, 8, 8]
    busy(data, 8)
    del data["leases"][alone]
    admit(data, 8), admit(data, 8)


def test_lease_written_before_limits_were_stored_keeps_the_stored_floor():
    data = ledger()
    old = admit(data, 2)
    del data["leases"][old]["limits"]
    data["lease_limits"] = dict.fromkeys(KEYS, 2)
    admit(data, 8)
    busy(data, 8)
    assert data["lease_limits"]["claude:provider"] == 2
    del data["leases"][old]
    admit(data, 8), admit(data, 8)


@pytest.mark.parametrize("value", [None, [8, 8], [8, 8, 8, 8], [8, 8, 0], [8, 8, -1], [8, 8, True], [8, 8, 2.0],
                                   [8, 8, "8"], "888", {"a": 1}, [8, 8, 2 ** 63]])
def test_malformed_stored_limits_count_as_absent(value):
    assert account_leases.own_limits({"limits": value}) is None
    assert account_leases.own_limits({}) is None
    assert account_leases.own_limits({"limits": [1, 2, 3]}) == [1, 2, 3]


def test_lease_with_stored_limits_still_validates():
    data = ledger()
    admit(data, 8)
    account_leases.validate(data)
