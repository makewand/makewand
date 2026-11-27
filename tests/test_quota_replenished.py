"""A stored refusal that names a reset time ends when the provider replenishes that pool early.

Seen 2026-11-27: Claude's weekly quota was restored hours after a refusal that
named Nov 29 as its reset. The official windows showed 1% used, yet the stored
refusal kept being reported until someone cleared it by hand.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

from datetime import datetime, timedelta, timezone

import pytest

from makewand import health, model_quota

IDENTITY = {"credential_root": "/home/alice/.claude", "account_ref": "claude:account:a", "identity_known": True,
            "pool_ref": "claude:account:a:subscription"}
NOW = datetime.now(timezone.utc)
RESET = NOW + timedelta(days=2)


def window(used, *, model="", reset=RESET, observed=NOW, pool=IDENTITY["pool_ref"], scope="account"):
    return {"used_percent": used, "window_minutes": 10080, "model": model, "scope": scope, "pool_ref": pool,
            "resets_at": reset.isoformat(), "observed_at": observed.isoformat()}


def official(*windows, account=IDENTITY["account_ref"], pool=IDENTITY["pool_ref"]):
    return {"account_ref": account, "pool_ref": pool, "windows": list(windows)}


@pytest.fixture(autouse=True)
def selected_account(monkeypatch):
    monkeypatch.setattr(model_quota, "selected_scope", lambda provider: dict(IDENTITY))


def replenished(model, quota, *, reset=RESET, since=NOW - timedelta(hours=4), provider="claude"):
    return model_quota.replenished(provider, model, reset.timestamp(),
                                   since.timestamp() if since else None, quota)


def test_pool_with_quota_again_is_replenished():
    assert replenished("claude-fable-5-2", official(window(1), window(2, model="Fable")))
    assert replenished(None, official(window(model_quota.REPLENISHED_USED_PERCENT)))
    assert replenished("claude-opus-5-5", official(window(1)), since=None)


@pytest.mark.parametrize("quota", [
    official(window(96)),                                              # still nearly exhausted
    official(window(1, observed=NOW - timedelta(hours=5))),            # read before the refusal
    official(window(1, observed=NOW - timedelta(minutes=16))),         # stale reading
    official(window(1, reset=NOW + timedelta(hours=3))),               # another pool (5-hour window)
    official(window(1, reset=RESET + timedelta(minutes=31))),
    official(window(1, pool="claude:account:b:subscription")),         # another account's window
    official(window(1), account="claude:account:b"),
    official(window(1), pool="claude:account:b:subscription"),
    official(window(1, scope="unknown")),
    official(window("1")), official(window(None)), official(window(-1)), official(window(float("nan"))),
    official({"used_percent": 1}), official(), {}, None, "x",
])
def test_no_trustworthy_reading_of_that_pool_clears_nothing(quota):
    assert not replenished("claude-fable-5-2", quota)


def test_model_sublimit_counts_only_for_its_model():
    quota = official(window(1), window(100, model="Fable"))
    assert not replenished("claude-fable-5-2", quota)
    assert replenished("claude-opus-5-5", quota)
    assert replenished(None, quota)
    # A sublimit alone says nothing about an account-wide refusal.
    assert not replenished(None, official(window(1, model="Fable")))


def test_only_providers_with_official_windows_and_a_known_account(monkeypatch):
    assert not replenished("x", official(window(1)), provider="agy")
    assert not model_quota.replenished("claude", None, None, None, official(window(1)))
    monkeypatch.setattr(model_quota, "selected_scope", lambda provider: dict(IDENTITY, identity_known=False))
    assert not replenished(None, official(window(1)))


def refusal(model="claude-fable-5-2", *, resets_at="2030-01-01T00:00:00+00:00", age=timedelta(hours=4)):
    return {"status": "limited", "reason": "额度耗尽", "resets_at": resets_at,
            "updated_at": (datetime.now() - age).isoformat(), "source": "dispatch",
            "quota_scope": dict(IDENTITY, model=model)}


def status(*rows):
    return {"status": "unknown", "quota_limits": list(rows)}


def test_stored_refusal_ends_when_official_reading_after_it_shows_quota(monkeypatch):
    row = refusal(resets_at=RESET.isoformat())
    info = status(row)
    readings = {"quota": official(window(100), window(100, model="Fable"))}
    monkeypatch.setattr(health, "_get_official_subscription_quota", lambda provider, *a, **k: readings["quota"])
    assert model_quota.refused_models("claude", info) == [("claude-fable-5-2", "额度耗尽")]
    assert model_quota.quota_reason("claude", "claude-fable-5-2", info=info, quota=readings["quota"])

    readings["quota"] = official(window(1), window(2, model="Fable"))
    assert model_quota.refused_models("claude", info) == []
    assert model_quota.quota_reason("claude", "claude-fable-5-2", info=info, quota=readings["quota"]) is None
    assert info["quota_limits"] == [row]  # the observation itself is kept

    # The same reading taken before the refusal proves nothing.
    readings["quota"] = official(window(1, observed=NOW - timedelta(hours=5)))
    assert model_quota.refused_models("claude", info) == [("claude-fable-5-2", "额度耗尽")]


def test_refusal_of_a_pool_the_official_windows_do_not_report_stays(monkeypatch):
    monkeypatch.setattr(health, "_get_official_subscription_quota",
                        lambda provider, *a, **k: official(window(1), window(2, model="Fable")))
    five_hour = refusal(resets_at=(NOW + timedelta(hours=3)).isoformat())
    assert model_quota.refused_models("claude", status(five_hour)) == [("claude-fable-5-2", "额度耗尽")]
    unnamed = refusal(resets_at=None, age=timedelta(minutes=5))
    assert model_quota.refused_models("claude", status(unnamed)) == [("claude-fable-5-2", "额度耗尽")]


def test_unreadable_official_evidence_keeps_the_refusal(monkeypatch):
    def broken(provider, *a, **k):
        raise OSError("snapshot unreadable")
    monkeypatch.setattr(health, "_get_official_subscription_quota", broken)
    assert model_quota.refused_models("claude", status(refusal(resets_at=RESET.isoformat())))


def test_official_reader_reentering_the_gate_does_not_recurse(monkeypatch):
    calls = []

    def reader(provider, *a, **k):
        calls.append(provider)
        # A reader that consults the stored refusals must not start a second read.
        assert model_quota.refused_models("claude", status(refusal(resets_at=RESET.isoformat())))
        return official(window(1))
    monkeypatch.setattr(health, "_get_official_subscription_quota", reader)
    assert model_quota.refused_models("claude", status(refusal(resets_at=RESET.isoformat()))) == []
    assert calls == ["claude"]
