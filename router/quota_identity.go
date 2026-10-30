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
	"sync"
	"sync/atomic"
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

// readQuotaIdentity reads the identity of provider's login under root anew, as
// admission and accounting bind to it.
func readQuotaIdentity(provider, root string) ProviderQuota {
	var fresh *quotaIdentityMemo
	return fresh.read(provider, root)
}

// quotaIdentityMemo remembers a quota source's last identity read. Routing asks
// for a candidate's identity several times per request (Snapshot, Sealed), and
// a long-used ~/.claude.json is hundreds of KB, so the file is read again only
// once its stamp changes. Where file info carries no change time (Windows),
// the file is read every time.
type quotaIdentityMemo struct {
	mu       sync.Mutex
	stamp    quotaIdentityStamp
	identity ProviderQuota
}

// quotaIdentityStamp identifies the identity file version a read saw. Writing
// the file, restoring its modification time and replacing it with a new inode,
// whatever its number, each move its change time (ctime), which no user can
// set.
type quotaIdentityStamp struct {
	provider, root, path string
	dev, ino             uint64
	size, mtimeNanos     int64
	changeTimeNanos      int64
}

// quotaIdentityRacyWindow: a file modified or changed this shortly before it
// was inspected can change again within the same tick of a coarse filesystem
// clock (up to 2 s on some filesystems) and keep its stamp, so its read is not
// reused.
const quotaIdentityRacyWindow = 2 * time.Second

// quotaIdentityFileRead is a test hook called with the path of every identity
// file read.
var quotaIdentityFileRead atomic.Pointer[func(path string)]

// quotaIdentityClock is a test hook that replaces time.Now as the time
// identity files are inspected at.
var quotaIdentityClock atomic.Pointer[func() time.Time]

// read returns the identity of provider's login under root: from the memo
// while the identity file keeps the stamp of its last read, otherwise read
// from the file. A nil memo always reads.
func (m *quotaIdentityMemo) read(provider, root string) ProviderQuota {
	root = canonicalQuotaRoot(root)
	q := unknownQuotaIdentity(provider, root)
	path := filepath.Join(root, "auth.json")
	if provider == "claude" {
		path = filepath.Join(root, ".claude.json")
		if home, err := os.UserHomeDir(); err == nil && root == canonicalQuotaRoot(filepath.Join(home, ".claude")) {
			path = filepath.Join(home, ".claude.json")
		}
	}
	inspected := time.Now()
	if clock := quotaIdentityClock.Load(); clock != nil {
		inspected = (*clock)()
	}
	info, err := cliInspectIdentityMetadata(path)
	if err != nil {
		return q
	}
	dev, ino, changed, versioned := quotaIdentityFileVersion(info)
	stamp := quotaIdentityStamp{provider: provider, root: root, path: path, dev: dev, ino: ino, size: info.Size(), mtimeNanos: info.ModTime().UnixNano(), changeTimeNanos: changed.UnixNano()}
	settled := inspected.Add(-quotaIdentityRacyWindow)
	reusable := m != nil && versioned && info.ModTime().Before(settled) && changed.Before(settled)
	if reusable {
		m.mu.Lock()
		identity, hit := m.identity, m.stamp == stamp
		m.mu.Unlock()
		if hit {
			return identity
		}
	}
	if hook := quotaIdentityFileRead.Load(); hook != nil {
		(*hook)(path)
	}
	raw, err := cliReadRegularFile(path)
	if err != nil {
		return q
	}
	if ref := quotaAccountRef(provider, quotaAccountID(provider, raw)); ref != "" {
		q.AccountRef, q.IdentityKnown, q.PoolRef = ref, true, ref+":subscription"
	}
	if reusable {
		// The stamp predates the read: a change in between is read again
		// next time, as its stamp differs.
		m.mu.Lock()
		m.stamp, m.identity = stamp, q
		m.mu.Unlock()
	}
	return q
}

// quotaAccountID is the plain account identifier in a login's metadata: codex
// auth.json tokens.account_id, Claude .claude.json oauthAccount.accountUuid.
// A whitelist projection: the decoder skips every secret field. No token
// value is decoded, retained in the identity, logged, hashed or serialized.
func quotaAccountID(provider string, raw []byte) string {
	var data struct {
		Tokens struct {
			AccountID string `json:"account_id"`
		} `json:"tokens"`
		OAuthAccount struct {
			AccountUUID string `json:"accountUuid"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return ""
	}
	if provider == "claude" {
		return data.OAuthAccount.AccountUUID
	}
	return data.Tokens.AccountID
}

// Identity reads share the credential-copy byte cap. Unavailable metadata is
// unknown, never an accounting failure that blocks another provider.
func cliReadIdentityMetadata(path string) ([]byte, error) {
	if _, err := cliInspectIdentityMetadata(path); err != nil {
		return nil, err
	}
	return cliReadRegularFile(path)
}

// cliInspectIdentityMetadata returns the file info of the identity file at
// path when it is a bounded regular file.
func cliInspectIdentityMetadata(path string) (os.FileInfo, error) {
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
	return info, nil
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

// quotaLogin is a quota source with its current login (sourceQuotaIdentity).
type quotaLogin struct {
	source         QuotaSource
	name           string // lower-cased provider
	identity       ProviderQuota
	requireAccount bool
}

// logins reads the current login of every source once, for one Snapshot or
// refresh.
func (s *QuotaSnapshotter) logins() []quotaLogin {
	sources := s.sources
	logins := make([]quotaLogin, len(sources))
	for i, src := range sources {
		identity, requireAccount := sourceQuotaIdentity(src)
		logins[i] = quotaLogin{source: src, name: strings.ToLower(src.Provider()), identity: identity, requireAccount: requireAccount}
	}
	return logins
}

// selectCachedQuotas projects the account-keyed cache onto the selected CLI
// logins. Unbound/unknown/stale history is never used as current evidence.
func (s *QuotaSnapshotter) selectCachedQuotas(cached map[string]ProviderQuota, logins []quotaLogin) map[string]ProviderQuota {
	selected := make(map[string]ProviderQuota)
	for _, login := range logins {
		name := login.name
		if s.providerAllowed != nil && !s.providerAllowed(name) {
			continue
		}
		identity, requireAccount := login.identity, login.requireAccount
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
		if w.Scope == "unknown" {
			continue
		}
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
