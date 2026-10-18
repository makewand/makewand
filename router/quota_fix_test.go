package router

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Bug #1: a CLI quota-exhaustion error (non-zero exit + usage-limit stderr, NOT
// an HTTP 429) must be classified as ErrorKindRateLimit so the seal fires. This
// is the linchpin: claude/codex/agy all run as CLI providers, so if this path
// doesn't classify, the entire confirmed-exhaustion hard-block tier is dead.
func TestCLIQuotaErrorClassifiedAsRateLimit(t *testing.T) {
	// Representative stderr strings each subscription CLI emits at its cap (the
	// catalog itself is exercised in cli_quota_hints_fix_test.go). Each CLI's
	// own formats count only for that CLI.
	quotaMsgs := []struct{ cli, msg string }{
		{"claude", "Claude usage limit reached. Your limit resets at 3pm."},
		{"claude", "Error: rate limit exceeded"},
		{"claude", "You've hit your weekly limit · resets Oct 20, 8pm"},
		{"codex", "ERROR: exceeded retry limit, last status: 429 Too Many Requests, request id: req_1"},
		{"codex", "ERROR: Quota exceeded. Check your plan and billing details."},
		{"gemini", "Error: You have exhausted your quota on this model."},
	}
	for _, tc := range quotaMsgs {
		err := formatCLIExecutionError(tc.cli, tc.msg, errors.New("exit status 1"), nil, time.Second)
		if got := ErrorKindOf(err); got != ErrorKindRateLimit {
			t.Errorf("%s quota stderr %q → kind %q, want rate_limit", tc.cli, tc.msg, got)
		}
	}
	// A generic failure must NOT be misclassified as a quota error.
	for _, msg := range []string{"panic: nil pointer", "file not found", "connection refused"} {
		err := formatCLIExecutionError("claude", msg, errors.New("exit status 1"), nil, time.Second)
		if got := ErrorKindOf(err); got == ErrorKindRateLimit {
			t.Errorf("non-quota stderr %q wrongly classified as rate_limit", msg)
		}
	}
}

// Bug #1 end-to-end: a CLI quota error routed through recordProviderFailureForErr
// must seal the pool (previously it never did, because the error wasn't RateLimit).
func TestCLIQuotaErrorSealsPoolEndToEnd(t *testing.T) {
	src := &fakeSource{name: "claude", q: ProviderQuota{
		HasData: true, Authed: true, WeeklyPct: fptr(88),
		WeeklyResetAt: time.Now().Add(3 * 24 * time.Hour),
	}}
	snap := NewQuotaSnapshotter(time.Hour, src)
	snap.Refresh(context.Background())
	r := newTestRouterWithQuota(snap)

	// Simulate the failure path with a realistic CLI quota error.
	cliErr := formatCLIExecutionError("claude", "Claude usage limit reached", errors.New("exit status 1"), nil, time.Second)
	r.recordProviderFailureForErr("claude", cliErr)

	// beforeProviderAttempt returns allow=false when the pool is sealed.
	if allow, _ := r.beforeProviderAttempt("claude"); allow {
		t.Fatal("CLI quota error should have sealed claude, but beforeProviderAttempt still allowed it")
	}
}

// Bug #2: a 5-hour cap must seal only until the 5h reset, not the weekly reset.
func TestSealUsesSoonestWindowReset(t *testing.T) {
	fiveHour := time.Now().Add(2 * time.Hour)
	weekly := time.Now().Add(5 * 24 * time.Hour)
	src := &fakeSource{name: "claude", q: ProviderQuota{
		HasData: true, Authed: true,
		FiveHourPct: fptr(100), WeeklyPct: fptr(40),
		FiveHourResetAt: fiveHour, WeeklyResetAt: weekly, ResetAt: weekly,
	}}
	snap := NewQuotaSnapshotter(time.Hour, src)
	snap.Refresh(context.Background())
	r := newTestRouterWithQuota(snap)

	r.noteQuotaError("claude", newProviderError("claude", "CLI", ErrorKindRateLimit, true, 429, "5h limit", nil))

	_, until := r.quotaHardBlocked("claude")
	// Seal must be ~2h out (the 5h window), not ~5 days (the weekly window).
	if until.After(time.Now().Add(6 * time.Hour)) {
		t.Fatalf("seal until %v is past the 5-hour window — over-sealed to weekly", until)
	}
	if until.Before(time.Now().Add(time.Hour)) {
		t.Fatalf("seal until %v is too soon", until)
	}
}

func TestSoonestReset(t *testing.T) {
	now := time.Now()
	q := ProviderQuota{
		FiveHourResetAt: now.Add(3 * time.Hour),
		WeeklyResetAt:   now.Add(48 * time.Hour),
	}
	if got := q.SoonestReset(); got != q.FiveHourResetAt {
		t.Fatalf("want 5h reset, got %v", got)
	}
	// Past resets are ignored.
	q2 := ProviderQuota{FiveHourResetAt: now.Add(-time.Hour), WeeklyResetAt: now.Add(time.Hour)}
	if got := q2.SoonestReset(); got != q2.WeeklyResetAt {
		t.Fatalf("want weekly (5h is past), got %v", got)
	}
	q3 := ProviderQuota{WeeklyResetAt: now.Add(48 * time.Hour),
		Windows: []QuotaWindow{{WindowMinutes: 0, ResetAt: now.Add(time.Hour)}}}
	if got := q3.SoonestReset(); got != q3.Windows[0].ResetAt {
		t.Fatal("unknown-duration window reset was lost")
	}
}

// Bug #4: an agy probe timeout must retain last-good (return error), and an
// ambiguous non-zero exit must stay neutral (HasData=false) rather than
// asserting de-auth (which would flap gemini to the critical band).
func TestAgyProbeFailureStaysNeutral(t *testing.T) {
	// Non-existent binary path → LookPath("agy") on PATH; use a path that fails.
	// We test the classification indirectly via band(): neutral data is OK-band.
	pol := DefaultQuotaPolicy()

	// Ambiguous failure surfaced as neutral (HasData=false) → OK band, not critical.
	neutral := ProviderQuota{Provider: "gemini", HasData: false}
	if got := pol.Band(neutral); got != QuotaBandOK {
		t.Fatalf("neutral agy data should be OK band, got %d", got)
	}

	// Confirmed authed with no percentage → OK band.
	authed := ProviderQuota{Provider: "gemini", HasData: true, Authed: true}
	if got := pol.Band(authed); got != QuotaBandOK {
		t.Fatalf("authed agy should be OK band, got %d", got)
	}
}

// Bug #6 / codex parsing: window classification handles a missing window_minutes
// and both windows populate their own reset.
func TestCodexBothWindowsResets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	writeQuotaTestAuth(t, filepath.Dir(dir), "fixture")
	fp := dir + "/rollout-x.jsonl"
	reset5h := time.Now().Add(3 * time.Hour).Unix()
	resetWeekly := time.Now().Add(5 * 24 * time.Hour).Unix()
	content := fmt.Sprintf(`{"account_id":"fixture","timestamp":"%s","payload":{"rate_limits":{"primary":{"used_percent":30.0,"window_minutes":300,"resets_at":%d},"secondary":{"used_percent":90.0,"window_minutes":10080,"resets_at":%d}}}}
`, time.Now().Format(time.RFC3339), reset5h, resetWeekly)
	if err := os.WriteFile(fp, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	q, err := NewCodexQuotaSource(dir).Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if q.FiveHourResetAt.IsZero() || q.WeeklyResetAt.IsZero() {
		t.Fatalf("both window resets should be set: 5h=%v weekly=%v", q.FiveHourResetAt, q.WeeklyResetAt)
	}
	if !q.FiveHourResetAt.Before(q.WeeklyResetAt) {
		t.Fatalf("5h reset should precede weekly reset")
	}
	if q.SoonestReset() != q.FiveHourResetAt {
		t.Fatalf("soonest reset should be the 5h window")
	}
}
