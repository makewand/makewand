package router

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// writebackFixVectorFile holds the credential write-back cases shared with
// the Python engine (tests/test_fix_writeback_identity.py).
const writebackFixVectorFile = "../tests/testdata/credential_writeback_cases.json"

type writebackFixCase struct {
	Name       string          `json:"name"`
	MintedKeys []string        `json:"minted_keys"`
	Host       json.RawMessage `json:"host"`
	Sandbox    json.RawMessage `json:"sandbox"`
	Expect     struct {
		Refuse  []string        `json:"refuse"`
		Host    json.RawMessage `json:"host"`
		Ignored []string        `json:"ignored"`
	} `json:"expect"`
}

func writebackFixCases(t *testing.T) []writebackFixCase {
	t.Helper()
	data, err := os.ReadFile(writebackFixVectorFile)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []writebackFixCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Cases) == 0 {
		t.Fatal("no write-back cases")
	}
	return file.Cases
}

func writebackFixCase1(t *testing.T, name string) writebackFixCase {
	t.Helper()
	for _, tc := range writebackFixCases(t) {
		if tc.Name == name {
			return tc
		}
	}
	t.Fatalf("no write-back case %q", name)
	return writebackFixCase{}
}

// writebackFixJWT returns the unsigned JWT-shaped token the vectors write as
// "$jwt:<claims>".
func writebackFixJWT(claims string) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString([]byte(claims)) + ".c2ln"
}

// writebackFixDecode decodes JSON with exact numbers and expands the
// vectors' $jwt notation.
func writebackFixDecode(t *testing.T, data []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	var expand func(v any) any
	expand = func(v any) any {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				v[k] = expand(x)
			}
		case []any:
			for i, x := range v {
				v[i] = expand(x)
			}
		case string:
			if claims, ok := strings.CutPrefix(v, "$jwt:"); ok {
				return writebackFixJWT(claims)
			}
		}
		return v
	}
	return expand(v)
}

// writebackFixFile returns the file a vector document stands for; nil: absent.
func writebackFixFile(t *testing.T, doc json.RawMessage) []byte {
	t.Helper()
	v := writebackFixDecode(t, doc)
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok && len(m) == 1 {
		if text, ok := m["$raw"].(string); ok {
			return []byte(text)
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// writebackFixHome prepares a private home holding a write-back copy of
// hostRoot/rel. hostContent == nil leaves the host file absent.
func writebackFixHome(t *testing.T, rel string, hostContent *string) (*cliPrivateHome, string, string) {
	t.Helper()
	hostRoot := t.TempDir()
	hostFile := filepath.Join(hostRoot, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(hostFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if hostContent != nil {
		if err := os.WriteFile(hostFile, []byte(*hostContent), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h, err := newCLIPrivateHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.cleanup)
	privRoot := filepath.Join(h.dir, "codex-home")
	if err := os.Mkdir(privRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := h.copyIn(hostRoot, privRoot, cliPrivateCredFile{rel, true}); err != nil {
		t.Fatal(err)
	}
	return h, hostFile, filepath.Join(privRoot, filepath.FromSlash(rel))
}

// writebackFixRunAndWait runs wrapped and waits until the private home bound
// at home has been written back and removed. The finished hook also fires for
// unrelated private homes (unstarted commands of earlier mount-plan tests are
// cleaned up when garbage collected), so only this run's directory counts.
func writebackFixRunAndWait(t *testing.T, wrapped *exec.Cmd, home string) string {
	t.Helper()
	dir := filepath.Dir(cliPrivateHomeSource(t, wrapped.Args, home))
	done := make(chan struct{})
	var once sync.Once
	hook := func(finished string) {
		if finished == dir {
			once.Do(func() { close(done) })
		}
	}
	old := cliPrivateHomeFinished.Swap(&hook)
	t.Cleanup(func() { cliPrivateHomeFinished.Store(old) })
	out, err := runWrappedCLIForTest(wrapped)
	if err != nil {
		t.Fatalf("sandbox run failed: %v %s (args=%v)", err, out, wrapped.Args)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("private home write-back did not run after the sandbox exited")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("private home %s was not removed: %v", dir, err)
	}
	return string(out)
}

// Every case of the shared vector file end to end through writeBack: the
// host file afterwards and the warnings printed. Only a same-account token
// refresh is merged into the host file; a refusal leaves it untouched and is
// reported once; protected fields added or removed are reported, benign
// metadata silently stays as on the host. Cases with minted keys (muse, a
// Python-engine provider) are checked by TestWritebackFix_SharedVectorCodes.
func TestWritebackFix_SharedVectors(t *testing.T) {
	for _, tc := range writebackFixCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			if len(tc.MintedKeys) > 0 {
				t.Skip("no provider of this engine mints keys")
			}
			old, cur := writebackFixFile(t, tc.Host), writebackFixFile(t, tc.Sandbox)
			var hostContent *string
			if old != nil {
				s := string(old)
				hostContent = &s
			}
			h, host, priv := writebackFixHome(t, "auth.json", hostContent)
			if err := os.WriteFile(priv, cur, 0o600); err != nil {
				t.Fatal(err)
			}
			warnings := h.writeBack()
			got, err := os.ReadFile(host)
			if tc.Expect.Refuse != nil {
				switch {
				case old == nil && !os.IsNotExist(err):
					t.Fatalf("credential created inside the sandbox reached the host: %q %v", got, err)
				case old != nil && !bytes.Equal(got, old):
					t.Fatalf("refused write-back changed the host: %q, want %q", got, old)
				case len(warnings) != 1 || !strings.Contains(warnings[0], host) || !strings.Contains(warnings[0], "host copy kept"):
					t.Fatalf("a refused write-back must be reported once: %q", warnings)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := writebackFixDecode(t, tc.Expect.Host)
			switch {
			case reflect.DeepEqual(writebackFixDecode(t, old), want):
				if !bytes.Equal(got, old) {
					t.Fatalf("nothing may be written, host = %q", got)
				}
			case reflect.DeepEqual(writebackFixDecode(t, cur), want):
				if !bytes.Equal(got, cur) {
					t.Fatalf("sandbox bytes not written verbatim: %q", got)
				}
			case !reflect.DeepEqual(writebackFixDecode(t, got), want):
				t.Fatalf("host = %s, want %v", got, want)
			}
			if fi, err := os.Lstat(host); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
				t.Fatalf("written host credential: %v %v", fi, err)
			}
			if len(tc.Expect.Ignored) == 0 {
				if len(warnings) != 0 {
					t.Fatalf("unexpected warnings: %q", warnings)
				}
				return
			}
			if len(warnings) != 1 {
				t.Fatalf("ignored protected fields must be reported once: %q", warnings)
			}
			for _, field := range tc.Expect.Ignored {
				if !strings.Contains(warnings[0], field) {
					t.Fatalf("warning %q does not name %s", warnings[0], field)
				}
			}
		})
	}
}

// A refusal names the field, never a credential value.
func TestWritebackFix_WarningsNameFieldsNotValues(t *testing.T) {
	old := `{"tokens":{"access_token":"a1","refresh_token":"r1","account_id":"acct-victim"}}`
	h, host, priv := writebackFixHome(t, "auth.json", &old)
	if err := os.WriteFile(priv, []byte(`{"tokens":{"access_token":"a2","refresh_token":"r2","account_id":"acct-attacker"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	warnings := h.writeBack()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "tokens.account_id") {
		t.Fatalf("account change not reported: %q", warnings)
	}
	for _, value := range []string{"acct-attacker", "acct-victim", "a2", "r2"} {
		if strings.Contains(strings.ReplaceAll(warnings[0], host, ""), value) {
			t.Fatalf("warning reveals %q: %q", value, warnings[0])
		}
	}
}

// The strict decoder behind the merge rejects what a lenient parser could
// read differently from the provider CLI.
func TestWritebackFix_StrictCredentialJSON(t *testing.T) {
	deep := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
	for _, doc := range []string{`{"a":1}`, ` {"a":[1,{"b":null}],"c":"é"} `, deep(cliCredentialMaxJSONDepth + 1),
		`{"a":"😀"}`, `{"a":"😀�"}`, `{"a":"\\ud800"}`, `{"\\udc00":"\\😀"}`} {
		if _, err := cliDecodeCredentialJSON([]byte(doc)); err != nil {
			t.Fatalf("valid JSON %.40q rejected: %v", doc, err)
		}
	}
	// Lone surrogate escapes: encoding/json reads them as U+FFFD, Python's
	// json as a lone surrogate no UTF-8 encoder accepts.
	for _, doc := range []string{``, `{"a" 1}`, `{"a":1,}`, `[1,]`, `{"a":1 "b":2}`, `{"a"}`, `{"a":1}}`,
		`{"a":1,"a":2}`, `{"a":{"b":1,"b":2}}`, `{"a":Infinity}`, `{"a":01}`, "{\"a\":\"\xff\"}",
		"\xef\xbb\xbf{}", deep(cliCredentialMaxJSONDepth + 2),
		`{"a":"\ud800"}`, `{"a":"x\udfff"}`, `{"a":"\ud800A"}`, `{"a":"\ude00\ud83d"}`, `{"a":"\ud83d😀"}`,
		`{"\udbff":1}`, `["\\\ud800"]`, `{"a":"\ud800"`} {
		if _, err := cliDecodeCredentialJSON([]byte(doc)); err == nil {
			t.Fatalf("invalid or ambiguous JSON %.40q accepted", doc)
		}
	}
	if v, err := cliDecodeCredentialJSON([]byte(`{"id":9007199254740993}`)); err != nil ||
		v.(map[string]any)["id"] != json.Number("9007199254740993") {
		t.Fatalf("number not kept exactly: %v %v", v, err)
	}
}

// End to end through a real bubblewrap sandbox: same-account refreshes are
// merged into the host file; account swaps (agy and gemini included) and
// logouts are not.
func TestWritebackFix_LiveSandboxWritesBackOnlySameAccountRefresh(t *testing.T) {
	auditFixRequireLiveBwrap(t)
	for _, live := range []struct{ provider, rel, name string }{
		{"codex", ".codex/auth.json", "codex refresh"},
		{"codex", ".codex/auth.json", "codex account swap"},
		{"codex", ".codex/auth.json", "codex logout writing an empty object"},
		{"agy", ".gemini/antigravity-cli/antigravity-oauth-token", "agy token refresh with a rotated id_token (same sub/email)"},
		{"agy", ".gemini/antigravity-cli/antigravity-oauth-token", "agy merged swap: victim id_token kept, opaque tokens swapped"},
		{"gemini", ".gemini/antigravity-cli/antigravity-oauth-token", "agy merged swap: victim id_token kept, opaque tokens swapped"},
		{"gemini", ".gemini/oauth_creds.json", "gemini refresh with reordered scopes: the scopes stay as on the host"},
		{"gemini", ".gemini/oauth_creds.json", "gemini oauth_creds merged swap: victim id_token kept"},
	} {
		t.Run(live.provider+": "+live.name, func(t *testing.T) {
			tc := writebackFixCase1(t, live.name)
			old, cur := writebackFixFile(t, tc.Host), writebackFixFile(t, tc.Sandbox)
			home, ws := t.TempDir(), t.TempDir()
			cred := filepath.Join(home, filepath.FromSlash(live.rel))
			if err := os.MkdirAll(filepath.Dir(cred), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cred, old, 0o600); err != nil {
				t.Fatal(err)
			}
			script := `printf '%s' "$1" > "$HOME/$2.tmp" && mv -f "$HOME/$2.tmp" "$HOME/$2" && echo done`
			//nolint:gosec // G204: fixed fixture script; the credential JSON and path are positional arguments, never shell source.
			cmd := exec.Command("/bin/sh", "-c", script, "fixture", string(cur), live.rel)
			cmd.Dir = ws
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
			wrapped, err := wrapCLICommandWithSandbox(context.Background(), live.provider, cmd)
			if err != nil {
				t.Fatal(err)
			}
			if out := writebackFixRunAndWait(t, wrapped, home); !strings.Contains(out, "done") {
				t.Fatalf("unexpected sandbox output: %q", out)
			}
			got, _ := os.ReadFile(cred)
			if tc.Expect.Refuse != nil {
				if !bytes.Equal(got, old) {
					t.Fatalf("host credential changed by a refused write-back: %q", got)
				}
				return
			}
			if want := writebackFixDecode(t, tc.Expect.Host); !reflect.DeepEqual(writebackFixDecode(t, got), want) {
				t.Fatalf("host credential = %s, want %v", got, want)
			}
		})
	}
}
