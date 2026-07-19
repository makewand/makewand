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
	Provider    string
	FiveHourPct *float64
	WeeklyPct   *float64
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
	FiveHourResetAt time.Time
	WeeklyResetAt   time.Time
	// ResetAt is the window reset shown to users (weekly when known). Prefer
	// SoonestReset for sealing decisions.
	ResetAt time.Time
}

// SoonestReset returns the earliest future window reset among the known windows,
// or the zero time if none is known/future. This is the conservative seal
// target: never hold a pool past the shortest window that could have caused the
// exhaustion.
func (q ProviderQuota) SoonestReset() time.Time {
	now := time.Now()
	var soonest time.Time
	for _, t := range []time.Time{q.FiveHourResetAt, q.WeeklyResetAt, q.ResetAt} {
		if t.After(now) && (soonest.IsZero() || t.Before(soonest)) {
			soonest = t
		}
	}
	return soonest
}

// EffectiveUsedPct returns the binding constraint for the *subscription pool*:
// the higher of the account-wide 5-hour and weekly used-percentages. The pool
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
	for _, p := range []*float64{q.FiveHourPct, q.WeeklyPct} {
		if p != nil {
			known = true
			if *p > worst {
				worst = *p
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

// QuotaSnapshotter maintains the latest QuotaSnapshot, refreshing it in the
// background. Routing reads the last-known snapshot without blocking on I/O.
type QuotaSnapshotter struct {
	sources  []QuotaSource
	interval time.Duration
	current  atomic.Pointer[QuotaSnapshot]

	// Optional read-through/write-through disk cache, shared across processes so
	// that repeated short-lived invocations (e.g. `makewand quota`) don't re-hit
	// the rate-limited usage endpoints within cacheTTL. Empty path disables it.
	cachePath string
	cacheTTL  time.Duration

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
// returns an empty (neutral) snapshot until the first refresh lands.
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
	return s.current.Load()
}

// Start launches the background refresh loop and blocks for the first refresh so
// callers immediately see local sources (codex). It returns after the initial
// refresh; the loop stops when ctx is cancelled.
func (s *QuotaSnapshotter) Start(ctx context.Context) {
	if s == nil || len(s.sources) == 0 {
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
			s.applySeals(cached)
			s.current.Store(&QuotaSnapshot{byProvider: cached, takenAt: takenAt})
			return
		}
		diskPrev, _, _ = loadQuotaCache(s.cachePath, 0) // ignore TTL: last-good only
	}

	prev := s.Snapshot()
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
		next[name] = r.q
	}

	// Write-through: persist the raw source data (pre-seal) so other processes
	// can reuse it. Seals stay in-memory only.
	takenAt := time.Now()
	if s.cachePath != "" {
		storeQuotaCache(s.cachePath, next, takenAt)
	}

	// Apply active 429 seals: force the sealed provider to 100% until its window
	// resets, overriding whatever the source reported.
	s.applySeals(next)

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
	if err := json.Unmarshal(raw, &d); err != nil || d.Version != 1 {
		return nil, time.Time{}, false
	}
	if ttl > 0 && time.Since(d.TakenAt) > ttl {
		return nil, time.Time{}, false
	}
	m := make(map[string]ProviderQuota, len(d.Providers))
	for _, q := range d.Providers {
		m[strings.ToLower(q.Provider)] = q
	}
	return m, d.TakenAt, true
}

// storeQuotaCache atomically writes the provider map to path. Best-effort:
// errors (e.g. unwritable cache dir) are ignored — caching is an optimization.
func storeQuotaCache(path string, m map[string]ProviderQuota, takenAt time.Time) {
	providers := make([]ProviderQuota, 0, len(m))
	for _, q := range m {
		providers = append(providers, q)
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Provider < providers[j].Provider })
	b, err := json.MarshalIndent(quotaDiskFormat{Version: 1, TakenAt: takenAt, Providers: providers}, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
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
	s.sealMu.Lock()
	if until.IsZero() || !until.After(time.Now()) {
		delete(s.seals, name)
	} else {
		s.seals[name] = until
	}
	s.sealMu.Unlock()

	// Reflect the seal into the current snapshot immediately. Hold refreshMu so a
	// concurrent refresh() can't interleave its own read-modify-Store and clobber
	// this update (or stamp it with a stale takenAt).
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	cur := s.Snapshot()
	next := make(map[string]ProviderQuota, len(cur.byProvider))
	for k, v := range cur.byProvider {
		next[k] = v
	}
	s.applySeals(next)
	s.current.Store(&QuotaSnapshot{byProvider: next, takenAt: cur.takenAt})
}

// Sealed reports whether a provider currently has an active 429-feedback seal,
// and until when. Expired seals are treated as absent.
func (s *QuotaSnapshotter) Sealed(provider string) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	name := strings.ToLower(strings.TrimSpace(provider))
	s.sealMu.Lock()
	defer s.sealMu.Unlock()
	until, ok := s.seals[name]
	if !ok || !until.After(time.Now()) {
		return time.Time{}, false
	}
	return until, true
}

func (s *QuotaSnapshotter) applySeals(m map[string]ProviderQuota) {
	s.sealMu.Lock()
	defer s.sealMu.Unlock()
	now := time.Now()
	for name, until := range s.seals {
		if !until.After(now) {
			delete(s.seals, name)
			continue
		}
		full := 100.0
		q := m[name]
		q.Provider = name
		q.WeeklyPct = &full
		q.HasData = true
		q.ResetAt = until
		m[name] = q
	}
}

// --- Source: Claude (OAuth usage endpoint) ---

type claudeQuotaSource struct {
	credsPath string
	endpoint  string
}

// NewClaudeQuotaSource reads Claude Pro/Max usage. credsPath empty →
// ~/.claude/.credentials.json.
func NewClaudeQuotaSource(credsPath string) QuotaSource {
	if credsPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			credsPath = filepath.Join(home, ".claude", ".credentials.json")
		}
	}
	return &claudeQuotaSource{
		credsPath: credsPath,
		endpoint:  "https://api.anthropic.com/api/oauth/usage",
	}
}

func (c *claudeQuotaSource) Provider() string { return "claude" }

func (c *claudeQuotaSource) Read(ctx context.Context) (ProviderQuota, error) {
	q := ProviderQuota{Provider: "claude"}

	raw, err := os.ReadFile(c.credsPath)
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
		Limits []struct {
			Kind    string   `json:"kind"`
			Percent *float64 `json:"percent"`
			Scope   *struct {
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
	// Worst per-model weekly scoped cap.
	for _, lim := range resp.Limits {
		if lim.Kind == "weekly_scoped" && lim.Percent != nil {
			if q.ScopedPct == nil || *lim.Percent > *q.ScopedPct {
				v := *lim.Percent
				q.ScopedPct = &v
			}
		}
	}
	return q, nil
}

// --- Source: Codex (session-log rate_limits) ---

type codexQuotaSource struct {
	sessionsDir string
}

// NewCodexQuotaSource reads Codex Plus/Pro usage. sessionsDir empty →
// ~/.codex/sessions.
func NewCodexQuotaSource(sessionsDir string) QuotaSource {
	if sessionsDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			sessionsDir = filepath.Join(home, ".codex", "sessions")
		}
	}
	return &codexQuotaSource{sessionsDir: sessionsDir}
}

func (c *codexQuotaSource) Provider() string { return "codex" }

func (c *codexQuotaSource) Read(ctx context.Context) (ProviderQuota, error) {
	q := ProviderQuota{Provider: "codex"}

	files, err := recentJSONL(c.sessionsDir, 8)
	if err != nil {
		return q, err
	}
	for _, fp := range files {
		if ctx.Err() != nil {
			return q, ctx.Err()
		}
		rl, at := lastRateLimits(fp)
		if rl == nil {
			continue
		}
		q.HasData = true
		q.Authed = true
		q.SourceAt = at
		for _, w := range []*codexWindow{rl.Primary, rl.Secondary} {
			if w == nil {
				continue
			}
			p := w.UsedPercent
			reset := parseUnixOrTime(w.ResetsAt)
			// Classify by window length: <=600min is the 5-hour window, the
			// larger one is weekly. A missing window_minutes (0) is ambiguous, so
			// fall back to filling whichever percentage isn't set yet.
			mins := w.windowMinutes()
			switch {
			case mins > 0 && mins <= 600:
				q.FiveHourPct = &p
				q.FiveHourResetAt = reset
			case mins >= 9000:
				q.WeeklyPct = &p
				q.WeeklyResetAt = reset
			case q.FiveHourPct == nil:
				q.FiveHourPct = &p
				q.FiveHourResetAt = reset
			default:
				q.WeeklyPct = &p
				q.WeeklyResetAt = reset
			}
		}
		q.ResetAt = q.WeeklyResetAt
		if q.ResetAt.IsZero() {
			q.ResetAt = q.FiveHourResetAt
		}
		return q, nil
	}
	return q, nil // no snapshot found — neutral, not an error
}

type codexRateLimits struct {
	Primary   *codexWindow `json:"primary"`
	Secondary *codexWindow `json:"secondary"`
	PlanType  string       `json:"plan_type"`
}

type codexWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      any     `json:"resets_at"`
}

func (w *codexWindow) windowMinutes() int {
	if w == nil {
		return 0
	}
	return w.WindowMinutes
}

// lastRateLimits scans a codex session JSONL and returns the most recent
// rate_limits object embedded anywhere in a line, plus its timestamp.
func lastRateLimits(path string) (*codexRateLimits, time.Time) {
	var best *codexRateLimits
	var bestAt time.Time
	// Session logs are line-delimited JSON; scan line by line for robustness.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "\"rate_limits\"") {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		rl := findRateLimits(obj)
		if rl == nil {
			continue
		}
		at := time.Time{}
		if tsRaw, ok := obj["timestamp"]; ok {
			var ts string
			if json.Unmarshal(tsRaw, &ts) == nil {
				at = parseTime(ts)
			}
		}
		best = rl
		bestAt = at
	}
	return best, bestAt
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

// execsLocalCLI reports that reading agy quota spawns the local `agy` binary,
// so this source is skipped in untrusted-repo mode (see localCLIQuotaSource).
func (a *agyQuotaSource) execsLocalCLI() bool { return true }

func (a *agyQuotaSource) Read(ctx context.Context) (ProviderQuota, error) {
	q := ProviderQuota{Provider: "gemini"}
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
