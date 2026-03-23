package serverauth

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
	return store, nil
}

func (s *SQLiteStore) ensureSchema() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS auth_tokens (
  id TEXT PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  description TEXT NOT NULL DEFAULT '',
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
	return err
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
  id, token_hash, description, scopes_json, workspace_prefixes_json,
  allowed_providers_json, allowed_modes_json, expires_at, revoked,
  max_requests_per_hour, max_requests_per_day, max_cost_usd_per_day,
  max_cost_usd_per_month, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rule.ID,
		hashToken(tokenValue),
		strings.TrimSpace(rule.Description),
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
SELECT id, token_hash, description, scopes_json, workspace_prefixes_json,
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
			rule          TokenRule
			tokenHash     string
			scopesJSON    string
			prefixesJSON  string
			providersJSON string
			modesJSON     string
			expiresAtRaw  string
			revoked       int
		)
		if err := rows.Scan(
			&rule.ID,
			&tokenHash,
			&rule.Description,
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
