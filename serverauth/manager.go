package serverauth

import (
	"net/http"
	"strings"
	"sync"
)

// Manager provides live token administration backed by an auth config file.
type Manager struct {
	path string

	mu   sync.RWMutex
	cfg  Config
	auth *Authorizer
}

// LoadManager loads a live token manager from path.
func LoadManager(path string) (*Manager, error) {
	cfg, err := LoadConfigFile(path)
	if err != nil {
		return nil, err
	}
	authz, err := NewAuthorizer(cfg)
	if err != nil {
		return nil, err
	}
	return &Manager{
		path: strings.TrimSpace(path),
		cfg:  cfg,
		auth: authz,
	}, nil
}

// AuthenticateRequest authenticates a request using the current in-memory authorizer.
func (m *Manager) AuthenticateRequest(req *http.Request) (*Grant, bool) {
	if m == nil {
		return nil, false
	}
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

// TokenRules returns the current sanitized token rules.
func (m *Manager) TokenRules() []TokenRuleView {
	if m == nil {
		return nil
	}
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

	m.mu.Lock()
	defer m.mu.Unlock()

	cfg := m.cfg
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
	// Carry over (share) usage counters and swap the authorizer while holding
	// m.mu, so no window exists during which the new authorizer is visible with
	// reset counters. Sharing the underlying usage state (rather than copying)
	// also means an increment in flight against the previous grant is not lost.
	if m.auth != nil {
		carryOverGrantUsage(authz.grants, m.auth.grants)
	}
	m.cfg = cfg
	m.auth = authz

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

	m.mu.Lock()
	defer m.mu.Unlock()

	cfg := m.cfg
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
	// Share usage counters and swap under m.mu (see Issue): no reset window and
	// no lost in-flight increment on the previous grant.
	if m.auth != nil {
		carryOverGrantUsage(authz.grants, m.auth.grants)
	}
	m.cfg = cfg
	m.auth = authz
	return nil
}
