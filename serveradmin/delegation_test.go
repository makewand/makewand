package serveradmin

import (
	"testing"
	"time"

	"github.com/makewand/makewand/serverauth"
)

func mustGrant(t *testing.T, rule serverauth.TokenRule) *serverauth.Grant {
	t.Helper()
	if len(rule.Scopes) == 0 {
		rule.Scopes = serverauth.AllScopes()
	}
	if rule.Token == "" {
		rule.Token = "issuer-token"
	}
	grant, err := serverauth.GrantFromRule(rule)
	if err != nil {
		t.Fatalf("GrantFromRule: %v", err)
	}
	return grant
}

func TestEnforceGrantTokenScope_RejectsScopeEscalation(t *testing.T) {
	issuer := mustGrant(t, serverauth.TokenRule{
		Scopes: []string{serverauth.ScopeChatInvoke, serverauth.ScopeAdminTokensWrite},
	})
	rule := &serverauth.TokenRule{
		Scopes: []string{serverauth.ScopeChatInvoke, serverauth.ScopeAdminUsersWrite},
	}
	if err := enforceGrantTokenScope(issuer, rule); err == nil {
		t.Fatal("expected rejection of scope not held by issuer")
	}
}

func TestEnforceGrantTokenScope_AllowsScopeSubset(t *testing.T) {
	issuer := mustGrant(t, serverauth.TokenRule{
		Scopes: []string{serverauth.ScopeChatInvoke, serverauth.ScopeAdminTokensWrite},
	})
	rule := &serverauth.TokenRule{Scopes: []string{serverauth.ScopeChatInvoke}}
	if err := enforceGrantTokenScope(issuer, rule); err != nil {
		t.Fatalf("subset scopes rejected: %v", err)
	}
}

func TestEnforceGrantTokenScope_RejectsExpiryBeyondIssuer(t *testing.T) {
	issuerExpiry := time.Now().Add(time.Hour).UTC()
	issuer := mustGrant(t, serverauth.TokenRule{
		Scopes:    serverauth.AllClientScopes(),
		ExpiresAt: issuerExpiry,
	})

	// Child expiring after the issuer must be rejected.
	beyond := &serverauth.TokenRule{
		Scopes:    serverauth.AllClientScopes(),
		ExpiresAt: issuerExpiry.Add(time.Hour),
	}
	if err := enforceGrantTokenScope(issuer, beyond); err == nil {
		t.Fatal("expected rejection of expiry beyond issuer")
	}

	// Child with no expiry (never-expiring) from an expiring issuer must be rejected.
	noExpiry := &serverauth.TokenRule{Scopes: serverauth.AllClientScopes()}
	if err := enforceGrantTokenScope(issuer, noExpiry); err == nil {
		t.Fatal("expected rejection of never-expiring child from expiring issuer")
	}

	// Child expiring before the issuer is allowed.
	within := &serverauth.TokenRule{
		Scopes:    serverauth.AllClientScopes(),
		ExpiresAt: issuerExpiry.Add(-time.Minute),
	}
	if err := enforceGrantTokenScope(issuer, within); err != nil {
		t.Fatalf("expiry within issuer rejected: %v", err)
	}
}

func TestEnforceGrantTokenScope_RejectsAllowlistAndQuotaEscalation(t *testing.T) {
	issuer := mustGrant(t, serverauth.TokenRule{
		Scopes:             serverauth.AllClientScopes(),
		WorkspacePrefixes:  []string{"repo-team/"},
		AllowedProviders:   []string{"codex"},
		AllowedModes:       []string{"balanced"},
		MaxRequestsPerHour: 100,
		MaxCostUSDPerDay:   5,
	})

	cases := map[string]*serverauth.TokenRule{
		"provider outside allowlist": {
			Scopes:            serverauth.AllClientScopes(),
			WorkspacePrefixes: []string{"repo-team/"},
			AllowedProviders:  []string{"claude"},
			AllowedModes:      []string{"balanced"},
		},
		"unrestricted providers": {
			Scopes:            serverauth.AllClientScopes(),
			WorkspacePrefixes: []string{"repo-team/"},
			AllowedModes:      []string{"balanced"},
		},
		"workspace outside prefix": {
			Scopes:            serverauth.AllClientScopes(),
			WorkspacePrefixes: []string{"repo-other/"},
			AllowedProviders:  []string{"codex"},
			AllowedModes:      []string{"balanced"},
		},
		"hourly quota over ceiling": {
			Scopes:             serverauth.AllClientScopes(),
			WorkspacePrefixes:  []string{"repo-team/"},
			AllowedProviders:   []string{"codex"},
			AllowedModes:       []string{"balanced"},
			MaxRequestsPerHour: 1000,
		},
		"cost over ceiling": {
			Scopes:            serverauth.AllClientScopes(),
			WorkspacePrefixes: []string{"repo-team/"},
			AllowedProviders:  []string{"codex"},
			AllowedModes:      []string{"balanced"},
			MaxCostUSDPerDay:  50,
		},
	}
	for name, rule := range cases {
		if err := enforceGrantTokenScope(issuer, rule); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}

	// A properly narrowed child is accepted.
	ok := &serverauth.TokenRule{
		Scopes:             serverauth.AllClientScopes(),
		WorkspacePrefixes:  []string{"repo-team/checkout"},
		AllowedProviders:   []string{"codex"},
		AllowedModes:       []string{"balanced"},
		MaxRequestsPerHour: 10,
		MaxCostUSDPerDay:   1,
	}
	if err := enforceGrantTokenScope(issuer, ok); err != nil {
		t.Fatalf("narrowed child rejected: %v", err)
	}
}

func TestEnforceGrantTokenScope_UnrestrictedIssuerImposesNoCeiling(t *testing.T) {
	// Root admin: full scopes, no expiry, no allowlists, no quotas.
	issuer := mustGrant(t, serverauth.TokenRule{Scopes: serverauth.AllScopes()})
	rule := &serverauth.TokenRule{
		Scopes:            serverauth.AllClientScopes(),
		WorkspacePrefixes: []string{"repo-"},
		AllowedProviders:  []string{"codex"},
		AllowedModes:      []string{"balanced"},
		ExpiresAt:         time.Now().Add(72 * time.Hour).UTC(),
		MaxRequestsPerDay: 500,
	}
	if err := enforceGrantTokenScope(issuer, rule); err != nil {
		t.Fatalf("unrestricted issuer wrongly constrained child: %v", err)
	}
}

// TestEnforceGrantTokenScope_RejectsWhitespaceProviderEscalation verifies that a
// child requesting a whitespace-only provider allowlist (which newGrant would
// normalize away to an empty, i.e. UNRESTRICTED, allowlist) is rejected when the
// issuer restricts providers, and that legitimate subset/superset requests are
// handled correctly.
func TestEnforceGrantTokenScope_RejectsWhitespaceProviderEscalation(t *testing.T) {
	issuer := mustGrant(t, serverauth.TokenRule{
		Scopes:           serverauth.AllClientScopes(),
		AllowedProviders: []string{"claude"},
	})

	// Whitespace-only allowlist normalizes to empty (=unrestricted): must be rejected.
	blank := &serverauth.TokenRule{
		Scopes:           serverauth.AllClientScopes(),
		AllowedProviders: []string{" "},
	}
	if err := enforceGrantTokenScope(issuer, blank); err == nil {
		t.Fatal("expected rejection of whitespace-only provider allowlist widening access to unrestricted")
	}

	// Exact subset: allowed.
	subset := &serverauth.TokenRule{
		Scopes:           serverauth.AllClientScopes(),
		AllowedProviders: []string{"claude"},
	}
	if err := enforceGrantTokenScope(issuer, subset); err != nil {
		t.Fatalf("subset provider allowlist rejected: %v", err)
	}

	// Superset (adds gemini beyond issuer): rejected.
	superset := &serverauth.TokenRule{
		Scopes:           serverauth.AllClientScopes(),
		AllowedProviders: []string{"claude", "gemini"},
	}
	if err := enforceGrantTokenScope(issuer, superset); err == nil {
		t.Fatal("expected rejection of provider allowlist exceeding issuer")
	}
}

// TestEnforceGrantTokenScope_RejectsWhitespaceWorkspaceEscalation verifies the
// same whitespace-normalization defense for workspace_prefixes.
func TestEnforceGrantTokenScope_RejectsWhitespaceWorkspaceEscalation(t *testing.T) {
	issuer := mustGrant(t, serverauth.TokenRule{
		Scopes:            serverauth.AllClientScopes(),
		WorkspacePrefixes: []string{"repo-team/"},
	})

	blank := &serverauth.TokenRule{
		Scopes:            serverauth.AllClientScopes(),
		WorkspacePrefixes: []string{"  "},
	}
	if err := enforceGrantTokenScope(issuer, blank); err == nil {
		t.Fatal("expected rejection of whitespace-only workspace_prefixes widening access to unrestricted")
	}

	subset := &serverauth.TokenRule{
		Scopes:            serverauth.AllClientScopes(),
		WorkspacePrefixes: []string{"repo-team/checkout"},
	}
	if err := enforceGrantTokenScope(issuer, subset); err != nil {
		t.Fatalf("subset workspace prefix rejected: %v", err)
	}

	outside := &serverauth.TokenRule{
		Scopes:            serverauth.AllClientScopes(),
		WorkspacePrefixes: []string{"repo-other/"},
	}
	if err := enforceGrantTokenScope(issuer, outside); err == nil {
		t.Fatal("expected rejection of workspace prefix outside issuer")
	}
}

// TestEnforceGrantTokenScope_RejectsWhitespaceModeEscalation verifies the same
// whitespace-normalization defense for allowed_modes.
func TestEnforceGrantTokenScope_RejectsWhitespaceModeEscalation(t *testing.T) {
	issuer := mustGrant(t, serverauth.TokenRule{
		Scopes:       serverauth.AllClientScopes(),
		AllowedModes: []string{"balanced"},
	})

	blank := &serverauth.TokenRule{
		Scopes:       serverauth.AllClientScopes(),
		AllowedModes: []string{"\t"},
	}
	if err := enforceGrantTokenScope(issuer, blank); err == nil {
		t.Fatal("expected rejection of whitespace-only allowed_modes widening access to unrestricted")
	}

	subset := &serverauth.TokenRule{
		Scopes:       serverauth.AllClientScopes(),
		AllowedModes: []string{"balanced"},
	}
	if err := enforceGrantTokenScope(issuer, subset); err != nil {
		t.Fatalf("subset mode allowlist rejected: %v", err)
	}

	superset := &serverauth.TokenRule{
		Scopes:       serverauth.AllClientScopes(),
		AllowedModes: []string{"balanced", "power"},
	}
	if err := enforceGrantTokenScope(issuer, superset); err == nil {
		t.Fatal("expected rejection of mode allowlist exceeding issuer")
	}
}
