package router

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// GR-3: the disk cache is read through per source. A source whose current
// login was read recently enough is served from the cache, also when another
// source has no account-bound evidence; a read that found none is reused for
// its own TTL; a cached read of another account never stands in.

// boundCountingSource is an account-bound quota source (quotaIdentitySource)
// that counts its reads. With used > 0 it reports a current weekly window;
// with used == 0 it has no evidence, like stock Codex logs.
type boundCountingSource struct {
	name, account string
	used          float64
	reads         atomic.Int64
}

func (s *boundCountingSource) Provider() string { return s.name }

func (s *boundCountingSource) quotaIdentity() ProviderQuota {
	ref := quotaAccountRef(s.name, s.account)
	return ProviderQuota{Provider: s.name, CredentialRoot: "/fixture/" + s.name, AccountRef: ref, IdentityKnown: true, PoolRef: ref + ":subscription"}
}

func (s *boundCountingSource) Read(context.Context) (ProviderQuota, error) {
	s.reads.Add(1)
	q := s.quotaIdentity()
	if s.used > 0 {
		q.HasData, q.Authed, q.SourceAt = true, true, time.Now()
		q.Windows = []QuotaWindow{{PoolRef: q.PoolRef, WindowMinutes: 10080, UsedPercent: s.used, SourceAt: q.SourceAt, ResetAt: time.Now().Add(time.Hour)}}
	}
	return q, nil
}

func quotaReads(sources ...*boundCountingSource) []int64 {
	reads := make([]int64, len(sources))
	for i, src := range sources {
		reads[i] = src.reads.Load()
	}
	return reads
}

func TestQuotaFreshCacheServesSourcesDespiteOneWithoutEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache := filepath.Join(t.TempDir(), "snapshot.json")
	claude := &boundCountingSource{name: "claude", account: "A", used: 40}
	codex := &boundCountingSource{name: "codex", account: "C"}
	NewQuotaSnapshotter(time.Hour, claude, codex).WithDiskCache(cache, time.Hour).Refresh(ctx)
	if got := quotaReads(claude, codex); got[0] != 1 || got[1] != 1 {
		t.Fatalf("first refresh reads = %v", got)
	}
	// Another process starting within the TTL reads neither source: Start's
	// first refresh and a forced refresh are both served from the cache.
	s := NewQuotaSnapshotter(time.Hour, claude, codex).WithDiskCache(cache, time.Hour)
	s.Start(ctx)
	s.Refresh(ctx)
	if got := quotaReads(claude, codex); got[0] != 1 || got[1] != 1 {
		t.Fatalf("fresh cache fanned out again: reads claude=%d codex=%d", got[0], got[1])
	}
	if q, _ := s.Snapshot().Get("claude"); !q.HasData || q.WeeklyPct == nil || *q.WeeklyPct != 40 {
		t.Fatalf("cached claude evidence not served: %+v", q)
	}
	if q, _ := s.Snapshot().Get("codex"); q.HasData || q.AccountRef != codex.quotaIdentity().AccountRef {
		t.Fatalf("codex without evidence: %+v", q)
	}
}

// ageQuotaRead moves the read recorded for key in the cache file back by age.
func ageQuotaRead(t *testing.T, cache, key string, age time.Duration) {
	t.Helper()
	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	reads, _ := file["reads"].(map[string]any)
	read, ok := reads[key].(map[string]any)
	if !ok {
		t.Fatalf("no read recorded for %s: %s", key, raw)
	}
	read["at"] = time.Now().Add(-age).Format(time.RFC3339Nano)
	if raw, err = json.Marshal(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestQuotaNoEvidenceReadHasItsOwnTTL(t *testing.T) {
	ctx := context.Background()
	cache := filepath.Join(t.TempDir(), "snapshot.json")
	claude := &boundCountingSource{name: "claude", account: "A", used: 40}
	codex := &boundCountingSource{name: "codex", account: "C"}
	// A cache TTL of 1ns makes every evidence read stale at once.
	s := NewQuotaSnapshotter(time.Hour, claude, codex).WithDiskCache(cache, time.Nanosecond)
	s.Refresh(ctx)
	time.Sleep(2 * time.Millisecond)
	s.Refresh(ctx)
	if got := quotaReads(claude, codex); got[0] != 2 || got[1] != 1 {
		t.Fatalf("reads claude=%d codex=%d, want the stale evidence re-read and the no-evidence read reused", got[0], got[1])
	}
	ageQuotaRead(t, cache, quotaCacheKey(codex.quotaIdentity()), quotaNoEvidenceTTL+time.Minute)
	s.Refresh(ctx)
	if got := quotaReads(claude, codex); got[1] != 2 {
		t.Fatalf("no-evidence read reused past its TTL: codex reads=%d", got[1])
	}
}

func TestQuotaCachedReadOfAnotherAccountIsIgnored(t *testing.T) {
	ctx := context.Background()
	for _, used := range []float64{0, 70} {
		cache := filepath.Join(t.TempDir(), "snapshot.json")
		a := &boundCountingSource{name: "claude", account: "A", used: 40}
		NewQuotaSnapshotter(time.Hour, a).WithDiskCache(cache, time.Hour).Refresh(ctx)
		// The selected login is now account B: A's fresh read is not B's.
		b := &boundCountingSource{name: "claude", account: "B", used: used}
		s := NewQuotaSnapshotter(time.Hour, b).WithDiskCache(cache, time.Hour)
		s.LoadCache()
		s.Refresh(ctx)
		if b.reads.Load() != 1 {
			t.Fatalf("used=%v: account A's fresh read stood in for account B (reads=%d)", used, b.reads.Load())
		}
		q, _ := s.Snapshot().Get("claude")
		if q.AccountRef != b.quotaIdentity().AccountRef || (used == 0 && q.HasData) || (used > 0 && (q.WeeklyPct == nil || *q.WeeklyPct != used)) {
			t.Fatalf("used=%v: account B got %+v", used, q)
		}
		// Account A's own fresh read still serves account A.
		NewQuotaSnapshotter(time.Hour, a).WithDiskCache(cache, time.Hour).Refresh(ctx)
		if a.reads.Load() != 1 {
			t.Fatalf("used=%v: account A re-read within its TTL: %d", used, a.reads.Load())
		}
	}
}
