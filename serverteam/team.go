package serverteam

import (
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Organization struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Slug             string    `json:"slug"`
	Description      string    `json:"description,omitempty"`
	MonthlyBudgetUSD float64   `json:"monthly_budget_usd,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	IsActive         bool      `json:"is_active"`
}

const (
	MembershipRoleViewer  = "viewer"
	MembershipRoleMember  = "member"
	MembershipRoleManager = "manager"
	MembershipRoleOwner   = "owner"
)

type Project struct {
	ID               string    `json:"id"`
	OrganizationID   string    `json:"organization_id"`
	Name             string    `json:"name"`
	Slug             string    `json:"slug"`
	Description      string    `json:"description,omitempty"`
	MonthlyBudgetUSD float64   `json:"monthly_budget_usd,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	IsActive         bool      `json:"is_active"`
}

type OrganizationMembership struct {
	OrganizationID string    `json:"organization_id"`
	UserID         string    `json:"user_id"`
	Role           string    `json:"role"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	IsActive       bool      `json:"is_active"`
}

type ProjectMembership struct {
	ProjectID      string    `json:"project_id"`
	OrganizationID string    `json:"organization_id"`
	UserID         string    `json:"user_id"`
	Role           string    `json:"role"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	IsActive       bool      `json:"is_active"`
}

type Store interface {
	CreateOrganization(org Organization) (*Organization, error)
	ListOrganizations() ([]Organization, error)
	GetOrganization(id string) (*Organization, error)
	CreateProject(project Project) (*Project, error)
	ListProjects(organizationID string) ([]Project, error)
	GetProject(id string) (*Project, error)
	UpsertOrganizationMembership(membership OrganizationMembership) (*OrganizationMembership, error)
	ListOrganizationMemberships(organizationID, userID string) ([]OrganizationMembership, error)
	GetOrganizationMembership(organizationID, userID string) (*OrganizationMembership, error)
	UpsertProjectMembership(membership ProjectMembership) (*ProjectMembership, error)
	ListProjectMemberships(projectID, userID string) ([]ProjectMembership, error)
	GetProjectMembership(projectID, userID string) (*ProjectMembership, error)
}

type BillingBucket struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	MonthlyBudgetUSD   float64 `json:"monthly_budget_usd,omitempty"`
	SpendUSD           float64 `json:"spend_usd,omitempty"`
	RemainingBudgetUSD float64 `json:"remaining_budget_usd,omitempty"`
	UtilizationPercent float64 `json:"utilization_percent,omitempty"`
	RequestCount       int     `json:"request_count,omitempty"`
	OverBudget         bool    `json:"over_budget,omitempty"`
}

type BillingSummary struct {
	Organizations []BillingBucket `json:"organizations,omitempty"`
	Projects      []BillingBucket `json:"projects,omitempty"`
}

type BudgetAlert struct {
	ScopeType          string  `json:"scope_type"`
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	Severity           string  `json:"severity"`
	MonthlyBudgetUSD   float64 `json:"monthly_budget_usd,omitempty"`
	SpendUSD           float64 `json:"spend_usd,omitempty"`
	RemainingBudgetUSD float64 `json:"remaining_budget_usd,omitempty"`
	UtilizationPercent float64 `json:"utilization_percent,omitempty"`
	RequestCount       int     `json:"request_count,omitempty"`
}

func normalizeSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	return out
}

func normalizeOrganization(org Organization) (Organization, error) {
	org.Name = strings.TrimSpace(org.Name)
	if org.Name == "" {
		return Organization{}, fmt.Errorf("organization name is required")
	}
	if org.MonthlyBudgetUSD < 0 {
		return Organization{}, fmt.Errorf("organization monthly budget must be >= 0")
	}
	org.ID = strings.TrimSpace(org.ID)
	if org.ID == "" {
		org.ID = "org_" + randomHex(6)
	}
	org.Slug = normalizeSlug(firstNonEmpty(org.Slug, org.Name))
	if org.Slug == "" {
		return Organization{}, fmt.Errorf("organization slug is required")
	}
	now := time.Now().UTC()
	if org.CreatedAt.IsZero() {
		org.CreatedAt = now
	}
	org.UpdatedAt = now
	if !org.IsActive {
		org.IsActive = true
	}
	org.Description = strings.TrimSpace(org.Description)
	return org, nil
}

func normalizeProject(project Project) (Project, error) {
	project.Name = strings.TrimSpace(project.Name)
	project.OrganizationID = strings.TrimSpace(project.OrganizationID)
	if project.OrganizationID == "" {
		return Project{}, fmt.Errorf("project organization_id is required")
	}
	if project.Name == "" {
		return Project{}, fmt.Errorf("project name is required")
	}
	if project.MonthlyBudgetUSD < 0 {
		return Project{}, fmt.Errorf("project monthly budget must be >= 0")
	}
	project.ID = strings.TrimSpace(project.ID)
	if project.ID == "" {
		project.ID = "prj_" + randomHex(6)
	}
	project.Slug = normalizeSlug(firstNonEmpty(project.Slug, project.Name))
	if project.Slug == "" {
		return Project{}, fmt.Errorf("project slug is required")
	}
	now := time.Now().UTC()
	if project.CreatedAt.IsZero() {
		project.CreatedAt = now
	}
	project.UpdatedAt = now
	if !project.IsActive {
		project.IsActive = true
	}
	project.Description = strings.TrimSpace(project.Description)
	return project, nil
}

func normalizeMembershipRole(role string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "", MembershipRoleMember:
		return MembershipRoleMember, nil
	case MembershipRoleViewer, MembershipRoleManager, MembershipRoleOwner:
		return strings.ToLower(strings.TrimSpace(role)), nil
	default:
		return "", fmt.Errorf("membership role %q is invalid", role)
	}
}

func RoleRank(role string) int {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case MembershipRoleOwner:
		return 4
	case MembershipRoleManager:
		return 3
	case MembershipRoleMember:
		return 2
	case MembershipRoleViewer:
		return 1
	default:
		return 0
	}
}

func RoleAtLeast(role, required string) bool {
	return RoleRank(role) >= RoleRank(required)
}

// BuildBillingBucket computes the current spend posture for one budgeted scope.
func BuildBillingBucket(id, name string, budgetUSD, spendUSD float64, requestCount int) BillingBucket {
	bucket := BillingBucket{
		ID:                 id,
		Name:               name,
		MonthlyBudgetUSD:   budgetUSD,
		SpendUSD:           spendUSD,
		RemainingBudgetUSD: budgetUSD - spendUSD,
		RequestCount:       requestCount,
		OverBudget:         budgetUSD > 0 && spendUSD > budgetUSD,
	}
	if budgetUSD <= 0 {
		bucket.RemainingBudgetUSD = 0
		return bucket
	}
	bucket.UtilizationPercent = math.Round((spendUSD/budgetUSD)*10000) / 100
	return bucket
}

// BuildBudgetAlert returns the alert view for a budget bucket when thresholds are exceeded.
func BuildBudgetAlert(scopeType string, bucket BillingBucket) (BudgetAlert, bool) {
	if bucket.MonthlyBudgetUSD <= 0 {
		return BudgetAlert{}, false
	}
	severity := ""
	switch {
	case bucket.OverBudget || bucket.UtilizationPercent >= 100:
		severity = "critical"
	case bucket.UtilizationPercent >= 90:
		severity = "high"
	case bucket.UtilizationPercent >= 80:
		severity = "warning"
	default:
		return BudgetAlert{}, false
	}
	return BudgetAlert{
		ScopeType:          scopeType,
		ID:                 bucket.ID,
		Name:               bucket.Name,
		Severity:           severity,
		MonthlyBudgetUSD:   bucket.MonthlyBudgetUSD,
		SpendUSD:           bucket.SpendUSD,
		RemainingBudgetUSD: bucket.RemainingBudgetUSD,
		UtilizationPercent: bucket.UtilizationPercent,
		RequestCount:       bucket.RequestCount,
	}, true
}

// WriteBillingSummaryCSV renders organization and project billing buckets as a unified CSV stream.
func WriteBillingSummaryCSV(w io.Writer, summary BillingSummary) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"scope_type",
		"id",
		"name",
		"monthly_budget_usd",
		"spend_usd",
		"remaining_budget_usd",
		"utilization_percent",
		"request_count",
		"over_budget",
	}); err != nil {
		return err
	}
	writeBucket := func(scopeType string, bucket BillingBucket) error {
		return cw.Write([]string{
			scopeType,
			bucket.ID,
			bucket.Name,
			strconv.FormatFloat(bucket.MonthlyBudgetUSD, 'f', 6, 64),
			strconv.FormatFloat(bucket.SpendUSD, 'f', 6, 64),
			strconv.FormatFloat(bucket.RemainingBudgetUSD, 'f', 6, 64),
			strconv.FormatFloat(bucket.UtilizationPercent, 'f', 2, 64),
			strconv.Itoa(bucket.RequestCount),
			strconv.FormatBool(bucket.OverBudget),
		})
	}
	for _, bucket := range summary.Organizations {
		if err := writeBucket("organization", bucket); err != nil {
			return err
		}
	}
	for _, bucket := range summary.Projects {
		if err := writeBucket("project", bucket); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteBudgetAlertsCSV renders budget alerts as CSV.
func WriteBudgetAlertsCSV(w io.Writer, alerts []BudgetAlert) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"scope_type",
		"id",
		"name",
		"severity",
		"monthly_budget_usd",
		"spend_usd",
		"remaining_budget_usd",
		"utilization_percent",
		"request_count",
	}); err != nil {
		return err
	}
	for _, alert := range alerts {
		if err := cw.Write([]string{
			alert.ScopeType,
			alert.ID,
			alert.Name,
			alert.Severity,
			strconv.FormatFloat(alert.MonthlyBudgetUSD, 'f', 6, 64),
			strconv.FormatFloat(alert.SpendUSD, 'f', 6, 64),
			strconv.FormatFloat(alert.RemainingBudgetUSD, 'f', 6, 64),
			strconv.FormatFloat(alert.UtilizationPercent, 'f', 2, 64),
			strconv.Itoa(alert.RequestCount),
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func normalizeOrganizationMembership(membership OrganizationMembership) (OrganizationMembership, error) {
	membership.OrganizationID = strings.TrimSpace(membership.OrganizationID)
	membership.UserID = strings.TrimSpace(membership.UserID)
	if membership.OrganizationID == "" {
		return OrganizationMembership{}, fmt.Errorf("organization_id is required")
	}
	if membership.UserID == "" {
		return OrganizationMembership{}, fmt.Errorf("user_id is required")
	}
	role, err := normalizeMembershipRole(membership.Role)
	if err != nil {
		return OrganizationMembership{}, err
	}
	membership.Role = role
	now := time.Now().UTC()
	if membership.CreatedAt.IsZero() {
		membership.CreatedAt = now
	}
	membership.UpdatedAt = now
	if !membership.IsActive {
		membership.IsActive = true
	}
	return membership, nil
}

func normalizeProjectMembership(membership ProjectMembership) (ProjectMembership, error) {
	membership.ProjectID = strings.TrimSpace(membership.ProjectID)
	membership.OrganizationID = strings.TrimSpace(membership.OrganizationID)
	membership.UserID = strings.TrimSpace(membership.UserID)
	if membership.ProjectID == "" {
		return ProjectMembership{}, fmt.Errorf("project_id is required")
	}
	if membership.UserID == "" {
		return ProjectMembership{}, fmt.Errorf("user_id is required")
	}
	role, err := normalizeMembershipRole(membership.Role)
	if err != nil {
		return ProjectMembership{}, err
	}
	membership.Role = role
	now := time.Now().UTC()
	if membership.CreatedAt.IsZero() {
		membership.CreatedAt = now
	}
	membership.UpdatedAt = now
	if !membership.IsActive {
		membership.IsActive = true
	}
	return membership, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func randomHex(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func SortOrganizations(values []Organization) {
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
}

func SortProjects(values []Project) {
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
}

func SortOrganizationMemberships(values []OrganizationMembership) {
	sort.Slice(values, func(i, j int) bool {
		if values[i].OrganizationID == values[j].OrganizationID {
			return values[i].UserID < values[j].UserID
		}
		return values[i].OrganizationID < values[j].OrganizationID
	})
}

func SortProjectMemberships(values []ProjectMembership) {
	sort.Slice(values, func(i, j int) bool {
		if values[i].ProjectID == values[j].ProjectID {
			return values[i].UserID < values[j].UserID
		}
		return values[i].ProjectID < values[j].ProjectID
	})
}
