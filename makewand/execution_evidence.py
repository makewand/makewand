"""Durable, metadata-only provider observations, shared with the native runner.

Protocol message bodies, arbitrary strings, tokens and credential documents are
never serialized. Missing observations stay unknown; argv is not an observation.

Retention and the read index mirror execution/evidence_store.go: records stay
one ``<attempt_id>.json`` file each, ``index.jsonl`` lists saves in write order
so readers stop after the newest records they need, and compaction prunes
settled records after the ledger's retention or beyond ``KEEP_PER_ACCOUNT``
newer settled records of the same engine, account and pool.
"""
import errno
import json
import math
import os
import re
import stat
import sys
import time
from datetime import datetime
from pathlib import Path

from makewand.account_identity import account_identity, event_account_ref
from makewand.execution_contract import identifier
from makewand.accounting_files import PublishedWriteError

INDEX = "index.jsonl"
COMPACTION_LOCK = "compact.lock"
KEEP_PER_ACCOUNT = 256
COMPACT_EVERY_MS = 3600 * 1000
INDEX_WINDOW = 1024 * 1024
RECORD_MAX = 1024 * 1024
TEMP_PREFIX = ".call-budget-"
_FILE_ID = re.compile(r"[A-Za-z0-9._-]{1,128}")
_GROUP_KEYS = ("engine", "account_ref", "pool_ref")
_warned = False


def directory():
    value = os.environ.get("MAKEWAND_EXECUTION_EVIDENCE_DIR")
    if value:
        return Path(value)
    if sys.platform == "win32":
        cache = os.environ.get("LocalAppData") or os.environ.get("LOCALAPPDATA")
        if not cache:
            raise OSError("provider evidence requires LocalAppData")
        cache = Path(cache)
    elif sys.platform == "darwin":
        cache = Path.home() / "Library" / "Caches"
    else:
        cache = Path(os.environ.get("XDG_CACHE_HOME") or Path.home() / ".cache")
        if not cache.is_absolute():
            raise OSError("provider evidence requires an absolute cache directory")
    return cache / "makewand" / "execution-evidence"


def new(request, attempt):
    identity = account_identity(request.engine) if request.engine in ("codex", "claude") else None
    return dict(schema=1, task_id=request.task_id, attempt_id=attempt, engine=request.engine,
                account_ref=identity["account_ref"] if identity else "",
                identity_known=bool(identity and identity["identity_known"]),
                pool_ref=identity["pool_ref"] if identity and identity["pool_ref"] else "",
                source="selected_login" if identity else "unavailable", observed_at_ms=int(time.time() * 1000),
                windows=[], result_status="UNKNOWN", outcome_known=False, tokens=None, process_exit_code=None,
                settled=False)


def _busy(error):
    return error.errno in (errno.EACCES, errno.EAGAIN, errno.EWOULDBLOCK) or getattr(error, "winerror", None) in (33, 158)


def _lock(fd, deadline):
    """Take the sidecar lock; the deadline bounds waiting for another holder."""
    from makewand import filelock
    while True:
        try:
            filelock.flock(fd, filelock.LOCK_EX | filelock.LOCK_NB)
            return True
        except OSError as error:
            if not _busy(error):
                raise
        if time.monotonic() >= deadline:
            return False
        time.sleep(.01)


def save(evidence):
    from makewand.accounting_files import pin
    identifier(evidence["attempt_id"], "attempt_id")
    evidence["observed_at_ms"] = int(time.time() * 1000)
    encoded = json.dumps(evidence, allow_nan=False, separators=(",", ":")).encode()
    published = False
    try:
        with pin(directory() / (evidence["attempt_id"] + ".json")) as files:
            _publish_record(files, encoded)
            published = True
            _append_index(files, evidence)
    except PublishedWriteError:
        raise
    except (OSError, ValueError, RuntimeError) as error:
        if published:
            raise PublishedWriteError(error) from error
        raise


def _publish_record(files, encoded):
    from makewand import filelock
    published = False
    try:
        fd = files.open(files.lock_name, create=True, writable=True)
        try:
            if not _lock(fd, time.monotonic() + 1):
                raise OSError("provider evidence lock unavailable")
            try:
                pending = files.open(files.name + ".pending", create=True, writable=True, delete=True)
                try:
                    os.fsync(pending)
                    if os.name != "nt":
                        os.fsync(files.parent)
                    files.save(encoded, fd)
                    published = True
                    try:
                        files.cleanup(files.name + ".pending", pending)
                        if os.name != "nt":
                            os.fsync(files.parent)
                    except (OSError, ValueError):
                        pass  # record committed; a leftover marker only retains it
                finally:
                    os.close(pending)
            finally:
                filelock.flock(fd, filelock.LOCK_UN)
        finally:
            os.close(fd)
    except PublishedWriteError:
        raise
    except (OSError, ValueError, RuntimeError) as error:
        if published:
            raise PublishedWriteError(error) from error
        raise


def warn_unavailable():
    """A first record that could not be written before launch only loses telemetry."""
    global _warned
    if not _warned:
        _warned = True
        try:
            print("makewand: provider execution evidence unavailable; continuing without a record", file=sys.stderr)
        except Exception:
            pass


def settle(evidence, ledger_settled=True):
    """Mark the record settled once its ledger released the attempt, then prune when due.

    A record that cannot be marked stays open: retention never deletes it.
    """
    if not ledger_settled:
        return
    if evidence.get("settled") is not True:
        evidence["settled"] = True
        try:
            save(evidence)
        except (OSError, ValueError, RuntimeError):
            evidence["settled"] = False
            raise
    compact()


def _model(value):
    return value if isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_.:-]{1,128}", value) else None


def _timestamp(value):
    try:
        if isinstance(value, bool) or value is None:
            return None
        if isinstance(value, (int, float)) or isinstance(value, str) and value.replace(".", "", 1).isdigit():
            parsed = float(value)
        else:
            parsed = datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
        return int(parsed * 1000) if math.isfinite(parsed) and parsed > 0 else None
    except (ValueError, TypeError, OverflowError, AttributeError):
        return None


def protocol(evidence, raw):
    """Inspect CLI-owned envelopes only, never item text or tool output."""
    engine = evidence["engine"]
    session_ref = None
    for line in raw.splitlines():
        if len(line) > 1024 * 1024:
            continue
        try:
            obj = json.loads(line)
            if not isinstance(obj, dict):
                continue
            kind = obj.get("type")
            payload = obj.get("payload")
            payload = payload if isinstance(payload, dict) else {}
            ref = event_account_ref(obj) if engine == "codex" else None
            if kind == "session_meta":
                session_ref = ref
            if (ref or session_ref) and (ref or session_ref) != evidence["account_ref"]:
                continue
            fields = (payload if engine == "codex" and kind in ("session_meta", "turn_context")
                      else obj if engine == "claude" and kind in ("system", "result") else {})
            if engine == "claude" and kind == "assistant" and isinstance(obj.get("message"), dict):
                fields = obj["message"]
            model = _model(fields.get("model"))
            if model:
                evidence["actual_model"] = model
            if engine == "claude" and kind == "result":
                models = obj.get("modelUsage")
                if isinstance(models, dict) and len(models) == 1 and _model(next(iter(models))):
                    evidence["actual_model"] = next(iter(models))
            usage = obj.get("usage") if ((engine == "codex" and kind == "turn.completed")
                                              or (engine == "claude" and kind == "result")) else None
            if isinstance(usage, dict):
                counts = [usage.get("input_tokens"), usage.get("output_tokens")]
                if all(type(value) is int and 0 <= value <= 9223372036854775807 for value in counts) and sum(counts) <= 9223372036854775807:
                    evidence["tokens"] = sum(counts)
            if engine != "codex" or not evidence["identity_known"]:
                continue
            if kind != "rate_limits.updated" and not (kind == "event_msg" and payload.get("type") == "token_count"):
                continue
            at = _timestamp(obj.get("timestamp"))
            limits = obj.get("rate_limits") or payload.get("rate_limits")
            if not at or at > time.time() * 1000 + 60000 or not isinstance(limits, dict):
                continue
            scoped = limits.get("model") or (limits.get("limit_id") if limits.get("limit_id") != "codex" else "") or ""
            if scoped and not _model(scoped):
                continue
            for slot, key in enumerate(("primary", "secondary")):
                window = limits.get(key)
                if not isinstance(window, dict):
                    continue
                used = window.get("used_percent")
                if type(used) not in (int, float) or not math.isfinite(used) or not 0 <= used <= 100:
                    continue
                minutes = window.get("window_minutes") or window.get("limit_window_minutes") or 0
                if type(minutes) is not int or not 0 <= minutes <= 525600:
                    continue
                reset = _timestamp(window.get("resets_at"))
                if window.get("resets_at") is not None and reset is None:
                    continue
                window_key = scoped + "|" + str(minutes) + ("|unknown_" + str(slot) if minutes == 0 else "")
                candidate = dict(model=scoped, window_key=window_key, window_minutes=minutes,
                                 used_percent=used, observed_at_ms=at, reset_at_ms=reset or 0)
                existing = next((w for w in evidence["windows"] if w["window_key"] == window_key), None)
                if existing is not None:
                    if existing["observed_at_ms"] <= at:
                        existing.update(candidate)
                elif len(evidence["windows"]) < 32:
                    evidence["windows"].append(candidate)
        except (ValueError, TypeError, AttributeError, RecursionError):
            continue


def private_home(evidence, home):
    """Use only the selected copied root; never follow worker-created symlinks."""
    from makewand.account_identity import _plain_identifier, account_ref
    from makewand.sandbox import _read_private_nofollow
    engine = evidence["engine"]
    if engine not in ("codex", "claude"):
        return
    # ProviderHome copies exactly one selected Codex/Claude root. Do not
    # re-resolve the host login here: its symlink may change during execution.
    root = None
    for cred in home.credentials:
        if engine == "claude" and cred.rel == ".credentials.json":
            root = Path(cred.private_root)
        if engine == "codex" and cred.rel == "auth.json":
            root = Path(cred.private_root)
    ref = None
    if home.identity_file is not None:
        found = _read_private_nofollow(*home.identity_file)
        if found[0] is not None:
            try:
                ref = account_ref(engine, _plain_identifier(found[0].decode(),
                    "tokens" if engine == "codex" else "oauthAccount",
                    "account_id" if engine == "codex" else "accountUuid"))
            except (ValueError, UnicodeError, RecursionError):
                pass
    if ref != evidence["account_ref"]:
        evidence["windows"] = []
    evidence.update(account_ref=ref or "", identity_known=bool(ref), pool_ref=ref + ":subscription" if ref else "")
    evidence["source"] = "private_home+cli_json" if "cli_json" in evidence["source"] else "private_home"
    if root is None:
        return
    count = 0
    for parent, dirs, files in os.walk(root / ("sessions" if engine == "codex" else "projects"), followlinks=False):
        count += 1
        if count > 256:
            return
        dirs[:] = [name for name in dirs if not (Path(parent) / name).is_symlink()]
        for name in files:
            count += 1
            if count > 256:
                return
            if not name.endswith(".jsonl"):
                continue
            found = _read_private_nofollow(str(root), str((Path(parent) / name).relative_to(root)))
            if found[0] is not None:
                protocol(evidence, found[0].decode("utf-8", errors="replace"))


def read_recent(engine=None, account_ref=None, pool_ref=None, limit=64):
    """The newest records that match (None matches any value), newest first.

    Quota binding and model discovery filter expiry and outcome themselves. The
    reader follows the index and opens only the records it yields or rejects. A
    directory that no runner of this release has written to has no index and
    is scanned as before: the newest records by modification time, then filtered.
    """
    from makewand.accounting_files import pin

    def admits(row):
        return all(want is None or row.get(key) == want
                   for key, want in zip(_GROUP_KEYS, (engine, account_ref, pool_ref)))

    found = []
    try:
        if not directory().is_dir():
            return
        with pin(directory() / INDEX) as files:
            entries = _index_entries(files)
            if entries is None:
                names = sorted(((st.st_mtime_ns, name) for name, st in _listing(files, ".json")), reverse=True)
                candidates = [name for _, name in names[:limit]]
            else:
                candidates = (entry["attempt_id"] + ".json" for entry in entries if admits(entry))
            for name in candidates:
                if len(found) == limit:
                    break
                record = _read_record(files, name)
                if record is not None and admits(record):
                    found.append(record)
    except (OSError, ValueError, RuntimeError):
        pass
    yield from found


def _file_id(value):
    return isinstance(value, str) and _FILE_ID.fullmatch(value) is not None and value not in (".", "..")


def _text(value):
    return value if isinstance(value, str) else ""


def _listdir(files):
    return os.listdir(files.path.parent if os.name == "nt" else files.parent)


def _lstat(files, name):
    try:
        if os.name == "nt":
            st = os.stat(files.path.parent / name, follow_symlinks=False)
        else:
            st = os.stat(name, dir_fd=files.parent, follow_symlinks=False)
    except (OSError, ValueError):
        return None
    # Regular files only; Windows reparse points are refused like Go's accountingReparse.
    if not stat.S_ISREG(st.st_mode) or getattr(st, "st_file_attributes", 0) & 0x400:
        return None
    return st


def _listing(files, suffix):
    for name in _listdir(files):
        if name.endswith(suffix):
            st = _lstat(files, name)
            if st is not None:
                yield name, st


def _read_bytes(files, name, limit):
    """Read a file of the pinned directory; delete sharing keeps Windows writers able to replace it."""
    fd = files.open(name, delete=True)
    with os.fdopen(fd, "rb") as stream:
        return stream.read(limit)


def _read_record(files, name):
    try:
        if _pending(files, name):
            return None
        raw = _read_bytes(files, name, RECORD_MAX + 1)
        if len(raw) > RECORD_MAX:
            return None
        value = json.loads(raw)
        if _pending(files, name):
            return None
    except (OSError, ValueError, RecursionError):
        return None
    return value if isinstance(value, dict) and value.get("schema") == 1 else None


def _pending(files, name):
    try:
        fd = files.open(name + ".pending", delete=True)
    except FileNotFoundError:
        return False
    except (OSError, ValueError):
        return True
    os.close(fd)
    return True


def _parse_entry(line):
    """One index line, or None for the header, a torn line or a foreign value."""
    try:
        entry = json.loads(line)
    except (ValueError, RecursionError):
        return None
    if not isinstance(entry, dict) or not _file_id(entry.get("attempt_id")):
        return None
    if any(entry.get(key) is not None and not isinstance(entry[key], str) for key in _GROUP_KEYS):
        return None
    return dict(attempt_id=entry["attempt_id"], **{key: _text(entry.get(key)) for key in _GROUP_KEYS})


def _index_entries(files):
    """Index entries newest first, one per record, decoded as they are consumed; None without an index."""
    try:
        fd = files.open(INDEX, delete=True)
    except FileNotFoundError:
        return None
    with os.fdopen(fd, "rb") as stream:
        size = os.fstat(stream.fileno()).st_size
        start = max(0, size - INDEX_WINDOW)
        stream.seek(start)
        data = stream.read(size - start)
    if start:
        data = data.partition(b"\n")[2]  # the window began inside a line
    return _newest_entries(data.split(b"\n"))


def _newest_entries(lines):
    seen = set()
    for line in reversed(lines):
        entry = _parse_entry(line)
        if entry is not None and entry["attempt_id"] not in seen:
            seen.add(entry["attempt_id"])
            yield entry


def _append_index(files, evidence):
    """Best effort: the record is authoritative and compaction indexes a lost line."""
    from makewand import filelock
    if not _file_id(evidence.get("attempt_id")):
        return
    line = (json.dumps(dict(attempt_id=evidence["attempt_id"], **{key: _text(evidence.get(key)) for key in _GROUP_KEYS}),
                       separators=(",", ":")) + "\n").encode()
    try:
        lock = files.open(INDEX + ".lock", create=True, writable=True)
    except (OSError, ValueError):
        return
    try:
        if not _lock(lock, time.monotonic() + 1):
            return
        try:
            files.assert_named(INDEX + ".lock", lock)
            fd = files.open(INDEX, create=True, writable=True)
            try:
                os.lseek(fd, 0, os.SEEK_END)
                while line:
                    line = line[os.write(fd, line):]
            finally:
                os.close(fd)
        finally:
            filelock.flock(lock, filelock.LOCK_UN)
    except (OSError, ValueError):
        pass
    finally:
        os.close(lock)


def _index_state(files):
    """The index's identity, size and header time (0 without a header), or None."""
    try:
        fd = files.open(INDEX, delete=True)
    except FileNotFoundError:
        return None
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        head = stream.read(512).partition(b"\n")[0]
    compacted = 0
    try:
        header = json.loads(head)
    except (ValueError, RecursionError):
        header = None
    if (isinstance(header, dict) and type(header.get("schema")) is int and header["schema"] == 1
            and type(header.get("compacted_at_ms")) is int and header["compacted_at_ms"] > 0):
        compacted = header["compacted_at_ms"]
    return dict(identity=(info.st_dev, info.st_ino), size=info.st_size, compacted=compacted)


def _due(state, now):
    if state is None or not state["compacted"]:
        return True
    age = int(now * 1000) - state["compacted"]
    return age >= COMPACT_EVERY_MS or age <= -COMPACT_EVERY_MS


def _reject_constant(_):
    raise ValueError("non-finite JSON number")


def _classify(data):
    """(group, settled) for a schema 1 record, or None for a file retention leaves alone.

    A record without ``settled`` predates the flag: its runner could not mark
    settlement, so only an outcome other than UNKNOWN proves the attempt ended.
    """
    try:
        value = json.loads(data, parse_constant=_reject_constant)
    except (ValueError, RecursionError):
        return None
    if not isinstance(value, dict) or type(value.get("schema")) is not int or value["schema"] != 1:
        return None
    group = tuple(_text(value.get(key)) for key in _GROUP_KEYS)
    if "settled" in value:
        return group, value["settled"] is True
    status = value.get("result_status")
    return group, (isinstance(status, str) and status not in ("", "UNKNOWN")) or value.get("outcome_known") is True


def _same(current, scanned):
    return (os.path.samestat(current, scanned) and current.st_mtime_ns == scanned.st_mtime_ns
            and current.st_size == scanned.st_size)


def _remove_locked(files, name, under_lock):
    """Delete record ``name`` and its sidecar while holding the sidecar's lock.

    ``under_lock()`` runs with the lock held: it deletes the record, or confirms
    it is gone, and reports whether the sidecar may go too. Writers check the
    sidecar again after locking it, so removing a held sidecar cannot let two
    writers lock different files.
    """
    from makewand import filelock
    try:
        lock = files.open(name + ".lock", create=True, writable=True, delete=True)
    except (OSError, ValueError):
        return False
    try:
        try:
            filelock.flock(lock, filelock.LOCK_EX | filelock.LOCK_NB)
        except OSError as error:
            if _busy(error):
                return False
            raise
        try:
            files.assert_named(name + ".lock", lock)
            if not under_lock():
                return False
            try:
                files.cleanup(name + ".lock", lock)
            except (OSError, ValueError):
                pass
            return True
        finally:
            filelock.flock(lock, filelock.LOCK_UN)
    except (OSError, ValueError):
        return False
    finally:
        os.close(lock)


def _remove_record(files, name, scanned):
    def unchanged_removed():
        if _pending(files, name):
            return False
        fd = files.open(name, delete=True)
        try:
            if not _same(os.fstat(fd), scanned):
                return False
            files.cleanup(name, fd)
            return True
        finally:
            os.close(fd)
    return _remove_locked(files, name, unchanged_removed)


def _absent(files, name):
    try:
        if os.name == "nt":
            os.stat(files.path.parent / name, follow_symlinks=False)
        else:
            os.stat(name, dir_fd=files.parent, follow_symlinks=False)
    except FileNotFoundError:
        return True
    return False


def _remove_sidecar(files, lock_name):
    """Delete a sidecar whose record is gone, unless a writer holds it or recreated the record."""
    record = lock_name[:-len(".lock")]
    return _remove_locked(files, record, lambda: _absent(files, record))


def _remove_temporary(files, name):
    try:
        fd = files.open(name, delete=True)
    except (OSError, ValueError):
        return
    try:
        files.cleanup(name, fd)
    except (OSError, ValueError):
        pass
    finally:
        os.close(fd)


def _prune(files, now):
    """Delete prunable records; returns the scanned records, oldest first, and the deleted names."""
    from makewand.ledger_compaction import RETENTION_SECONDS
    names = _listdir(files)
    present = set(names)
    cutoff = int(now * 1e9) - RETENTION_SECONDS * 10**9
    records = []
    for name in names:
        if not name.endswith(".json"):
            continue
        st = _lstat(files, name)
        if st is None or st.st_size > RECORD_MAX:
            continue
        try:
            # The deletion check compares fstat with fstat: Windows os.stat and
            # os.fstat need not fill st_ino alike.
            with os.fdopen(files.open(name, delete=True), "rb") as stream:
                st = os.fstat(stream.fileno())
                managed = _classify(stream.read(RECORD_MAX + 1))
        except (OSError, ValueError):
            continue
        if managed is not None:
            records.append(dict(name=name, stat=st, group=managed[0], settled=managed[1] and not _pending(files, name),
                                indexed=_file_id(name[:-len(".json")])))
    records.sort(key=lambda record: (record["stat"].st_mtime_ns, record["name"]))
    newer, deleted = {}, set()
    for record in reversed(records):
        if not record["settled"]:
            continue
        newer[record["group"]] = newer.get(record["group"], 0) + 1
        if ((newer[record["group"]] > KEEP_PER_ACCOUNT or record["stat"].st_mtime_ns < cutoff)
                and _remove_record(files, record["name"], record["stat"])):
            deleted.add(record["name"])
    for name in names:
        temporary = name.startswith(TEMP_PREFIX)
        pending = name.endswith(".json.pending") and name[:-len(".pending")] not in present
        if not temporary and not pending and (not name.endswith(".json.lock") or name[:-len(".lock")] in present):
            continue
        st = _lstat(files, name)
        if st is None or st.st_mtime_ns >= cutoff:
            continue
        if temporary:
            _remove_temporary(files, name)  # it outlived the writer that removes its own
        elif pending:
            _remove_orphan_pending(files, name)
        else:
            _remove_sidecar(files, name)
    return records, deleted


def _remove_orphan_pending(files, name):
    from makewand import filelock
    record = name[:-len(".pending")]
    fd = files.open(record + ".lock", create=True, writable=True)
    try:
        if not _lock(fd, time.monotonic()):
            return
        try:
            try:
                probe = files.open(record, delete=True)
            except FileNotFoundError:
                _remove_temporary(files, name)
            else:
                os.close(probe)
        finally:
            filelock.flock(fd, filelock.LOCK_UN)
    finally:
        os.close(fd)


def _index_image(records, deleted, appended, now):
    lines, position = [], {}

    def add(entry):
        if entry["attempt_id"] + ".json" in deleted:
            return
        if entry["attempt_id"] in position:
            lines[position[entry["attempt_id"]]] = None
        position[entry["attempt_id"]] = len(lines)
        lines.append(json.dumps(entry, separators=(",", ":")).encode())

    for record in records:
        if record["indexed"]:
            add(dict(attempt_id=record["name"][:-len(".json")], **dict(zip(_GROUP_KEYS, record["group"]))))
    for raw in appended.split(b"\n"):
        entry = _parse_entry(raw)
        if entry is not None:
            add(entry)
    header = json.dumps({"schema": 1, "compacted_at_ms": int(now * 1000)}, separators=(",", ":")).encode()
    return b"".join(line + b"\n" for line in [header, *lines] if line is not None)


def compact(force=False, now=None):
    """Prune settled records and rewrite the index when it is due (or with force).

    Never runs on a read path. Open records, records whose sidecar is locked and
    records changed since the scan are kept. The scan and deletions run without
    the index lock; the rewrite takes it and keeps the lines appended meanwhile.
    Best effort: any failure leaves the previous index in place.
    """
    from makewand.accounting_files import pin
    from makewand import filelock
    now = time.time() if now is None else now
    try:
        if not directory().is_dir():
            return
        with pin(directory() / INDEX) as files:
            start = _index_state(files)
            if not force and not _due(start, now):
                return
            # A stable lock shared with Go covers the due check and pruning.
            # The index lock stays free while the directory is scanned.
            compactor = files.open(COMPACTION_LOCK, create=True, writable=True)
            try:
                if not _lock(compactor, time.monotonic()):
                    return
                try:
                    files.assert_named(COMPACTION_LOCK, compactor)
                    _compact_locked(files, now, force)
                finally:
                    filelock.flock(compactor, filelock.LOCK_UN)
            finally:
                os.close(compactor)
    except (OSError, ValueError, RuntimeError):
        pass


def _compact_locked(files, now, force):
    from makewand import filelock
    start = _index_state(files)
    if not force and not _due(start, now):
        return
    records, deleted = _prune(files, now)
    lock = files.open(files.lock_name, create=True, writable=True)
    try:
        if not _lock(lock, time.monotonic() + 1):
            return
        try:
            files.assert_named(files.lock_name, lock)
            current = _index_state(files)
            if not force and not _due(current, now):
                return
            appended = b""
            if current is not None:
                offset = 0
                if start is not None:
                    if start["identity"] != current["identity"]:
                        return
                    offset = start["size"]
                fd = files.open(INDEX, delete=True)
                with os.fdopen(fd, "rb") as stream:
                    stream.seek(offset)
                    appended = stream.read(max(0, current["size"] - offset))
            files.save(_index_image(records, deleted, appended, now), lock)
        finally:
            filelock.flock(lock, filelock.LOCK_UN)
    finally:
        os.close(lock)


def api_transport():
    """An API key does not establish a subscription account or quota pool."""
    from makewand.execution_runtime import current_context
    context = current_context()
    from makewand import call_budget
    request = context.get("_request")
    if request is not None:
        call_budget.api_lease(context.get("_attempt"), request.engine,
            budget_file=context.get("_budget_file"), max_model_calls=context.get("_max_model_calls"))
    evidence = current_context().get("_evidence")
    if evidence is not None:
        evidence.update(account_ref="", identity_known=False, pool_ref="", windows=[], tokens=None, source="api")
        evidence.pop("actual_model", None)
