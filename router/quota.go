// quota.go — Subscription quota awareness.
//
// makewand's circuit breaker is reactive: it trips a provider only after the
// provider itself returns errors. Quota awareness is proactive: it reads each
// subscription's remaining usage (5-hour session window + weekly cap) and steers
// routing away from a pool *before* it hits the limit and starts refusing or
// charging metered overflow prices.
//
// Data sources (ported from the `headgate` prototype), all read locally or via
// the user's own already-stored credentials — nothing new is uploaded:
//
//   - claude: Anthropic OAuth usage endpoint, using the token Claude Code stores
//     in ~/.claude/.credentials.json. Network call; refreshed in the background.
//   - codex:  the `rate_limits` snapshots Codex CLI writes into its own session
//     logs under ~/.codex/sessions/. Local file read.
//   - gemini: login-state probe via `agy models` (Antigravity CLI). agy does not
//     expose a usage percentage, so gemini quota is a two-state authed/not signal.
//
// These are undocumented, vendor-owned surfaces and may break without notice.
// Every read degrades gracefully: a source that fails leaves that provider with
// no quota data (HasData=false), which the gating logic treats as neutral — the
// provider keeps routing on its other signals (Thompson score, error rate,
// circuit breaker) exactly as before quota awareness existed.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errUntrustedRepoSkipProbe marks a quota source that was intentionally not read
// because reading it would exec a local repo-aware CLI while the active
// repository is untrusted. The refresh merge treats it like any other read
// failure: keep last-good/neutral, but crucially never launch the subprocess.
var errUntrustedRepoSkipProbe = errors.New("untrusted repo mode: skipped local CLI quota probe")

// localCLIQuotaSource is implemented by a QuotaSource whose Read execs a local
// CLI process to probe quota (e.g. `agy models`). In untrusted-repo mode the
// snapshotter skips such sources so an unsafe host agent is never launched merely
// to read usage. Sources that do not implement it are treated as safe to probe
// (claude = HTTPS OAuth endpoint, codex = local session-log file read).
type localCLIQuotaSource interface {
	execsLocalCLI() bool
}

// sourceExecsLocalCLI reports whether reading src spawns a local CLI process.
func sourceExecsLocalCLI(src QuotaSource) bool {
	e, ok := src.(localCLIQuotaSource)
	return ok && e.execsLocalCLI()
}

// QuotaBand is an orthogonal, coarse ranking key derived from a provider's
// remaining subscription headroom. It is deliberately *not* folded into the
// Thompson quality score (which would pollute the quality signal); instead it
// sorts candidates into bands, and Thompson decides order within a band.
type QuotaBand int

const (
	// QuotaBandOK: provider is below the warn threshold, or has no quota data.
	// Treated as fully preferred.
	QuotaBandOK QuotaBand = iota
	// QuotaBandWarn: provider is between warn and crit — usable but deprioritized.
	QuotaBandWarn
	// QuotaBandCritical: provider is at/above crit — sorted last in candidate
	// ranking (soft signal), but never removed. Predicted quota alone can't cause
	// a routing failure; only a confirmed-exhaustion seal hard-blocks a pool.
	QuotaBandCritical
)

// ProviderQuota is one provider's quota snapshot at a point in time. Percentages
// are *used* percentages in [0,100] (100 = pool exhausted). A nil pointer field
// means that dimension is unknown for this provider.
type ProviderQuota struct {
	Provider       string
	CredentialRoot string
	AccountRef     string
	IdentityKnown  bool
	// PoolRef is empty for unknown identities: directories are not quota pools.
	PoolRef      string
	Windows      []QuotaWindow
	FiveHourPct  *float64
	WeeklyPct    *float64
	ThirtyDayPct *float64
	// ScopedPct is the worst per-model weekly cap (e.g. Claude's Fable weekly
	// limit), which can bind before the account-wide weekly cap.
	ScopedPct *float64
	// Authed reports whether the provider's subscription is reachable. For agy,
	// this is the only signal (no percentage available).
	Authed  bool
	HasData bool
	// SourceAt is when the underlying source produced this data (not when we read
	// it); lets callers judge staleness independent of refresh cadence.
	SourceAt time.Time
	// FiveHourResetAt / WeeklyResetAt are the per-window reset times, when known.
	// The 429-feedback path seals only until the *soonest* future reset (see
	// SoonestReset): a 5-hour cap must not seal a pool for the whole weekly window.
	FiveHourResetAt  time.Time
	WeeklyResetAt    time.Time
	ThirtyDayResetAt time.Time
	// ResetAt is the latest known shared-window reset. Prefer per-window fields
	// for display and
	// SoonestReset for sealing decisions.
	ResetAt time.Time
}

// QuotaWindow separates shared pool limits from model-specific sublimits.
type QuotaWindow struct {
	PoolRef       string
	Model         string
	WindowKey     string
	WindowMinutes int
	UsedPercent   float64
	SourceAt      time.Time
	ResetAt       time.Time
}

// SoonestReset returns the earliest future window reset among the known windows,
// or the zero time if none is known/future. This is the conservative seal
// target: never hold a pool past the shortest window that could have caused the
// exhaustion.
func (q ProviderQuota) SoonestReset() time.Time {
	now := time.Now()
	var soonest time.Time
	resets := []time.Time{q.FiveHourResetAt, q.WeeklyResetAt, q.ThirtyDayResetAt, q.ResetAt}
	for _, w := range q.Windows {
		resets = append(resets, w.ResetAt)
	}
	for _, t := range resets {
		if t.After(now) && (soonest.IsZero() || t.Before(soonest)) {
			soonest = t
		}
	}
	return soonest
}

// EffectiveUsedPct returns the binding constraint for the *subscription pool*:
// the highest account-wide window percentage, including the 30-day cap. The pool
// that runs out first is the one that gates routing.
//
// ScopedPct (a per-model weekly cap, e.g. Claude's Fable-only limit) is
// deliberately excluded: it binds only when routing targets that specific model,
// but makewand's tiers map to different models (haiku/sonnet/opus), so a maxed
// scoped cap must not hard-exclude the whole account. Scoped is surfaced for
// display and soft warnings via ScopedPct directly.
//
// Returns (0, false) when neither account-wide dimension is known.
func (q ProviderQuota) EffectiveUsedPct() (float64, bool) {
	worst := 0.0
	known := false
	for _, p := range []*float64{q.FiveHourPct, q.WeeklyPct, q.ThirtyDayPct} {
		if p != nil {
			known = true
			if *p > worst {
				worst = *p
			}
		}
	}
	for _, w := range q.Windows {
		if w.Model == "" {
			known = true
			if w.UsedPercent > worst {
				worst = w.UsedPercent
			}
		}
	}
	return worst, known
}

// QuotaPolicy defines the thresholds for banding a provider. Percentages are
// used-percentages. Hysteresis prevents a provider hovering around crit from
// flapping in and out of the critical band on every refresh.
type QuotaPolicy struct {
	WarnPct    float64
	CritPct    float64
	Hysteresis float64
}

// DefaultQuotaPolicy mirrors the headgate prototype defaults.
func DefaultQuotaPolicy() QuotaPolicy {
	return QuotaPolicy{WarnPct: 70, CritPct: 90, Hysteresis: 5}
}

// band computes a provider's band. sealed reports whether this provider was
// previously in the critical band, so hysteresis can hold it there until it
// drops meaningfully below crit.
func (pol QuotaPolicy) band(q ProviderQuota, sealed bool) QuotaBand {
	used, known := q.EffectiveUsedPct()
	if !known {
		// No percentage (e.g. agy): an explicitly de-authed subscription is
		// unusable and must be excluded; otherwise treat as OK.
		if q.HasData && !q.Authed {
			return QuotaBandCritical
		}
		return QuotaBandOK
	}
	critFloor := pol.CritPct
	if sealed {
		critFloor = pol.CritPct - pol.Hysteresis
	}
	switch {
	case used >= critFloor:
		return QuotaBandCritical
	case used >= pol.WarnPct:
		return QuotaBandWarn
	default:
		return QuotaBandOK
	}
}

// Band returns a provider's band under this policy using plain thresholds (no
// hysteresis memory) — suitable for display and soft-ranking.
func (pol QuotaPolicy) Band(q ProviderQuota) QuotaBand {
	return pol.band(q, false)
}

// QuotaSnapshot is an immutable set of provider quotas. It is swapped wholesale
// by the Snapshotter; readers never mutate it, so it needs no locking.
type QuotaSnapshot struct {
	byProvider map[string]ProviderQuota
	takenAt    time.Time
}

// Get returns the quota for a provider (lower-cased match) and whether it exists.
func (s *QuotaSnapshot) Get(provider string) (ProviderQuota, bool) {
	if s == nil {
		return ProviderQuota{}, false
	}
	q, ok := s.byProvider[strings.ToLower(provider)]
	return q, ok
}

// TakenAt reports when this snapshot was assembled.
func (s *QuotaSnapshot) TakenAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.takenAt
}

// All returns the provider quotas sorted by name (stable output for display).
func (s *QuotaSnapshot) All() []ProviderQuota {
	if s == nil {
		return nil
	}
	out := make([]ProviderQuota, 0, len(s.byProvider))
	for _, q := range s.byProvider {
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// QuotaSource reads one provider's current quota. Implementations must be safe
// for concurrent use and must not block indefinitely (honor ctx).
type QuotaSource interface {
	Provider() string
	Read(ctx context.Context) (ProviderQuota, error)
}

// QuotaSnapshotter maintains the latest QuotaSnapshot in the background.
// Snapshot checks each login's local identity file so a changed login cannot
// inherit another account's quota, reading it again only when it changed; it
// never fetches usage over the network.
type QuotaSnapshotter struct {
	sources         []QuotaSource
	providerAllowed func(string) bool
	interval        time.Duration
	current         atomic.Pointer[QuotaSnapshot]

	// Optional read-through/write-through disk cache, shared across processes so
	// that repeated short-lived invocations (e.g. `makewand quota`) don't re-hit
	// the rate-limited usage endpoints within cacheTTL. Empty path disables it.
	cachePath string
	cacheTTL  time.Duration

	// started guards the background refresh loop so repeated Start calls (e.g.
	// several entrypoints defensively starting the same snapshotter) never spawn
	// more than one ticker goroutine. Once started, it stays started for the life
	// of the snapshotter; the loop ends when the Start ctx is cancelled.
	started atomic.Bool

	refreshMu sync.Mutex // serializes refreshes; prevents overlapping fan-out
	// readAt is when this snapshotter last read a source itself (refreshMu).
	readAt time.Time
	// seals holds provider→until overrides from the 429-feedback path. Never
	// persisted to disk: a seal reflects a live 429 in this process, and a fresh
	// process must not inherit one (window resets handle recovery anyway).
	sealMu sync.Mutex
	seals  map[string]time.Time

	// repoTrust records the repository trust level so a background/forced refresh
	// never execs a local CLI (e.g. `agy models`) to probe quota when the active
	// repository is untrusted. Stored as int32 for lock-free atomic access. The
	// zero value is RepoTrustTrusted, so an un-wired snapshotter probes everything
	// exactly as before.
	repoTrust atomic.Int32
}

// SetRepoTrust records the repository trust level. In untrusted mode, refresh
// skips quota sources whose Read execs a local repo-aware CLI (see
// localCLIQuotaSource). Returns the receiver for chaining. Safe to call
// concurrently with refresh.
func (s *QuotaSnapshotter) SetRepoTrust(t RepoTrust) *QuotaSnapshotter {
	if s == nil {
		return s
	}
	s.repoTrust.Store(int32(t))
	return s
}

// WithProviderFilter excludes disabled pools before any source reads or CLI
// probes. Configure it before Start. Cached entries obey the same filter.
func (s *QuotaSnapshotter) WithProviderFilter(allowed func(string) bool) *QuotaSnapshotter {
	if s == nil {
		return s
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.providerAllowed = allowed
	if allowed != nil {
		sources := make([]QuotaSource, 0, len(s.sources))
		for _, source := range s.sources {
			if allowed(source.Provider()) {
				sources = append(sources, source)
			}
		}
		s.sources = sources
		previous := s.Snapshot()
		s.current.Store(&QuotaSnapshot{byProvider: s.filterCachedProviders(previous.byProvider), takenAt: previous.takenAt})
	}
	return s
}

func (s *QuotaSnapshotter) filterCachedProviders(cached map[string]ProviderQuota) map[string]ProviderQuota {
	if s.providerAllowed == nil {
		return cached
	}
	filtered := make(map[string]ProviderQuota)
	for name, value := range cached {
		if s.providerAllowed(name) {
			filtered[name] = value
		}
	}
	return filtered
}

// repoTrustLevel returns the snapshotter's current repository trust level.
func (s *QuotaSnapshotter) repoTrustLevel() RepoTrust {
	return RepoTrust(s.repoTrust.Load())
}

// WithDiskCache enables a shared disk cache at path with the given freshness TTL.
// Returns the receiver for chaining. ttl<=0 defaults to the refresh interval.
// A refresh reads only the sources whose current login was not read within ttl
// (within quotaNoEvidenceTTL when that read found no current evidence) and
// serves the others from the cache.
func (s *QuotaSnapshotter) WithDiskCache(path string, ttl time.Duration) *QuotaSnapshotter {
	if ttl <= 0 {
		ttl = s.interval
	}
	s.cachePath = path
	s.cacheTTL = ttl
	return s
}

// NewDefaultQuotaSnapshotter builds a snapshotter over the standard sources
// (Claude OAuth, Codex session logs, agy login-state) using default paths.
// interval<=0 defaults to 120s.
func NewDefaultQuotaSnapshotter(interval time.Duration) *QuotaSnapshotter {
	s := NewQuotaSnapshotter(interval,
		NewClaudeQuotaSource(""),
		NewCodexQuotaSource(""),
		NewAgyQuotaSource(""),
	)
	if home, err := os.UserHomeDir(); err == nil {
		s.WithDiskCache(filepath.Join(home, ".cache", "makewand", "quota-snapshot.json"), 0)
	}
	return s
}

// NewQuotaSnapshotter builds a snapshotter over the given sources. interval<=0
// defaults to 120s. The snapshotter is inert until Start is called; Snapshot()
// returns neutral identities until the first refresh lands.
func NewQuotaSnapshotter(interval time.Duration, sources ...QuotaSource) *QuotaSnapshotter {
	if interval <= 0 {
		interval = 120 * time.Second
	}
	s := &QuotaSnapshotter{
		sources:  sources,
		interval: interval,
		seals:    make(map[string]time.Time),
	}
	s.current.Store(&QuotaSnapshot{byProvider: map[string]ProviderQuota{}})
	return s
}

// Snapshot returns the most recent snapshot. Never nil.
func (s *QuotaSnapshotter) Snapshot() *QuotaSnapshot {
	if s == nil {
		return &QuotaSnapshot{byProvider: map[string]ProviderQuota{}}
	}
	current := s.current.Load()
	logins := s.logins()
	selected := s.selectCachedQuotas(current.byProvider, logins)
	for _, login := range logins {
		if login.identity.AccountRef != "" {
			if _, exists := selected[login.name]; !exists {
				selected[login.name] = login.identity
			}
		}
	}
	s.applySeals(selected, logins)
	return &QuotaSnapshot{byProvider: selected, takenAt: current.takenAt}
}

// LoadCache does a synchronous, network-free load of the shared on-disk quota
// cache into the current snapshot. It lets a freshly constructed router serve
// the last known headroom on its very first request without blocking startup on
// any usage-source I/O and without leaking a background goroutine. Account
// sources accept only bound observations within 15 minutes and active windows;
// they do not inherit unbound history or stale evidence.
//
// No-op when the snapshotter is nil, has no disk cache configured, or the cache
// is missing/unreadable. Safe to call before Start.
func (s *QuotaSnapshotter) LoadCache() {
	if s == nil || s.cachePath == "" {
		return
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if cached, takenAt, ok := loadQuotaCache(s.cachePath, 0); ok {
		cached = s.selectCachedQuotas(cached, s.logins())
		s.current.Store(&QuotaSnapshot{byProvider: cached, takenAt: takenAt})
	}
}

// Start launches the background refresh loop and blocks for the first refresh so
// callers immediately see local sources (codex); with a fresh disk cache that
// refresh reads no source (WithDiskCache). It returns after the initial
// refresh; the loop stops when ctx is cancelled.
//
// Start is idempotent: repeated calls are a no-op (they neither refresh again
// nor spawn a second ticker). Callers wanting a one-off synchronous refresh use
// Refresh; callers wanting a network-free warm start use LoadCache.
func (s *QuotaSnapshotter) Start(ctx context.Context) {
	if s == nil || len(s.sources) == 0 {
		return
	}
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	s.refresh(ctx, false)
	go func() {
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refresh(ctx, true)
			}
		}
	}()
}

// Refresh forces a synchronous refresh (used by the `quota` subcommand and by
// the coalesced 429-feedback path). Overlapping calls are serialized.
func (s *QuotaSnapshotter) Refresh(ctx context.Context) *QuotaSnapshot {
	if s == nil {
		return &QuotaSnapshot{byProvider: map[string]ProviderQuota{}}
	}
	s.refresh(ctx, false)
	return s.Snapshot()
}

// refresh reads the sources; tick is set for the background loop's refresh.
func (s *QuotaSnapshotter) refresh(ctx context.Context, tick bool) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	// Read-through, per source: a login read recently enough (freshCachedRead)
	// is served from the shared disk cache, and only the missing and stale
	// sources are read. The whole cache, stale or not, also serves as the
	// cross-process last-good baseline below. A tick reads again what this
	// snapshotter read itself: stamped when its refresh ended, that read is
	// less than one interval old at the next tick and would otherwise skip
	// every other one. Reads another process made since are still served.
	var ownReadsUntil time.Time
	if tick {
		ownReadsUntil = s.readAt
	}
	logins := s.logins()
	var disk quotaDiskCache
	var diskPrev map[string]ProviderQuota
	if s.cachePath != "" {
		disk, _ = readQuotaDiskCache(s.cachePath)
		diskPrev = s.selectCachedQuotas(disk.entries, logins)
	}

	prev := &QuotaSnapshot{byProvider: s.selectCachedQuotas(s.current.Load().byProvider, logins)}
	next := make(map[string]ProviderQuota, len(s.sources))

	type result struct {
		q    ProviderQuota
		err  error
		read bool // read now, not served from the disk cache
	}
	results := make([]result, len(s.sources))
	untrusted := s.repoTrustLevel() == RepoTrustUntrusted
	readAny := false
	var wg sync.WaitGroup
	for i, src := range s.sources {
		if q, fresh := s.freshCachedRead(logins[i], disk, ownReadsUntil); fresh {
			results[i] = result{q: q}
			continue
		}
		// Untrusted repo mode: never exec a local CLI (e.g. `agy models`) just to
		// probe quota. Mark the source as skipped so the merge below keeps last-good
		// or neutral for it, without launching the subprocess.
		if untrusted && sourceExecsLocalCLI(src) {
			results[i] = result{q: ProviderQuota{Provider: src.Provider()}, err: errUntrustedRepoSkipProbe}
			continue
		}
		readAny = true
		wg.Add(1)
		go func(i int, src QuotaSource) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			q, err := src.Read(rctx)
			results[i] = result{q: q, err: err, read: true}
		}(i, src)
	}
	wg.Wait()

	takenAt := time.Now()
	if readAny {
		s.readAt = takenAt
	}
	reads := make(map[string]quotaRead)
	for i, src := range s.sources {
		name := strings.ToLower(src.Provider())
		r := results[i]
		identity, requireAccount := sourceQuotaIdentity(src)
		if requireAccount {
			if !quotaMatches(r.q, identity) || !quotaEvidenceCurrent(r.q) {
				r.q = identity
			} else {
				r.q = currentQuotaWindows(r.q)
			}
		} else if identity.AccountRef != "" && r.q.AccountRef != identity.AccountRef {
			r.q = identity
		}
		if r.read {
			reads[quotaReadKey(src, identity)] = quotaRead{At: takenAt, NoEvidence: r.err == nil && !r.q.HasData}
		}
		if r.err != nil || !r.q.HasData {
			// Keep last-good rather than dropping to neutral: a transient read
			// failure shouldn't reopen a pool we just learned was exhausted, nor
			// show "unavailable" when we have a recent good value on disk.
			if pq, ok := prev.byProvider[name]; ok && pq.HasData {
				next[name] = pq
				continue
			}
			if pq, ok := diskPrev[name]; ok && pq.HasData {
				next[name] = pq
				continue
			}
			// No prior data — record what we got (HasData=false is neutral).
			r.q.Provider = src.Provider()
			next[name] = r.q
			continue
		}
		r.q.Provider = src.Provider()
		if old, ok := diskPrev[name]; ok {
			r.q = mergeQuotaObservations(old, r.q)
		}
		next[name] = r.q
	}

	if s.cachePath != "" && !readAny {
		// Every source was served from the cache: there is nothing new to store.
		s.current.Store(&QuotaSnapshot{byProvider: next, takenAt: disk.takenAt})
		return
	}
	// Write-through: persist the raw source data (pre-seal) so other processes
	// can reuse it. Seals stay in-memory only.
	if s.cachePath != "" {
		storeQuotaCache(s.cachePath, next, reads, takenAt)
	}

	// Keep raw observations immutable; Snapshot overlays account-bound seals.
	s.current.Store(&QuotaSnapshot{byProvider: next, takenAt: takenAt})
}

// quotaNoEvidenceTTL is how long a source read that found no current
// account-bound evidence is reused before the source is read again. A source
// that rarely or never has any (stock Codex logs carry no account id, agy may be
// missing, a Claude login may name no account) would otherwise be read by every
// refresh; evidence it gains meanwhile shows up at most this late.
const quotaNoEvidenceTTL = 10 * time.Minute

// quotaRead records when the snapshotter last read a source for one login and
// whether that read found no current evidence. Reads are keyed like the cache
// entries (quotaReadKey).
type quotaRead struct {
	At         time.Time `json:"at"`
	NoEvidence bool      `json:"no_evidence,omitempty"`
}

// quotaReadKey is the cache key of src's current login: that of its identity,
// or of the provider for a source without identity support.
func quotaReadKey(src QuotaSource, identity ProviderQuota) string {
	if identity.AccountRef != "" {
		return quotaCacheKey(identity)
	}
	return strings.ToLower(src.Provider())
}

// freshCachedRead returns the disk cache's entry for a source's current login
// when that login was read recently enough not to read it again: within the
// cache TTL, or within quotaNoEvidenceTTL (at least the cache TTL) when that
// read found no current evidence. A read with evidence made no later than
// ownReadsUntil is read again. A read of another account or credential root is
// keyed apart and never stands in for this login.
func (s *QuotaSnapshotter) freshCachedRead(login quotaLogin, disk quotaDiskCache, ownReadsUntil time.Time) (ProviderQuota, bool) {
	identity := login.identity
	key := quotaReadKey(login.source, identity)
	read, ok := disk.reads[key]
	if !ok || !read.NoEvidence && !read.At.After(ownReadsUntil) {
		return ProviderQuota{}, false
	}
	ttl := s.cacheTTL
	if read.NoEvidence {
		ttl = max(ttl, quotaNoEvidenceTTL)
	}
	if age := time.Since(read.At); age > ttl || age < -time.Minute {
		return ProviderQuota{}, false
	}
	q, ok := disk.entries[key]
	if !ok {
		q = identity
	}
	return q, true
}

// quotaDiskFormat is the on-disk cache schema.
type quotaDiskFormat struct {
	Version   int             `json:"version"`
	TakenAt   time.Time       `json:"taken_at"`
	Providers []ProviderQuota `json:"providers"`
	// Reads (freshCachedRead) are missing from a cache that a binary from
	// before them wrote, and that binary drops them when it rewrites the cache:
	// it keeps only the fields it decodes, under the same version. A login
	// without a read is read again at the next refresh, as every source was
	// before reads, so such a binary costs reads but never serves a stale or
	// foreign entry (TestQuotaCacheWithoutReadsIsReadThroughAndRewritten).
	Reads map[string]quotaRead `json:"reads,omitempty"`
}

// quotaDiskCache is a decoded disk cache: its entries and source reads by
// cache key, and when it was last written.
type quotaDiskCache struct {
	entries map[string]ProviderQuota
	reads   map[string]quotaRead
	takenAt time.Time
}

func readQuotaDiskCache(path string) (quotaDiskCache, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return quotaDiskCache{}, false
	}
	var d quotaDiskFormat
	if err := json.Unmarshal(raw, &d); err != nil || (d.Version != 1 && d.Version != 2) {
		return quotaDiskCache{}, false
	}
	c := quotaDiskCache{entries: make(map[string]ProviderQuota, len(d.Providers)), reads: d.Reads, takenAt: d.TakenAt}
	for _, q := range d.Providers {
		if d.Version == 1 {
			// Legacy snapshots remain history, regardless of extra binding fields.
			q.IdentityKnown, q.PoolRef = false, ""
		}
		key := quotaCacheKey(q)
		if old, exists := c.entries[key]; !exists || q.SourceAt.After(old.SourceAt) {
			c.entries[key] = q
		}
	}
	return c, true
}

// loadQuotaCache returns the cached provider map and its timestamp when the cache
// file exists and is younger than ttl.
func loadQuotaCache(path string, ttl time.Duration) (map[string]ProviderQuota, time.Time, bool) {
	c, ok := readQuotaDiskCache(path)
	if !ok || (ttl > 0 && time.Since(c.takenAt) > ttl) {
		return nil, time.Time{}, false
	}
	return c.entries, c.takenAt, true
}

// storeQuotaCache atomically writes the provider map, with the source reads
// behind it, to path. Best-effort: errors (e.g. unwritable cache dir) are
// ignored — caching is an optimization.
func storeQuotaCache(path string, m map[string]ProviderQuota, reads map[string]quotaRead, takenAt time.Time) {
	if os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return
	}
	locked := false
	defer func() {
		var unlockErr error
		if locked {
			unlockErr = unlockUserHandle(lock)
		}
		// Closing also releases the OS lock if explicit unlocking failed.
		// Cache failures stay nonfatal, but a failed release must be visible.
		if err := errors.Join(unlockErr, lock.Close()); err != nil {
			log.Printf("makewand: release quota cache lock: %v", err)
		}
	}()
	if lockUserHandle(lock, true) != nil {
		return
	}
	locked = true
	// Preserve other accounts (and legacy history), instead of provider-wide
	// overwrite. Only a newer observation can replace the same account's data,
	// and only a newer read the record of a login's last read.
	cached, _ := readQuotaDiskCache(path)
	previous := cached.entries
	if previous == nil {
		previous = make(map[string]ProviderQuota)
	}
	for _, q := range m {
		key := quotaCacheKey(q)
		if old, exists := previous[key]; exists && quotaMatches(old, q) {
			previous[key] = mergeQuotaObservations(old, q)
		} else if !exists || !old.SourceAt.After(q.SourceAt) {
			previous[key] = q
		}
	}
	merged := make(map[string]quotaRead, len(cached.reads)+len(reads))
	for key, read := range cached.reads {
		merged[key] = read
	}
	for key, read := range reads {
		if old, exists := merged[key]; !exists || read.At.After(old.At) {
			merged[key] = read
		}
	}
	providers := make([]ProviderQuota, 0, len(previous))
	for _, q := range previous {
		providers = append(providers, q)
	}
	sort.Slice(providers, func(i, j int) bool { return quotaCacheKey(providers[i]) < quotaCacheKey(providers[j]) })
	b, err := json.MarshalIndent(quotaDiskFormat{Version: 2, TakenAt: takenAt, Providers: providers, Reads: merged}, "", "  ")
	if err != nil {
		return
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(path), ".quota-*.tmp")
	if err != nil {
		return
	}
	tmp := tmpFile.Name()
	defer os.Remove(tmp)
	_, writeErr := tmpFile.Write(b)
	closeErr := tmpFile.Close()
	if writeErr != nil || closeErr != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// MarkExhausted seals a provider as fully used until `until` (typically the
// window reset time from a 429 response). This is the reactive feedback path:
// when a provider returns a confirmed quota error mid-window, we stop routing to
// it immediately instead of waiting for the next background refresh. A zero or
// past `until` clears any existing seal.
func (s *QuotaSnapshotter) MarkExhausted(provider string, until time.Time) {
	if s == nil {
		return
	}
	name := strings.ToLower(strings.TrimSpace(provider))
	if name == "" {
		return
	}
	key := s.sealKey(name)
	s.sealMu.Lock()
	if until.IsZero() || !until.After(time.Now()) {
		delete(s.seals, key)
	} else {
		s.seals[key] = until
	}
	s.sealMu.Unlock()

	// Snapshot overlays seals at read time, without mutating raw observations or
	// allowing a transient source failure to persist a seal into the disk cache.
}

// Sealed reports whether a provider currently has an active 429-feedback seal,
// and until when. Expired seals are treated as absent.
func (s *QuotaSnapshotter) Sealed(provider string) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	name := strings.ToLower(strings.TrimSpace(provider))
	key := s.sealKey(name)
	s.sealMu.Lock()
	defer s.sealMu.Unlock()
	until, ok := s.seals[key]
	if !ok || !until.After(time.Now()) {
		return time.Time{}, false
	}
	return until, true
}

func (s *QuotaSnapshotter) applySeals(m map[string]ProviderQuota, logins []quotaLogin) {
	keys := make(map[string]string)
	for _, login := range logins {
		keys[sealKeyAmong(login.name, logins)] = login.name
	}
	s.sealMu.Lock()
	defer s.sealMu.Unlock()
	now := time.Now()
	for key, until := range s.seals {
		if !until.After(now) {
			delete(s.seals, key)
			continue
		}
		name, active := keys[key]
		if !active {
			continue
		}
		full := 100.0
		q := m[name]
		if strings.Contains(key, "|") && quotaCacheKey(q) != key {
			continue
		}
		q.Provider = name
		q.WeeklyPct = &full
		q.HasData = true
		q.ResetAt = until
		q.WeeklyResetAt = until
		if q.AccountRef != "" {
			q.Windows = append(q.Windows, QuotaWindow{PoolRef: q.PoolRef, WindowMinutes: 0, UsedPercent: full, SourceAt: now, ResetAt: until})
		}
		m[name] = q
	}
}

// sealKey is the key a seal of provider name is held under: the cache key of
// its source's current login, or the name for a source without identity
// support.
func (s *QuotaSnapshotter) sealKey(name string) string {
	for _, src := range s.sources {
		if strings.EqualFold(src.Provider(), name) {
			if identity, _ := sourceQuotaIdentity(src); identity.AccountRef != "" {
				return quotaCacheKey(identity)
			}
		}
	}
	return name
}

// sealKeyAmong is sealKey over logins already read.
func sealKeyAmong(name string, logins []quotaLogin) string {
	for _, login := range logins {
		if strings.EqualFold(login.name, name) && login.identity.AccountRef != "" {
			return quotaCacheKey(login.identity)
		}
	}
	return name
}

// --- Source: Claude (OAuth usage endpoint) ---

type claudeQuotaSource struct {
	credsPath    string
	endpoint     string
	selectedHome bool
	identity     quotaIdentityMemo
}

// NewClaudeQuotaSource reads Claude Pro/Max usage. credsPath empty →
// ~/.claude/.credentials.json.
func NewClaudeQuotaSource(credsPath string) QuotaSource {
	selectedHome := credsPath == ""
	if credsPath == "" {
		root := os.Getenv("CLAUDE_CONFIG_DIR")
		if root == "" {
			home, _ := os.UserHomeDir()
			root = filepath.Join(home, ".claude")
		}
		credsPath = filepath.Join(canonicalQuotaRoot(root), ".credentials.json")
	}
	return &claudeQuotaSource{
		credsPath:    credsPath,
		selectedHome: selectedHome,
		endpoint:     "https://api.anthropic.com/api/oauth/usage",
	}
}

func (c *claudeQuotaSource) Provider() string { return "claude" }

func (c *claudeQuotaSource) quotaIdentity() ProviderQuota {
	if c.selectedHome {
		return c.identity.read("claude", selectedQuotaRoot("claude"))
	}
	return c.identity.read("claude", filepath.Dir(c.credsPath))
}

func (c *claudeQuotaSource) Read(ctx context.Context) (ProviderQuota, error) {
	q := c.quotaIdentity()
	// Without a plain account identifier, do not read tokens or call an endpoint
	// merely to turn unbound data into apparent quota evidence.
	if !q.IdentityKnown {
		return q, nil
	}

	credsPath := c.credsPath
	if c.selectedHome {
		credsPath = filepath.Join(q.CredentialRoot, ".credentials.json")
	}
	raw, err := os.ReadFile(credsPath)
	if err != nil {
		return q, err
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &creds); err != nil {
		return q, err
	}
	token := creds.ClaudeAiOauth.AccessToken
	if token == "" {
		return q, errNoToken
	}

	body, err := httpGetJSON(ctx, c.endpoint, map[string]string{
		"Authorization":  "Bearer " + token,
		"anthropic-beta": "oauth-2025-04-20",
	})
	if err != nil {
		return q, err
	}

	var resp struct {
		FiveHour struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		} `json:"seven_day"`
		SevenDayOpus struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		} `json:"seven_day_opus"`
		SevenDaySonnet struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		} `json:"seven_day_sonnet"`
		Limits []struct {
			Kind     string   `json:"kind"`
			Percent  *float64 `json:"percent"`
			ResetsAt string   `json:"resets_at"`
			Scope    *struct {
				Model *struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return q, err
	}

	q.HasData = true
	q.Authed = true
	q.SourceAt = time.Now()
	q.FiveHourPct = resp.FiveHour.Utilization
	q.WeeklyPct = resp.SevenDay.Utilization
	q.FiveHourResetAt = parseTime(resp.FiveHour.ResetsAt)
	q.WeeklyResetAt = parseTime(resp.SevenDay.ResetsAt)
	q.ResetAt = q.WeeklyResetAt // weekly is the user-facing reset shown in `quota`
	for _, w := range []struct {
		pct   *float64
		mins  int
		reset time.Time
	}{
		{q.FiveHourPct, 300, q.FiveHourResetAt}, {q.WeeklyPct, 10080, q.WeeklyResetAt},
	} {
		if w.pct != nil {
			q.Windows = append(q.Windows, QuotaWindow{PoolRef: q.PoolRef, WindowMinutes: w.mins, UsedPercent: *w.pct, SourceAt: q.SourceAt, ResetAt: w.reset})
		}
	}
	for _, scoped := range []struct {
		model string
		pct   *float64
		reset string
	}{
		{"opus", resp.SevenDayOpus.Utilization, resp.SevenDayOpus.ResetsAt},
		{"sonnet", resp.SevenDaySonnet.Utilization, resp.SevenDaySonnet.ResetsAt},
	} {
		if scoped.pct != nil {
			q.Windows = append(q.Windows, QuotaWindow{PoolRef: q.PoolRef, Model: scoped.model, WindowMinutes: 10080, UsedPercent: *scoped.pct, SourceAt: q.SourceAt, ResetAt: parseTime(scoped.reset)})
		}
	}
	// Worst per-model weekly scoped cap.
	for _, lim := range resp.Limits {
		if lim.Kind == "weekly_scoped" && lim.Percent != nil {
			// Unknown model scope must never become an account-wide cap.
			model := "unknown"
			if lim.Scope != nil && lim.Scope.Model != nil && lim.Scope.Model.DisplayName != "" {
				model = lim.Scope.Model.DisplayName
			}
			reset := parseTime(lim.ResetsAt)
			if lim.ResetsAt == "" {
				reset = q.WeeklyResetAt
			}
			q.Windows = append(q.Windows, QuotaWindow{PoolRef: q.PoolRef, Model: model, WindowMinutes: 10080, UsedPercent: *lim.Percent, SourceAt: q.SourceAt, ResetAt: reset})
			if q.ScopedPct == nil || *lim.Percent > *q.ScopedPct {
				v := *lim.Percent
				q.ScopedPct = &v
			}
		}
	}
	if !quotaMatches(q, c.quotaIdentity()) {
		return c.quotaIdentity(), nil
	}
	return currentQuotaWindows(q), nil
}

// --- Source: Codex (session-log rate_limits) ---

type codexQuotaSource struct {
	sessionsDir  string
	selectedHome bool
	identity     quotaIdentityMemo
}

// NewCodexQuotaSource follows exactly one selected CLI credential root.
// It never scans default or alternate accounts in addition to CODEX_HOME.
func NewCodexQuotaSource(sessionsDir string) QuotaSource {
	if sessionsDir != "" {
		sessionsDir = canonicalQuotaRoot(sessionsDir)
	}
	return &codexQuotaSource{sessionsDir: sessionsDir, selectedHome: sessionsDir == ""}
}

func (c *codexQuotaSource) Provider() string { return "codex" }
func (c *codexQuotaSource) quotaIdentity() ProviderQuota {
	if c.selectedHome {
		return c.identity.read("codex", selectedQuotaRoot("codex"))
	}
	return c.identity.read("codex", filepath.Dir(c.sessionsDir))
}

func (c *codexQuotaSource) Read(ctx context.Context) (ProviderQuota, error) {
	q := c.quotaIdentity()
	if !q.IdentityKnown {
		return q, nil
	}
	sessionsDir := c.sessionsDir
	if c.selectedHome {
		sessionsDir = filepath.Join(q.CredentialRoot, "sessions")
	}
	files, err := recentJSONLFromDirs([]string{canonicalQuotaRoot(sessionsDir)}, 8)
	if err != nil {
		return q, err
	}
	windows := make(map[string]QuotaWindow)
	for _, fp := range files {
		if ctx.Err() != nil {
			return q, ctx.Err()
		}
		data, info, err := readCodexSessionTail(fp)
		if err != nil {
			continue
		}
		sessionRef := ""
		for len(data) > 0 {
			if ctx.Err() != nil {
				return q, ctx.Err()
			}
			line := data
			if i := bytes.IndexByte(data, '\n'); i >= 0 {
				line, data = data[:i], data[i+1:]
			} else {
				data = nil
			}
			// Only these lines can bind a session or carry a window: skip the
			// rest of a session's transcript undecoded, as Python does.
			if !bytes.Contains(line, []byte("rate_limits")) && !bytes.Contains(line, []byte("session_meta")) {
				continue
			}
			var obj map[string]json.RawMessage
			if json.Unmarshal(line, &obj) != nil {
				continue
			}
			ref := codexEventAccountRef(obj)
			var kind string
			_ = json.Unmarshal(obj["type"], &kind)
			if kind == "session_meta" {
				sessionRef = ref
				continue
			}
			if ref == "" {
				ref = sessionRef
			}
			if ref != q.AccountRef {
				continue
			}
			rl := findRateLimits(obj)
			if rl == nil {
				continue
			}
			var ts string
			_ = json.Unmarshal(obj["timestamp"], &ts)
			at := parseTime(ts)
			// Missing timestamps are history; touching a file cannot renew an event.
			if at.IsZero() || at.After(time.Now().Add(time.Minute)) || time.Since(at) > quotaObservationTTL {
				continue
			}
			if info.ModTime().Before(at) {
				at = info.ModTime()
			}
			model := rl.Model
			if model == "" && rl.LimitID != "" && rl.LimitID != "codex" {
				model = rl.LimitID
			}
			for slot, w := range []*codexWindow{rl.Primary, rl.Secondary} {
				if w == nil || w.UsedPercent == nil || *w.UsedPercent < 0 || *w.UsedPercent > 100 {
					continue
				}
				reset := parseUnixOrTime(w.ResetsAt)
				if w.ResetsAt != nil && reset.IsZero() && w.ResetsAt != "" && w.ResetsAt != "0001-01-01T00:00:00Z" {
					continue
				}
				if !reset.IsZero() && !reset.After(time.Now()) {
					continue
				}
				minutes := w.windowMinutes()
				key := model + "|" + strconv.Itoa(minutes)
				if minutes == 0 {
					key += "|unknown_" + strconv.Itoa(slot)
				}
				candidate := QuotaWindow{PoolRef: q.PoolRef, Model: model, WindowKey: key, WindowMinutes: minutes, UsedPercent: *w.UsedPercent, SourceAt: at, ResetAt: reset}
				if previous, ok := windows[key]; !ok || !previous.SourceAt.After(at) {
					windows[key] = candidate
				}
			}
		}
	}
	for _, w := range windows {
		q.Windows = append(q.Windows, w)
		if w.SourceAt.After(q.SourceAt) {
			q.SourceAt = w.SourceAt
		}
	}
	sort.Slice(q.Windows, func(i, j int) bool {
		a, b := q.Windows[i], q.Windows[j]
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.WindowMinutes < b.WindowMinutes
	})
	q.Authed = true
	q = mergeExecutionQuota(q)
	q = currentQuotaWindows(q)
	if !quotaMatches(q, c.quotaIdentity()) {
		return c.quotaIdentity(), nil
	}
	return q, nil
}

// codexSessionTailBytes is how much of each session log the Codex source
// reads: Python's reader (health.py) reads the same tail.
const codexSessionTailBytes = 4 << 20

// readCodexSessionTail returns the complete lines in the last
// codexSessionTailBytes of the session log at path, and the log's file info.
// The line the tail starts inside is dropped, as Python drops it.
func readCodexSessionTail(path string) ([]byte, os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	// A FIFO would block the open itself; only a regular file is read.
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("session log %s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	if info, err = f.Stat(); err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("session log %s is not a regular file", path)
	}
	start := max(0, info.Size()-codexSessionTailBytes)
	data, err := io.ReadAll(io.NewSectionReader(f, start, codexSessionTailBytes))
	if err != nil {
		return nil, nil, err
	}
	if start > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	return data, info, nil
}

func codexEventAccountRef(obj map[string]json.RawMessage) string {
	var id, ref string
	if raw, ok := obj["account_id"]; ok {
		_ = json.Unmarshal(raw, &id)
		if ref := quotaAccountRef("codex", id); ref != "" {
			return ref
		}
		return "invalid"
	}
	if raw, ok := obj["account_ref"]; ok {
		_ = json.Unmarshal(raw, &ref)
		if strings.HasPrefix(ref, "codex:account:") {
			return ref
		}
		return "invalid"
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(obj["payload"], &payload) == nil {
		return codexEventAccountRef(payload)
	}
	return ""
}

type codexRateLimits struct {
	Primary   *codexWindow `json:"primary"`
	Secondary *codexWindow `json:"secondary"`
	PlanType  string       `json:"plan_type"`
	Model     string       `json:"model"`
	LimitID   string       `json:"limit_id"`
}

type codexWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	WindowMinutes      int      `json:"window_minutes"`
	LimitWindowMinutes int      `json:"limit_window_minutes"`
	ResetsAt           any      `json:"resets_at"`
}

func (w *codexWindow) windowMinutes() int {
	if w == nil {
		return 0
	}
	if w.WindowMinutes > 0 {
		return w.WindowMinutes
	}
	return w.LimitWindowMinutes
}

// findRateLimits recursively searches a decoded JSON object for a "rate_limits"
// field and unmarshals it.
func findRateLimits(obj map[string]json.RawMessage) *codexRateLimits {
	if raw, ok := obj["rate_limits"]; ok {
		var rl codexRateLimits
		if json.Unmarshal(raw, &rl) == nil && (rl.Primary != nil || rl.Secondary != nil) {
			return &rl
		}
	}
	for _, raw := range obj {
		var child map[string]json.RawMessage
		if json.Unmarshal(raw, &child) == nil {
			if rl := findRateLimits(child); rl != nil {
				return rl
			}
		}
	}
	return nil
}

// --- Source: Gemini via agy (login-state only) ---

type agyQuotaSource struct {
	binPath string
}

// NewAgyQuotaSource probes agy (Antigravity CLI) login state. binPath empty →
// look up "agy" on PATH at read time.
func NewAgyQuotaSource(binPath string) QuotaSource {
	return &agyQuotaSource{binPath: binPath}
}

func (a *agyQuotaSource) Provider() string { return "gemini" }

func (a *agyQuotaSource) quotaDirectoryIdentity() ProviderQuota {
	home, _ := os.UserHomeDir()
	return unknownQuotaIdentity("gemini", filepath.Join(home, ".gemini"))
}

// execsLocalCLI reports that reading agy quota spawns the local `agy` binary,
// so this source is skipped in untrusted-repo mode (see localCLIQuotaSource).
func (a *agyQuotaSource) execsLocalCLI() bool { return true }

func (a *agyQuotaSource) Read(ctx context.Context) (ProviderQuota, error) {
	q := a.quotaDirectoryIdentity()
	bin := a.binPath
	if bin == "" {
		p, err := exec.LookPath("agy")
		if err != nil {
			return q, nil // agy not installed — neutral
		}
		bin = p
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, "models")
	err := cmd.Run()
	if cctx.Err() != nil {
		// Probe timed out — return an error so the snapshotter retains last-good
		// instead of flipping gemini to a stale/neutral reading.
		return q, cctx.Err()
	}
	if err != nil {
		// A non-zero exit is ambiguous: it could mean signed-out, but it could
		// also be a transient agy hiccup. Stay neutral (HasData=false) rather than
		// asserting de-auth, which would wrongly deprioritize gemini. A genuinely
		// signed-out agy fails at call time and is handled by the circuit breaker
		// and the confirmed-exhaustion seal path.
		return q, nil
	}
	// agy exposes no usage percentage; report authed only. Banding treats an
	// authed-but-percentless provider as neutral (OK).
	q.HasData = true
	q.Authed = true
	q.SourceAt = time.Now()
	return q, nil
}
