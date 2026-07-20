package serverusage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/makewand/makewand/serverdb"
)

// sinceBoundaryLayout is a fixed-width, all-9-fractional-digit RFC3339 layout
// used ONLY to format an inclusive `>= Since` boundary. Timestamps are compared
// as TEXT in SQL, and variable-width RFC3339Nano omits trailing zero fractions,
// so an integer-second boundary like "…00:00:00Z" sorts AFTER a sub-second entry
// "…00:00:00.123Z" and a `>= month-start` filter silently drops the very first
// sub-second entries of the month. A boundary of "…00:00:00.000000000Z" sorts at
// or before every representation of that instant, so it includes them.
//
// It is applied to the Since boundary only: the ENTRY rows and the `<= Until`
// boundary keep RFC3339Nano so that old (variable-width) rows already in a
// state.db compare exactly as they did before — using a fixed boundary against
// legacy variable-width rows would wrongly exclude equal-instant rows from an
// inclusive Until query.
const sinceBoundaryLayout = "2006-01-02T15:04:05.000000000Z07:00"

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
func (s *SQLiteStore) Log(entry Entry) error {
	// Return an error for an unavailable store so strict accounting does not treat
	// an unrecorded entry as success.
	if s == nil || s.db == nil {
		return fmt.Errorf("usage sqlite store is unavailable")
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	_, err := s.db.Exec(`
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
	if err != nil {
		return fmt.Errorf("insert usage entry: %w", err)
	}
	return nil
}

// Load loads entries matching the supplied filter.
func (s *SQLiteStore) Load(filter Filter) ([]Entry, error) {
	// An unavailable store returns an error, not empty results, so a budget check
	// reading it fails closed instead of seeing "zero spend".
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("usage sqlite store is unavailable")
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
		// Fixed-width lower bound so an inclusive >= includes sub-second entries at
		// the boundary instant (see sinceBoundaryLayout).
		query.WriteString(` AND timestamp >= ?`)
		args = append(args, filter.Since.UTC().Format(sinceBoundaryLayout))
	}
	if !filter.Until.IsZero() {
		// RFC3339Nano to match how rows (including legacy variable-width rows) are
		// stored, so an inclusive <= keeps equal-instant rows.
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
