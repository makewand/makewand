"""Redeem a granted Codex rate-limit reset once the weekly limit is really used up.

A redeemed reset cannot be undone. It is spent only on a live reading of the
selected account at 100%: a stale status cache, makewand's own reserve or a short
window never spends one. Requests never start a model. The idempotency key is
derived from the window, so concurrent or retried callers redeem at most one
credit for it.
"""
import json
import os
import signal
import subprocess
import sys
import threading
import time
import uuid

from makewand import filelock
from makewand.config import CONFIG_DIR, ensure_config_dir

DISABLE_ENV = "MAKEWAND_CODEX_RESET_CREDIT"
STATE_FILE = "codex-reset-credit.json"
AUDIT_FILE = "codex-reset-credit-audit.jsonl"
WEEKLY_MINUTES = 10080
SESSION_SECONDS = 60
LOCK_SECONDS = 70
_KEY_NAMESPACE = uuid.UUID("7f1c7a52-3c55-4a7e-9a41-6f0c8a6d2b11")


class _AppServer:
    """A short `codex app-server` session over stdio, for account requests only."""

    def __init__(self):
        from makewand.providers.base import safe_which
        exe = safe_which("codex", cwd=str(CONFIG_DIR))
        if not exe:
            raise OSError("codex not found")
        from makewand.proxy import proxy_environment
        self.proc = subprocess.Popen([exe, "app-server"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=subprocess.DEVNULL, cwd=str(CONFIG_DIR),
                                     env=proxy_environment(provider="codex"), start_new_session=True)
        # `codex` can be a wrapper whose native child shares stdout, so only killing
        # the whole group ends a blocked read with EOF.
        self.timer = threading.Timer(SESSION_SECONDS, self._kill)
        self.timer.daemon = True
        self.timer.start()
        self.next_id = 0
        self.request("initialize", {"clientInfo": {"name": "makewand", "title": None, "version": "0"}})
        self._send({"method": "initialized"})

    def _send(self, message):
        self.proc.stdin.write((json.dumps(message) + "\n").encode())
        self.proc.stdin.flush()

    def request(self, method, params=None):
        self.next_id += 1
        message = {"id": self.next_id, "method": method}
        if params is not None:
            message["params"] = params
        self._send(message)
        while True:
            line = self.proc.stdout.readline()
            if not line:
                raise OSError("codex app-server closed")
            try:
                reply = json.loads(line)
            except ValueError:
                continue
            if isinstance(reply, dict) and reply.get("id") == self.next_id:
                if "result" not in reply:
                    raise OSError(method + " failed")
                return reply["result"]

    def _kill(self):
        try:
            os.killpg(self.proc.pid, signal.SIGKILL)
        except OSError:
            pass

    def close(self):
        self.timer.cancel()
        self._kill()
        self.proc.wait()
        for stream in (self.proc.stdin, self.proc.stdout):
            stream.close()


def _state(account):
    """This account's record; accounts never share or overwrite a pending key."""
    try:
        with open(CONFIG_DIR / STATE_FILE, encoding="utf-8") as handle:
            state = json.load(handle)[account]
        return state if isinstance(state, dict) else {}
    except (OSError, ValueError, KeyError, TypeError):
        return {}


def _save_state(account, record):
    path = CONFIG_DIR / STATE_FILE
    try:
        with open(path, encoding="utf-8") as handle:
            state = json.load(handle)
        if not isinstance(state, dict):
            state = {}
    except (OSError, ValueError):
        state = {}
    state[account] = record
    temporary = path.with_suffix(".tmp")
    with open(temporary, "w", encoding="utf-8") as handle:
        json.dump(state, handle)
    os.replace(temporary, path)


def redeemed_at(account):
    """When a reset was last redeemed for this account (seconds), or 0."""
    at = _state(account).get("redeemed_at")
    return at if type(at) in (int, float) else 0


def _audit(**fields):
    try:
        with open(CONFIG_DIR / AUDIT_FILE, "a", encoding="utf-8") as handle:
            handle.write(json.dumps(dict(fields, at=time.time()), ensure_ascii=False) + "\n")
    except OSError:
        pass


def _weekly_window(limits):
    """The weekly window, unless a shorter one is also full: it would still block the
    launch after the reset, and it clears by itself soon."""
    if not isinstance(limits, dict) or limits.get("limitId") != "codex":
        return None
    weekly = None
    for key in ("primary", "secondary"):
        window = limits.get(key)
        if not isinstance(window, dict):
            continue
        if window.get("windowDurationMins") == WEEKLY_MINUTES:
            weekly = window
        elif type(window.get("usedPercent")) not in (int, float) or window["usedPercent"] >= 100:
            return None
    return weekly


def _available(credits_info):
    now = time.time()
    return any(isinstance(credit, dict) and credit.get("status") == "available"
               and credit.get("resetType") == "codexRateLimits"
               and (credit.get("expiresAt") is None or credit["expiresAt"] > now)
               for credit in (credits_info.get("credits") or [] if isinstance(credits_info, dict) else []))


def _restore(identity, trigger):
    account = identity["account_ref"]
    server = _AppServer()
    try:
        reply = server.request("account/rateLimits/read")
        from makewand.account_identity import account_ref
        if account_ref("codex", reply.get("accountId")) != account:
            return False
        window = _weekly_window(reply.get("rateLimits"))
        used = window.get("usedPercent") if window else None
        resets = window.get("resetsAt") if window else None
        if (type(used) not in (int, float) or used < 100 or type(resets) is not int or resets <= time.time()
                or not _available(reply.get("rateLimitResetCredits"))):
            return False
        # A lost reply may have redeemed the credit: the retry must repeat its key,
        # whatever the window now says, or it could spend a second one.
        record = _state(account)
        pending = record.get("pending") if isinstance(record.get("pending"), str) else None
        asked_at = record.get("pending_at") if pending else time.time()
        key = pending or str(uuid.uuid5(_KEY_NAMESPACE, f"{account}:{resets}"))
        _save_state(account, {"redeemed_at": redeemed_at(account), "pending": key, "pending_at": asked_at})
        _audit(event="redeem_requested", trigger=trigger, used_percent=used, window_resets_at=resets)
        outcome = server.request("account/rateLimitResetCredit/consume", {"idempotencyKey": key}).get("outcome")
        _audit(event="redeem_result", trigger=trigger, outcome=outcome)
        if outcome == "reset" or (outcome == "alreadyRedeemed" and pending):
            # Evidence newer than the moment of the reset must keep counting.
            at = time.time() if outcome == "reset" else asked_at
            _save_state(account, {"redeemed_at": at if type(at) in (int, float) else time.time()})
            print("makewand: Codex 周额度已用完，已启用 1 次赠送的额度重置", file=sys.stderr)
            return True
        if outcome in ("alreadyRedeemed", "noCredit", "nothingToReset"):
            _save_state(account, {"redeemed_at": redeemed_at(account)})
        return False
    finally:
        server.close()


def restore_quota(trigger):
    """True when a reset credit was redeemed for this used-up week.

    Every failure leaves the caller's refusal in place and spends nothing it knows of.
    """
    if (os.name != "posix" or os.environ.get(DISABLE_ENV) == "off"
            or os.environ.get("MAKEWAND_PROVIDER_WORKER") == "1"):
        return False
    from makewand.model_quota import selected_scope
    identity = selected_scope("codex")
    if not identity["identity_known"]:
        return False
    try:
        ensure_config_dir()
        with open(CONFIG_DIR / (STATE_FILE + ".lock"), "w") as lock:
            deadline = time.monotonic() + LOCK_SECONDS
            while True:
                try:
                    filelock.flock(lock, filelock.LOCK_EX | filelock.LOCK_NB)
                    break
                except OSError:
                    if time.monotonic() >= deadline:
                        return False
                    time.sleep(0.2)
            return _restore(identity, trigger)
    except (OSError, ValueError, TypeError, AttributeError) as error:
        _audit(event="error", trigger=trigger, error=type(error).__name__)
        return False
