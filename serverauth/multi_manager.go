package serverauth

import (
	"errors"
	"fmt"
	"net/http"
)

// MultiTokenManager issues into the primary store and administers every token
// store accepted by the server. In particular, password and membership changes
// must revoke both bootstrap-file and SQLite user credentials.
type MultiTokenManager struct {
	primary TokenManager
	stores  []TokenManager
}

func NewMultiTokenManager(primary TokenManager, additional ...TokenManager) *MultiTokenManager {
	m := &MultiTokenManager{primary: primary, stores: []TokenManager{primary}}
	for _, store := range additional {
		if store != nil {
			m.stores = append(m.stores, store)
		}
	}
	return m
}

func (m *MultiTokenManager) AuthenticateRequest(req *http.Request) (*Grant, bool) {
	for _, store := range m.stores {
		if grant, ok := store.AuthenticateRequest(req); ok {
			return grant, true
		}
	}
	return nil, false
}

func (m *MultiTokenManager) TokenRules() []TokenRuleView {
	var views []TokenRuleView
	for _, store := range m.stores {
		views = append(views, store.TokenRules()...)
	}
	return views
}

func (m *MultiTokenManager) Issue(rule TokenRule) (TokenRuleView, string, error) {
	for _, view := range m.TokenRules() {
		if rule.ID != "" && rule.ID == view.ID {
			return TokenRuleView{}, "", fmt.Errorf("token id %q already exists", rule.ID)
		}
	}
	return m.primary.Issue(rule)
}

func (m *MultiTokenManager) Revoke(tokenID string) error {
	var errs []error
	found := false
	for _, store := range m.stores {
		for _, view := range store.TokenRules() {
			if view.ID == tokenID {
				found = true
				errs = append(errs, store.Revoke(tokenID))
				break
			}
		}
	}
	if !found {
		return fmt.Errorf("token %q not found", tokenID)
	}
	return errors.Join(errs...)
}

func (m *MultiTokenManager) RevokeByUserID(userID string) error {
	var errs []error
	for _, store := range m.stores {
		errs = append(errs, store.RevokeByUserID(userID))
	}
	return errors.Join(errs...)
}
