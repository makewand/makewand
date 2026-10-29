"""Shared ledger retention policy (mirrored in execution/compaction.go)."""
import math

from makewand.layered_budget import effective_clock

RETENTION_SECONDS = 2 * 86400


def compact(data):
    now = effective_clock(data)
    cutoff = now - RETENTION_SECONDS
    leases = data.get("leases", {})
    kept = []
    archived = data.get("archived_used", 0)
    capacity_checked, capacity_refs = False, None
    for entry in data["attempts"]:
        finished = entry.get("finished_at")
        # Old Go completions recorded start + elapsed monotonic seconds.
        # Start alone cannot date a long-running job's settlement; records
        # without that proof begin retention on upgrade.
        if entry.get("status") in ("completed", "released") and finished is None:
            started, elapsed = entry.get("started_at"), entry.get("seconds")
            dated = (entry["status"] == "completed" and
                     all(type(v) in (int, float) and math.isfinite(v) and v >= 0
                         for v in (started, elapsed)))
            finished = entry["finished_at"] = started + elapsed if dated else now
        expired = (entry.get("status") in ("completed", "released") and entry["id"] not in leases
                   and type(finished) in (int, float) and math.isfinite(finished)
                   and 0 <= finished < cutoff)
        if expired:
            if not capacity_checked:
                from makewand.shared_capacity import referenced_attempts
                capacity_refs, capacity_checked = referenced_attempts(), True
            # Keep the settlement receipt until the shared cache observes it.
            # An unreadable snapshot never licenses deleting that proof.
            expired = capacity_refs is not None and entry["id"] not in capacity_refs
        if expired:
            archived = min(9223372036854775807, archived + (entry["status"] != "released"))
        else:
            kept.append(entry)
    data["attempts"] = kept
    # Only lifetime ledgers need an aggregate; layered counters already count
    # retained and archived attempts. legacy_used is the migration baseline.
    if "scopes" not in data or "legacy_maximum" in data:
        data["archived_used"] = archived
    holds = data.get("holds", {})
    for key in list(holds):
        if holds[key]["expires_at"] <= now:
            del holds[key]
    active_keys = {key for lease in leases.values() for key in lease["keys"]}
    limits = data.get("lease_limits", {})
    for key in list(limits):
        if key not in active_keys:
            del limits[key]
    s = data.get("scopes")
    if s is None:
        return
    referenced = {(group, key) for entry in [*kept, *holds.values()]
                  for group, key in (entry.get("scopes") or [])}
    for group in ("tasks", "projects", "machine"):
        for key, counter in list(s[group].items()):
            # References (including completed attempts and active holds) must
            # remain valid for cross-language validation and late settlement.
            if (group, key) in referenced:
                counter.setdefault("last_used", now)
                continue
            touched = counter.setdefault("last_used", now)
            old_day = key.rsplit(":", 1)[-1] < s["day"]
            if (group != "tasks" and old_day) or (group == "tasks" and touched < cutoff):
                del s[group][key]
