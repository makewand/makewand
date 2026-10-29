"""Subscription concurrency leases; expiry alone is never proof of death."""
import os
import math
import sys
from pathlib import Path

TTL = 30


def process_ref(pid):
    if sys.platform != "linux":
        from makewand.lease_process import process_ref as platform_ref
        return platform_ref(pid)
    ref = {"pid": pid, "token": None, "boot": None, "group": None}
    if Path("/proc").is_dir():
        try:
            fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
            ref.update(token=fields[19], group=int(fields[2]),
                       boot=Path("/proc/sys/kernel/random/boot_id").read_text().strip())
        except (OSError, ValueError, IndexError):
            pass
    return ref


def alive(ref, group=False):
    """True also means unverifiable. Linux identity includes boot + start ticks."""
    if sys.platform != "linux":
        from makewand.lease_process import alive as platform_alive
        return platform_alive(ref, group)
    if not ref.get("token") or not ref.get("boot"):
        return True  # conservative on unsupported platforms / inaccessible proc
    try:
        boot = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
        if boot != ref["boot"]:
            return False
        raw = Path(f"/proc/{ref['pid']}/stat").read_text()
        fields = raw.rsplit(")", 1)[1].split()
        if fields[19] == ref["token"] and fields[0] not in ("Z", "X"):
            return True
    except FileNotFoundError:
        pass
    except (OSError, ValueError, IndexError):
        return True
    if group and ref.get("group"):
        # A CLI leader can exit while children keep running in its process group.
        try:
            for entry in Path("/proc").iterdir():
                if not entry.name.isdigit():
                    continue
                try:
                    fields = (entry / "stat").read_text().rsplit(")", 1)[1].split()
                    if int(fields[2]) == ref["group"] and fields[0] not in ("Z", "X"):
                        return True
                except FileNotFoundError:
                    continue
                except (OSError, ValueError, IndexError):
                    return True
        except OSError:
            return True
    return False


def lease_provider(engine):
    from makewand.config import normalize_provider_name
    engine = engine.strip().lower()
    base, suffix = (engine[:-4], ":api") if engine.endswith(":api") else (engine, "")
    base = normalize_provider_name(base)
    return ("agy" if base == "antigravity" else base) + suffix


def workers_alive(lease):
    owner_dead = not alive(lease["owner"])
    for worker in lease["workers"]:
        # Only WindowsJob/start-suspended, no-breakaway runners set this marker.
        # Their non-inheritable Job handle is held solely by the lease owner.
        if (sys.platform == "win32" and owner_dead and
                worker.get("job") == "owner-kill-on-close"):
            continue
        if alive(worker, True):
            return True
    return False


def keys(engine, account):
    engine = lease_provider(engine)
    if account and ":api:" in account and lease_provider(account.split(":api:", 1)[0]) == engine.removesuffix(":api"):
        engine = engine.removesuffix(":api") + ":api"
        account = None
    # Unknown roots cannot establish independent pools. Serialize all unknown
    # accounts of the provider rather than treating directory hashes as accounts.
    account = account if account and ":account:" in account else engine + ":unknown"
    return [engine + ":provider", account, account + ":subscription"]


def admission(data, engine, account, capacity_policy=None):
    wanted = keys(engine, account)
    engine = wanted[0][:-len(":provider")]
    from makewand.call_budget import ConcurrencyBusyError
    from makewand.layered_budget import effective_clock, positive
    leases = data.setdefault("leases", {})
    # A persisted clock far ahead of real time would keep a dead owner's lease unexpired.
    now = effective_clock(data)
    completed = {entry["id"] for entry in data.get("attempts", []) if entry.get("status") == "completed"}
    for identifier, lease in list(leases.items()):
        owner_alive = alive(lease["owner"])
        stopped_attempt = identifier in completed and not lease["launch_pending"]
        if (not owner_alive or stopped_attempt) and not workers_alive(lease) and lease["expires_at"] <= now:
            del leases[identifier]
    limits = data.setdefault("lease_limits", {})
    # Compaction ran before admission reaped dead leases. Drop their unreferenced
    # floors now, before a new lease can accidentally inherit an obsolete cap.
    referenced = {key for lease in leases.values() for key in lease["keys"]}
    for key in list(limits):
        if key not in referenced:
            del limits[key]
    for index, (key, variable, default) in enumerate(zip(wanted, (
            "MAKEWAND_PROVIDER_CONCURRENCY", "MAKEWAND_ACCOUNT_CONCURRENCY", "MAKEWAND_POOL_CONCURRENCY"), (64, 1, 1))):
        if capacity_policy:
            policy_limit = capacity_policy["providers"].get(engine.removesuffix(":api"), 1)
            limit = min(positive(os.environ.get(variable) or policy_limit), policy_limit)
        else:
            limit = positive(os.environ.get(variable, default))
        limits[key] = min(limits.get(key, limit), limit)
        def conflicts(lease):
            if key in lease["keys"]:
                return True
            # An unknown login can be any already known account of this
            # provider; neither direction establishes an independent pool.
            return (index > 0 and wanted[0] in lease["keys"] and
                    (wanted[1] == engine + ":unknown" or lease["keys"][1] == engine + ":unknown"))
        if sum(conflicts(lease) for lease in leases.values()) >= limits[key]:
            raise ConcurrencyBusyError("account concurrency capacity exhausted")
    return {"keys": wanted, "owner": process_ref(os.getpid()), "workers": [],
            "expires_at": now + TTL, "heartbeat_at": now, "launch_pending": False}


def validate(data):
    from makewand.call_budget import BudgetError
    leases = data.get("leases", {})
    if not isinstance(leases, dict) or not isinstance(data.get("lease_limits", {}), dict):
        raise BudgetError("invalid account lease ledger")
    for maximum in data.get("lease_limits", {}).values():
        if type(maximum) is not int or not 0 < maximum <= 9223372036854775807:
            raise BudgetError("invalid account lease limit")
    for lease in leases.values():
        if (not isinstance(lease, dict) or not isinstance(lease.get("keys"), list)
                or len(lease["keys"]) != 3 or not all(isinstance(k, str) and k for k in lease["keys"])
                or not isinstance(lease.get("owner"), dict) or not isinstance(lease.get("workers"), list)
                or type(lease.get("launch_pending")) is not bool
                or type(lease.get("expires_at")) not in (int, float)
                or not math.isfinite(lease["expires_at"]) or lease["expires_at"] < 0):
            raise BudgetError("invalid account lease")
        for ref in [lease["owner"], *lease["workers"]]:
            if not isinstance(ref, dict) or type(ref.get("pid")) is not int or ref["pid"] <= 0:
                raise BudgetError("invalid account lease process")


def settle(data, attempt):
    lease = data.get("leases", {}).get(attempt)
    if lease is not None and not lease["launch_pending"] and not any(alive(w, True) for w in lease["workers"]):
        del data["leases"][attempt]
