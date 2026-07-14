package serverusage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/makewand/makewand/serverdb"
)

// SQLiteStore persists usage entries in SQLite.
type SQLiteStore struct {
	path string
	db   *sql.DB
}

// OpenSQLiteStore opens or creates a SQLite-backed usage store.
func OpenSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := serverdb.Open(path)
	if err != nil {
		return nil, err
	}
	store := &SQLiteStore{path: path, db: db}
	if err := store.ensureSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) ensureSchema() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS usage_entries (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  timestamp TEXT NOT NULL,
  request_id TEXT NOT NULL DEFAULT '',
  token_id TEXT NOT NULL DEFAULT '',
  token_description TEXT NOT NULL DEFAULT '',
  user_id TEXT NOT NULL DEFAULT '',
  organization_id TEXT NOT NULL DEFAULT '',
  project_id TEXT NOT NULL DEFAULT '',
  requested_mode TEXT NOT NULL DEFAULT '',
  requested_model TEXT NOT NULL DEFAULT '',
  actual_provider TEXT NOT NULL DEFAULT '',
  status INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd REAL NOT NULL DEFAULT 0,
  stream INTEGER NOT NULL DEFAULT 0
);`)
	if err != nil {
		return err
	}
	return serverdb.EnsureColumns(s.db, "usage_entries", map[string]string{
		"user_id":         "user_id TEXT NOT NULL DEFAULT ''",
		"organization_id": "organization_id TEXT NOT NULL DEFAULT ''",
		"project_id":      "project_id TEXT NOT NULL DEFAULT ''",
	})
}

// Close closes the underlying database handle.
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

// Log inserts one usage entry.
func (s *SQLiteStore) Log(entry Entry) {
	if s == nil || s.db == nil {
		return
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	_, _ = s.db.Exec(`
INSERT INTO usage_entries (
  timestamp, request_id, token_id, token_description, user_id, organization_id, project_id, requested_mode,
  requested_model, actual_provider, status, duration_ms, prompt_tokens,
  completion_tokens, cost_usd, stream
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.Timestamp.UTC().Format(time.RFC3339Nano),
		entry.RequestID,
		entry.TokenID,
		entry.TokenDescription,
		entry.UserID,
		entry.OrganizationID,
		entry.ProjectID,
		entry.RequestedMode,
		entry.RequestedModel,
		entry.ActualProvider,
		entry.Status,
		entry.DurationMS,
		entry.PromptTokens,
		entry.CompletionTokens,
		entry.CostUSD,
		boolToInt(entry.Stream),
	)
}

// Load loads entries matching the supplied filter.
func (s *SQLiteStore) Load(filter Filter) ([]Entry, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	query := strings.Builder{}
	query.WriteString(`
SELECT timestamp, request_id, token_id, token_description, user_id, organization_id, project_id,
       requested_mode, requested_model, actual_provider,
       status, duration_ms, prompt_tokens, completion_tokens, cost_usd, stream
FROM usage_entries WHERE 1=1`)
	args := make([]any, 0, 8)
	if filter.TokenID != "" {
		query.WriteString(` AND token_id = ?`)
		args = append(args, filter.TokenID)
	}
	if filter.UserID != "" {
		query.WriteString(` AND user_id = ?`)
		args = append(args, filter.UserID)
	}
	if filter.OrgID != "" {
		query.WriteString(` AND organization_id = ?`)
		args = append(args, filter.OrgID)
	}
	if filter.ProjectID != "" {
		query.WriteString(` AND project_id = ?`)
		args = append(args, filter.ProjectID)
	}
	if filter.RequestID != "" {
		query.WriteString(` AND request_id = ?`)
		args = append(args, filter.RequestID)
	}
	if filter.Provider != "" {
		query.WriteString(` AND lower(actual_provider) = lower(?)`)
		args = append(args, filter.Provider)
	}
	if filter.Status != 0 {
		query.WriteString(` AND status = ?`)
		args = append(args, filter.Status)
	}
	if filter.StreamOnly {
		query.WriteString(` AND stream = 1`)
	}
	if !filter.Since.IsZero() {
		query.WriteString(` AND timestamp >= ?`)
		args = append(args, filter.Since.UTC().Format(time.RFC3339Nano))
	}
	if !filter.Until.IsZero() {
		query.WriteString(` AND timestamp <= ?`)
		args = append(args, filter.Until.UTC().Format(time.RFC3339Nano))
	}
	query.WriteString(` ORDER BY timestamp DESC`)
	if filter.Limit > 0 {
		query.WriteString(` LIMIT ?`)
		args = append(args, filter.Limit)
	}
	rows, err := s.db.Query(query.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]Entry, 0, 64)
	for rows.Next() {
		var (
			entry        Entry
			timestampRaw string
			stream       int
		)
		if err := rows.Scan(
			&timestampRaw,
			&entry.RequestID,
			&entry.TokenID,
			&entry.TokenDescription,
			&entry.UserID,
			&entry.OrganizationID,
			&entry.ProjectID,
			&entry.RequestedMode,
			&entry.RequestedModel,
			&entry.ActualProvider,
			&entry.Status,
			&entry.DurationMS,
			&entry.PromptTokens,
			&entry.CompletionTokens,
			&entry.CostUSD,
			&stream,
		); err != nil {
			return nil, err
		}
		ts, err := time.Parse(time.RFC3339Nano, timestampRaw)
		if err != nil {
			return nil, err
		}
		entry.Timestamp = ts.UTC()
		entry.Stream = stream == 1
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// LoadSQLiteEntries is a convenience helper for one-shot local commands.
func LoadSQLiteEntries(path string, filter Filter) ([]Entry, error) {
	store, err := OpenSQLiteStore(path)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.Load(filter)
}

func (s *SQLiteStore) String() string {
	return fmt.Sprintf("sqlite:%s", s.path)
}
