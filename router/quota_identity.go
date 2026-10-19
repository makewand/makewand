package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const quotaObservationTTL = 15 * time.Minute

// quotaIdentitySource binds a source to the CLI's current login. Implementations
// read plain identifier fields only, never JWT claims or secret token values.
type quotaIdentitySource interface {
	quotaIdentity() ProviderQuota
}

// Login-only sources cannot assert an actual account or a shared quota pool.
// Their authentication observations still need directory isolation in caches.
type quotaDirectorySource interface {
	quotaDirectoryIdentity() ProviderQuota
}

func sourceQuotaIdentity(src QuotaSource) (ProviderQuota, bool) {
	if bound, ok := src.(quotaIdentitySource); ok {
		return bound.quotaIdentity(), true
	}
	if directory, ok := src.(quotaDirectorySource); ok {
		return directory.quotaDirectoryIdentity(), false
	}
	return ProviderQuota{}, false
}

func unknownQuotaIdentity(provider, root string) ProviderQuota {
	root = canonicalQuotaRoot(root)
	return ProviderQuota{Provider: provider, CredentialRoot: root, AccountRef: provider + ":unknown:" + identityHash(root)}
}

func selectedQuotaRoot(provider string) string {
	variable := "CODEX_HOME"
	if provider == "claude" {
		variable = "CLAUDE_CONFIG_DIR"
	}
	if root := os.Getenv(variable); root != "" {
		return root
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "."+provider)
}

func identityHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:32]
}

func quotaAccountRef(provider, id string) string {
	if strings.TrimSpace(id) == "" {
		return ""
	}
	return provider + ":account:" + identityHash(id)
}

func canonicalQuotaRoot(root string) string {
	if root == "~" || strings.HasPrefix(root, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, strings.TrimPrefix(root, "~"))
		}
	}
	// Resolve existing parents too, so aliases of not-yet-created directories
	// have the same unknown isolation key as Python Path.resolve().
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		absolute, _ := filepath.Abs(resolved)
		return absolute
	}
	root, _ = filepath.Abs(root)
	parent := filepath.Dir(root)
	if parent != root {
		return filepath.Join(canonicalQuotaRoot(parent), filepath.Base(root))
	}
	return root
}

func readQuotaIdentity(provider, root string) ProviderQuota {
	root = canonicalQuotaRoot(root)
	q := unknownQuotaIdentity(provider, root)
	path := filepath.Join(root, "auth.json")
	if provider == "claude" {
		path = filepath.Join(root, ".claude.json")
		if home, err := os.UserHomeDir(); err == nil && root == canonicalQuotaRoot(filepath.Join(home, ".claude")) {
			path = filepath.Join(home, ".claude.json")
		}
	}
	raw, err := cliReadIdentityMetadata(path)
	if err != nil {
		return q
	}
	// A whitelist projection: the decoder skips every secret field. No token
	// value is decoded, retained in the identity, logged, hashed or serialized.
	var data struct {
		Tokens struct {
			AccountID string `json:"account_id"`
		} `json:"tokens"`
		OAuthAccount struct {
			AccountUUID string `json:"accountUuid"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return q
	}
	id := data.Tokens.AccountID
	if provider == "claude" {
		id = data.OAuthAccount.AccountUUID
	}
	if ref := quotaAccountRef(provider, id); ref != "" {
		q.AccountRef, q.IdentityKnown, q.PoolRef = ref, true, ref+":subscription"
	}
	return q
}

// Identity reads share the credential-copy byte cap. Unavailable metadata is
// unknown, never an accounting failure that blocks another provider.
func cliReadIdentityMetadata(path string) ([]byte, error) {
	// Use the credential mask walk's local-component, rooted inspection. It
	// follows documented credential aliases without an unrestricted Stat, and
	// rejects FIFOs before the reader can block while opening them.
	_, info, err := cliInspectMaskTarget(cliCredentialMaskTarget{path})
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > cliPrivateCredMaxBytes {
		return nil, fmt.Errorf("identity metadata is not a bounded regular file")
	}
	return cliReadRegularFile(path)
}

func quotaCacheKey(q ProviderQuota) string {
	if q.AccountRef != "" {
		return strings.ToLower(q.Provider) + "|" + q.AccountRef
	}
	return strings.ToLower(q.Provider) // legacy history / sources without identity support
}

func quotaMatches(q, identity ProviderQuota) bool {
	return identity.IdentityKnown && q.IdentityKnown && q.AccountRef == identity.AccountRef && q.PoolRef == identity.PoolRef
}

// Account refreshes may observe different windows in different directories.
// Preserve each still-current window, choosing its own newest observation.
func mergeQuotaObservations(previous, next ProviderQuota) ProviderQuota {
	if !quotaMatches(previous, next) {
		return next
	}
	previous, next = currentQuotaWindows(previous), currentQuotaWindows(next)
	windows := make(map[string]QuotaWindow)
	for _, q := range []ProviderQuota{previous, next} {
		for _, w := range q.Windows {
			key := w.WindowKey
			if key == "" {
				key = w.Model + "|" + strconv.Itoa(w.WindowMinutes)
			}
			if old, exists := windows[key]; !exists || !old.SourceAt.After(w.SourceAt) {
				windows[key] = w
			}
		}
	}
	next.Windows = nil
	for _, w := range windows {
		next.Windows = append(next.Windows, w)
	}
	sort.Slice(next.Windows, func(i, j int) bool {
		a, b := next.Windows[i], next.Windows[j]
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.WindowMinutes != b.WindowMinutes {
			return a.WindowMinutes < b.WindowMinutes
		}
		return a.WindowKey < b.WindowKey
	})
	if previous.SourceAt.After(next.SourceAt) {
		next.SourceAt = previous.SourceAt
	}
	return currentQuotaWindows(next)
}

func quotaEvidenceCurrent(q ProviderQuota) bool {
	now := time.Now()
	if q.SourceAt.IsZero() || now.Sub(q.SourceAt) > quotaObservationTTL || q.SourceAt.After(now.Add(time.Minute)) {
		return false
	}
	return q.HasData
}

// selectCachedQuotas projects the account-keyed cache onto the selected CLI
// logins. Unbound/unknown/stale history is never used as current evidence.
func (s *QuotaSnapshotter) selectCachedQuotas(cached map[string]ProviderQuota) map[string]ProviderQuota {
	selected := make(map[string]ProviderQuota)
	for _, src := range s.sources {
		name := strings.ToLower(src.Provider())
		if s.providerAllowed != nil && !s.providerAllowed(name) {
			continue
		}
		identity, requireAccount := sourceQuotaIdentity(src)
		if identity.AccountRef != "" {
			q, exists := cached[quotaCacheKey(identity)]
			// In-memory snapshots are projected by provider already.
			if !exists {
				q, exists = cached[name]
			}
			if !requireAccount {
				// Authentication state is not a quota percentage or pool identity.
				if exists && q.AccountRef == identity.AccountRef && q.CredentialRoot == identity.CredentialRoot && !q.IdentityKnown && q.PoolRef == "" && len(q.Windows) == 0 && q.FiveHourPct == nil && q.WeeklyPct == nil && q.ThirtyDayPct == nil && q.ScopedPct == nil && quotaEvidenceCurrent(q) {
					selected[name] = q
				}
			} else if exists && quotaMatches(q, identity) && quotaEvidenceCurrent(q) {
				q.CredentialRoot = identity.CredentialRoot
				q = currentQuotaWindows(q)
				if q.HasData {
					selected[name] = q
				}
			}
		} else if q, ok := cached[name]; ok {
			selected[name] = q
		}
	}
	return selected
}

// Expired windows do not borrow a newer observation or another window's reset.
func currentQuotaWindows(q ProviderQuota) ProviderQuota {
	if len(q.Windows) == 0 {
		for _, dim := range []struct {
			pct     *float64
			minutes int
			reset   time.Time
		}{
			{q.FiveHourPct, 300, q.FiveHourResetAt}, {q.WeeklyPct, 10080, q.WeeklyResetAt}, {q.ThirtyDayPct, 43200, q.ThirtyDayResetAt},
		} {
			if dim.pct != nil {
				q.Windows = append(q.Windows, QuotaWindow{PoolRef: q.PoolRef, WindowMinutes: dim.minutes, UsedPercent: *dim.pct, SourceAt: q.SourceAt, ResetAt: dim.reset})
			}
		}
		if q.ScopedPct != nil {
			q.Windows = append(q.Windows, QuotaWindow{PoolRef: q.PoolRef, Model: "unknown", WindowMinutes: 10080, UsedPercent: *q.ScopedPct, SourceAt: q.SourceAt, ResetAt: q.WeeklyResetAt})
		}
	}
	windows := q.Windows
	q.Windows, q.FiveHourPct, q.WeeklyPct, q.ThirtyDayPct, q.ScopedPct = nil, nil, nil, nil, nil
	q.FiveHourResetAt, q.WeeklyResetAt, q.ThirtyDayResetAt, q.ResetAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	for _, w := range windows {
		if w.PoolRef != q.PoolRef || math.IsNaN(w.UsedPercent) || math.IsInf(w.UsedPercent, 0) || w.UsedPercent < 0 || w.UsedPercent > 100 || w.SourceAt.IsZero() || w.SourceAt.After(time.Now().Add(time.Minute)) || time.Since(w.SourceAt) > quotaObservationTTL || (!w.ResetAt.IsZero() && !w.ResetAt.After(time.Now())) {
			continue
		}
		q.Windows = append(q.Windows, w)
		pct := w.UsedPercent
		if w.Model != "" {
			if q.ScopedPct == nil || pct > *q.ScopedPct {
				q.ScopedPct = &pct
			}
			continue
		}
		switch w.WindowMinutes {
		case 300:
			q.FiveHourPct, q.FiveHourResetAt = &pct, w.ResetAt
		case 10080:
			q.WeeklyPct, q.WeeklyResetAt = &pct, w.ResetAt
		case 43200:
			q.ThirtyDayPct, q.ThirtyDayResetAt = &pct, w.ResetAt
		}
		if w.ResetAt.After(q.ResetAt) {
			q.ResetAt = w.ResetAt
		}
	}
	q.HasData = len(q.Windows) > 0
	return q
}
