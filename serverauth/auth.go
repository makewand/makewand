package serverauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ScopeChatInvoke       = "chat:invoke"
	ScopeModelsRead       = "models:read"
	ScopeSessionsRead     = "sessions:read"
	ScopeSessionsWrite    = "sessions:write"
	ScopeSessionsDelete   = "sessions:delete"
	ScopeAdminTokensRead  = "admin:tokens:read"
	ScopeAdminTokensWrite = "admin:tokens:write" //nolint:gosec // G101: OAuth-style scope identifier, not a credential.
	ScopeAdminAuditRead   = "admin:audit:read"
	ScopeAdminUsageRead   = "admin:usage:read"
	ScopeAdminMetricsRead = "admin:metrics:read"
	ScopeAdminUsersRead   = "admin:users:read"
	ScopeAdminUsersWrite  = "admin:users:write"
)

var validScopes = map[string]struct{}{
	ScopeChatInvoke:       {},
	ScopeModelsRead:       {},
	ScopeSessionsRead:     {},
	ScopeSessionsWrite:    {},
	ScopeSessionsDelete:   {},
	ScopeAdminTokensRead:  {},
	ScopeAdminTokensWrite: {},
	ScopeAdminAuditRead:   {},
	ScopeAdminUsageRead:   {},
	ScopeAdminMetricsRead: {},
	ScopeAdminUsersRead:   {},
	ScopeAdminUsersWrite:  {},
}

var validModes = map[string]struct{}{
	"fast":     {},
	"balanced": {},
	"power":    {},
}

// Config defines a scoped token policy file for the makewand server.
type Config struct {
	Tokens []TokenRule `json:"tokens"`
}

// TokenRule defines one remote client token and its permissions.
type TokenRule struct {
	ID                 string    `json:"id,omitempty"`
	Token              string    `json:"token"`
	Description        string    `json:"description,omitempty"`
	UserID             string    `json:"user_id,omitempty"`
	OrganizationID     string    `json:"organization_id,omitempty"`
	ProjectID          string    `json:"project_id,omitempty"`
	Scopes             []string  `json:"scopes"`
	WorkspacePrefixes  []string  `json:"workspace_prefixes,omitempty"`
	AllowedProviders   []string  `json:"allowed_providers,omitempty"`
	AllowedModes       []string  `json:"allowed_modes,omitempty"`
	ExpiresAt          time.Time `json:"expires_at,omitempty"`
	Revoked            bool      `json:"revoked,omitempty"`
	MaxRequestsPerHour int       `json:"max_requests_per_hour,omitempty"`
	MaxRequestsPerDay  int       `json:"max_requests_per_day,omitempty"`
	MaxCostUSDPerDay   float64   `json:"max_cost_usd_per_day,omitempty"`
	MaxCostUSDPerMonth float64   `json:"max_cost_usd_per_month,omitempty"`
}

// TokenRuleView is a non-secret representation of a token rule suitable for
// listing via CLI or admin APIs.
type TokenRuleView struct {
	ID                 string    `json:"id,omitempty"`
	Description        string    `json:"description,omitempty"`
	UserID             string    `json:"user_id,omitempty"`
	OrganizationID     string    `json:"organization_id,omitempty"`
	ProjectID          string    `json:"project_id,omitempty"`
	Scopes             []string  `json:"scopes"`
	WorkspacePrefixes  []string  `json:"workspace_prefixes,omitempty"`
	AllowedProviders   []string  `json:"allowed_providers,omitempty"`
	AllowedModes       []string  `json:"allowed_modes,omitempty"`
	ExpiresAt          time.Time `json:"expires_at,omitempty"`
	Revoked            bool      `json:"revoked,omitempty"`
	MaxRequestsPerHour int       `json:"max_requests_per_hour,omitempty"`
	MaxRequestsPerDay  int       `json:"max_requests_per_day,omitempty"`
	MaxCostUSDPerDay   float64   `json:"max_cost_usd_per_day,omitempty"`
	MaxCostUSDPerMonth float64   `json:"max_cost_usd_per_month,omitempty"`
}

// RequestAuthorizer authenticates request-scoped grants for HTTP handlers.
type RequestAuthorizer interface {
	AuthenticateRequest(req *http.Request) (*Grant, bool)
}

// TokenManager is the minimal interface required by admin APIs and login flows
// to manage bearer tokens regardless of the backing store.
type TokenManager interface {
	RequestAuthorizer
	TokenRules() []TokenRuleView
	Issue(rule TokenRule) (TokenRuleView, string, error)
	Revoke(tokenID string) error
}

// Authorizer authenticates Bearer tokens and returns scoped grants.
type Authorizer struct {
	grants map[string]*Grant
}

// Grant is the normalized permission view for an authenticated token.
type Grant struct {
	tokenID            string
	description        string
	userID             string
	organizationID     string
	projectID          string
	expiresAt          time.Time
	revoked            bool
	maxRequestsPerHour int
	maxRequestsPerDay  int
	maxCostUSDPerDay   float64
	maxCostUSDPerMonth float64
	scopes             map[string]struct{}
	workspacePrefixes  []string
	allowedProviders   map[string]struct{}
	allowedModes       map[string]struct{}
	// usage holds the mutable quota/spend counters. It is referenced by pointer
	// so a rebuilt authorizer can share the SAME counter state with the previous
	// grant for a carried-over token (see carryOverGrantUsage), eliminating the
	// copy-then-swap race where an in-flight increment on the old grant was lost.
	usage *grantUsage
}

// grantUsage holds the in-memory quota and spend counters for a single token,
// guarded by its own mutex. Multiple Grant instances (across an authorizer
// rebuild) may point at the same grantUsage so that concurrent increments from
// an in-flight request holding the old grant and new requests using the new
// grant all accrue against one shared, mutex-protected set of counters.
type grantUsage struct {
	mu               sync.Mutex
	quotaWindowStart time.Time
	quotaWindowCount int
	quotaDayStart    time.Time
	quotaDayCount    int
	costDayStart     time.Time
	costDaySpent     float64
	costMonthStart   time.Time
	costMonthSpent   float64
}

var (
	ErrHourlyQuotaExceeded = errors.New("token exceeded max_requests_per_hour")
	ErrDailyQuotaExceeded  = errors.New("token exceeded max_requests_per_day")
	ErrDailyCostExceeded   = errors.New("token exceeded max_cost_usd_per_day")
	ErrMonthlyCostExceeded = errors.New("token exceeded max_cost_usd_per_month")
)

// AllClientScopes returns the default non-admin scope set for remote clients.
func AllClientScopes() []string {
	return []string{
		ScopeChatInvoke,
		ScopeModelsRead,
		ScopeSessionsRead,
		ScopeSessionsWrite,
		ScopeSessionsDelete,
	}
}

// AllScopes returns the full scope set used by privileged admin tokens.
func AllScopes() []string {
	out := AllClientScopes()
	out = append(out,
		ScopeAdminTokensRead,
		ScopeAdminTokensWrite,
		ScopeAdminAuditRead,
		ScopeAdminUsageRead,
		ScopeAdminMetricsRead,
		ScopeAdminUsersRead,
		ScopeAdminUsersWrite,
	)
	return out
}

// NewSingleTokenAuthorizer creates a permissive authorizer for the legacy
// single-token mode used by --token and MAKEWAND_SERVER_TOKEN.
func NewSingleTokenAuthorizer(token string) *Authorizer {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	grant, err := newGrant(TokenRule{
		Token:  token,
		Scopes: AllScopes(),
	})
	if err != nil {
		return nil
	}
	return &Authorizer{
		grants: map[string]*Grant{
			token: grant,
		},
	}
}

// LoadFile parses a JSON auth config from disk.
func LoadFile(path string) (*Authorizer, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("auth config path is empty")
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfg Config
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	return NewAuthorizer(cfg)
}

// NewAuthorizer validates and constructs a multi-token authorizer.
func NewAuthorizer(cfg Config) (*Authorizer, error) {
	if len(cfg.Tokens) == 0 {
		return nil, errors.New("auth config requires at least one token")
	}

	grants := make(map[string]*Grant, len(cfg.Tokens))
	ids := make(map[string]string, len(cfg.Tokens))
	for i, rule := range cfg.Tokens {
		token := strings.TrimSpace(rule.Token)
		if token == "" {
			return nil, fmt.Errorf("token entry %d has an empty token", i)
		}
		if _, exists := grants[token]; exists {
			return nil, fmt.Errorf("duplicate token entry %q", token)
		}
		grant, err := newGrant(rule)
		if err != nil {
			return nil, fmt.Errorf("token entry %q: %w", token, err)
		}
		if existingToken, exists := ids[grant.tokenID]; exists {
			return nil, fmt.Errorf("duplicate token id %q (tokens %q and %q)", grant.tokenID, existingToken, token)
		}
		ids[grant.tokenID] = token
		grants[token] = grant
	}

	return &Authorizer{grants: grants}, nil
}

// LoadConfigFile parses a JSON auth config from disk without constructing grants.
func LoadConfigFile(path string) (Config, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Config{}, fmt.Errorf("auth config path is empty")
	}

	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()

	var cfg Config
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// AuthenticateRequest authenticates the request Authorization header.
// When the authorizer is nil, the request is treated as unrestricted.
func (a *Authorizer) AuthenticateRequest(req *http.Request) (*Grant, bool) {
	if a == nil {
		return nil, true
	}
	if req == nil {
		return nil, false
	}
	return a.AuthenticateHeader(req.Header.Get("Authorization"))
}

// AuthenticateHeader authenticates a raw Authorization header value.
func (a *Authorizer) AuthenticateHeader(header string) (*Grant, bool) {
	if a == nil {
		return nil, true
	}
	token, ok := bearerTokenFromHeader(header)
	if !ok {
		return nil, false
	}
	grant, ok := a.grants[token]
	if !ok || grant == nil {
		return nil, false
	}
	if grant.revoked || grant.IsExpiredAt(time.Now()) {
		return nil, false
	}
	return grant, true
}

// AllowsScope reports whether the grant includes the given scope.
func (g *Grant) AllowsScope(scope string) bool {
	if g == nil {
		return true
	}
	scope = normalizeScope(scope)
	if scope == "" {
		return true
	}
	_, ok := g.scopes[scope]
	return ok
}

// AllowsWorkspace reports whether the grant allows the given workspace id.
func (g *Grant) AllowsWorkspace(workspaceID string) bool {
	if g == nil || len(g.workspacePrefixes) == 0 {
		return true
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return false
	}
	for _, prefix := range g.workspacePrefixes {
		if strings.HasPrefix(workspaceID, prefix) {
			return true
		}
	}
	return false
}

// AllowsProvider reports whether the grant allows the given provider.
func (g *Grant) AllowsProvider(provider string) bool {
	if g == nil || len(g.allowedProviders) == 0 {
		return true
	}
	provider = normalizeName(provider)
	if provider == "" {
		return true
	}
	_, ok := g.allowedProviders[provider]
	return ok
}

// AllowedProviders returns the normalized provider allowlist, if any.
func (g *Grant) AllowedProviders() []string {
	if g == nil || len(g.allowedProviders) == 0 {
		return nil
	}
	out := make([]string, 0, len(g.allowedProviders))
	for provider := range g.allowedProviders {
		out = append(out, provider)
	}
	sort.Strings(out)
	return out
}

// AllowsMode reports whether the grant allows the given routing mode.
func (g *Grant) AllowsMode(mode string) bool {
	if g == nil || len(g.allowedModes) == 0 {
		return true
	}
	mode = normalizeMode(mode)
	if mode == "" {
		return false
	}
	_, ok := g.allowedModes[mode]
	return ok
}

// TokenID returns a stable, non-secret identifier for the token.
func (g *Grant) TokenID() string {
	if g == nil {
		return ""
	}
	return g.tokenID
}

// Description returns the optional human-readable token description.
func (g *Grant) Description() string {
	if g == nil {
		return ""
	}
	return g.description
}

// UserID returns the user associated with the token, if any.
func (g *Grant) UserID() string {
	if g == nil {
		return ""
	}
	return g.userID
}

// OrganizationID returns the organization associated with the token, if any.
func (g *Grant) OrganizationID() string {
	if g == nil {
		return ""
	}
	return g.organizationID
}

// ProjectID returns the project associated with the token, if any.
func (g *Grant) ProjectID() string {
	if g == nil {
		return ""
	}
	return g.projectID
}

// Scopes returns the sorted scope set held by the grant.
func (g *Grant) Scopes() []string {
	if g == nil || len(g.scopes) == 0 {
		return nil
	}
	out := make([]string, 0, len(g.scopes))
	for scope := range g.scopes {
		out = append(out, scope)
	}
	sort.Strings(out)
	return out
}

// ExpiresAt returns the grant expiry, or the zero time when it never expires.
func (g *Grant) ExpiresAt() time.Time {
	if g == nil {
		return time.Time{}
	}
	return g.expiresAt
}

// WorkspacePrefixes returns the workspace prefix allowlist, if any.
func (g *Grant) WorkspacePrefixes() []string {
	if g == nil || len(g.workspacePrefixes) == 0 {
		return nil
	}
	return append([]string(nil), g.workspacePrefixes...)
}

// AllowedModes returns the normalized mode allowlist, if any.
func (g *Grant) AllowedModes() []string {
	if g == nil || len(g.allowedModes) == 0 {
		return nil
	}
	out := make([]string, 0, len(g.allowedModes))
	for mode := range g.allowedModes {
		out = append(out, mode)
	}
	sort.Strings(out)
	return out
}

// MaxRequestsPerHour returns the hourly request quota (0 = unlimited).
func (g *Grant) MaxRequestsPerHour() int {
	if g == nil {
		return 0
	}
	return g.maxRequestsPerHour
}

// MaxRequestsPerDay returns the daily request quota (0 = unlimited).
func (g *Grant) MaxRequestsPerDay() int {
	if g == nil {
		return 0
	}
	return g.maxRequestsPerDay
}

// MaxCostUSDPerDay returns the daily spend budget (0 = unlimited).
func (g *Grant) MaxCostUSDPerDay() float64 {
	if g == nil {
		return 0
	}
	return g.maxCostUSDPerDay
}

// MaxCostUSDPerMonth returns the monthly spend budget (0 = unlimited).
func (g *Grant) MaxCostUSDPerMonth() float64 {
	if g == nil {
		return 0
	}
	return g.maxCostUSDPerMonth
}

// CheckCostBudgetAt reports whether the token has already exhausted its spend
// budget before processing another request.
func (g *Grant) CheckCostBudgetAt(now time.Time) error {
	if g == nil || (g.maxCostUSDPerDay <= 0 && g.maxCostUSDPerMonth <= 0) {
		return nil
	}

	utcNow := now.UTC()
	dayWindow := time.Date(utcNow.Year(), utcNow.Month(), utcNow.Day(), 0, 0, 0, 0, time.UTC)
	monthWindow := time.Date(utcNow.Year(), utcNow.Month(), 1, 0, 0, 0, 0, time.UTC)

	u := g.usage
	u.mu.Lock()
	defer u.mu.Unlock()

	u.resetCostWindowsLocked(dayWindow, monthWindow)
	if g.maxCostUSDPerDay > 0 && u.costDaySpent >= g.maxCostUSDPerDay {
		return ErrDailyCostExceeded
	}
	if g.maxCostUSDPerMonth > 0 && u.costMonthSpent >= g.maxCostUSDPerMonth {
		return ErrMonthlyCostExceeded
	}
	return nil
}

// RecordCostAt records the realized usage cost against the token's spend
// budgets. Zero-cost requests are ignored.
func (g *Grant) RecordCostAt(now time.Time, costUSD float64) {
	if g == nil || costUSD <= 0 || (g.maxCostUSDPerDay <= 0 && g.maxCostUSDPerMonth <= 0) {
		return
	}

	utcNow := now.UTC()
	dayWindow := time.Date(utcNow.Year(), utcNow.Month(), utcNow.Day(), 0, 0, 0, 0, time.UTC)
	monthWindow := time.Date(utcNow.Year(), utcNow.Month(), 1, 0, 0, 0, 0, time.UTC)

	u := g.usage
	u.mu.Lock()
	defer u.mu.Unlock()

	u.resetCostWindowsLocked(dayWindow, monthWindow)
	if g.maxCostUSDPerDay > 0 {
		u.costDaySpent += costUSD
	}
	if g.maxCostUSDPerMonth > 0 {
		u.costMonthSpent += costUSD
	}
}

// IsExpiredAt reports whether the token is expired at the supplied time.
func (g *Grant) IsExpiredAt(now time.Time) bool {
	if g == nil || g.expiresAt.IsZero() {
		return false
	}
	return !now.Before(g.expiresAt)
}

// AllowRequestAt consumes one request from the token's hourly quota.
func (g *Grant) AllowRequestAt(now time.Time) bool {
	return g.CheckAndConsumeRequestAt(now) == nil
}

// CheckAndConsumeRequestAt consumes one request from the token's quotas and
// returns a descriptive error when the request would exceed them.
func (g *Grant) CheckAndConsumeRequestAt(now time.Time) error {
	if g == nil || (g.maxRequestsPerHour <= 0 && g.maxRequestsPerDay <= 0) {
		return nil
	}

	utcNow := now.UTC()
	hourWindow := utcNow.Truncate(time.Hour)
	dayWindow := time.Date(utcNow.Year(), utcNow.Month(), utcNow.Day(), 0, 0, 0, 0, time.UTC)

	u := g.usage
	u.mu.Lock()
	defer u.mu.Unlock()

	if g.maxRequestsPerHour > 0 {
		if u.quotaWindowStart.IsZero() || !u.quotaWindowStart.Equal(hourWindow) {
			u.quotaWindowStart = hourWindow
			u.quotaWindowCount = 0
		}
		if u.quotaWindowCount >= g.maxRequestsPerHour {
			return ErrHourlyQuotaExceeded
		}
	}
	if g.maxRequestsPerDay > 0 {
		if u.quotaDayStart.IsZero() || !u.quotaDayStart.Equal(dayWindow) {
			u.quotaDayStart = dayWindow
			u.quotaDayCount = 0
		}
		if u.quotaDayCount >= g.maxRequestsPerDay {
			return ErrDailyQuotaExceeded
		}
	}
	if g.maxRequestsPerHour > 0 {
		u.quotaWindowCount++
	}
	if g.maxRequestsPerDay > 0 {
		u.quotaDayCount++
	}
	return nil
}

// adoptUsageFrom shares the previous grant's usage counter state so rebuilding
// an authorizer neither resets accrued usage nor loses increments that are
// still in flight against the old grant. Because the SAME *grantUsage is shared
// (not copied), a request that captured the old grant before the swap and
// increments it afterwards accrues against the same mutex-protected counters
// the new grant reads.
func (g *Grant) adoptUsageFrom(prev *Grant) {
	if g == nil || prev == nil || prev.usage == nil {
		return
	}
	g.usage = prev.usage
}

// carryOverGrantUsage preserves in-memory quota and spend counters across
// authorizer rebuilds by matching grants on their storage key (token value or
// token hash) and sharing the previous grant's usage state with the new grant.
// Sharing rather than copying closes the copy-then-swap race: an in-flight
// increment on the old grant is never lost because both grants reference one
// shared, mutex-guarded counter set.
// TODO: counters are still in-memory only; durable usage accounting across
// restarts and multiple server instances is a larger design item.
func carryOverGrantUsage(newGrants, oldGrants map[string]*Grant) {
	if len(newGrants) == 0 || len(oldGrants) == 0 {
		return
	}
	for key, next := range newGrants {
		next.adoptUsageFrom(oldGrants[key])
	}
}

func (u *grantUsage) resetCostWindowsLocked(dayWindow, monthWindow time.Time) {
	if !dayWindow.IsZero() && (u.costDayStart.IsZero() || !u.costDayStart.Equal(dayWindow)) {
		u.costDayStart = dayWindow
		u.costDaySpent = 0
	}
	if !monthWindow.IsZero() && (u.costMonthStart.IsZero() || !u.costMonthStart.Equal(monthWindow)) {
		u.costMonthStart = monthWindow
		u.costMonthSpent = 0
	}
}

func newGrant(rule TokenRule) (*Grant, error) {
	if len(rule.Scopes) == 0 {
		return nil, errors.New("scopes must not be empty")
	}
	if rule.MaxRequestsPerHour < 0 {
		return nil, errors.New("max_requests_per_hour must be >= 0")
	}
	if rule.MaxRequestsPerDay < 0 {
		return nil, errors.New("max_requests_per_day must be >= 0")
	}
	if rule.MaxCostUSDPerDay < 0 {
		return nil, errors.New("max_cost_usd_per_day must be >= 0")
	}
	if rule.MaxCostUSDPerMonth < 0 {
		return nil, errors.New("max_cost_usd_per_month must be >= 0")
	}

	scopeSet := make(map[string]struct{}, len(rule.Scopes))
	for _, scope := range rule.Scopes {
		scope = normalizeScope(scope)
		if scope == "" {
			return nil, errors.New("scope must not be empty")
		}
		if _, ok := validScopes[scope]; !ok {
			return nil, fmt.Errorf("unknown scope %q", scope)
		}
		scopeSet[scope] = struct{}{}
	}

	var workspacePrefixes []string
	for _, prefix := range rule.WorkspacePrefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		workspacePrefixes = append(workspacePrefixes, prefix)
	}
	sort.Strings(workspacePrefixes)
	workspacePrefixes = compactStrings(workspacePrefixes)

	providers := make(map[string]struct{})
	for _, provider := range rule.AllowedProviders {
		provider = normalizeName(provider)
		if provider == "" {
			continue
		}
		providers[provider] = struct{}{}
	}
	if len(providers) == 0 {
		providers = nil
	}

	modes := make(map[string]struct{})
	for _, mode := range rule.AllowedModes {
		mode = normalizeMode(mode)
		if mode == "" {
			continue
		}
		if _, ok := validModes[mode]; !ok {
			return nil, fmt.Errorf("unknown mode %q", mode)
		}
		modes[mode] = struct{}{}
	}
	if len(modes) == 0 {
		modes = nil
	}

	tokenID := strings.TrimSpace(rule.ID)
	if tokenID == "" {
		tokenID = defaultTokenID(rule.Token)
	}

	return &Grant{
		tokenID:            tokenID,
		description:        strings.TrimSpace(rule.Description),
		userID:             strings.TrimSpace(rule.UserID),
		organizationID:     strings.TrimSpace(rule.OrganizationID),
		projectID:          strings.TrimSpace(rule.ProjectID),
		expiresAt:          rule.ExpiresAt.UTC(),
		revoked:            rule.Revoked,
		maxRequestsPerHour: rule.MaxRequestsPerHour,
		maxRequestsPerDay:  rule.MaxRequestsPerDay,
		maxCostUSDPerDay:   rule.MaxCostUSDPerDay,
		maxCostUSDPerMonth: rule.MaxCostUSDPerMonth,
		scopes:             scopeSet,
		workspacePrefixes:  workspacePrefixes,
		allowedProviders:   providers,
		allowedModes:       modes,
		usage:              &grantUsage{},
	}, nil
}

// GrantFromRule constructs an in-memory grant from a token rule without
// persisting the underlying token. It is used for ephemeral session auth such
// as browser-admin cookies.
func GrantFromRule(rule TokenRule) (*Grant, error) {
	return newGrant(rule)
}

func bearerTokenFromHeader(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", false
	}
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", false
	}
	return token, true
}

func normalizeScope(scope string) string {
	return strings.ToLower(strings.TrimSpace(scope))
}

func normalizeMode(mode string) string {
	return strings.ToLower(strings.TrimSpace(mode))
}

func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func compactStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := values[:0]
	var prev string
	for i, value := range values {
		if i == 0 || value != prev {
			out = append(out, value)
			prev = value
		}
	}
	return out
}

func defaultTokenID(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return "tok_" + hex.EncodeToString(sum[:6])
}

// DerivedTokenID returns the stable non-secret token identifier derived from
// the token value when an explicit ID is not supplied.
func DerivedTokenID(token string) string {
	return defaultTokenID(token)
}

// SanitizedRules returns non-secret token rules suitable for display.
func SanitizedRules(rules []TokenRule) []TokenRuleView {
	if len(rules) == 0 {
		return nil
	}
	views := make([]TokenRuleView, 0, len(rules))
	for _, rule := range rules {
		id := strings.TrimSpace(rule.ID)
		if id == "" {
			id = DerivedTokenID(rule.Token)
		}
		views = append(views, TokenRuleView{
			ID:                 id,
			Description:        rule.Description,
			UserID:             strings.TrimSpace(rule.UserID),
			OrganizationID:     strings.TrimSpace(rule.OrganizationID),
			ProjectID:          strings.TrimSpace(rule.ProjectID),
			Scopes:             append([]string(nil), rule.Scopes...),
			WorkspacePrefixes:  append([]string(nil), rule.WorkspacePrefixes...),
			AllowedProviders:   append([]string(nil), rule.AllowedProviders...),
			AllowedModes:       append([]string(nil), rule.AllowedModes...),
			ExpiresAt:          rule.ExpiresAt,
			Revoked:            rule.Revoked,
			MaxRequestsPerHour: rule.MaxRequestsPerHour,
			MaxRequestsPerDay:  rule.MaxRequestsPerDay,
			MaxCostUSDPerDay:   rule.MaxCostUSDPerDay,
			MaxCostUSDPerMonth: rule.MaxCostUSDPerMonth,
		})
	}
	return views
}

// IssueTokenRule validates and appends a token rule, backfilling a stable ID.
func IssueTokenRule(cfg *Config, rule TokenRule) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("auth config is nil")
	}
	if strings.TrimSpace(rule.Token) == "" {
		return "", fmt.Errorf("token value is empty")
	}
	cfg.Tokens = append(cfg.Tokens, rule)
	authz, err := NewAuthorizer(*cfg)
	if err != nil {
		cfg.Tokens = cfg.Tokens[:len(cfg.Tokens)-1]
		return "", err
	}
	grant, ok := authz.AuthenticateHeader("Bearer " + rule.Token)
	if !ok {
		cfg.Tokens = cfg.Tokens[:len(cfg.Tokens)-1]
		return "", fmt.Errorf("failed to authenticate newly issued token")
	}
	finalID := grant.TokenID()
	if cfg.Tokens[len(cfg.Tokens)-1].ID == "" {
		cfg.Tokens[len(cfg.Tokens)-1].ID = finalID
	}
	return finalID, nil
}

// RevokeTokenRule marks the token identified by tokenID as revoked.
func RevokeTokenRule(cfg *Config, tokenID string) error {
	tokenID = strings.TrimSpace(tokenID)
	if cfg == nil || tokenID == "" {
		return fmt.Errorf("token id is required")
	}
	for i := range cfg.Tokens {
		id := strings.TrimSpace(cfg.Tokens[i].ID)
		if id == "" {
			id = DerivedTokenID(cfg.Tokens[i].Token)
		}
		if id != tokenID {
			continue
		}
		cfg.Tokens[i].Revoked = true
		if cfg.Tokens[i].ID == "" {
			cfg.Tokens[i].ID = id
		}
		return nil
	}
	return fmt.Errorf("token %q not found", tokenID)
}

// SaveConfigFile validates and writes cfg to path with restrictive permissions.
func SaveConfigFile(path string, cfg Config) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("auth config path is empty")
	}
	if _, err := NewAuthorizer(cfg); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

// GenerateToken creates a random bearer token suitable for auth config issuance.
func GenerateToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "mw_" + base64.RawURLEncoding.EncodeToString(buf), nil
}
