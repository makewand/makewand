package serverauth

import "net/http"

// MultiAuthorizer tries multiple authorizers in order until one authenticates
// the incoming request.
type MultiAuthorizer struct {
	items []RequestAuthorizer
}

// NewMultiAuthorizer composes multiple authorizers into one.
func NewMultiAuthorizer(items ...RequestAuthorizer) *MultiAuthorizer {
	filtered := make([]RequestAuthorizer, 0, len(items))
	for _, item := range items {
		if item != nil {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return &MultiAuthorizer{items: filtered}
}

// AuthenticateRequest returns the first successful grant across composed
// authorizers.
func (m *MultiAuthorizer) AuthenticateRequest(req *http.Request) (*Grant, bool) {
	if m == nil {
		return nil, false
	}
	for _, item := range m.items {
		if grant, ok := item.AuthenticateRequest(req); ok {
			return grant, true
		}
	}
	return nil, false
}
