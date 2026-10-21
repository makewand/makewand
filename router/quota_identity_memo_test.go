package router

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// GR-4: routing asks for every candidate's quota identity several times per
// request (Snapshot, Sealed), and a long-used ~/.claude.json is hundreds of KB.

// writeLargeClaudeIdentity writes a ~280 KB .claude.json naming account and
// dates it at modified.
func writeLargeClaudeIdentity(t testing.TB, path, account string, modified time.Time) {
	t.Helper()
	projects := make(map[string]any, 400)
	for i := range 400 {
		projects[fmt.Sprintf("/work/project-%03d", i)] = map[string]any{
			"history":         []string{strings.Repeat("h", 400), strings.Repeat("i", 200)},
			"allowedTools":    []string{"Bash", "Edit", "Read"},
			"lastSessionCost": 0.25,
		}
	}
	raw, err := json.Marshal(map[string]any{"numStartups": 42, "projects": projects,
		"oauthAccount": map[string]string{"accountUuid": account, "emailAddress": "user@example.invalid"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

// quotaIdentityHome selects a fresh HOME whose Claude login (~/.claude.json)
// names account and whose Codex login names "codex-account".
func quotaIdentityHome(t testing.TB, account string, modified time.Time) (home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeLargeClaudeIdentity(t, filepath.Join(home, ".claude.json"), account, modified)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(`{"tokens":{"account_id":"codex-account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func BenchmarkQuotaSnapshotAndSealedIdentity(b *testing.B) {
	quotaIdentityHome(b, "fake-claude-A", time.Now().Add(-time.Hour))
	s := NewQuotaSnapshotter(time.Hour, NewClaudeQuotaSource(""), NewCodexQuotaSource(""))
	b.ResetTimer()
	for b.Loop() {
		// One candidate's quota band (quotaBandFor), then its admission (quotaHardBlocked).
		if q, ok := s.Snapshot().Get("claude"); !ok || !q.IdentityKnown {
			b.Fatalf("claude identity = %+v", q)
		}
		s.Sealed("claude")
		s.Sealed("claude")
	}
}

// countIdentityReads counts the reads of identity files named base until the
// test ends.
func countIdentityReads(t *testing.T, base string) *atomic.Int64 {
	t.Helper()
	var reads atomic.Int64
	hook := func(path string) {
		if filepath.Base(path) == base {
			reads.Add(1)
		}
	}
	quotaIdentityFileRead.Store(&hook)
	t.Cleanup(func() { quotaIdentityFileRead.Store(nil) })
	return &reads
}

// A long-used ~/.claude.json is read once per version, not for every routing
// call; a rewritten or replaced file is read again at once.
func TestQuotaIdentityFileReadOncePerVersion_GR4(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	home := quotaIdentityHome(t, "fake-claude-A", old)
	path := filepath.Join(home, ".claude.json")
	reads := countIdentityReads(t, ".claude.json")
	s := NewQuotaSnapshotter(time.Hour, NewClaudeQuotaSource(""), NewCodexQuotaSource(""))
	account := func() string {
		t.Helper()
		q, _ := s.Snapshot().Get("claude")
		s.Sealed("claude")
		return q.AccountRef
	}
	for range 20 {
		if got := account(); got != quotaAccountRef("claude", "fake-claude-A") {
			t.Fatalf("account = %q", got)
		}
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("20 routing calls read the identity file %d times, want 1", got)
	}

	// The same size, written in place with another modification time.
	writeLargeClaudeIdentity(t, path, "fake-claude-B", old.Add(time.Minute))
	if got := account(); got != quotaAccountRef("claude", "fake-claude-B") {
		t.Fatalf("rewritten login not read again: account = %q", got)
	}
	if runtime.GOOS != "windows" {
		// Replaced by a file of the same size and modification time: a new inode.
		replacement := path + ".new"
		writeLargeClaudeIdentity(t, replacement, "fake-claude-C", old.Add(time.Minute))
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
		if got := account(); got != quotaAccountRef("claude", "fake-claude-C") {
			t.Fatalf("replaced login not read again: account = %q", got)
		}
	}
	before := reads.Load()
	for range 5 {
		account()
	}
	if got := reads.Load() - before; got != 0 {
		t.Fatalf("an unchanged login was read %d more times", got)
	}

	// A file modified less than 2 s before its inspection can change again
	// within one tick of a coarse filesystem clock and keep its stamp: it is
	// read on every call. Dated a minute ahead, it stays that recent however
	// slowly this test runs.
	recent := time.Now().Add(time.Minute)
	writeLargeClaudeIdentity(t, path, "fake-claude-D", recent)
	account()
	writeLargeClaudeIdentity(t, path, "fake-claude-E", recent)
	if got := account(); got != quotaAccountRef("claude", "fake-claude-E") {
		t.Fatalf("a rewrite keeping a recent stamp was served from memory: account = %q", got)
	}
}

// Admission and accounting (readQuotaIdentity) bind to what the login names
// now: they read the identity file every time.
func TestReadQuotaIdentityAlwaysReadsTheFile_GR4(t *testing.T) {
	home := quotaIdentityHome(t, "fake-claude-A", time.Now().Add(-time.Hour))
	reads := countIdentityReads(t, ".claude.json")
	for range 3 {
		if q := readQuotaIdentity("claude", filepath.Join(home, ".claude")); !q.IdentityKnown {
			t.Fatalf("identity = %+v", q)
		}
	}
	if got := reads.Load(); got != 3 {
		t.Fatalf("reads = %d, want 3", got)
	}
}
