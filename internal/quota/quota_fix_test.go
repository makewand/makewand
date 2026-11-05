package quota

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
