package router

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func fptr(v float64) *float64 { return &v }

func TestEffectiveUsedPctExcludesScoped(t *testing.T) {
	// Scoped at 100 must NOT dominate; account-wide weekly (63) binds.
	q := ProviderQuota{FiveHourPct: fptr(17), WeeklyPct: fptr(63), ScopedPct: fptr(100)}
	used, known := q.EffectiveUsedPct()
	if !known || used != 63 {
		t.Fatalf("want 63/known, got %.0f/%v", used, known)
	}
	// 5h higher than weekly binds.
	q2 := ProviderQuota{FiveHourPct: fptr(95), WeeklyPct: fptr(40)}
	if used, _ := q2.EffectiveUsedPct(); used != 95 {
		t.Fatalf("want 95, got %.0f", used)
	}
	// No account-wide dimension → unknown.
	q3 := ProviderQuota{ScopedPct: fptr(100)}
	if _, known := q3.EffectiveUsedPct(); known {
		t.Fatalf("scoped-only should be unknown")
	}
}

func TestQuotaBandThresholdsAndHysteresis(t *testing.T) {
	pol := DefaultQuotaPolicy() // warn 70, crit 90, hyst 5
	cases := []struct {
		used   float64
		sealed bool
		want   QuotaBand
	}{
		{50, false, QuotaBandOK},
		{70, false, QuotaBandWarn},
		{89, false, QuotaBandWarn},
		{90, false, QuotaBandCritical},
		// Hysteresis: once sealed (previously critical), stays critical until it
		// drops below crit-hyst = 85.
		{87, true, QuotaBandCritical},
		{84, true, QuotaBandWarn},
	}
	for _, c := range cases {
		q := ProviderQuota{WeeklyPct: fptr(c.used)}
		if got := pol.band(q, c.sealed); got != c.want {
			t.Errorf("used=%.0f sealed=%v: want %d got %d", c.used, c.sealed, c.want, got)
		}
	}
}

func TestQuotaBandDeauthedIsCritical(t *testing.T) {
	pol := DefaultQuotaPolicy()
	// agy-style: has data, not authed, no percentages → critical (unusable).
	q := ProviderQuota{HasData: true, Authed: false}
	if got := pol.band(q, false); got != QuotaBandCritical {
		t.Fatalf("de-authed should be critical, got %d", got)
	}
	// Authed with no percentages → OK.
	q2 := ProviderQuota{HasData: true, Authed: true}
	if got := pol.band(q2, false); got != QuotaBandOK {
		t.Fatalf("authed no-pct should be OK, got %d", got)
	}
}

// fakeSource is a controllable QuotaSource for snapshotter tests.
type fakeSource struct {
	name string
	mu   sync.Mutex
	q    ProviderQuota
	err  error
}

func (f *fakeSource) Provider() string { return f.name }
func (f *fakeSource) Read(ctx context.Context) (ProviderQuota, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.q, f.err
}
func (f *fakeSource) set(q ProviderQuota, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.q, f.err = q, err
}

func TestSnapshotterRefreshAndLastGood(t *testing.T) {
	src := &fakeSource{name: "claude", q: ProviderQuota{HasData: true, Authed: true, WeeklyPct: fptr(40)}}
	s := NewQuotaSnapshotter(time.Hour, src)
	s.Refresh(context.Background())

	q, ok := s.Snapshot().Get("claude")
	if !ok || q.WeeklyPct == nil || *q.WeeklyPct != 40 {
		t.Fatalf("first refresh: want 40, got %+v ok=%v", q, ok)
	}

	// Source starts failing — last-good (40) must be retained, not dropped to neutral.
	src.set(ProviderQuota{}, context.DeadlineExceeded)
	s.Refresh(context.Background())
	q, ok = s.Snapshot().Get("claude")
	if !ok || q.WeeklyPct == nil || *q.WeeklyPct != 40 {
		t.Fatalf("after failure: want retained 40, got %+v ok=%v", q, ok)
	}
}

func TestSnapshotterMarkExhausted(t *testing.T) {
	src := &fakeSource{name: "codex", q: ProviderQuota{HasData: true, Authed: true, WeeklyPct: fptr(50)}}
	s := NewQuotaSnapshotter(time.Hour, src)
	s.Refresh(context.Background())

	until := time.Now().Add(time.Hour)
	s.MarkExhausted("codex", until)
	q, _ := s.Snapshot().Get("codex")
	if used, _ := q.EffectiveUsedPct(); used != 100 {
		t.Fatalf("sealed provider should read 100%%, got %.0f", used)
	}

	// A refresh must not clear an active seal.
	s.Refresh(context.Background())
	q, _ = s.Snapshot().Get("codex")
	if used, _ := q.EffectiveUsedPct(); used != 100 {
		t.Fatalf("seal must survive refresh, got %.0f", used)
	}

	// Clearing the seal restores source data.
	s.MarkExhausted("codex", time.Time{})
	s.Refresh(context.Background())
	q, _ = s.Snapshot().Get("codex")
	if used, _ := q.EffectiveUsedPct(); used != 50 {
		t.Fatalf("after unseal should read source 50%%, got %.0f", used)
	}
}

func TestSnapshotConcurrentReads(t *testing.T) {
	src := &fakeSource{name: "claude", q: ProviderQuota{HasData: true, Authed: true, WeeklyPct: fptr(30)}}
	s := NewQuotaSnapshotter(time.Hour, src)
	s.Refresh(context.Background())

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = s.Snapshot().All()
				s.MarkExhausted("claude", time.Now().Add(time.Minute))
			}
		}()
	}
	wg.Wait()
}

func TestCodexRateLimitsParsing(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "rollout-test.jsonl")
	// Two rate_limits lines; the later one must win.
	content := `{"timestamp":"2026-07-14T08:00:00Z","payload":{"rate_limits":{"primary":{"used_percent":10.0,"window_minutes":300,"resets_at":1784585336},"secondary":{"used_percent":50.0,"window_minutes":10080},"plan_type":"pro"}}}
{"other":"line"}
{"timestamp":"2026-07-14T09:00:00Z","payload":{"rate_limits":{"primary":{"used_percent":22.0,"window_minutes":300,"resets_at":1784585336},"secondary":{"used_percent":84.0,"window_minutes":10080},"plan_type":"pro"}}}
`
	if err := os.WriteFile(fp, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	src := NewCodexQuotaSource(dir)
	q, err := src.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !q.HasData || q.FiveHourPct == nil || *q.FiveHourPct != 22 {
		t.Fatalf("5h: want 22, got %+v", q.FiveHourPct)
	}
	if q.WeeklyPct == nil || *q.WeeklyPct != 84 {
		t.Fatalf("weekly: want 84, got %+v", q.WeeklyPct)
	}
	if used, _ := q.EffectiveUsedPct(); used != 84 {
		t.Fatalf("effective: want 84, got %.0f", used)
	}
}

func TestCodexNestedRateLimitsFound(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "rollout-nested.jsonl")
	// rate_limits buried deeper in the object graph.
	content := `{"timestamp":"2026-07-14T09:00:00Z","a":{"b":{"rate_limits":{"secondary":{"used_percent":66.0,"window_minutes":10080}}}}}
`
	if err := os.WriteFile(fp, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	q, err := NewCodexQuotaSource(dir).Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if q.WeeklyPct == nil || *q.WeeklyPct != 66 {
		t.Fatalf("nested weekly: want 66, got %+v", q.WeeklyPct)
	}
}
