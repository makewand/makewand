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
	"context"
	"encoding/json"
	"errors"
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
// Snapshot checks the small local identity file so a changed login cannot
// inherit another account's quota; it never fetches usage over the network.
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
	selected := s.selectCachedQuotas(current.byProvider)
	for _, src := range s.sources {
		identity, _ := sourceQuotaIdentity(src)
		if identity.AccountRef != "" {
			name := strings.ToLower(src.Provider())
			if _, exists := selected[name]; !exists {
				selected[name] = identity
			}
		}
	}
	s.applySeals(selected)
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
		cached = s.selectCachedQuotas(cached)
		s.current.Store(&QuotaSnapshot{byProvider: cached, takenAt: takenAt})
	}
}

// Start launches the background refresh loop and blocks for the first refresh so
// callers immediately see local sources (codex). It returns after the initial
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
	s.refresh(ctx)
	go func() {
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refresh(ctx)
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
	s.refresh(ctx)
	return s.Snapshot()
}

func (s *QuotaSnapshotter) refresh(ctx context.Context) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	// Read-through: adopt a fresh on-disk snapshot instead of hitting the sources.
	// A stale cache still serves as the cross-process last-good baseline below.
	var diskPrev map[string]ProviderQuota
	if s.cachePath != "" {
		if cached, takenAt, ok := loadQuotaCache(s.cachePath, s.cacheTTL); ok {
			cached = s.selectCachedQuotas(cached)
			if len(cached) == len(s.sources) {
				s.current.Store(&QuotaSnapshot{byProvider: cached, takenAt: takenAt})
				return
			}
		}
		diskPrev, _, _ = loadQuotaCache(s.cachePath, 0) // ignore TTL: last-good only
		diskPrev = s.selectCachedQuotas(diskPrev)
	}

	prev := &QuotaSnapshot{byProvider: s.selectCachedQuotas(s.current.Load().byProvider)}
	next := make(map[string]ProviderQuota, len(s.sources))

	type result struct {
		q   ProviderQuota
		err error
	}
	results := make([]result, len(s.sources))
	untrusted := s.repoTrustLevel() == RepoTrustUntrusted
	var wg sync.WaitGroup
	for i, src := range s.sources {
		// Untrusted repo mode: never exec a local CLI (e.g. `agy models`) just to
		// probe quota. Mark the source as skipped so the merge below keeps last-good
		// or neutral for it, without launching the subprocess.
		if untrusted && sourceExecsLocalCLI(src) {
			results[i] = result{q: ProviderQuota{Provider: src.Provider()}, err: errUntrustedRepoSkipProbe}
			continue
		}
		wg.Add(1)
		go func(i int, src QuotaSource) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			q, err := src.Read(rctx)
			results[i] = result{q: q, err: err}
		}(i, src)
	}
	wg.Wait()

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

	// Write-through: persist the raw source data (pre-seal) so other processes
	// can reuse it. Seals stay in-memory only.
	takenAt := time.Now()
	if s.cachePath != "" {
		storeQuotaCache(s.cachePath, next, takenAt)
	}

	// Keep raw observations immutable; Snapshot overlays account-bound seals.
	s.current.Store(&QuotaSnapshot{byProvider: next, takenAt: takenAt})
}

// quotaDiskFormat is the on-disk cache schema.
type quotaDiskFormat struct {
	Version   int             `json:"version"`
	TakenAt   time.Time       `json:"taken_at"`
	Providers []ProviderQuota `json:"providers"`
}

// loadQuotaCache returns the cached provider map and its timestamp when the cache
// file exists and is younger than ttl.
func loadQuotaCache(path string, ttl time.Duration) (map[string]ProviderQuota, time.Time, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, false
	}
	var d quotaDiskFormat
	if err := json.Unmarshal(raw, &d); err != nil || (d.Version != 1 && d.Version != 2) {
		return nil, time.Time{}, false
	}
	if ttl > 0 && time.Since(d.TakenAt) > ttl {
		return nil, time.Time{}, false
	}
	m := make(map[string]ProviderQuota, len(d.Providers))
	for _, q := range d.Providers {
		if d.Version == 1 {
			// Legacy snapshots remain history, regardless of extra binding fields.
			q.IdentityKnown, q.PoolRef = false, ""
		}
		key := quotaCacheKey(q)
		if old, exists := m[key]; !exists || q.SourceAt.After(old.SourceAt) {
			m[key] = q
		}
	}
	return m, d.TakenAt, true
}

// storeQuotaCache atomically writes the provider map to path. Best-effort:
// errors (e.g. unwritable cache dir) are ignored — caching is an optimization.
func storeQuotaCache(path string, m map[string]ProviderQuota, takenAt time.Time) {
	if os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return
	}
	defer lock.Close()
	if lockUserHandle(lock, true) != nil {
		return
	}
	defer unlockUserHandle(lock)
	// Preserve other accounts (and legacy history), instead of provider-wide
	// overwrite. Only a newer observation can replace the same account's data.
	previous, _, _ := loadQuotaCache(path, 0)
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
	providers := make([]ProviderQuota, 0, len(previous))
	for _, q := range previous {
		providers = append(providers, q)
	}
	sort.Slice(providers, func(i, j int) bool { return quotaCacheKey(providers[i]) < quotaCacheKey(providers[j]) })
	b, err := json.MarshalIndent(quotaDiskFormat{Version: 2, TakenAt: takenAt, Providers: providers}, "", "  ")
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

func (s *QuotaSnapshotter) applySeals(m map[string]ProviderQuota) {
	keys := make(map[string]string)
	for _, src := range s.sources {
		keys[s.sealKey(strings.ToLower(src.Provider()))] = strings.ToLower(src.Provider())
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

// --- Source: Claude (OAuth usage endpoint) ---

type claudeQuotaSource struct {
	credsPath    string
	endpoint     string
	selectedHome bool
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
		return readQuotaIdentity("claude", selectedQuotaRoot("claude"))
	}
	return readQuotaIdentity("claude", filepath.Dir(c.credsPath))
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
		return readQuotaIdentity("codex", selectedQuotaRoot("codex"))
	}
	return readQuotaIdentity("codex", filepath.Dir(c.sessionsDir))
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
		info, err := os.Stat(fp)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(fp)
		if err != nil {
			continue
		}
		sessionRef := ""
		for _, line := range strings.Split(string(data), "\n") {
			var obj map[string]json.RawMessage
			if json.Unmarshal([]byte(line), &obj) != nil {
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
