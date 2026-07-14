package serverteam

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/makewand/makewand/serverdb"
)

type SQLiteStore struct {
	path string
	db   *sql.DB
}

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
	if s == nil || s.db == nil {
		return fmt.Errorf("sqlite team store is unavailable")
	}
	if _, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS organizations (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  slug TEXT NOT NULL UNIQUE,
  description TEXT NOT NULL DEFAULT '',
  monthly_budget_usd REAL NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  is_active INTEGER NOT NULL DEFAULT 1
);`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS projects (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL,
  name TEXT NOT NULL,
  slug TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  monthly_budget_usd REAL NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  is_active INTEGER NOT NULL DEFAULT 1,
  UNIQUE(organization_id, slug),
  FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS organization_memberships (
  organization_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  role TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  is_active INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (organization_id, user_id),
  FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS project_memberships (
  project_id TEXT NOT NULL,
  organization_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  role TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  is_active INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (project_id, user_id),
  FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE,
  FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE
);`); err != nil {
		return err
	}
	return nil
}

func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *SQLiteStore) CreateOrganization(org Organization) (*Organization, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("sqlite team store is unavailable")
	}
	org, err := normalizeOrganization(org)
	if err != nil {
		return nil, err
	}
	_, err = s.db.Exec(`
INSERT INTO organizations (id, name, slug, description, monthly_budget_usd, created_at, updated_at, is_active)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		org.ID,
		org.Name,
		org.Slug,
		org.Description,
		org.MonthlyBudgetUSD,
		org.CreatedAt.Format(time.RFC3339),
		org.UpdatedAt.Format(time.RFC3339),
		boolToInt(org.IsActive),
	)
	if err != nil {
		return nil, err
	}
	return &org, nil
}

func (s *SQLiteStore) ListOrganizations() ([]Organization, error) {
	rows, err := s.db.Query(`
SELECT id, name, slug, description, monthly_budget_usd, created_at, updated_at, is_active
FROM organizations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Organization, 0, 8)
	for rows.Next() {
		org, err := scanOrganization(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *org)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	SortOrganizations(out)
	return out, nil
}

func (s *SQLiteStore) GetOrganization(id string) (*Organization, error) {
	row := s.db.QueryRow(`
SELECT id, name, slug, description, monthly_budget_usd, created_at, updated_at, is_active
FROM organizations WHERE id = ?`, strings.TrimSpace(id))
	return scanOrganization(row)
}

func (s *SQLiteStore) CreateProject(project Project) (*Project, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("sqlite team store is unavailable")
	}
	project, err := normalizeProject(project)
	if err != nil {
		return nil, err
	}
	if _, err := s.GetOrganization(project.OrganizationID); err != nil {
		return nil, fmt.Errorf("organization %q not found", project.OrganizationID)
	}
	_, err = s.db.Exec(`
INSERT INTO projects (id, organization_id, name, slug, description, monthly_budget_usd, created_at, updated_at, is_active)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		project.ID,
		project.OrganizationID,
		project.Name,
		project.Slug,
		project.Description,
		project.MonthlyBudgetUSD,
		project.CreatedAt.Format(time.RFC3339),
		project.UpdatedAt.Format(time.RFC3339),
		boolToInt(project.IsActive),
	)
	if err != nil {
		return nil, err
	}
	return &project, nil
}

func (s *SQLiteStore) ListProjects(organizationID string) ([]Project, error) {
	orgID := strings.TrimSpace(organizationID)
	query := `
SELECT id, organization_id, name, slug, description, monthly_budget_usd, created_at, updated_at, is_active
FROM projects`
	args := []any{}
	if orgID != "" {
		query += ` WHERE organization_id = ?`
		args = append(args, orgID)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Project, 0, 8)
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *project)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	SortProjects(out)
	return out, nil
}

func (s *SQLiteStore) GetProject(id string) (*Project, error) {
	row := s.db.QueryRow(`
SELECT id, organization_id, name, slug, description, monthly_budget_usd, created_at, updated_at, is_active
FROM projects WHERE id = ?`, strings.TrimSpace(id))
	return scanProject(row)
}

func (s *SQLiteStore) UpsertOrganizationMembership(membership OrganizationMembership) (*OrganizationMembership, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("sqlite team store is unavailable")
	}
	membership, err := normalizeOrganizationMembership(membership)
	if err != nil {
		return nil, err
	}
	if _, err := s.GetOrganization(membership.OrganizationID); err != nil {
		return nil, fmt.Errorf("organization %q not found", membership.OrganizationID)
	}
	_, err = s.db.Exec(`
INSERT INTO organization_memberships (organization_id, user_id, role, created_at, updated_at, is_active)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(organization_id, user_id) DO UPDATE SET
  role = excluded.role,
  updated_at = excluded.updated_at,
  is_active = excluded.is_active`,
		membership.OrganizationID,
		membership.UserID,
		membership.Role,
		membership.CreatedAt.Format(time.RFC3339),
		membership.UpdatedAt.Format(time.RFC3339),
		boolToInt(membership.IsActive),
	)
	if err != nil {
		return nil, err
	}
	return s.GetOrganizationMembership(membership.OrganizationID, membership.UserID)
}

func (s *SQLiteStore) ListOrganizationMemberships(organizationID, userID string) ([]OrganizationMembership, error) {
	query := `
SELECT organization_id, user_id, role, created_at, updated_at, is_active
FROM organization_memberships WHERE 1=1`
	args := []any{}
	if value := strings.TrimSpace(organizationID); value != "" {
		query += ` AND organization_id = ?`
		args = append(args, value)
	}
	if value := strings.TrimSpace(userID); value != "" {
		query += ` AND user_id = ?`
		args = append(args, value)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OrganizationMembership, 0, 8)
	for rows.Next() {
		item, err := scanOrganizationMembership(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	SortOrganizationMemberships(out)
	return out, nil
}

func (s *SQLiteStore) GetOrganizationMembership(organizationID, userID string) (*OrganizationMembership, error) {
	row := s.db.QueryRow(`
SELECT organization_id, user_id, role, created_at, updated_at, is_active
FROM organization_memberships WHERE organization_id = ? AND user_id = ?`,
		strings.TrimSpace(organizationID),
		strings.TrimSpace(userID),
	)
	return scanOrganizationMembership(row)
}

func (s *SQLiteStore) UpsertProjectMembership(membership ProjectMembership) (*ProjectMembership, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("sqlite team store is unavailable")
	}
	membership, err := normalizeProjectMembership(membership)
	if err != nil {
		return nil, err
	}
	project, err := s.GetProject(membership.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("project %q not found", membership.ProjectID)
	}
	if membership.OrganizationID == "" {
		membership.OrganizationID = project.OrganizationID
	}
	if membership.OrganizationID != project.OrganizationID {
		return nil, fmt.Errorf("project %q belongs to organization %q", membership.ProjectID, project.OrganizationID)
	}
	_, err = s.db.Exec(`
INSERT INTO project_memberships (project_id, organization_id, user_id, role, created_at, updated_at, is_active)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(project_id, user_id) DO UPDATE SET
  organization_id = excluded.organization_id,
  role = excluded.role,
  updated_at = excluded.updated_at,
  is_active = excluded.is_active`,
		membership.ProjectID,
		membership.OrganizationID,
		membership.UserID,
		membership.Role,
		membership.CreatedAt.Format(time.RFC3339),
		membership.UpdatedAt.Format(time.RFC3339),
		boolToInt(membership.IsActive),
	)
	if err != nil {
		return nil, err
	}
	return s.GetProjectMembership(membership.ProjectID, membership.UserID)
}

func (s *SQLiteStore) ListProjectMemberships(projectID, userID string) ([]ProjectMembership, error) {
	query := `
SELECT project_id, organization_id, user_id, role, created_at, updated_at, is_active
FROM project_memberships WHERE 1=1`
	args := []any{}
	if value := strings.TrimSpace(projectID); value != "" {
		query += ` AND project_id = ?`
		args = append(args, value)
	}
	if value := strings.TrimSpace(userID); value != "" {
		query += ` AND user_id = ?`
		args = append(args, value)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ProjectMembership, 0, 8)
	for rows.Next() {
		item, err := scanProjectMembership(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	SortProjectMemberships(out)
	return out, nil
}

func (s *SQLiteStore) GetProjectMembership(projectID, userID string) (*ProjectMembership, error) {
	row := s.db.QueryRow(`
SELECT project_id, organization_id, user_id, role, created_at, updated_at, is_active
FROM project_memberships WHERE project_id = ? AND user_id = ?`,
		strings.TrimSpace(projectID),
		strings.TrimSpace(userID),
	)
	return scanProjectMembership(row)
}

type orgScanner interface {
	Scan(dest ...any) error
}

func scanOrganization(scanner orgScanner) (*Organization, error) {
	var (
		org        Organization
		createdRaw string
		updatedRaw string
		isActive   int
	)
	if err := scanner.Scan(
		&org.ID,
		&org.Name,
		&org.Slug,
		&org.Description,
		&org.MonthlyBudgetUSD,
		&createdRaw,
		&updatedRaw,
		&isActive,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("organization not found")
		}
		return nil, err
	}
	createdAt, err := time.Parse(time.RFC3339, createdRaw)
	if err != nil {
		return nil, err
	}
	updatedAt, err := time.Parse(time.RFC3339, updatedRaw)
	if err != nil {
		return nil, err
	}
	org.CreatedAt = createdAt.UTC()
	org.UpdatedAt = updatedAt.UTC()
	org.IsActive = isActive == 1
	return &org, nil
}

func scanProject(scanner orgScanner) (*Project, error) {
	var (
		project    Project
		createdRaw string
		updatedRaw string
		isActive   int
	)
	if err := scanner.Scan(
		&project.ID,
		&project.OrganizationID,
		&project.Name,
		&project.Slug,
		&project.Description,
		&project.MonthlyBudgetUSD,
		&createdRaw,
		&updatedRaw,
		&isActive,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("project not found")
		}
		return nil, err
	}
	createdAt, err := time.Parse(time.RFC3339, createdRaw)
	if err != nil {
		return nil, err
	}
	updatedAt, err := time.Parse(time.RFC3339, updatedRaw)
	if err != nil {
		return nil, err
	}
	project.CreatedAt = createdAt.UTC()
	project.UpdatedAt = updatedAt.UTC()
	project.IsActive = isActive == 1
	return &project, nil
}

func scanOrganizationMembership(scanner orgScanner) (*OrganizationMembership, error) {
	var (
		item       OrganizationMembership
		createdRaw string
		updatedRaw string
		isActive   int
	)
	if err := scanner.Scan(
		&item.OrganizationID,
		&item.UserID,
		&item.Role,
		&createdRaw,
		&updatedRaw,
		&isActive,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("organization membership not found")
		}
		return nil, err
	}
	createdAt, err := time.Parse(time.RFC3339, createdRaw)
	if err != nil {
		return nil, err
	}
	updatedAt, err := time.Parse(time.RFC3339, updatedRaw)
	if err != nil {
		return nil, err
	}
	item.CreatedAt = createdAt.UTC()
	item.UpdatedAt = updatedAt.UTC()
	item.IsActive = isActive == 1
	return &item, nil
}

func scanProjectMembership(scanner orgScanner) (*ProjectMembership, error) {
	var (
		item       ProjectMembership
		createdRaw string
		updatedRaw string
		isActive   int
	)
	if err := scanner.Scan(
		&item.ProjectID,
		&item.OrganizationID,
		&item.UserID,
		&item.Role,
		&createdRaw,
		&updatedRaw,
		&isActive,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("project membership not found")
		}
		return nil, err
	}
	createdAt, err := time.Parse(time.RFC3339, createdRaw)
	if err != nil {
		return nil, err
	}
	updatedAt, err := time.Parse(time.RFC3339, updatedRaw)
	if err != nil {
		return nil, err
	}
	item.CreatedAt = createdAt.UTC()
	item.UpdatedAt = updatedAt.UTC()
	item.IsActive = isActive == 1
	return &item, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
