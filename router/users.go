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
	"strings"
	"time"

	"github.com/makewand/makewand/serverauth"
	"golang.org/x/crypto/argon2"
)

var (
	// ErrUserExists indicates the registration email is already present.
	ErrUserExists = errors.New("user already exists")
	// ErrUserNotFound indicates no stored user matched the requested email.
	ErrUserNotFound = errors.New("user not found")
)

// User represents a registered user in the system.
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"password_hash"`
	Salt         string    `json:"salt"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	IsActive     bool      `json:"is_active"`
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
	CreatedAt time.Time `json:"created_at"`
	Message   string    `json:"message"`
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
	users, err := us.loadUsers()
	if err != nil {
		return nil, fmt.Errorf("load users: %w", err)
	}

	// Check if user already exists
	for _, user := range users {
		if strings.ToLower(user.Email) == strings.ToLower(email) {
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
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
		IsActive:     true,
	}

	users[user.ID] = user

	if err := us.saveUsers(users); err != nil {
		return nil, fmt.Errorf("save users: %w", err)
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
		if strings.ToLower(user.Email) == strings.ToLower(email) {
			return user, nil
		}
	}

	return nil, ErrUserNotFound
}

// ValidatePassword checks if the provided password matches the user's password.
func (u *User) ValidatePassword(password string) bool {
	expectedHash := hashPassword(password, u.Salt)
	return subtle.ConstantTimeCompare([]byte(expectedHash), []byte(u.PasswordHash)) == 1
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

// HandleUserRegistration handles POST /v1/users/register requests.
func (r *Router) HandleUserRegistration(userStore *UserStore) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			writeHTTPError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
			return
		}

		var regReq UserRegistrationRequest
		if err := json.NewDecoder(req.Body).Decode(&regReq); err != nil {
			writeHTTPError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
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

		// Create user
		user, err := userStore.CreateUser(regReq.Email, regReq.Password)
		if err != nil {
			if errors.Is(err, ErrUserExists) {
				writeHTTPError(w, http.StatusConflict, "user_exists", err.Error())
				return
			}
			writeHTTPError(w, http.StatusInternalServerError, "registration_failed", "failed to create user account")
			return
		}

		// Return success response
		resp := UserRegistrationResponse{
			ID:        user.ID,
			Email:     user.Email,
			CreatedAt: user.CreatedAt,
			Message:   "User account created successfully",
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// HTTPHandlerWithUsers returns an http.Handler that includes user registration
// endpoints in addition to the existing chat completion functionality.
func (r *Router) HTTPHandlerWithUsers(userStore *UserStore, opts ...HTTPHandlerOptions) http.Handler {
	var opt HTTPHandlerOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	authz := authorizerForHTTPOptions(opt)

	mux := http.NewServeMux()

	// Existing endpoints
	mux.HandleFunc("/v1/chat/completions", r.requireScope(authz, serverauth.ScopeChatInvoke, r.handleChatCompletionsWithOptions(opt)))
	mux.HandleFunc("/v1/models", r.requireScope(authz, serverauth.ScopeModelsRead, r.handleListModelsWithGrant))

	// User management endpoints
	mux.HandleFunc("/v1/users/register", r.HandleUserRegistration(userStore))

	// Health check (always unauthenticated)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	return mux
}
