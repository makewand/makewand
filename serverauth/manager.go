package serverauth

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Manager provides live token administration backed by an auth config file.
type Manager struct {
	path string

	mu       sync.RWMutex
	cfg      Config
	auth     *Authorizer
	lastMod  time.Time
	lastSize int64
}

// LoadManager loads a live token manager from path.
func LoadManager(path string) (*Manager, error) {
	trimmed := strings.TrimSpace(path)
	cfg, err := LoadConfigFile(trimmed)
	if err != nil {
		return nil, err
	}
	authz, err := NewAuthorizer(cfg)
	if err != nil {
		return nil, err
	}
	mgr := &Manager{
		path: trimmed,
		cfg:  cfg,
		auth: authz,
	}
	if fi, err := os.Stat(trimmed); err == nil {
		mgr.lastMod = fi.ModTime()
		mgr.lastSize = fi.Size()
	}
	return mgr, nil
}

// ReloadIfChanged reloads the on-disk auth config file if its modtime or size changed,
// carrying over active grant usage counters to prevent losing in-flight quotas.
func (m *Manager) ReloadIfChanged() error {
	if m == nil || m.path == "" {
		return nil
	}
	fi, err := os.Stat(m.path)
	if err != nil {
		return err
	}

	m.mu.RLock()
	unchanged := fi.ModTime().Equal(m.lastMod) && fi.Size() == m.lastSize
	m.mu.RUnlock()
	if unchanged {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check under write lock
	fi, err = os.Stat(m.path)
	if err != nil {
		return err
	}
	if fi.ModTime().Equal(m.lastMod) && fi.Size() == m.lastSize {
		return nil
	}

	cfg, err := LoadConfigFile(m.path)
	if err != nil {
		return err
	}
	authz, err := NewAuthorizer(cfg)
	if err != nil {
		return err
	}
	if m.auth != nil {
		carryOverGrantUsage(authz.grants, m.auth.grants)
	}
	m.cfg = cfg
	m.auth = authz
	m.lastMod = fi.ModTime()
	m.lastSize = fi.Size()
	return nil
}

// AuthenticateRequest authenticates a request using the current in-memory authorizer,
// reloading from disk if the auth config was modified offline.
func (m *Manager) AuthenticateRequest(req *http.Request) (*Grant, bool) {
	if m == nil {
		return nil, false
	}
	_ = m.ReloadIfChanged()
	m.mu.RLock()
	authz := m.auth
	m.mu.RUnlock()
	return authz.AuthenticateRequest(req)
}

// Path returns the on-disk auth config path managed by this instance.
func (m *Manager) Path() string {
	if m == nil {
		return ""
	}
	return m.path
}

// TokenRules returns the current sanitized token rules,
// reloading from disk if the auth config was modified offline.
func (m *Manager) TokenRules() []TokenRuleView {
	if m == nil {
		return nil
	}
	_ = m.ReloadIfChanged()
	m.mu.RLock()
	defer m.mu.RUnlock()
	return SanitizedRules(m.cfg.Tokens)
}

// Issue appends a new token rule, persists it, and reloads the active authorizer.
func (m *Manager) Issue(rule TokenRule) (TokenRuleView, string, error) {
	if m == nil {
		return TokenRuleView{}, "", http.ErrServerClosed
	}
	if strings.TrimSpace(rule.Token) == "" {
		token, err := GenerateToken()
		if err != nil {
			return TokenRuleView{}, "", err
		}
		rule.Token = token
	}

	unlock, err := LockConfigFile(m.path, true)
	if err != nil {
		return TokenRuleView{}, "", err
	}
	defer unlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Read-modify-write: load fresh state from disk under lock
	diskCfg, err := LoadConfigFile(m.path)
	if err != nil {
		diskCfg = cloneConfig(m.cfg)
	}
	cfg := cloneConfig(diskCfg)
	tokenValue := rule.Token
	finalID, err := IssueTokenRule(&cfg, rule)
	if err != nil {
		return TokenRuleView{}, "", err
	}
	if err := SaveConfigFile(m.path, cfg); err != nil {
		return TokenRuleView{}, "", err
	}
	authz, err := NewAuthorizer(cfg)
	if err != nil {
		return TokenRuleView{}, "", err
	}
	if m.auth != nil {
		carryOverGrantUsage(authz.grants, m.auth.grants)
	}
	m.cfg = cfg
	m.auth = authz
	if fi, err := os.Stat(m.path); err == nil {
		m.lastMod = fi.ModTime()
		m.lastSize = fi.Size()
	}

	views := SanitizedRules([]TokenRule{cfg.Tokens[len(cfg.Tokens)-1]})
	if len(views) == 0 {
		return TokenRuleView{}, tokenValue, nil
	}
	views[0].ID = finalID
	return views[0], tokenValue, nil
}

// Revoke marks a token revoked, persists the file, and reloads the authorizer.
func (m *Manager) Revoke(tokenID string) error {
	if m == nil {
		return http.ErrServerClosed
	}

	unlock, err := LockConfigFile(m.path, true)
	if err != nil {
		return err
	}
	defer unlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	diskCfg, err := LoadConfigFile(m.path)
	if err != nil {
		diskCfg = cloneConfig(m.cfg)
	}
	cfg := cloneConfig(diskCfg)
	if err := RevokeTokenRule(&cfg, tokenID); err != nil {
		return err
	}
	if err := SaveConfigFile(m.path, cfg); err != nil {
		return err
	}
	authz, err := NewAuthorizer(cfg)
	if err != nil {
		return err
	}
	if m.auth != nil {
		carryOverGrantUsage(authz.grants, m.auth.grants)
	}
	m.cfg = cfg
	m.auth = authz
	if fi, err := os.Stat(m.path); err == nil {
		m.lastMod = fi.ModTime()
		m.lastSize = fi.Size()
	}
	return nil
}

// RevokeByUserID marks all tokens for a user revoked, persists the file, and reloads the authorizer.
func (m *Manager) RevokeByUserID(userID string) error {
	if m == nil {
		return http.ErrServerClosed
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil
	}

	unlock, err := LockConfigFile(m.path, true)
	if err != nil {
		return err
	}
	defer unlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	diskCfg, err := LoadConfigFile(m.path)
	if err != nil {
		diskCfg = cloneConfig(m.cfg)
	}
	cfg := cloneConfig(diskCfg)
	changed := false
	for i := range cfg.Tokens {
		if strings.TrimSpace(cfg.Tokens[i].UserID) == userID && !cfg.Tokens[i].Revoked {
			cfg.Tokens[i].Revoked = true
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := SaveConfigFile(m.path, cfg); err != nil {
		return err
	}
	authz, err := NewAuthorizer(cfg)
	if err != nil {
		return err
	}
	if m.auth != nil {
		carryOverGrantUsage(authz.grants, m.auth.grants)
	}
	m.cfg = cfg
	m.auth = authz
	if fi, err := os.Stat(m.path); err == nil {
		m.lastMod = fi.ModTime()
		m.lastSize = fi.Size()
	}
	return nil
}

// cloneConfig copies cfg deeply enough for Issue/Revoke edits, which append to
// or modify elements of Tokens.
func cloneConfig(cfg Config) Config {
	out := cfg
	out.Tokens = append([]TokenRule(nil), cfg.Tokens...)
	return out
}
