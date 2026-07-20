package serverauth

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/makewand/makewand/serverdb"
)

// SQLiteStore persists token rules in SQLite while keeping active grants
// resident in memory for efficient quota and budget accounting.
type SQLiteStore struct {
	path string
	db   *sql.DB

	// mutationMu serializes a whole mutation+reload (Issue/Revoke) so two
	// concurrent writers cannot each SELECT the table, build a grants snapshot,
	// and publish them out of order — which could drop a just-issued token or
	// resurrect a just-revoked one. mu still guards the in-memory grants/views for
	// concurrent readers.
	mutationMu sync.Mutex

	mu     sync.RWMutex
	grants map[string]*Grant
	views  []TokenRuleView
}

// OpenSQLiteStore opens or creates a SQLite-backed token store.
func OpenSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := serverdb.Open(path)
	if err != nil {
		return nil, err
	}
	store := &SQLiteStore{
		path: path,
		db:   db,
	}
	if err := store.ensureSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.reload(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.loadUsageCounters(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) ensureSchema() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS auth_tokens (
  id TEXT PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  description TEXT NOT NULL DEFAULT '',
  user_id TEXT NOT NULL DEFAULT '',
  organization_id TEXT NOT NULL DEFAULT '',
  project_id TEXT NOT NULL DEFAULT '',
  scopes_json TEXT NOT NULL,
  workspace_prefixes_json TEXT NOT NULL DEFAULT '[]',
  allowed_providers_json TEXT NOT NULL DEFAULT '[]',
  allowed_modes_json TEXT NOT NULL DEFAULT '[]',
  expires_at TEXT NOT NULL DEFAULT '',
  revoked INTEGER NOT NULL DEFAULT 0,
  max_requests_per_hour INTEGER NOT NULL DEFAULT 0,
  max_requests_per_day INTEGER NOT NULL DEFAULT 0,
  max_cost_usd_per_day REAL NOT NULL DEFAULT 0,
  max_cost_usd_per_month REAL NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);`)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS token_usage_counters (
  token_id TEXT PRIMARY KEY,
  quota_window_start TEXT NOT NULL DEFAULT '',
  quota_window_count INTEGER NOT NULL DEFAULT 0,
  quota_day_start TEXT NOT NULL DEFAULT '',
  quota_day_count INTEGER NOT NULL DEFAULT 0,
  cost_day_start TEXT NOT NULL DEFAULT '',
  cost_day_spent REAL NOT NULL DEFAULT 0,
  cost_month_start TEXT NOT NULL DEFAULT '',
  cost_month_spent REAL NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL DEFAULT ''
);`); err != nil {
		return err
	}
	return serverdb.EnsureColumns(s.db, "auth_tokens", map[string]string{
		"user_id":         "user_id TEXT NOT NULL DEFAULT ''",
		"organization_id": "organization_id TEXT NOT NULL DEFAULT ''",
		"project_id":      "project_id TEXT NOT NULL DEFAULT ''",
	})
}

// loadUsageCounters restores persisted per-token accounting counters into the
// in-memory grants so that hourly/daily request counts and daily/monthly spend
// survive a server restart. Grants without a persisted row keep their zeroed
// counters. Stale windows (a prior day/month) self-heal on next use.
func (s *SQLiteStore) loadUsageCounters() error {
	rows, err := s.db.Query(`
SELECT token_id, quota_window_start, quota_window_count, quota_day_start, quota_day_count,
       cost_day_start, cost_day_spent, cost_month_start, cost_month_spent
FROM token_usage_counters`)
	if err != nil {
		return err
	}
	defer rows.Close()

	byToken := make(map[string]UsageCounters)
	for rows.Next() {
		var (
			c                                         UsageCounters
			windowStart, dayStart, costDay, costMonth string
		)
		if err := rows.Scan(
			&c.TokenID,
			&windowStart, &c.QuotaWindowCount,
			&dayStart, &c.QuotaDayCount,
			&costDay, &c.CostDaySpent,
			&costMonth, &c.CostMonthSpent,
		); err != nil {
			return err
		}
		// Fail closed on a corrupt window timestamp: silently treating it as a
		// zero (new) window would reset an exhausted quota/spend counter and let a
		// token bypass its limit. An operator can fix or clear the bad row.
		for field, raw := range map[string]string{
			"quota_window_start": windowStart,
			"quota_day_start":    dayStart,
			"cost_day_start":     costDay,
			"cost_month_start":   costMonth,
		} {
			if _, err := parseUsageTime(raw); err != nil {
				return fmt.Errorf("corrupt token_usage_counters.%s for token %s: %w", field, c.TokenID, err)
			}
		}
		c.QuotaWindowStart, _ = parseUsageTime(windowStart)
		c.QuotaDayStart, _ = parseUsageTime(dayStart)
		c.CostDayStart, _ = parseUsageTime(costDay)
		c.CostMonthStart, _ = parseUsageTime(costMonth)
		byToken[c.TokenID] = c
	}
	if err := rows.Err(); err != nil {
		return err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, grant := range s.grants {
		if c, ok := byToken[grant.TokenID()]; ok {
			grant.importUsage(c)
		}
	}
	return nil
}

// PersistUsageCounters writes the current in-memory accounting counters for
// every active token to the state DB. Safe to call periodically and at
// shutdown; each token is upserted in its own statement so a single failure
// does not abort the rest.
func (s *SQLiteStore) PersistUsageCounters() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.mu.RLock()
	grants := make([]*Grant, 0, len(s.grants))
	for _, g := range s.grants {
		grants = append(grants, g)
	}
	s.mu.RUnlock()

	now := time.Now().UTC().Format(time.RFC3339)
	var errs []error
	for _, g := range grants {
		c, ok := g.exportUsage()
		if !ok {
			continue
		}
		if _, err := s.db.Exec(`
INSERT INTO token_usage_counters (
  token_id, quota_window_start, quota_window_count, quota_day_start, quota_day_count,
  cost_day_start, cost_day_spent, cost_month_start, cost_month_spent, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(token_id) DO UPDATE SET
  quota_window_start = excluded.quota_window_start,
  quota_window_count = excluded.quota_window_count,
  quota_day_start = excluded.quota_day_start,
  quota_day_count = excluded.quota_day_count,
  cost_day_start = excluded.cost_day_start,
  cost_day_spent = excluded.cost_day_spent,
  cost_month_start = excluded.cost_month_start,
  cost_month_spent = excluded.cost_month_spent,
  updated_at = excluded.updated_at`,
			c.TokenID,
			formatUsageTime(c.QuotaWindowStart), c.QuotaWindowCount,
			formatUsageTime(c.QuotaDayStart), c.QuotaDayCount,
			formatUsageTime(c.CostDayStart), c.CostDaySpent,
			formatUsageTime(c.CostMonthStart), c.CostMonthSpent,
			now,
		); err != nil {
			// Continue so one bad token does not block persisting the rest.
			errs = append(errs, fmt.Errorf("token %s: %w", c.TokenID, err))
		}
	}
	return errors.Join(errs...)
}

// parseUsageTime parses a persisted RFC3339 window timestamp. An empty value is
// a valid zero time (no window yet); a non-empty value that does not parse is an
// error so the caller can fail closed rather than silently reset a counter.
func parseUsageTime(raw string) (time.Time, error) {
	// Only an exactly-empty value is a valid "no window yet". A whitespace-only or
	// otherwise malformed value must not be trimmed into a zero time (which would
	// silently reset a counter) — it fails strict RFC3339 parsing instead.
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func formatUsageTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Close releases the underlying database handle.
func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path returns the SQLite file path.
func (s *SQLiteStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// AuthenticateRequest authenticates the incoming HTTP request.
func (s *SQLiteStore) AuthenticateRequest(req *http.Request) (*Grant, bool) {
	if req == nil {
		return nil, false
	}
	return s.AuthenticateHeader(req.Header.Get("Authorization"))
}

// AuthenticateHeader authenticates a Bearer token header value.
func (s *SQLiteStore) AuthenticateHeader(header string) (*Grant, bool) {
	token, ok := bearerTokenFromHeader(header)
	if !ok {
		return nil, false
	}
	return s.authenticateToken(token)
}

func (s *SQLiteStore) authenticateToken(token string) (*Grant, bool) {
	if s == nil {
		return nil, false
	}
	hash := hashToken(token)
	s.mu.RLock()
	grant, ok := s.grants[hash]
	s.mu.RUnlock()
	if !ok || grant == nil {
		return nil, false
	}
	if grant.revoked || grant.IsExpiredAt(time.Now()) {
		return nil, false
	}
	return grant, true
}

// TokenRules returns the current non-secret token views.
func (s *SQLiteStore) TokenRules() []TokenRuleView {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TokenRuleView, len(s.views))
	copy(out, s.views)
	return out
}

// Issue inserts a new token rule into SQLite and returns its issued secret.
func (s *SQLiteStore) Issue(rule TokenRule) (TokenRuleView, string, error) {
	if s == nil {
		return TokenRuleView{}, "", fmt.Errorf("sqlite token store is unavailable")
	}
	// Serialize the INSERT + reload against other mutations so concurrent Issue
	// calls (e.g. simultaneous logins) cannot publish a snapshot that drops a
	// just-issued token.
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if strings.TrimSpace(rule.Token) == "" {
		token, err := GenerateToken()
		if err != nil {
			return TokenRuleView{}, "", err
		}
		rule.Token = token
	}
	grant, err := newGrant(rule)
	if err != nil {
		return TokenRuleView{}, "", err
	}
	rule.ID = grant.TokenID()
	tokenValue := rule.Token

	encodedScopes, err := encodeStringSlice(rule.Scopes)
	if err != nil {
		return TokenRuleView{}, "", err
	}
	encodedPrefixes, err := encodeStringSlice(rule.WorkspacePrefixes)
	if err != nil {
		return TokenRuleView{}, "", err
	}
	encodedProviders, err := encodeStringSlice(rule.AllowedProviders)
	if err != nil {
		return TokenRuleView{}, "", err
	}
	encodedModes, err := encodeStringSlice(rule.AllowedModes)
	if err != nil {
		return TokenRuleView{}, "", err
	}
	expiresAt := ""
	if !rule.ExpiresAt.IsZero() {
		expiresAt = rule.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if _, err := s.db.Exec(`
INSERT INTO auth_tokens (
  id, token_hash, description, user_id, organization_id, project_id, scopes_json, workspace_prefixes_json,
  allowed_providers_json, allowed_modes_json, expires_at, revoked,
  max_requests_per_hour, max_requests_per_day, max_cost_usd_per_day,
  max_cost_usd_per_month, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rule.ID,
		hashToken(tokenValue),
		strings.TrimSpace(rule.Description),
		strings.TrimSpace(rule.UserID),
		strings.TrimSpace(rule.OrganizationID),
		strings.TrimSpace(rule.ProjectID),
		encodedScopes,
		encodedPrefixes,
		encodedProviders,
		encodedModes,
		expiresAt,
		boolToInt(rule.Revoked),
		rule.MaxRequestsPerHour,
		rule.MaxRequestsPerDay,
		rule.MaxCostUSDPerDay,
		rule.MaxCostUSDPerMonth,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return TokenRuleView{}, "", err
	}
	if err := s.reload(); err != nil {
		return TokenRuleView{}, "", err
	}
	for _, view := range s.TokenRules() {
		if view.ID == rule.ID {
			return view, tokenValue, nil
		}
	}
	return TokenRuleView{}, tokenValue, nil
}

// Revoke marks a token revoked and refreshes in-memory grants.
func (s *SQLiteStore) Revoke(tokenID string) error {
	if s == nil {
		return fmt.Errorf("sqlite token store is unavailable")
	}
	// Serialize against Issue/other Revoke so an interleaved reload cannot
	// resurrect a just-revoked token or drop a concurrently-issued one.
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	result, err := s.db.Exec(`UPDATE auth_tokens SET revoked = 1 WHERE id = ?`, strings.TrimSpace(tokenID))
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("token %q not found", tokenID)
	}
	return s.reload()
}

func (s *SQLiteStore) reload() error {
	rows, err := s.db.Query(`
SELECT id, token_hash, description, user_id, organization_id, project_id,
       scopes_json, workspace_prefixes_json,
       allowed_providers_json, allowed_modes_json, expires_at, revoked,
       max_requests_per_hour, max_requests_per_day, max_cost_usd_per_day,
       max_cost_usd_per_month
FROM auth_tokens`)
	if err != nil {
		return err
	}
	defer rows.Close()

	grants := make(map[string]*Grant)
	views := make([]TokenRuleView, 0, 8)
	for rows.Next() {
		var (
			rule           TokenRule
			tokenHash      string
			userID         string
			organizationID string
			projectID      string
			scopesJSON     string
			prefixesJSON   string
			providersJSON  string
			modesJSON      string
			expiresAtRaw   string
			revoked        int
		)
		if err := rows.Scan(
			&rule.ID,
			&tokenHash,
			&rule.Description,
			&userID,
			&organizationID,
			&projectID,
			&scopesJSON,
			&prefixesJSON,
			&providersJSON,
			&modesJSON,
			&expiresAtRaw,
			&revoked,
			&rule.MaxRequestsPerHour,
			&rule.MaxRequestsPerDay,
			&rule.MaxCostUSDPerDay,
			&rule.MaxCostUSDPerMonth,
		); err != nil {
			return err
		}
		rule.UserID = userID
		rule.OrganizationID = organizationID
		rule.ProjectID = projectID
		if err := decodeStringSlice(scopesJSON, &rule.Scopes); err != nil {
			return err
		}
		if err := decodeStringSlice(prefixesJSON, &rule.WorkspacePrefixes); err != nil {
			return err
		}
		if err := decodeStringSlice(providersJSON, &rule.AllowedProviders); err != nil {
			return err
		}
		if err := decodeStringSlice(modesJSON, &rule.AllowedModes); err != nil {
			return err
		}
		rule.Revoked = revoked == 1
		if strings.TrimSpace(expiresAtRaw) != "" {
			value, err := time.Parse(time.RFC3339, expiresAtRaw)
			if err != nil {
				return err
			}
			rule.ExpiresAt = value.UTC()
		}
		grant, err := newGrant(rule)
		if err != nil {
			return err
		}
		grants[tokenHash] = grant
		views = append(views, SanitizedRules([]TokenRule{rule})...)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })

	s.mu.Lock()
	carryOverGrantUsage(grants, s.grants)
	s.grants = grants
	s.views = views
	s.mu.Unlock()
	return nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func encodeStringSlice(values []string) (string, error) {
	if len(values) == 0 {
		return "[]", nil
	}
	data, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func decodeStringSlice(raw string, dest *[]string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		*dest = nil
		return nil
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return err
	}
	*dest = values
	return nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
