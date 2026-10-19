"""Non-secret subscription identity; never decode JWTs or inspect token fields.

An unknown directory reference is an isolation key, not an independent pool.
Quota evidence without a known account binding cannot authorize admission.
"""
import hashlib
import json
import os
import re
from pathlib import Path


_JSON_TOKEN = re.compile(r'\s*("(?:[^"\\\x00-\x1f]|\\(?:["\\/bfnrt]|u[0-9a-fA-F]{4}))*"|[{}\[\]:,]|true|false|null|-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?)')


def _plain_identifier(raw, section, field):
    """Project just section.field, skipping other string values undecoded.

    Credential files contain secret values alongside the plain account ID.
    Unlike json.loads of the whole document, this parser never decodes those
    values into Python objects. It validates the JSON structure and only
    decodes object keys and the whitelisted identifier.
    """
    offset = 0

    def token():
        nonlocal offset
        match = _JSON_TOKEN.match(raw, offset)
        if not match:
            raise ValueError("invalid identity document")
        offset = match.end()
        return match[1]

    def value(first, path):
        if first == "{":
            found = None
            key = token()
            if key == "}":
                return found
            while True:
                if not key.startswith('"') or token() != ":":
                    raise ValueError("invalid object")
                name = json.loads(key)
                child_path = path[1:] if path and name == path[0] else None
                child = value(token(), child_path)
                if child_path is not None:
                    found = child
                delimiter = token()
                if delimiter == "}":
                    return found
                if delimiter != ",":
                    raise ValueError("invalid object")
                key = token()
        if first == "[":
            item = token()
            if item == "]":
                return None
            while True:
                value(item, None)
                delimiter = token()
                if delimiter == "]":
                    return None
                if delimiter != ",":
                    raise ValueError("invalid array")
                item = token()
        if first in ("}", "]", ":", ","):
            raise ValueError("invalid value")
        return json.loads(first) if path == () and first.startswith('"') else None

    first = token()
    if first != "{":
        raise ValueError("invalid identity object")
    result = value(first, (section, field))
    if raw[offset:].strip():
        raise ValueError("trailing identity data")
    return result


def account_ref(provider, account_id):
    if not isinstance(account_id, str) or not account_id.strip():
        return None
    return provider + ":account:" + hashlib.sha256(account_id.encode()).hexdigest()[:32]


def account_identity(provider, root=None):
    if provider not in ("codex", "claude"):
        return None
    variable = "CODEX_HOME" if provider == "codex" else "CLAUDE_CONFIG_DIR"
    root = Path(root or os.environ.get(variable) or Path.home() / ("." + provider)).expanduser().resolve()
    path = root / ("auth.json" if provider == "codex" else ".claude.json")
    if provider == "claude" and root == (Path.home() / ".claude").resolve():
        path = Path.home() / ".claude.json"
    identity = None
    try:
        # Only the documented plain identifier is selected. Token claims are
        # deliberately not a fallback when this field is missing.
        from makewand.sandbox import _read_host_regular
        found = _read_host_regular(str(path))
        if found is not None:
            identity = account_ref(provider, _plain_identifier(found[0].decode("utf-8"),
                "tokens" if provider == "codex" else "oauthAccount",
                "account_id" if provider == "codex" else "accountUuid"))
    except (OSError, ValueError, TypeError, RecursionError):
        pass
    return {"credential_root": str(root), "account_ref": identity or
            provider + ":unknown:" + hashlib.sha256(str(root).encode()).hexdigest()[:32],
            "identity_known": identity is not None,
            "pool_ref": identity + ":subscription" if identity else None}


def evidence_matches(entry, identity):
    return bool(identity and identity["identity_known"] and entry.get("IdentityKnown") is True
                and entry.get("AccountRef") == identity["account_ref"]
                and entry.get("PoolRef") == identity["pool_ref"])


def event_account_ref(record):
    """Explicit non-secret event binding; callers may inherit session metadata."""
    payload = record.get("payload") or {}
    for obj in (record, payload):
        if not isinstance(obj, dict):
            continue
        if "account_id" in obj:
            return account_ref("codex", obj["account_id"]) or "invalid"
        value = obj.get("account_ref")
        if isinstance(value, str) and value.startswith("codex:account:"):
            return value
        if "account_ref" in obj:
            return "invalid"
    return None


def _claude_identity_fingerprint(data):
    identity = None
    if data is not None:
        try:
            identity = account_ref("claude", _plain_identifier(data.decode("utf-8"), "oauthAccount", "accountUuid"))
        except (UnicodeError, ValueError, TypeError, RecursionError):
            pass
    return {"kind": "identity", "exists": data is not None, "ref": identity}


def refuse_credential_launch(message):
    # The flag survives adapters which turn SandboxConfigError into a tuple.
    # Only pre-launch account validation calls this; an invoked transport is
    # never refunded, even if another later check reports the same error.
    from makewand.execution_runtime import current_context
    from makewand.sandbox import SandboxConfigError
    invocation = current_context().get("_provider_invocation")
    if invocation is not None:
        invocation.account_refused = True
    raise SandboxConfigError(message)


def credential_binding(provider):
    """Ephemeral fingerprints for admission-to-copy consistency; never persisted."""
    if provider not in ("codex", "claude"):
        return None
    from makewand.sandbox import _read_host_regular
    variable = "CODEX_HOME" if provider == "codex" else "CLAUDE_CONFIG_DIR"
    root = Path(os.environ.get(variable) or Path.home() / ("." + provider)).expanduser().resolve()
    paths = [root / ("auth.json" if provider == "codex" else ".credentials.json")]
    if provider == "claude":
        paths.append(Path.home() / ".claude.json" if root == (Path.home() / ".claude").resolve() else root / ".claude.json")
    files = {}
    for path in paths:
        found = _read_host_regular(str(path))
        files[os.path.realpath(path)] = (_claude_identity_fingerprint(found[0] if found else None)
                                    if path.name == ".claude.json" else
                                    hashlib.sha256(found[0]).hexdigest() if found else None)
    return {"root": str(root), "files": files}


def verify_credential_copy(path, data):
    from makewand.execution_runtime import current_context
    binding = current_context().get("_credential_binding")
    if binding is None:
        return
    key = os.path.realpath(path)
    if key not in binding["files"] and Path(path).name in ("auth.json", ".credentials.json", ".claude.json"):
        refuse_credential_launch("credential source differs from admitted account")
    if key in binding["files"]:
        digest = (_claude_identity_fingerprint(data) if Path(path).name == ".claude.json" else
                  hashlib.sha256(data).hexdigest() if data is not None else None)
        if digest != binding["files"][key]:
            refuse_credential_launch("selected login changed after admission; refusing account switch")
