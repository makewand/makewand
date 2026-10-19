package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClaudeQuotaPlainUUIDBindingAndScopedWindows(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".claude.json"), []byte(`{"oauthAccount":{"accountUuid":"fake-claude-A"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(root, ".credentials.json")
	if err := os.WriteFile(creds, []byte(`{"claudeAiOauth":{"accessToken":"fake-test-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-test-token" {
			t.Error("unexpected fake auth")
		}
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":20},"seven_day":{"utilization":40},"seven_day_opus":{"utilization":99},"limits":[{"kind":"weekly_scoped","percent":100,"scope":{"model":{"display_name":"test-model"}}}]}`))
	}))
	defer endpoint.Close()
	src := NewClaudeQuotaSource(creds).(*claudeQuotaSource)
	src.endpoint = endpoint.URL
	q, err := src.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !q.IdentityKnown || q.AccountRef != quotaAccountRef("claude", "fake-claude-A") || len(q.Windows) != 4 {
		t.Fatalf("Claude binding lost: %+v", q)
	}
	used, _ := q.EffectiveUsedPct()
	if used != 40 || *q.ScopedPct != 100 {
		t.Fatal("model cap leaked into shared pool")
	}
	raw, _ := json.Marshal(q)
	if strings.Contains(string(raw), "fake-test-token") || strings.Contains(string(raw), "fake-claude-A") {
		t.Fatal("secret or raw account ID leaked")
	}
	if err := os.Remove(filepath.Join(root, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	q, err = src.Read(context.Background())
	if err != nil || q.HasData || q.IdentityKnown {
		t.Fatal("missing UUID should be unknown without endpoint use")
	}
}

type staticDirectoryLoginSource struct{ root string }

func (s *staticDirectoryLoginSource) Provider() string { return "gemini" }
func (s *staticDirectoryLoginSource) quotaDirectoryIdentity() ProviderQuota {
	return unknownQuotaIdentity("gemini", s.root)
}
func (s *staticDirectoryLoginSource) Read(context.Context) (ProviderQuota, error) {
	q := s.quotaDirectoryIdentity()
	q.HasData, q.Authed, q.SourceAt = true, true, time.Now()
	return q, nil
}

func TestUnknownLoginOnlyCacheIsDirectoryBound(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	cache := filepath.Join(t.TempDir(), "snapshot.json")
	src := &staticDirectoryLoginSource{root: first}
	s := NewQuotaSnapshotter(time.Hour, src).WithDiskCache(cache, time.Hour)
	s.Refresh(context.Background())
	q, _ := s.Snapshot().Get("gemini")
	if !q.Authed || q.IdentityKnown || q.PoolRef != "" {
		t.Fatal("login observation asserted a known pool")
	}
	src.root = second
	q, _ = s.Snapshot().Get("gemini")
	if q.HasData || q.CredentialRoot != second {
		t.Fatal("unknown directory inherited another directory's state")
	}
	s.Refresh(context.Background())
	cached, _, _ := loadQuotaCache(cache, 0)
	if len(cached) != 2 {
		t.Fatal("unknown directories still share a provider-only cache key")
	}
}

func writeQuotaTestAuth(t *testing.T, root, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"tokens": map[string]string{"account_id": id}})
	if err := os.WriteFile(filepath.Join(root, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func quotaTestEvent(id string, used float64, minutes int, at time.Time, model string) string {
	raw, _ := json.Marshal(map[string]any{"account_id": id, "timestamp": at.Format(time.RFC3339Nano),
		"payload": map[string]any{"rate_limits": map[string]any{"model": model, "primary": map[string]any{
			"used_percent": used, "window_minutes": minutes, "resets_at": time.Now().Add(time.Hour).Unix()}}}})
	return string(raw) + "\n"
}

func writeQuotaTestLog(t *testing.T, root, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "sessions", "events.jsonl"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexQuotaSelectedAccountAndWindows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "selected")
	t.Setenv("CODEX_HOME", root)
	writeQuotaTestAuth(t, root, "A")
	writeQuotaTestAuth(t, filepath.Join(home, ".codex"), "B")
	writeQuotaTestAuth(t, filepath.Join(home, ".codex-2"), "C")
	writeQuotaTestLog(t, filepath.Join(home, ".codex"), quotaTestEvent("B", 99, 10080, time.Now(), ""))
	at := time.Now().Add(-time.Minute)
	writeQuotaTestLog(t, root, quotaTestEvent("A", 20, 300, at, "")+
		quotaTestEvent("B", 99, 300, time.Now(), "")+
		quotaTestEvent("A", 40, 10080, at, "")+
		quotaTestEvent("A", 55, 43200, at, "")+
		quotaTestEvent("A", 100, 10080, at, "model-only")+
		quotaTestEvent("A", 90, 300, at.Add(-time.Hour), ""))
	q, err := NewCodexQuotaSource("").Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	used, known := q.EffectiveUsedPct()
	if !q.HasData || !known || used != 55 || *q.FiveHourPct != 20 || *q.WeeklyPct != 40 || *q.ThirtyDayPct != 55 || *q.ScopedPct != 100 || len(q.Windows) != 4 {
		t.Fatalf("wrong selected account/windows: %+v", q)
	}
	if q.CredentialRoot != root || q.PoolRef == "" {
		t.Fatalf("missing identity: %+v", q)
	}
}

func TestQuotaAccountCacheSharingSwitchAndSeal(t *testing.T) {
	home := t.TempDir()
	first, second := filepath.Join(home, "one"), filepath.Join(home, "two")
	writeQuotaTestAuth(t, first, "A")
	writeQuotaTestAuth(t, second, "A")
	writeQuotaTestLog(t, first, quotaTestEvent("A", 73, 10080, time.Now(), ""))
	cache := filepath.Join(home, "snapshot.json")
	newSnap := func(root string) *QuotaSnapshotter {
		return NewQuotaSnapshotter(time.Hour, NewCodexQuotaSource(filepath.Join(root, "sessions"))).WithDiskCache(cache, time.Hour)
	}
	one := newSnap(first)
	one.Refresh(context.Background())
	two := newSnap(second)
	two.LoadCache()
	q, _ := two.Snapshot().Get("codex")
	if !q.HasData || *q.WeeklyPct != 73 || q.CredentialRoot != second {
		t.Fatalf("same account should share: %+v", q)
	}
	two.MarkExhausted("codex", time.Now().Add(time.Hour))
	writeQuotaTestAuth(t, second, "B")
	q, _ = two.Snapshot().Get("codex")
	if q.HasData {
		t.Fatal("changed login inherited A quota")
	}
	if _, sealed := two.Sealed("codex"); sealed {
		t.Fatal("changed login inherited A seal")
	}
	writeQuotaTestLog(t, second, quotaTestEvent("B", 12, 10080, time.Now(), ""))
	two.Refresh(context.Background())
	q, _ = two.Snapshot().Get("codex")
	if !q.HasData || *q.WeeklyPct != 12 {
		t.Fatal("fresh B evidence missing")
	}
	one.LoadCache()
	q, _ = one.Snapshot().Get("codex")
	if !q.HasData || *q.WeeklyPct != 73 {
		t.Fatal("interleaved B refresh erased A")
	}
	cached, _, _ := loadQuotaCache(cache, 0)
	if len(cached) != 2 {
		t.Fatalf("cache is still provider keyed: %d", len(cached))
	}
}

func TestQuotaUnknownLegacyAndSymlinkIsolation(t *testing.T) {
	root := t.TempDir()
	unknown := readQuotaIdentity("codex", root)
	other := readQuotaIdentity("codex", t.TempDir())
	if unknown.IdentityKnown || unknown.PoolRef != "" || other.AccountRef == unknown.AccountRef {
		t.Fatal("unknown directories became pools")
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skip(err)
	}
	if readQuotaIdentity("codex", alias).AccountRef != unknown.AccountRef {
		t.Fatal("symlink identity differs")
	}
	writeQuotaTestAuth(t, root, "A")
	writeQuotaTestLog(t, root, `{"timestamp":"`+time.Now().Format(time.RFC3339)+`","payload":{"rate_limits":{"primary":{"used_percent":1,"window_minutes":300}}}}`)
	s := NewQuotaSnapshotter(time.Hour, NewCodexQuotaSource(filepath.Join(alias, "sessions")))
	q, _ := s.Refresh(context.Background()).Get("codex")
	if q.HasData {
		t.Fatal("unbound session authorized current login")
	}
	cache := filepath.Join(t.TempDir(), "snapshot.json")
	storeQuotaCache(cache, map[string]ProviderQuota{"codex": {Provider: "codex", HasData: true, SourceAt: time.Now(), WeeklyPct: fptr(1)}}, time.Now())
	s.WithDiskCache(cache, time.Hour).LoadCache()
	q, _ = s.Snapshot().Get("codex")
	if q.HasData {
		t.Fatal("unbound default snapshot accepted")
	}
}

func TestQuotaStaleCacheAndWindowExpiry(t *testing.T) {
	root := t.TempDir()
	writeQuotaTestAuth(t, root, "A")
	q := readQuotaIdentity("codex", root)
	q.HasData, q.SourceAt = true, time.Now()
	q.Windows = []QuotaWindow{
		{PoolRef: q.PoolRef, WindowMinutes: 300, UsedPercent: 99, SourceAt: time.Now(), ResetAt: time.Now().Add(-time.Second)},
		{PoolRef: q.PoolRef, WindowMinutes: 10080, UsedPercent: 44, SourceAt: time.Now(), ResetAt: time.Now().Add(time.Hour)},
		{PoolRef: q.PoolRef, WindowMinutes: 43200, UsedPercent: 98, SourceAt: time.Now().Add(-time.Hour)},
	}
	cache := filepath.Join(t.TempDir(), "snapshot.json")
	storeQuotaCache(cache, map[string]ProviderQuota{"codex": q}, time.Now())
	s := NewQuotaSnapshotter(time.Hour, NewCodexQuotaSource(filepath.Join(root, "sessions"))).WithDiskCache(cache, time.Hour)
	s.LoadCache()
	got, _ := s.Snapshot().Get("codex")
	if got.FiveHourPct != nil || got.ThirtyDayPct != nil || *got.WeeklyPct != 44 {
		t.Fatalf("expired windows survived: %+v", got)
	}
	q.SourceAt = time.Now().Add(-time.Hour)
	storeQuotaCache(cache, map[string]ProviderQuota{"codex": q}, time.Now()) // newer timestamps must not be replaced
	raw, _ := os.ReadFile(cache)
	if !strings.Contains(string(raw), q.AccountRef) {
		t.Fatal("account not persisted")
	}
	// Age both the source and its windows in a static disk fixture.
	for i := range q.Windows {
		q.Windows[i].SourceAt = q.SourceAt
	}
	b, _ := json.Marshal(quotaDiskFormat{Version: 2, TakenAt: time.Now(), Providers: []ProviderQuota{q}})
	if err := os.WriteFile(cache, b, 0o600); err != nil {
		t.Fatal(err)
	}
	s.LoadCache()
	got, _ = s.Snapshot().Get("codex")
	if got.HasData {
		t.Fatal("recent cache write renewed stale observation")
	}
}

func TestQuotaCacheConcurrentAccounts(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "snapshot.json")
	var wg sync.WaitGroup
	for _, id := range []string{"A", "B", "C"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			q := ProviderQuota{Provider: "codex", AccountRef: quotaAccountRef("codex", id), IdentityKnown: true, SourceAt: time.Now(), HasData: true}
			storeQuotaCache(cache, map[string]ProviderQuota{"codex": q}, time.Now())
		}(id)
	}
	wg.Wait()
	cached, _, ok := loadQuotaCache(cache, time.Hour)
	if !ok || len(cached) != 3 {
		t.Fatalf("concurrent accounts lost: %+v", cached)
	}
}

func TestSameAccountRefreshPreservesOtherDirectoryShortWindow(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeQuotaTestAuth(t, first, "A")
	writeQuotaTestAuth(t, second, "A")
	writeQuotaTestLog(t, first, quotaTestEvent("A", 98, 300, time.Now().Add(-time.Minute), ""))
	writeQuotaTestLog(t, second, quotaTestEvent("A", 20, 10080, time.Now(), ""))
	cache := filepath.Join(t.TempDir(), "snapshot.json")
	NewQuotaSnapshotter(time.Hour, NewCodexQuotaSource(filepath.Join(first, "sessions"))).WithDiskCache(cache, time.Nanosecond).Refresh(context.Background())
	s := NewQuotaSnapshotter(time.Hour, NewCodexQuotaSource(filepath.Join(second, "sessions"))).WithDiskCache(cache, time.Nanosecond)
	q, _ := s.Refresh(context.Background()).Get("codex")
	if q.FiveHourPct == nil || *q.FiveHourPct != 98 || q.WeeklyPct == nil || *q.WeeklyPct != 20 {
		t.Fatalf("fresh short window erased by other directory: %+v", q)
	}
	// Another process loading only the cache must see the same shared constraints.
	s.LoadCache()
	q, _ = s.Snapshot().Get("codex")
	if used, _ := q.EffectiveUsedPct(); used != 98 {
		t.Fatal("cache lost shared short-window cap")
	}
}

func TestSelectedCodexHomeChangeCannotReuseCurrentSnapshot(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeQuotaTestAuth(t, first, "A")
	writeQuotaTestAuth(t, second, "B")
	writeQuotaTestLog(t, first, quotaTestEvent("A", 10, 300, time.Now(), ""))
	writeQuotaTestLog(t, second, quotaTestEvent("B", 90, 300, time.Now(), ""))
	t.Setenv("CODEX_HOME", first)
	s := NewQuotaSnapshotter(time.Hour, NewCodexQuotaSource(""))
	s.Refresh(context.Background())
	t.Setenv("CODEX_HOME", second)
	q, _ := s.Snapshot().Get("codex")
	if q.HasData || q.CredentialRoot != second {
		t.Fatal("changed selected home inherited another account")
	}
	q, _ = s.Refresh(context.Background()).Get("codex")
	if !q.HasData || q.FiveHourPct == nil || *q.FiveHourPct != 90 {
		t.Fatal("new selected home was not observed")
	}
}

func TestGoQuotaSnapshotPythonReader(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	home := t.TempDir()
	root := filepath.Join(home, ".codex")
	writeQuotaTestAuth(t, root, "interop-account")
	writeQuotaTestLog(t, root, quotaTestEvent("interop-account", 25, 300, time.Now(), "")+
		quotaTestEvent("interop-account", 50, 10080, time.Now(), "")+
		quotaTestEvent("interop-account", 70, 43200, time.Now(), "")+
		quotaTestEvent("interop-account", 100, 10080, time.Now(), "model-only"))
	cache := filepath.Join(home, ".cache", "makewand", "quota-snapshot.json")
	s := NewQuotaSnapshotter(time.Hour, NewCodexQuotaSource(filepath.Join(root, "sessions"))).WithDiskCache(cache, time.Hour)
	s.Refresh(context.Background())
	// Remove logs so Python has to consume the Go wire, rather than reparse logs.
	if err := os.RemoveAll(filepath.Join(root, "sessions")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-I", "-c", `import sys, os, json
sys.path.insert(0, os.getcwd())
from makewand.health import _get_official_subscription_quota
q = _get_official_subscription_quota("codex")
assert q is not None, "Go quota snapshot was not found in the Python home directory"
assert q["percentage"] == 30, q
assert q["identity_known"] and q["pool_ref"], q
assert {w["window_minutes"] for w in q["windows"]} == {300, 10080, 43200}, q
assert any(w["model"] == "model-only" for w in q["windows"]), q
print(json.dumps(q))`)
	cmd.Dir = ".."
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !strings.EqualFold(key, "HOME") && !strings.EqualFold(key, "USERPROFILE") && !strings.EqualFold(key, "CODEX_HOME") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+home, "USERPROFILE="+home, "CODEX_HOME="+root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python wire reader: %v %s", err, out)
	}
	if strings.Contains(string(out), "interop-account") {
		t.Fatal("raw account identifier leaked")
	}
}
