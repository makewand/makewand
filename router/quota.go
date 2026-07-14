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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

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
	// QuotaBandCritical: provider is at/above crit — hard-excluded from routing
	// unless excluding it would leave zero candidates.
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
	// ResetAt, when non-zero, is when the binding window resets. Used by the
	// 429-feedback path to seal a pool only until its quota actually refreshes.
	ResetAt time.Time
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

	refreshMu sync.Mutex // serializes refreshes; prevents overlapping fan-out
	// seals holds provider→until overrides from the 429-feedback path.
	sealMu sync.Mutex
	seals  map[string]time.Time
}

// NewDefaultQuotaSnapshotter builds a snapshotter over the standard sources
// (Claude OAuth, Codex session logs, agy login-state) using default paths.
// interval<=0 defaults to 120s.
func NewDefaultQuotaSnapshotter(interval time.Duration) *QuotaSnapshotter {
	return NewQuotaSnapshotter(interval,
		NewClaudeQuotaSource(""),
		NewCodexQuotaSource(""),
		NewAgyQuotaSource(""),
	)
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

	prev := s.Snapshot()
	next := make(map[string]ProviderQuota, len(s.sources))

	type result struct {
		q   ProviderQuota
		err error
	}
	results := make([]result, len(s.sources))
	var wg sync.WaitGroup
	for i, src := range s.sources {
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
			// failure shouldn't reopen a pool we just learned was exhausted.
			if pq, ok := prev.byProvider[name]; ok {
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

	// Apply active 429 seals: force the sealed provider to 100% until its window
	// resets, overriding whatever the source reported.
	s.applySeals(next)

	s.current.Store(&QuotaSnapshot{byProvider: next, takenAt: time.Now()})
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

	// Reflect the seal into the current snapshot immediately.
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
	if t := parseTime(resp.SevenDay.ResetsAt); !t.IsZero() {
		q.ResetAt = t
	}
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
		if rl.Primary != nil {
			p := rl.Primary.UsedPercent
			if rl.Primary.windowMinutes() <= 600 {
				q.FiveHourPct = &p
			} else {
				q.WeeklyPct = &p
			}
			if t := parseUnixOrTime(rl.Primary.ResetsAt); !t.IsZero() {
				q.ResetAt = t
			}
		}
		if rl.Secondary != nil {
			p := rl.Secondary.UsedPercent
			if rl.Secondary.windowMinutes() >= 9000 {
				q.WeeklyPct = &p
			} else if q.FiveHourPct == nil {
				q.FiveHourPct = &p
			}
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
	q.HasData = true
	q.Authed = err == nil
	q.SourceAt = time.Now()
	// agy exposes no usage percentage; leave FiveHour/Weekly nil so banding
	// falls back to the authed/not two-state.
	return q, nil
}
