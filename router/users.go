package router

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverteam"
	"golang.org/x/crypto/argon2"
)

var (
	// ErrUserExists indicates the registration email is already present.
	ErrUserExists = errors.New("user already exists")
	// ErrUserNotFound indicates no stored user matched the requested email.
	ErrUserNotFound = errors.New("user not found")
	// ErrInvalidUserRole indicates an unsupported role value.
	ErrInvalidUserRole = errors.New("invalid user role")
)

const (
	UserRoleMember = "member"
	UserRoleAdmin  = "admin"
)

// User represents a registered user in the system.
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"password_hash"`
	Salt         string    `json:"salt"`
	Role         string    `json:"role,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	IsActive     bool      `json:"is_active"`
}

// UserView is the safe operator-facing representation of a stored user.
type UserView struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	IsActive  bool      `json:"is_active"`
}

// UserRegistrationRequest represents the registration request payload.
type UserRegistrationRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// UserRegistrationResponse represents the registration response.
type UserRegistrationResponse struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	Message   string    `json:"message"`
}

// UserLoginRequest represents a password-based login request.
type UserLoginRequest struct {
	Email          string `json:"email"`
	Password       string `json:"password"`
	OrganizationID string `json:"organization_id,omitempty"`
	ProjectID      string `json:"project_id,omitempty"`
}

// UserLoginResponse returns a newly issued bearer token for the user.
type UserLoginResponse struct {
	Token     string                   `json:"token"`
	TokenID   string                   `json:"token_id"`
	ExpiresAt time.Time                `json:"expires_at,omitempty"`
	Scopes    []string                 `json:"scopes"`
	User      UserView                 `json:"user"`
	Rule      serverauth.TokenRuleView `json:"rule"`
}

// UserManager is the storage contract required by registration, login, and admin user management.
type UserManager interface {
	CreateUser(email, password string) (*User, error)
	CreateUserWithRole(email, password, role string) (*User, error)
	// CreateUserWithRoleActive creates a user with an explicit role and initial
	// active state persisted in a single write. Self-registration uses it to
	// create inactive accounts atomically (no active window, no create-then-
	// deactivate dance).
	CreateUserWithRoleActive(email, password, role string, active bool) (*User, error)
	GetUserByID(userID string) (*User, error)
	GetUserByEmail(email string) (*User, error)
	ListUsers() ([]UserView, error)
	SetUserActive(userID string, active bool) (*User, error)
	SetUserRole(userID, role string) (*User, error)
	SetUserPassword(userID, password string) (*User, error)
}

// UserStore manages user data persistence.
type UserStore struct {
	dataDir string
}

// NewUserStore creates a new UserStore with the given data directory.
func NewUserStore(dataDir string) *UserStore {
	return &UserStore{dataDir: dataDir}
}

// usersFilePath returns the path to the users.json file.
func (us *UserStore) usersFilePath() string {
	return filepath.Join(us.dataDir, "users.json")
}

// ensureDataDir creates the data directory if it doesn't exist.
func (us *UserStore) ensureDataDir() error {
	return os.MkdirAll(us.dataDir, 0700)
}

// loadUsers loads all users from the JSON file.
func (us *UserStore) loadUsers() (map[string]*User, error) {
	users := make(map[string]*User)

	filePath := us.usersFilePath()
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return users, nil // Empty store for new installations
		}
		return nil, fmt.Errorf("read users file: %w", err)
	}

	if err := json.Unmarshal(data, &users); err != nil {
		return nil, fmt.Errorf("parse users file: %w", err)
	}
	for _, user := range users {
		if user == nil {
			continue
		}
		if user.Role == "" {
			user.Role = UserRoleMember
		}
	}

	return users, nil
}

// saveUsers saves all users to the JSON file.
func (us *UserStore) saveUsers(users map[string]*User) error {
	if err := us.ensureDataDir(); err != nil {
		return fmt.Errorf("ensure data dir: %w", err)
	}

	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal users: %w", err)
	}

	filePath := us.usersFilePath()
	if err := os.WriteFile(filePath, data, 0600); err != nil {
		return fmt.Errorf("write users file: %w", err)
	}

	return nil
}

// CreateUser creates a new user account.
func (us *UserStore) CreateUser(email, password string) (*User, error) {
	return us.CreateUserWithRole(email, password, UserRoleMember)
}

// CreateUserWithRole creates a new user account with an explicit role.
func (us *UserStore) CreateUserWithRole(email, password, role string) (*User, error) {
	return us.CreateUserWithRoleActive(email, password, role, true)
}

// CreateUserWithRoleActive creates a new user account with an explicit role and
// initial active state persisted in a single write.
func (us *UserStore) CreateUserWithRoleActive(email, password, role string, active bool) (*User, error) {
	users, err := us.loadUsers()
	if err != nil {
		return nil, fmt.Errorf("load users: %w", err)
	}
	role, err = normalizeUserRole(role)
	if err != nil {
		return nil, err
	}

	// Check if user already exists
	for _, user := range users {
		if strings.EqualFold(user.Email, email) {
			return nil, fmt.Errorf("%w: %s", ErrUserExists, email)
		}
	}

	// Generate salt and hash password
	salt := generateSalt()
	passwordHash := hashPassword(password, salt)

	// Create new user
	user := &User{
		ID:           generateUserID(),
		Email:        strings.ToLower(email),
		PasswordHash: passwordHash,
		Salt:         salt,
		Role:         role,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
		IsActive:     active,
	}

	users[user.ID] = user

	if err := us.saveUsers(users); err != nil {
		return nil, fmt.Errorf("save users: %w", err)
	}

	return user, nil
}

// GetUserByID retrieves a user by ID.
func (us *UserStore) GetUserByID(userID string) (*User, error) {
	users, err := us.loadUsers()
	if err != nil {
		return nil, fmt.Errorf("load users: %w", err)
	}
	user, ok := users[strings.TrimSpace(userID)]
	if !ok || user == nil {
		return nil, ErrUserNotFound
	}
	return user, nil
}

// GetUserByEmail retrieves a user by email address.
func (us *UserStore) GetUserByEmail(email string) (*User, error) {
	users, err := us.loadUsers()
	if err != nil {
		return nil, fmt.Errorf("load users: %w", err)
	}

	for _, user := range users {
		if strings.EqualFold(user.Email, email) {
			return user, nil
		}
	}

	return nil, ErrUserNotFound
}

// ListUsers returns all users as safe operator-facing views.
func (us *UserStore) ListUsers() ([]UserView, error) {
	users, err := us.loadUsers()
	if err != nil {
		return nil, fmt.Errorf("load users: %w", err)
	}
	ids := make([]string, 0, len(users))
	for id := range users {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	views := make([]UserView, 0, len(ids))
	for _, id := range ids {
		if user := users[id]; user != nil {
			views = append(views, user.View())
		}
	}
	return views, nil
}

// SetUserActive updates a user's active flag.
func (us *UserStore) SetUserActive(userID string, active bool) (*User, error) {
	users, err := us.loadUsers()
	if err != nil {
		return nil, fmt.Errorf("load users: %w", err)
	}
	user, ok := users[strings.TrimSpace(userID)]
	if !ok || user == nil {
		return nil, ErrUserNotFound
	}
	user.IsActive = active
	user.UpdatedAt = time.Now().UTC()
	if err := us.saveUsers(users); err != nil {
		return nil, fmt.Errorf("save users: %w", err)
	}
	return user, nil
}

// SetUserRole updates a user's role.
func (us *UserStore) SetUserRole(userID, role string) (*User, error) {
	users, err := us.loadUsers()
	if err != nil {
		return nil, fmt.Errorf("load users: %w", err)
	}
	user, ok := users[strings.TrimSpace(userID)]
	if !ok || user == nil {
		return nil, ErrUserNotFound
	}
	role, err = normalizeUserRole(role)
	if err != nil {
		return nil, err
	}
	user.Role = role
	user.UpdatedAt = time.Now().UTC()
	if err := us.saveUsers(users); err != nil {
		return nil, fmt.Errorf("save users: %w", err)
	}
	return user, nil
}

func (us *UserStore) SetUserPassword(userID, password string) (*User, error) {
	if !isValidPassword(password) {
		return nil, fmt.Errorf("password must be at least 8 characters long")
	}
	users, err := us.loadUsers()
	if err != nil {
		return nil, fmt.Errorf("load users: %w", err)
	}
	user, ok := users[strings.TrimSpace(userID)]
	if !ok || user == nil {
		return nil, ErrUserNotFound
	}
	user.Salt = generateSalt()
	user.PasswordHash = hashPassword(password, user.Salt)
	user.UpdatedAt = time.Now().UTC()
	if err := us.saveUsers(users); err != nil {
		return nil, fmt.Errorf("save users: %w", err)
	}
	return user, nil
}

// ValidatePassword checks if the provided password matches the user's password.
func (u *User) ValidatePassword(password string) bool {
	expectedHash := hashPassword(password, u.Salt)
	return subtle.ConstantTimeCompare([]byte(expectedHash), []byte(u.PasswordHash)) == 1
}

// View returns the sanitized form of a stored user.
func (u *User) View() UserView {
	if u == nil {
		return UserView{}
	}
	role := u.Role
	if role == "" {
		role = UserRoleMember
	}
	return UserView{
		ID:        u.ID,
		Email:     u.Email,
		Role:      role,
		CreatedAt: u.CreatedAt,
		UpdatedAt: u.UpdatedAt,
		IsActive:  u.IsActive,
	}
}

// generateSalt creates a random salt for password hashing.
func generateSalt() string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(fmt.Sprintf("failed to generate salt: %v", err))
	}
	return fmt.Sprintf("%x", salt)
}

// hashPassword hashes a password using Argon2id.
func hashPassword(password, salt string) string {
	saltBytes := []byte(salt)
	hash := argon2.IDKey([]byte(password), saltBytes, 1, 64*1024, 4, 32)
	return fmt.Sprintf("%x", hash)
}

// generateUserID creates a unique user ID.
func generateUserID() string {
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		panic(fmt.Sprintf("failed to generate user ID: %v", err))
	}
	return fmt.Sprintf("usr_%x", id)
}

// isValidEmail performs basic email validation.
func isValidEmail(email string) bool {
	emailRegex := regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	return emailRegex.MatchString(email)
}

// isValidPassword checks password requirements.
func isValidPassword(password string) bool {
	return len(password) >= 8
}

func normalizeUserRole(role string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "", UserRoleMember:
		return UserRoleMember, nil
	case UserRoleAdmin:
		return UserRoleAdmin, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidUserRole, role)
	}
}

// UserEndpointOptions configures the optional self-service user endpoints.
type UserEndpointOptions struct {
	// EnableRegistration mounts POST /v1/users/register for public self-signup.
	// It is disabled by default: enabling users (auth + login + admin
	// management) does not open public registration unless this is set.
	EnableRegistration bool

	// RegistrationLimiter bounds concurrent Argon2id hashing and applies per-IP
	// and global registration rate limits. It is used only when registration is
	// enabled; a nil limiter disables throttling (intended for tests).
	RegistrationLimiter *serverauth.RegistrationRateLimiter

	// ActivateOnRegistration makes self-registered accounts immediately active.
	// Default false: new accounts require an admin to activate them before they
	// can log in.
	ActivateOnRegistration bool
}

// HandleUserRegistration handles POST /v1/users/register requests.
func (r *Router) HandleUserRegistration(userStore UserManager, userOpts UserEndpointOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			writeHTTPError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
			return
		}

		var regReq UserRegistrationRequest
		if err := decodeLimitedHTTPJSON(w, req, &regReq); err != nil {
			status, code, message := httpJSONDecodeError(err)
			writeHTTPError(w, status, code, message)
			return
		}

		// Validate email
		if regReq.Email == "" {
			writeHTTPError(w, http.StatusBadRequest, "invalid_request", "email is required")
			return
		}

		if !isValidEmail(regReq.Email) {
			writeHTTPError(w, http.StatusBadRequest, "invalid_request", "invalid email format")
			return
		}

		// Validate password
		if regReq.Password == "" {
			writeHTTPError(w, http.StatusBadRequest, "invalid_request", "password is required")
			return
		}

		if !isValidPassword(regReq.Password) {
			writeHTTPError(w, http.StatusBadRequest, "invalid_request", "password must be at least 8 characters long")
			return
		}

		// Apply per-IP and global registration rate limits before committing any
		// expensive work. The source IP honors forwarding headers only behind a
		// configured trusted proxy.
		limiter := userOpts.RegistrationLimiter
		sourceIP := serverauth.ClientIP(req, limiter.TrustedProxies())
		if !limiter.AllowAt(sourceIP, time.Now().UTC()) {
			writeHTTPError(w, http.StatusTooManyRequests, "rate_limited", "too many registration attempts; try again later")
			return
		}

		// Bound concurrent Argon2id hashing (64 MiB / 4 threads each) so a burst
		// of registrations cannot exhaust host memory/CPU.
		release, ok := limiter.Acquire()
		if !ok {
			writeHTTPError(w, http.StatusServiceUnavailable, "registration_busy", "registration is temporarily busy; try again shortly")
			return
		}
		defer release()

		// Create user. The account's initial active state is persisted in the same
		// write, so a self-registered account is never briefly log-in-able before
		// a follow-up deactivation and can never be left permanently active by a
		// failed deactivation.
		user, err := userStore.CreateUserWithRoleActive(regReq.Email, regReq.Password, UserRoleMember, userOpts.ActivateOnRegistration)
		if err != nil {
			if errors.Is(err, ErrUserExists) {
				writeHTTPError(w, http.StatusConflict, "user_exists", err.Error())
				return
			}
			writeHTTPError(w, http.StatusInternalServerError, "registration_failed", "failed to create user account")
			return
		}

		message := "User account created successfully"
		if !userOpts.ActivateOnRegistration {
			message = "User account created; awaiting administrator activation"
		}

		// Return success response
		resp := UserRegistrationResponse{
			ID:        user.ID,
			Email:     user.Email,
			Role:      user.Role,
			CreatedAt: user.CreatedAt,
			Message:   message,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// HandleUserLogin handles POST /v1/users/login requests and returns a bearer
// token issued from the configured server token manager.
func (r *Router) HandleUserLogin(userStore UserManager, tokenManager serverauth.TokenManager, teamStore serverteam.Store, limiter *serverauth.LoginRateLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			writeHTTPError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
			return
		}
		if userStore == nil || tokenManager == nil {
			writeHTTPError(w, http.StatusNotFound, "not_found", "user login is unavailable")
			return
		}

		var loginReq UserLoginRequest
		if err := decodeLimitedHTTPJSON(w, req, &loginReq); err != nil {
			status, code, message := httpJSONDecodeError(err)
			writeHTTPError(w, status, code, message)
			return
		}
		key := limiter.ThrottleKey(req, loginReq.Email)
		if allowed, retryAfter := limiter.Allow(key, time.Now().UTC()); !allowed {
			writeHTTPError(w, http.StatusTooManyRequests, "rate_limited", fmt.Sprintf("too many failed logins; try again in %s", retryAfter.Round(time.Second)))
			return
		}
		user, err := userStore.GetUserByEmail(loginReq.Email)
		if err != nil || user == nil || !user.IsActive || !user.ValidatePassword(loginReq.Password) {
			limiter.RecordFailure(key, time.Now().UTC())
			writeHTTPError(w, http.StatusUnauthorized, "unauthorized", "invalid email or password")
			return
		}
		limiter.Reset(key)

		expiresAt := time.Now().UTC().Add(24 * time.Hour)
		rule, err := loginRuleForUser(user, loginReq, teamStore, expiresAt)
		if err != nil {
			writeHTTPError(w, http.StatusForbidden, "forbidden", err.Error())
			return
		}
		view, tokenValue, err := tokenManager.Issue(rule)
		if err != nil {
			writeHTTPError(w, http.StatusInternalServerError, "login_failed", "failed to issue login token")
			return
		}

		resp := UserLoginResponse{
			Token:     tokenValue,
			TokenID:   view.ID,
			ExpiresAt: view.ExpiresAt,
			Scopes:    append([]string(nil), view.Scopes...),
			User:      user.View(),
			Rule:      view,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func loginRuleForUser(user *User, req UserLoginRequest, teamStore serverteam.Store, expiresAt time.Time) (serverauth.TokenRule, error) {
	if user == nil {
		return serverauth.TokenRule{}, fmt.Errorf("login rule: nil user")
	}
	rule := serverauth.TokenRule{
		ID:          fmt.Sprintf("%s_%d", user.ID, time.Now().UTC().UnixNano()),
		Description: "user login " + user.Email,
		UserID:      user.ID,
		Scopes:      loginScopesForUser(user),
		ExpiresAt:   expiresAt,
	}
	if strings.EqualFold(user.Role, UserRoleAdmin) {
		rule.OrganizationID = strings.TrimSpace(req.OrganizationID)
		rule.ProjectID = strings.TrimSpace(req.ProjectID)
		return rule, nil
	}
	if teamStore == nil {
		return rule, nil
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID != "" {
		projectMembership, err := teamStore.GetProjectMembership(projectID, user.ID)
		if err != nil || projectMembership == nil || !projectMembership.IsActive {
			return serverauth.TokenRule{}, fmt.Errorf("user is not an active member of project %q", projectID)
		}
		if !serverteam.RoleAtLeast(projectMembership.Role, serverteam.MembershipRoleViewer) {
			return serverauth.TokenRule{}, fmt.Errorf("user is not allowed to access project %q", projectID)
		}
		rule.OrganizationID = projectMembership.OrganizationID
		rule.ProjectID = projectMembership.ProjectID
		return rule, nil
	}
	orgID := strings.TrimSpace(req.OrganizationID)
	if orgID != "" {
		orgMembership, err := teamStore.GetOrganizationMembership(orgID, user.ID)
		if err != nil || orgMembership == nil || !orgMembership.IsActive {
			return serverauth.TokenRule{}, fmt.Errorf("user is not an active member of organization %q", orgID)
		}
		if !serverteam.RoleAtLeast(orgMembership.Role, serverteam.MembershipRoleViewer) {
			return serverauth.TokenRule{}, fmt.Errorf("user is not allowed to access organization %q", orgID)
		}
		rule.OrganizationID = orgMembership.OrganizationID
		return rule, nil
	}
	projectMemberships, err := teamStore.ListProjectMemberships("", user.ID)
	if err == nil {
		activeProjects := make([]serverteam.ProjectMembership, 0, len(projectMemberships))
		for _, membership := range projectMemberships {
			if membership.IsActive {
				activeProjects = append(activeProjects, membership)
			}
		}
		if len(activeProjects) == 1 {
			rule.OrganizationID = activeProjects[0].OrganizationID
			rule.ProjectID = activeProjects[0].ProjectID
			return rule, nil
		}
		if len(activeProjects) > 1 {
			return serverauth.TokenRule{}, fmt.Errorf("multiple project memberships found; specify --project-id")
		}
	}
	orgMemberships, err := teamStore.ListOrganizationMemberships("", user.ID)
	if err == nil {
		activeOrgs := make([]serverteam.OrganizationMembership, 0, len(orgMemberships))
		for _, membership := range orgMemberships {
			if membership.IsActive {
				activeOrgs = append(activeOrgs, membership)
			}
		}
		if len(activeOrgs) == 1 {
			rule.OrganizationID = activeOrgs[0].OrganizationID
			return rule, nil
		}
		if len(activeOrgs) > 1 {
			return serverauth.TokenRule{}, fmt.Errorf("multiple organization memberships found; specify --organization-id or --project-id")
		}
	}
	return serverauth.TokenRule{}, fmt.Errorf("user has no assigned organization or project membership")
}

func loginScopesForUser(user *User) []string {
	if user != nil && strings.EqualFold(user.Role, UserRoleAdmin) {
		return serverauth.AllScopes()
	}
	return serverauth.AllClientScopes()
}

// HTTPHandlerWithUsers returns an http.Handler that includes user login (and,
// when explicitly enabled via userOpts, public registration) in addition to
// the existing chat completion functionality.
func (r *Router) HTTPHandlerWithUsers(userStore UserManager, userOpts UserEndpointOptions, opts ...HTTPHandlerOptions) http.Handler {
	var opt HTTPHandlerOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	base := r.HTTPHandler(opt)
	if userStore == nil {
		return base
	}

	mux := http.NewServeMux()
	// Public self-registration is mounted only when explicitly enabled so that
	// enabling auth does not, by itself, expose an open signup endpoint.
	if userOpts.EnableRegistration {
		mux.HandleFunc("/v1/users/register", r.HandleUserRegistration(userStore, userOpts))
	}
	mux.HandleFunc("/v1/users/login", r.HandleUserLogin(userStore, opt.UserTokenManager, opt.TeamStore, opt.UserLoginLimiter))
	mux.Handle("/", base)
	return mux
}
