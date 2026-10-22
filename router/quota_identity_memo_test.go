package router

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// inspectIdentityLater inspects identity files an hour after now, when every
// stamp a test writes has left the racy window, until the test ends.
func inspectIdentityLater(t testing.TB) {
	t.Helper()
	clock := func() time.Time { return time.Now().Add(time.Hour) }
	quotaIdentityClock.Store(&clock)
	t.Cleanup(func() { quotaIdentityClock.Store(nil) })
}

// inspectIdentityAt inspects identity files at the time at returns.
func inspectIdentityAt(t testing.TB, at func() time.Time) {
	t.Helper()
	quotaIdentityClock.Store(&at)
	t.Cleanup(func() { quotaIdentityClock.Store(nil) })
}

// identityFileVersion returns the version stamp parts of the file at path and
// whether this platform reports them (quotaIdentityFileVersion).
func identityFileVersion(t testing.TB, path string) (info os.FileInfo, dev, ino uint64, changed time.Time, versioned bool) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	dev, ino, changed, versioned = quotaIdentityFileVersion(info)
	return info, dev, ino, changed, versioned
}

func BenchmarkQuotaSnapshotAndSealedIdentity(b *testing.B) {
	quotaIdentityHome(b, "fake-claude-A", time.Now().Add(-time.Hour))
	inspectIdentityLater(b)
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
// call; a rewritten or replaced file is read again at once. Where file info
// carries no change time (Windows) it is read on every call.
func TestQuotaIdentityFileReadOncePerVersion_GR4(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	home := quotaIdentityHome(t, "fake-claude-A", old)
	path := filepath.Join(home, ".claude.json")
	_, _, _, _, versioned := identityFileVersion(t, path)
	inspectIdentityLater(t)
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
	if got := reads.Load(); versioned && got != 1 || !versioned && got < 20 {
		t.Fatalf("20 routing calls read the identity file %d times, want 1 (versioned=%v)", got, versioned)
	}

	// The same size, written in place with another modification time.
	writeLargeClaudeIdentity(t, path, "fake-claude-B", old.Add(time.Minute))
	if got := account(); got != quotaAccountRef("claude", "fake-claude-B") {
		t.Fatalf("rewritten login not read again: account = %q", got)
	}
	// Replaced by a file of the same size and modification time.
	replacement := path + ".new"
	writeLargeClaudeIdentity(t, replacement, "fake-claude-C", old.Add(time.Minute))
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if got := account(); got != quotaAccountRef("claude", "fake-claude-C") {
		t.Fatalf("replaced login not read again: account = %q", got)
	}
	before := reads.Load()
	for range 5 {
		account()
	}
	if got := reads.Load() - before; versioned && got != 0 || !versioned && got < 5 {
		t.Fatalf("an unchanged login was read %d more times (versioned=%v)", got, versioned)
	}
}

// The reviewer's case: a login file rewritten in place for another account
// with the same size, its modification time restored, keeps its device, inode,
// size and modification time. Its change time moves, so it is read again; so
// is a file deleted and created anew, which can reuse the inode number.
func TestQuotaIdentityRewriteRestoringSizeAndMtimeIsReadAgain(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "auth.json")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	write := func(account string) {
		t.Helper()
		// #nosec G703 -- path is the auth.json this test created under t.TempDir().
		if err := os.WriteFile(path, []byte(`{"tokens":{"account_id":"`+account+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	write("account-A")
	inspectIdentityLater(t)
	var memo quotaIdentityMemo
	if got := memo.read("codex", root).AccountRef; got != quotaAccountRef("codex", "account-A") {
		t.Fatalf("account = %q", got)
	}
	before, dev, ino, _, _ := identityFileVersion(t, path)

	write("account-B")
	after, devAfter, inoAfter, _, _ := identityFileVersion(t, path)
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || devAfter != dev || inoAfter != ino {
		t.Fatal("the rewrite changed the size, modification time or inode; the case is not reproduced")
	}
	if got := memo.read("codex", root).AccountRef; got != quotaAccountRef("codex", "account-B") {
		t.Fatalf("a rewrite restoring size and modification time was served from memory: account = %q", got)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	write("account-C")
	if got := memo.read("codex", root).AccountRef; got != quotaAccountRef("codex", "account-C") {
		t.Fatalf("a recreated login was served from memory: account = %q", got)
	}
}

// A file modified or changed less than 2 s before its inspection can change
// again within one tick of a coarse filesystem clock and keep its stamp: it is
// read on every call until both times have left that window.
func TestQuotaIdentityRacyStampIsReadEveryTime(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "auth.json")
	write := func(modified time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(`{"tokens":{"account_id":"account-A"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	reads := countIdentityReads(t, "auth.json")
	readTwice := func(at time.Time) int64 {
		t.Helper()
		inspectIdentityAt(t, func() time.Time { return at })
		var memo quotaIdentityMemo
		before := reads.Load()
		for range 2 {
			if q := memo.read("codex", root); !q.IdentityKnown {
				t.Fatalf("identity = %+v", q)
			}
		}
		return reads.Load() - before
	}

	// An old modification time, but changed just now.
	write(time.Now().Add(-time.Hour))
	_, _, _, changed, versioned := identityFileVersion(t, path)
	if !versioned {
		if got := readTwice(time.Now().Add(time.Hour)); got != 2 {
			t.Fatalf("without a change time, two calls read %d times, want 2", got)
		}
		return
	}
	if got := readTwice(changed.Add(time.Second)); got != 2 {
		t.Fatalf("changed 1 s before inspection: two calls read %d times, want 2", got)
	}
	if got := readTwice(changed.Add(3 * time.Second)); got != 1 {
		t.Fatalf("changed 3 s before inspection: two calls read %d times, want 1", got)
	}
	// A modification time dated after the change, as a coarse or skewed clock
	// can leave it.
	modified := time.Now().Add(time.Minute)
	write(modified)
	_, _, _, changed, _ = identityFileVersion(t, path)
	if got := readTwice(changed.Add(3 * time.Second)); got != 2 {
		t.Fatalf("modified 1 min after its change, inspected 3 s after it: two calls read %d times, want 2", got)
	}
	if got := readTwice(modified.Add(3 * time.Second)); got != 1 {
		t.Fatalf("modified 3 s before inspection: two calls read %d times, want 1", got)
	}
}

// Admission and accounting (readQuotaIdentity) bind to what the login names
// now: they read the identity file every time.
func TestReadQuotaIdentityAlwaysReadsTheFile_GR4(t *testing.T) {
	home := quotaIdentityHome(t, "fake-claude-A", time.Now().Add(-time.Hour))
	inspectIdentityLater(t)
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
