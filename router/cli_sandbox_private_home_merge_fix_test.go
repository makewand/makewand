package router

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The merge reports the refusal codes, ignored fields and merged document the
// shared vector file expects, exactly as the Python engine does.
func TestWritebackFix_SharedVectorCodes(t *testing.T) {
	for _, tc := range writebackFixCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			old, cur := writebackFixFile(t, tc.Host), writebackFixFile(t, tc.Sandbox)
			m := cliMergeCredential(old, cur, old != nil, tc.MintedKeys...)
			if tc.Expect.Refuse != nil {
				var codes []string
				for _, p := range m.problems {
					codes = append(codes, p.code)
				}
				sort.Strings(codes)
				if codes = slices.Compact(codes); !slices.Equal(codes, tc.Expect.Refuse) || m.data != nil {
					t.Fatalf("refusal codes = %v (data %q), want %v", codes, m.data, tc.Expect.Refuse)
				}
				return
			}
			if len(m.problems) != 0 || !slices.Equal(m.ignored, tc.Expect.Ignored) {
				t.Fatalf("problems = %v, ignored = %v, want ignored %v", m.problems, m.ignored, tc.Expect.Ignored)
			}
			got := old
			if m.data != nil {
				got = m.data
			}
			if want := writebackFixDecode(t, tc.Expect.Host); !reflect.DeepEqual(writebackFixDecode(t, got), want) {
				t.Fatalf("merged host document = %s, want %v", got, want)
			}
		})
	}
}

// A merged document that differs from the sandbox copy is serialized like
// the host file: indented when the host file spans lines, with the host's
// trailing newline, number literals kept.
func TestWritebackFix_MergedDocumentFormat(t *testing.T) {
	sandbox := []byte(`{"tokens":{"access_token":"a2","refresh_token":"r1"},"expires_at":2.50,"plan":"pro"}`)
	for _, tc := range []struct{ name, host, want string }{
		{"pretty", "{\n  \"tokens\": {\"access_token\": \"a1\", \"refresh_token\": \"r1\"},\n  \"expires_at\": 1.50,\n  \"plan\": \"max\"\n}\n",
			"{\n  \"expires_at\": 2.50,\n  \"plan\": \"max\",\n  \"tokens\": {\n    \"access_token\": \"a2\",\n    \"refresh_token\": \"r1\"\n  }\n}\n"},
		{"compact", `{"tokens":{"access_token":"a1","refresh_token":"r1"},"expires_at":1.50,"plan":"max"}`,
			`{"expires_at":2.50,"plan":"max","tokens":{"access_token":"a2","refresh_token":"r1"}}`},
		{"unicode and html", `{"access_token":"a1","refresh_token":"r1","expires_at":1E3,"name":"Zoë <&>"}`,
			`{"access_token":"a2","expires_at":1001,"name":"Zoë <&>","refresh_token":"r1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := sandbox
			if tc.name == "unicode and html" {
				cur = []byte(`{"access_token":"a2","refresh_token":"r1","expires_at":1001,"name":"Zoe"}`)
			}
			m := cliMergeCredential([]byte(tc.host), cur, true)
			if len(m.problems) != 0 || string(m.data) != tc.want {
				t.Fatalf("merged = %q (problems %v), want %q", m.data, m.problems, tc.want)
			}
		})
	}
}

func TestWritebackFix_ProblemTextListsAtMostFive(t *testing.T) {
	doc := map[string]any{}
	for i := range 7 {
		doc[string(rune('a'+i))] = map[string]any{"access_token": "a", "refresh_token": "r"}
	}
	old, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	m := cliMergeCredential(old, []byte(`{}`), true)
	if text := m.problemText(); len(m.problems) != 7 || strings.Count(text, "removes") != 5 || !strings.HasSuffix(text, "...") {
		t.Fatalf("problem text %q for %d problems", text, len(m.problems))
	}
}

func TestWritebackFix_FieldClassification(t *testing.T) {
	key := writebackFixJWT(`{"sub":"s"}`)
	for _, tc := range []struct {
		key  string
		v    any
		kind string
	}{
		{"access_token", "a", "access"}, {"accessToken", "a", "access"}, {"key", key, "access"}, {"key", "xai-api-key", ""},
		{"refresh_token", "r", "refresh"}, {"refreshToken", "r", "refresh"}, {"id_token", "i", "id"}, {"idToken", "i", "id"},
		{"token_type", "Bearer", "type"}, {"expires_at", json.Number("1"), "time"}, {"expiresAt", "2026-01-01T00:00:00Z", "time"},
		{"refreshTokenExpiresAt", json.Number("1"), "time"}, {"expiry", "t", "time"}, {"expiry_date", json.Number("1"), "time"},
		{"last_refresh", "t", "time"}, {"access_token", "", ""}, {"access_token", nil, ""}, {"expires_at", nil, ""},
		{"token", "t", ""}, {"expires_in", json.Number("1"), ""},
	} {
		if got := cliCredentialKind(tc.key, tc.v); got != tc.kind {
			t.Errorf("kind(%q, %v) = %q, want %q", tc.key, tc.v, got, tc.kind)
		}
	}
	for _, k := range []string{"email", "user_email", "emailAddress", "api_key", "OPENAI_API_KEY", "x-api-key", "apiKey",
		"client_secret", "oidc_client_id", "key", "auth_mode", "auth_method", "mechanism", "obtained_via", "oidc_issuer",
		"api_base_url", "token_uri", "sub", "id", "uid", "account_id", "accountUuid", "user_id", "userId", "principal_id",
		"team_id", "organization_id", "organizationUuid", "tenant", "tenant_id", "session_token", "token"} {
		if !cliCredentialProtected(k, "x") {
			t.Errorf("%q must be protected", k)
		}
	}
	for _, k := range []string{"subscriptionType", "rateLimitTier", "profile_image_asset_id", "first_name", "user_full_name",
		"user_avatar_url", "create_time", "coding_data_retention_opt_out", "schema_version", "scope", "scopes", "team_name",
		"organization_role", "principal_type", "subs_active", "expires_in", "refresh_token_expires_at", "agent_identity",
		"can_administer_team"} {
		if cliCredentialProtected(k, "x") {
			t.Errorf("%q must not be protected", k)
		}
	}
	for _, tc := range []struct {
		key       string
		v         any
		protected bool
	}{
		{"active", "victim@example.com", true}, {"server", "https://api.example", true},
		{"old", []any{map[string]any{"who": "victim@example.com"}}, true},
		{"https://auth.x.ai::id", map[string]any{"refresh_token": "r"}, true},
		{"scope", "https://www.googleapis.com/auth/cloud-platform", false},
		{"note", "see https://example.com today", false}, {"avatar", "https://cdn.example/a.png", false},
	} {
		if got := cliCredentialProtected(tc.key, tc.v); got != tc.protected {
			t.Errorf("protected(%q, %v) = %v", tc.key, tc.v, got)
		}
	}
}

func TestWritebackFix_TimesAndClaims(t *testing.T) {
	at := func(s string) cliCredentialInstant {
		t.Helper()
		v, ok := cliCredentialRFC3339(s)
		if !ok {
			t.Fatalf("%q rejected", s)
		}
		return v
	}
	for _, tc := range []struct {
		s         string
		sec, nsec int64
	}{
		{"1970-01-01T00:00:00Z", 0, 0}, {"1970-01-01T08:00:00+08:00", 0, 0}, {"2000-03-01T00:00:00.5Z", 951868800, 500000000},
		{"0001-01-01T00:00:00Z", -62135596800, 0}, {"2024-02-29T23:59:59-00:30", 1709251199 + 1800, 0},
	} {
		if got := at(tc.s); got.sec != tc.sec || got.nsec != tc.nsec {
			t.Errorf("%q = %+v, want %d.%09d", tc.s, got, tc.sec, tc.nsec)
		}
	}
	if at("2026-10-12T12:00:00.123456789+08:00") != at("2026-10-12T04:00:00.123456789z") {
		t.Error("offsets must compare as instants")
	}
	for _, bad := range []string{"2026-10-12", "2026-10-12 00:00:00Z", "2026-10-12T00:00:00", "2026-02-29T00:00:00Z",
		"2026-13-01T00:00:00Z", "2026-10-12T24:00:00Z", "2026-10-12T00:00:60Z", "0000-01-01T00:00:00Z",
		"2026-10-12T00:00:00.1234567890Z", "2026-10-12T00:00:00+24:00", "２０２６-10-12T00:00:00Z"} {
		if _, ok := cliCredentialRFC3339(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if v, ok := cliCredentialInstantOf(json.Number("1760000000000")); !ok || v.num != 1760000000000 {
		t.Errorf("number instant = %+v %v", v, ok)
	}
	if _, ok := cliCredentialInstantOf(json.Number("1e400")); ok {
		t.Error("an infinite time was accepted")
	}

	claims := map[string]any{"sub": "s", "iat": json.Number("1"),
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "a", "plan": "plus"}}
	same := map[string]any{"sub": "s", "iat": json.Number("2"), "email": "new-claim@example.com",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "a", "plan": "pro"}}
	if claim := cliCredentialAccountMismatch(claims, same, ""); claim != "" {
		t.Errorf("same account reported as %q", claim)
	}
	other := map[string]any{"sub": "s", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "b"}}
	for _, cur := range []map[string]any{other, {"sub": "s"}} {
		if claim := cliCredentialAccountMismatch(claims, cur, ""); claim != "https://api.openai.com/auth.chatgpt_account_id" {
			t.Errorf("nested account claim mismatch reported as %q", claim)
		}
	}
	if claim := cliCredentialAccountMismatch(map[string]any{"sub": json.Number("1")}, map[string]any{"sub": "1"}, ""); claim != "sub" {
		t.Errorf("retyped sub reported as %q", claim)
	}
}

// flow_auth.json (a browser cookie jar) is copied into the private home but
// is not a write-back credential: no merge, no warning.
func TestWritebackFix_CookieJarIsNeverWrittenBack(t *testing.T) {
	var entry *cliPrivateCredFile
	for i, f := range cliPrivateCredentialFiles[".gemini"] {
		if f.rel == "flow_auth.json" {
			entry = &cliPrivateCredentialFiles[".gemini"][i]
		}
	}
	if entry == nil || entry.writeBack {
		t.Fatalf("flow_auth.json must be copied in only: %+v", entry)
	}
	jar := `[{"name":"SID","value":"victim","domain":".example","path":"/","expires":1.5,"httpOnly":true,"secure":true,"sameSite":"Lax"}]`
	hostRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostRoot, "flow_auth.json"), []byte(jar), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := newCLIPrivateHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.cleanup)
	if err := h.copyIn(hostRoot, h.home, *entry); err != nil {
		t.Fatal(err)
	}
	priv := filepath.Join(h.home, "flow_auth.json")
	if data, _ := os.ReadFile(priv); string(data) != jar {
		t.Fatalf("cookie jar not copied in: %q", data)
	}
	if err := os.WriteFile(priv, []byte(`[{"name":"SID","value":"attacker"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if warnings := h.writeBack(); len(warnings) != 0 {
		t.Fatalf("cookie jar change reported: %q", warnings)
	}
	if data, _ := os.ReadFile(filepath.Join(hostRoot, "flow_auth.json")); string(data) != jar {
		t.Fatalf("cookie jar written back: %q", data)
	}
}
