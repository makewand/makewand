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


# The host-wide gate (shared_capacity) reads each lease's limits from the ledger.

def _row(identifier, limits, project):
    return {"id": identifier, "kind": "model", "project": project, "task_id": identifier,
            "lease": {"keys": list(KEYS)}, "limits": list(limits)}


def test_host_gate_rows_carry_each_leases_own_limits(tmp_path):
    from makewand import shared_capacity

    def lease(with_limits):
        value = {"keys": list(KEYS), "owner": account_leases.process_ref(os.getpid()), "workers": [],
                 "expires_at": 4102444800, "heartbeat_at": 0, "launch_pending": False}
        if with_limits:
            value["limits"] = [8, 8, 8]
        return value

    ledger_file = tmp_path / "call-budget.json"
    ledger_file.write_text(__import__("json").dumps({
        "schema": 1, "maximum": 3,
        "attempts": [{"id": "old", "status": "started", "task_id": "t-old", "started_at": 1},
                     {"id": "new", "status": "started", "task_id": "t-new", "started_at": 2}],
        "leases": {"old": lease(False), "new": lease(True)},
        # The stored floor stands in only for the lease written before limits were kept.
        "lease_limits": dict.fromkeys(KEYS, 2),
    }))
    rows, _attempts, _now = shared_capacity._source_rows(str(ledger_file))
    assert rows["old"]["limits"] == [2, 2, 2]
    assert rows["new"]["limits"] == [8, 8, 8]


def test_host_gate_admits_up_to_the_raised_limit_once_older_leases_end():
    from makewand import shared_capacity
    current = {"global_max": 8, "project_max": 1, "tool_max": 2, "providers": {"claude": 8}}

    def fits(active, number):
        ticket = dict(_row(f"ticket-{number}", [8, 8, 8], f"project-{number}"), task_id=f"ticket-{number}")
        return shared_capacity.fits(ticket, active, current)

    old = [_row("old-1", [2, 2, 2], "a"), _row("old-2", [2, 2, 2], "b")]
    assert not fits(old, 3)                       # both were admitted with 2
    mixed = [old[1], _row("new-1", [8, 8, 8], "c")]
    assert not fits(mixed, 4)                     # one lease admitted with 2 is still running
    new = [_row(f"new-{i}", [8, 8, 8], f"p{i}") for i in range(7)]
    assert fits(new, 8)                           # seven running, limit eight
    assert not fits(new + [_row("new-7", [8, 8, 8], "p7")], 9)
