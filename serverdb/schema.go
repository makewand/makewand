package serverdb

import (
	"database/sql"
	"fmt"
	"strings"
)

// EnsureColumns adds missing columns to an existing SQLite table.
func EnsureColumns(db *sql.DB, table string, definitions map[string]string) error {
	if db == nil {
		return fmt.Errorf("sqlite db is nil")
	}
	table = strings.TrimSpace(table)
	if table == "" {
		return fmt.Errorf("table name is empty")
	}
	existing, err := ExistingColumns(db, table)
	if err != nil {
		return err
	}
	for column, definition := range definitions {
		column = strings.TrimSpace(column)
		definition = strings.TrimSpace(definition)
		if column == "" || definition == "" {
			continue
		}
		if _, ok := existing[column]; ok {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", table, definition)); err != nil {
			return err
		}
	}
	return nil
}

// ExistingColumns returns the current column names for a SQLite table.
func ExistingColumns(db *sql.DB, table string) (map[string]struct{}, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", strings.TrimSpace(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notNull    int
			defaultV   sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &defaultV, &primaryKey); err != nil {
			return nil, err
		}
		out[strings.TrimSpace(name)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
