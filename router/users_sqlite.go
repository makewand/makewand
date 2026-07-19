package router

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/makewand/makewand/serverdb"
)

// SQLiteUserStore persists users in a SQLite state database.
type SQLiteUserStore struct {
	path string
	db   *sql.DB
}

// OpenSQLiteUserStore opens or creates a SQLite-backed user store.
func OpenSQLiteUserStore(path string) (*SQLiteUserStore, error) {
	db, err := serverdb.Open(path)
	if err != nil {
		return nil, err
	}
	store := &SQLiteUserStore{path: path, db: db}
	if err := store.ensureSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteUserStore) ensureSchema() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  salt TEXT NOT NULL,
  role TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  is_active INTEGER NOT NULL DEFAULT 1
);`)
	return err
}

// Close closes the underlying database handle.
func (s *SQLiteUserStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path returns the backing SQLite path.
func (s *SQLiteUserStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *SQLiteUserStore) CreateUser(email, password string) (*User, error) {
	return s.CreateUserWithRole(email, password, UserRoleMember)
}

func (s *SQLiteUserStore) CreateUserWithRole(email, password, role string) (*User, error) {
	return s.CreateUserWithRoleActive(email, password, role, true)
}

// CreateUserWithRoleActive creates a new user account with an explicit role and
// initial active state persisted in a single INSERT.
func (s *SQLiteUserStore) CreateUserWithRoleActive(email, password, role string, active bool) (*User, error) {
	if s == nil {
		return nil, fmt.Errorf("sqlite user store is unavailable")
	}
	role, err := normalizeUserRole(role)
	if err != nil {
		return nil, err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if existing, err := s.GetUserByEmail(email); err == nil && existing != nil {
		return nil, fmt.Errorf("%w: %s", ErrUserExists, email)
	} else if err != nil && err != ErrUserNotFound {
		return nil, err
	}

	now := time.Now().UTC()
	user := &User{
		ID:        generateUserID(),
		Email:     email,
		Salt:      generateSalt(),
		Role:      role,
		CreatedAt: now,
		UpdatedAt: now,
		IsActive:  active,
	}
	user.PasswordHash = hashPassword(password, user.Salt)
	_, err = s.db.Exec(`
INSERT INTO users (id, email, password_hash, salt, role, created_at, updated_at, is_active)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		user.ID,
		user.Email,
		user.PasswordHash,
		user.Salt,
		user.Role,
		user.CreatedAt.Format(time.RFC3339),
		user.UpdatedAt.Format(time.RFC3339),
		boolToInt(active),
	)
	if err != nil {
		return nil, err
	}
	return user, nil
}

func (s *SQLiteUserStore) GetUserByID(userID string) (*User, error) {
	if s == nil {
		return nil, fmt.Errorf("sqlite user store is unavailable")
	}
	row := s.db.QueryRow(`
SELECT id, email, password_hash, salt, role, created_at, updated_at, is_active
FROM users WHERE id = ?`, strings.TrimSpace(userID))
	return scanUser(row)
}

func (s *SQLiteUserStore) GetUserByEmail(email string) (*User, error) {
	if s == nil {
		return nil, fmt.Errorf("sqlite user store is unavailable")
	}
	row := s.db.QueryRow(`
SELECT id, email, password_hash, salt, role, created_at, updated_at, is_active
FROM users WHERE email = ?`, strings.ToLower(strings.TrimSpace(email)))
	return scanUser(row)
}

func (s *SQLiteUserStore) ListUsers() ([]UserView, error) {
	if s == nil {
		return nil, fmt.Errorf("sqlite user store is unavailable")
	}
	rows, err := s.db.Query(`
SELECT id, email, password_hash, salt, role, created_at, updated_at, is_active
FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	views := make([]UserView, 0, 8)
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		views = append(views, user.View())
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	return views, nil
}

func (s *SQLiteUserStore) SetUserActive(userID string, active bool) (*User, error) {
	if s == nil {
		return nil, fmt.Errorf("sqlite user store is unavailable")
	}
	now := time.Now().UTC()
	result, err := s.db.Exec(`UPDATE users SET is_active = ?, updated_at = ? WHERE id = ?`, boolToInt(active), now.Format(time.RFC3339), strings.TrimSpace(userID))
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, ErrUserNotFound
	}
	return s.GetUserByID(userID)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *SQLiteUserStore) SetUserRole(userID, role string) (*User, error) {
	if s == nil {
		return nil, fmt.Errorf("sqlite user store is unavailable")
	}
	role, err := normalizeUserRole(role)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	result, err := s.db.Exec(`UPDATE users SET role = ?, updated_at = ? WHERE id = ?`, role, now.Format(time.RFC3339), strings.TrimSpace(userID))
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, ErrUserNotFound
	}
	return s.GetUserByID(userID)
}

func (s *SQLiteUserStore) SetUserPassword(userID, password string) (*User, error) {
	if s == nil {
		return nil, fmt.Errorf("sqlite user store is unavailable")
	}
	if !isValidPassword(password) {
		return nil, fmt.Errorf("password must be at least 8 characters long")
	}
	salt := generateSalt()
	passwordHash := hashPassword(password, salt)
	now := time.Now().UTC()
	result, err := s.db.Exec(`UPDATE users SET password_hash = ?, salt = ?, updated_at = ? WHERE id = ?`,
		passwordHash,
		salt,
		now.Format(time.RFC3339),
		strings.TrimSpace(userID),
	)
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, ErrUserNotFound
	}
	return s.GetUserByID(userID)
}

type userScanner interface {
	Scan(dest ...any) error
}

func scanUser(scanner userScanner) (*User, error) {
	var (
		user         User
		createdAtRaw string
		updatedAtRaw string
		isActive     int
	)
	if err := scanner.Scan(
		&user.ID,
		&user.Email,
		&user.PasswordHash,
		&user.Salt,
		&user.Role,
		&createdAtRaw,
		&updatedAtRaw,
		&isActive,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	createdAt, err := time.Parse(time.RFC3339, createdAtRaw)
	if err != nil {
		return nil, err
	}
	updatedAt, err := time.Parse(time.RFC3339, updatedAtRaw)
	if err != nil {
		return nil, err
	}
	user.CreatedAt = createdAt.UTC()
	user.UpdatedAt = updatedAt.UTC()
	user.IsActive = isActive == 1
	return &user, nil
}
