"""
Credential write-back merges only same-account token refreshes (py-sec-3).

A sandboxed provider (read-only reviews included) may rewrite the private copy
of its credential files. After the run the copy is merged into the host file:
only access, refresh and id tokens and, as part of a refresh, the token type
and expiry times are copied, after checks that the refresh belongs to the host
account (JWT account claims, rotation consistency, no logout, no stale times,
no new Google refresh token). Protected fields (e-mails, keys, endpoints, auth
modes, account ids) are never written; benign metadata stays as on the host.

tests/testdata/credential_writeback_cases.json holds the cases; the Go engine
(router/cli_sandbox_private_home_writeback_fix_test.go) runs the same file, so
both engines provably agree. A key the provider CLI mints itself (muse's
api_key) is a token leaf of that file only. Everything runs against a
throw-away HOME; no provider CLI is started (the live cases run /bin/sh inside
the real bubblewrap sandbox when it is usable).
"""

try:  # isolation must precede makewand imports
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import base64
import contextlib
import json
import os
import shlex
import shutil
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import sandbox
from makewand.sandbox import ProviderHome, wrap_bwrap

VECTORS = Path(__file__).resolve().parent / "testdata" / "credential_writeback_cases.json"


def _b64url(raw):
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def jwt(claims):
    """An unsigned JWT-shaped token carrying claims (the vectors' $jwt notation)."""
    text = claims if isinstance(claims, str) else json.dumps(claims, separators=(",", ":"))
    return ".".join([_b64url(b'{"alg":"RS256","typ":"JWT"}'), _b64url(text.encode()), "c2ln"])


def expand(value):
    """Replaces the vectors' "$jwt:<claims>" strings by JWTs."""
    if isinstance(value, dict):
        return {k: expand(v) for k, v in value.items()}
    if isinstance(value, list):
        return [expand(v) for v in value]
    if isinstance(value, str) and value.startswith("$jwt:"):
        return jwt(value[len("$jwt:"):])
    return value


def file_bytes(doc):
    """The file a vector document stands for; None: absent."""
    if doc is None:
        return None
    if isinstance(doc, dict) and set(doc) == {"$raw"}:
        return doc["$raw"].encode()
    return json.dumps(expand(doc), separators=(",", ":"), ensure_ascii=False).encode()


def canonical(value):
    """Order-insensitive text of a decoded JSON value (true and 1 stay distinct)."""
    return json.dumps(value, sort_keys=True, ensure_ascii=False)


def load_cases():
    with open(VECTORS, encoding="utf-8") as fh:
        return json.load(fh)["cases"]


@contextlib.contextmanager
def fake_home():
    home = Path(tempfile.mkdtemp(prefix="wbfix-home-"))
    try:
        with patch.dict(os.environ, {"HOME": str(home)}):
            os.environ.pop("CODEX_HOME", None)
            yield home
    finally:
        shutil.rmtree(home, ignore_errors=True)


def _root(provider):
    return sandbox.PROVIDER_PROFILES["agy" if provider == "gemini" else provider]["roots"][0]


def _materialized_home(home, provider, rel, content):
    """ProviderHome with one credential file; content None leaves it absent on the host."""
    root = home / _root(provider)
    root.mkdir(parents=True, exist_ok=True)
    host = root / rel
    host.parent.mkdir(parents=True, exist_ok=True)
    if content is not None:
        host.write_bytes(content)
        os.chmod(host, 0o600)
    h = ProviderHome(provider, str(home))
    h.materialize([])
    cred = next(c for c in h.credentials if c.rel == rel)
    return h, Path(cred.private_root, rel), host


MUSE_MINTED = sandbox.PROVIDER_PROFILES["muse"]["minted_keys"][(".config/muse", "auth.json")]


class TestSharedWriteBackVectors(unittest.TestCase):
    def test_vectors_through_the_provider_home(self):
        """Every case end to end: host file afterwards and the warnings printed.
        Cases with minted keys are muse's auth.json and run through its profile."""
        for case in load_cases():
            with self.subTest(case["name"]), fake_home() as home:
                old, new, expect = file_bytes(case["host"]), file_bytes(case["sandbox"]), case["expect"]
                provider = "codex"
                if case.get("minted_keys"):
                    self.assertEqual(tuple(case["minted_keys"]), MUSE_MINTED)
                    provider = "muse"
                h, priv, host = _materialized_home(home, provider, "auth.json", old)
                priv.write_bytes(new)
                with patch("makewand.sandbox._warn") as warn:
                    h.finalize()
                messages = [c.args[0] for c in warn.call_args_list]
                if "refuse" in expect:
                    self.assertEqual(host.read_bytes() if old is not None else host.exists(),
                                     old if old is not None else False)
                    self.assertEqual(len(messages), 1, messages)
                    self.assertIn(str(host), messages[0])
                    self.assertIn("保留宿主版本", messages[0])
                    continue
                want = expand(expect["host"])
                if canonical(want) == canonical(json.loads(old)):
                    self.assertEqual(host.read_bytes(), old, "nothing may be written")
                elif canonical(want) == canonical(json.loads(new)):
                    self.assertEqual(host.read_bytes(), new, "the sandbox bytes are written verbatim")
                else:
                    self.assertEqual(canonical(json.loads(host.read_bytes())), canonical(want))
                self.assertFalse(host.is_symlink())
                self.assertEqual(stat.S_IMODE(host.stat().st_mode), 0o600)
                if expect["ignored"]:
                    self.assertEqual(len(messages), 1, messages)
                    for field in expect["ignored"]:
                        self.assertIn(field, messages[0])
                else:
                    self.assertEqual(messages, [])

    def test_vectors_refusal_codes_and_ignored_fields(self):
        for case in load_cases():
            with self.subTest(case["name"]):
                expect = case["expect"]
                merge = sandbox._credential_writeback_merge(file_bytes(case["host"]), file_bytes(case["sandbox"]),
                                                            case.get("minted_keys", ()))
                if "refuse" in expect:
                    self.assertEqual(sorted({code for _path, code, _detail in merge.problems}), expect["refuse"])
                    self.assertIsNone(merge.data)
                else:
                    self.assertEqual(merge.problems, [])
                    self.assertEqual(merge.ignored, expect["ignored"])


class TestWriteBackWarnings(unittest.TestCase):
    def test_refusal_names_the_field_but_never_its_value(self):
        with fake_home() as home:
            old = json.dumps({"tokens": {"access_token": "a1", "refresh_token": "r1", "account_id": "acct-victim"}})
            h, priv, host = _materialized_home(home, "codex", "auth.json", old.encode())
            priv.write_text(json.dumps({"tokens": {"access_token": "a2", "refresh_token": "r2",
                                                   "account_id": "acct-attacker"}}))
            with patch("makewand.sandbox._warn") as warn:
                h.finalize()
            message = warn.call_args[0][0]
            self.assertIn("tokens.account_id", message)
            for value in ("acct-attacker", "acct-victim", "a2", "r2"):
                self.assertNotIn(value, message.replace(str(host), ""))
            self.assertEqual(host.read_text(), old)

    def test_refused_write_back_still_removes_the_private_home(self):
        with fake_home() as home:
            old = b'{"tokens":{"access_token":"a1","refresh_token":"r1"}}'
            h, priv, host = _materialized_home(home, "codex", "auth.json", old)
            private_dir = h.private_dir
            priv.write_text("{}")
            with patch("makewand.sandbox._warn"):
                h.finalize()
            self.assertFalse(os.path.exists(private_dir))
            self.assertEqual(host.read_bytes(), old)

    def test_problem_text_lists_at_most_five_problems(self):
        old = json.dumps({f"t{i}": {"access_token": "a", "refresh_token": "r"} for i in range(7)}).encode()
        merge = sandbox._credential_writeback_merge(old, b"{}")
        self.assertEqual(len(merge.problems), 7)
        self.assertTrue(merge.problem_text().endswith("……"))
        self.assertEqual(merge.problem_text().count("删除了"), 5)


class TestMergedDocumentFormat(unittest.TestCase):
    """A merged document that differs from the sandbox copy is serialized like the host file."""

    HOST = {"tokens": {"access_token": "a1", "refresh_token": "r1"}, "expires_at": 1.50, "plan": "max"}
    SANDBOX = {"tokens": {"access_token": "a2", "refresh_token": "r1"}, "expires_at": 2.50, "plan": "pro"}

    def test_pretty_host_with_trailing_newline(self):
        old = (json.dumps(self.HOST, indent=2) + "\n").encode()
        merge = sandbox._credential_writeback_merge(old, json.dumps(self.SANDBOX).encode())
        self.assertEqual(merge.data.decode(), json.dumps(dict(self.SANDBOX, plan="max"), indent=2) + "\n")

    def test_compact_host_without_trailing_newline(self):
        old = b'{"tokens":{"access_token":"a1","refresh_token":"r1"},"expires_at":1.50,"plan":"max"}'
        new = b'{\n    "tokens": {"access_token": "a2", "refresh_token": "r1"},\n    "expires_at": 2.50,\n    "plan": "pro"\n}\n'
        merge = sandbox._credential_writeback_merge(old, new)
        self.assertEqual(merge.data, b'{"tokens":{"access_token":"a2","refresh_token":"r1"},"expires_at":2.50,"plan":"max"}')

    def test_number_literals_and_unicode_survive(self):
        old = '{"access_token":"a1","refresh_token":"r1","expires_at":1E3,"id":9007199254740993,"name":"Zoë"}'
        new = '{"access_token":"a2","refresh_token":"r1","expires_at":1001,"id":9007199254740993,"name":"Zoe"}'
        merge = sandbox._credential_writeback_merge(old.encode(), new.encode())
        self.assertEqual(merge.data.decode(), new.replace('"Zoe"', '"Zoë"'))


class TestFieldClassification(unittest.TestCase):
    def test_token_refresh_roles(self):
        token = jwt({"sub": "s"})
        for key, value, kind in (("access_token", "a", "access"), ("accessToken", "a", "access"),
                                 ("key", token, "access"), ("key", "xai-api-key", None),
                                 ("refresh_token", "r", "refresh"), ("refreshToken", "r", "refresh"),
                                 ("id_token", "i", "id"), ("idToken", "i", "id"), ("token_type", "Bearer", "type"),
                                 ("expires_at", sandbox._JsonNumber("1"), "time"), ("expiresAt", "2026-01-01T00:00:00Z", "time"),
                                 ("refreshTokenExpiresAt", sandbox._JsonNumber("1"), "time"),
                                 ("expiry", "t", "time"), ("expiry_date", sandbox._JsonNumber("1"), "time"),
                                 ("last_refresh", "t", "time"), ("access_token", "", None), ("access_token", None, None),
                                 ("expires_at", None, None), ("token", "t", None), ("expires_in", sandbox._JsonNumber("1"), None)):
            with self.subTest(key=key, value=value):
                self.assertEqual(sandbox._cred_kind(key, value), kind)

    def test_protected_fields(self):
        for key in ("email", "user_email", "emailAddress", "api_key", "OPENAI_API_KEY", "x-api-key", "apiKey",
                    "client_secret", "oidc_client_id", "key", "auth_mode", "auth_method", "mechanism", "obtained_via",
                    "oidc_issuer", "api_base_url", "token_uri", "sub", "id", "uid", "account_id", "accountUuid",
                    "user_id", "userId", "principal_id", "team_id", "organization_id", "organizationUuid", "tenant",
                    "tenant_id", "session_token", "token"):
            with self.subTest(key):
                self.assertTrue(sandbox._cred_protected(key, "x"))
        for key in ("subscriptionType", "rateLimitTier", "profile_image_asset_id", "first_name", "user_full_name",
                    "user_avatar_url", "create_time", "coding_data_retention_opt_out", "schema_version", "scope",
                    "scopes", "team_name", "organization_role", "principal_type", "subs_active", "expires_in",
                    "refresh_token_expires_at", "agent_identity", "can_administer_team"):
            with self.subTest(key):
                self.assertFalse(sandbox._cred_protected(key, "x"))

    def test_protected_values(self):
        self.assertTrue(sandbox._cred_protected("active", "victim@example.com"))
        self.assertTrue(sandbox._cred_protected("server", "https://api.example"))
        self.assertTrue(sandbox._cred_protected("old", [{"who": "victim@example.com"}]))
        self.assertTrue(sandbox._cred_protected("https://auth.x.ai::id", {"refresh_token": "r"}))
        self.assertFalse(sandbox._cred_protected("scope", "https://www.googleapis.com/auth/cloud-platform"))
        self.assertFalse(sandbox._cred_protected("note", "see https://example.com today"))
        self.assertFalse(sandbox._cred_protected("avatar", "https://cdn.example/a.png"))


class TestTimesAndClaims(unittest.TestCase):
    def test_rfc3339_instants(self):
        at = sandbox._cred_instant
        self.assertEqual(at("1970-01-01T00:00:00Z"), (0, 0))
        self.assertEqual(at("1970-01-01T08:00:00+08:00"), (0, 0))
        self.assertEqual(at("2026-10-12T12:00:00.123456789+08:00"), at("2026-10-12T04:00:00.123456789z"))
        self.assertEqual(at("2000-03-01T00:00:00.5Z"), (951868800, 500000000))
        self.assertEqual(at("0001-01-01T00:00:00Z"), (-62135596800, 0))
        self.assertEqual(at("2024-02-29T23:59:59-00:30"), (1709251199 + 1800, 0))
        for bad in ("2026-10-12", "2026-10-12 00:00:00Z", "2026-10-12T00:00:00", "2026-02-29T00:00:00Z",
                    "2026-13-01T00:00:00Z", "2026-10-12T24:00:00Z", "2026-10-12T00:00:60Z", "0000-01-01T00:00:00Z",
                    "2026-10-12T00:00:00.1234567890Z", "2026-10-12T00:00:00+24:00", "２０２６-10-12T00:00:00Z"):
            with self.subTest(bad):
                self.assertIsNone(at(bad))

    def test_number_instants(self):
        self.assertEqual(sandbox._cred_instant(sandbox._JsonNumber("1760000000000")), (1760000000000.0,))
        self.assertIsNone(sandbox._cred_instant(sandbox._JsonNumber("1e400")))

    def test_account_claims_compared_at_any_depth(self):
        claims = {"sub": "s", "iat": 1, "https://api.openai.com/auth": {"chatgpt_account_id": "a", "plan": "plus"}}
        same = {"sub": "s", "iat": 2, "https://api.openai.com/auth": {"chatgpt_account_id": "a", "plan": "pro"},
                "email": "new-claim@example.com"}
        self.assertIsNone(sandbox._cred_account_mismatch(claims, same))
        other = {"sub": "s", "https://api.openai.com/auth": {"chatgpt_account_id": "b"}}
        self.assertEqual(sandbox._cred_account_mismatch(claims, other), "https://api.openai.com/auth.chatgpt_account_id")
        self.assertEqual(sandbox._cred_account_mismatch(claims, {"sub": "s"}), "https://api.openai.com/auth.chatgpt_account_id")
        self.assertEqual(sandbox._cred_account_mismatch({"sub": 1}, {"sub": "1"}), "sub")


class TestStrictCredentialJSON(unittest.TestCase):
    """The decoder behind the merge rejects input a lenient parser could read
    differently from the provider CLI."""

    def test_valid_documents_decode(self):
        deep = "[" * (sandbox._CRED_JSON_MAX_DEPTH + 1) + "]" * (sandbox._CRED_JSON_MAX_DEPTH + 1)
        for doc in ('{"a":1}', ' {"a":[1,{"b":null}],"c":"\\u00e9"} ', deep, '{"a":"\\ud83d\\ude00"}',
                    '{"a":"\\uD83D\\uDE00\\ufffd"}', '{"a":"\\\\ud800"}', '{"\\\\udc00":"\\\\\\ud83d\\ude00"}'):
            sandbox._cred_load_json(doc.encode())

    def test_invalid_or_ambiguous_documents_are_rejected(self):
        deep = "[" * (sandbox._CRED_JSON_MAX_DEPTH + 2) + "]" * (sandbox._CRED_JSON_MAX_DEPTH + 2)
        for doc in (b"", b'{"a" 1}', b'{"a":1,}', b"[1,]", b'{"a":1 "b":2}', b'{"a"}', b'{"a":1}}',
                    b'{"a":1,"a":2}', b'{"a":{"b":1,"\\u0062":2}}', b'{"a":Infinity}', b'{"a":01}',
                    b'{"a":"\xff"}', b"\xef\xbb\xbf{}", deep.encode(), b"[" * 100000,
                    # lone surrogate escapes (Go's decoder reads U+FFFD; no UTF-8 encoder accepts them)
                    b'{"a":"\\ud800"}', b'{"a":"x\\udfff"}', b'{"a":"\\ud800\\u0041"}', b'{"a":"\\ude00\\ud83d"}',
                    b'{"a":"\\ud83d\\ud83d\\ude00"}', b'{"\\udbff":1}', b'["\\\\\\ud800"]'):
            with self.subTest(doc[:40]), self.assertRaises(ValueError):
                sandbox._cred_load_json(doc)

    def test_numbers_are_kept_exactly(self):
        doc = sandbox._cred_load_json(b'{"id":9007199254740993}')
        self.assertEqual(doc["id"], "9007199254740993")


class TestCookieJarIsNeverWrittenBack(unittest.TestCase):
    """flow_auth.json (a browser cookie jar) is copied into the private home but
    is not a write-back credential: no merge, no warning."""

    def test_flow_auth_is_copied_in_only(self):
        profile = sandbox.PROVIDER_PROFILES["agy"]
        self.assertNotIn((".gemini", "flow_auth.json"), profile["credentials"])
        self.assertIn((".gemini", "flow_auth.json"), profile["seed"])
        with fake_home() as home:
            jar = home / ".gemini" / "flow_auth.json"
            jar.parent.mkdir(parents=True)
            jar.write_text('[{"name":"SID","value":"victim","domain":".example","path":"/","expires":1.5,'
                           '"httpOnly":true,"secure":true,"sameSite":"Lax"}]')
            h = ProviderHome("agy", str(home))
            h.materialize([])
            priv = Path(h.private_dir, "root0", "flow_auth.json")
            self.assertEqual(priv.read_text(), jar.read_text())
            before = jar.read_bytes()
            priv.write_text('[{"name":"SID","value":"attacker"}]')
            with patch("makewand.sandbox._warn") as warn:
                h.finalize()
            warn.assert_not_called()
            self.assertEqual(jar.read_bytes(), before)


@unittest.skipUnless(sandbox.is_bwrap_available(), "bubblewrap not usable on this host")
class TestLiveSandboxWriteBack(unittest.TestCase):
    """End to end through the real bubblewrap sandbox, read-only and writable runs alike."""

    def _run(self, provider, ws, readonly, rel, new):
        path = f"~/{_root(provider)}/{rel}"
        payload = f"printf '%s' {shlex.quote(new)} > {path}.tmp && mv -f {path}.tmp {path} && echo done\n"
        with sandbox.provider_home_session(provider, [str(ws)]) as session:
            cmd = wrap_bwrap(["bash", "-c", payload], workspace=str(ws), is_provider=True, provider_name=provider,
                             readonly=readonly, allow_network=False, provider_home=session)
            res = subprocess.run(cmd, cwd=str(ws), capture_output=True, text=True, timeout=60)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn("done", res.stdout)

    def _case(self, name):
        return next(c for c in load_cases() if c["name"] == name)

    def test_refresh_written_back_swap_and_logout_refused(self):
        live = [("codex", "auth.json", "codex refresh"), ("codex", "auth.json", "codex account swap"),
                ("codex", "auth.json", "codex logout writing an empty object"),
                ("agy", "antigravity-cli/antigravity-oauth-token", "agy token refresh with a rotated id_token (same sub/email)"),
                ("agy", "antigravity-cli/antigravity-oauth-token", "agy merged swap: victim id_token kept, opaque tokens swapped"),
                ("gemini", "oauth_creds.json", "gemini refresh with reordered scopes: the scopes stay as on the host"),
                ("gemini", "oauth_creds.json", "gemini oauth_creds merged swap: victim id_token kept")]
        for readonly in (True, False):
            for provider, rel, name in live:
                case = self._case(name)
                with self.subTest(readonly=readonly, case=name), fake_home() as home:
                    ws = Path(tempfile.mkdtemp(prefix="wbfix-ws-"))
                    try:
                        old, new = file_bytes(case["host"]), file_bytes(case["sandbox"])
                        cred = home / _root(provider) / rel
                        cred.parent.mkdir(parents=True)
                        cred.write_bytes(old)
                        os.chmod(cred, 0o600)
                        with patch("makewand.sandbox._warn") as warn:
                            self._run(provider, ws, readonly, rel, new.decode())
                        after = cred.read_bytes()
                        if "refuse" in case["expect"]:
                            self.assertEqual(after, old)
                            self.assertEqual(warn.call_count, 1, warn.call_args_list)
                        else:
                            self.assertEqual(canonical(json.loads(after)), canonical(expand(case["expect"]["host"])))
                            self.assertEqual(warn.call_count, 0, warn.call_args_list)
                    finally:
                        shutil.rmtree(ws, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
