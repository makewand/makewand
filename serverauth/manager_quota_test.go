package serverauth

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// TestManager_PreservesQuotaAcrossIssueAndRevoke verifies that rebuilding the
// authorizer on token issue/revoke does not reset a live token's consumed quota
// counters.
func TestManager_PreservesQuotaAcrossIssueAndRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server_auth.json")
	if err := SaveConfigFile(path, Config{
		Tokens: []TokenRule{
			{ID: "admin", Token: "admin-secret", Scopes: AllScopes()},
			{ID: "runner", Token: "runner-secret", Scopes: AllClientScopes(), MaxRequestsPerHour: 1},
		},
	}); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}
	manager, err := LoadManager(path)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}

	now := time.Date(2026, 7, 18, 12, 30, 0, 0, time.UTC)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer runner-secret")

	grant, ok := manager.AuthenticateRequest(req)
	if !ok {
		t.Fatal("authenticate runner (1) failed")
	}
	if err := grant.CheckAndConsumeRequestAt(now); err != nil {
		t.Fatalf("first consume: %v", err)
	}

	// Issue an unrelated token; this rebuilds the authorizer/grants.
	if _, _, err := manager.Issue(TokenRule{ID: "other", Scopes: AllClientScopes()}); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	grant2, ok := manager.AuthenticateRequest(req)
	if !ok {
		t.Fatal("authenticate runner (2) failed")
	}
	// Without carry-over the counter would be 0 again and this would succeed.
	if err := grant2.CheckAndConsumeRequestAt(now); err == nil {
		t.Fatal("quota counter was reset on Issue; expected it to be preserved")
	}

	// Revoking also rebuilds the authorizer; the counter must still be preserved.
	if err := manager.Revoke("other"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	grant3, ok := manager.AuthenticateRequest(req)
	if !ok {
		t.Fatal("authenticate runner (3) failed")
	}
	if err := grant3.CheckAndConsumeRequestAt(now); err == nil {
		t.Fatal("quota counter was reset on Revoke; expected it to be preserved")
	}
}

// TestManager_SharesUsageAcrossRebuild verifies that a rebuild shares (not
// copies) usage counters, so an increment against a grant captured BEFORE the
// rebuild — the copy-then-swap race window — is observed by grants obtained
// AFTER the rebuild. Under the old copy-based carry-over this increment was lost
// and the final consume would wrongly succeed.
func TestManager_SharesUsageAcrossRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server_auth.json")
	if err := SaveConfigFile(path, Config{
		Tokens: []TokenRule{
			{ID: "admin", Token: "admin-secret", Scopes: AllScopes()},
			{ID: "runner", Token: "runner-secret", Scopes: AllClientScopes(), MaxRequestsPerHour: 2},
		},
	}); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}
	manager, err := LoadManager(path)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}

	now := time.Date(2026, 7, 18, 12, 30, 0, 0, time.UTC)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer runner-secret")

	// Capture the grant BEFORE the rebuild and consume one request (count = 1).
	oldGrant, ok := manager.AuthenticateRequest(req)
	if !ok {
		t.Fatal("authenticate runner (before rebuild) failed")
	}
	if err := oldGrant.CheckAndConsumeRequestAt(now); err != nil {
		t.Fatalf("first consume: %v", err)
	}

	// Rebuild the authorizer by issuing an unrelated token.
	if _, _, err := manager.Issue(TokenRule{ID: "other", Scopes: AllClientScopes()}); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Simulate an in-flight request that still holds the pre-rebuild grant and
	// increments it AFTER the swap (count = 2). Sharing means this lands on the
	// same counters the post-rebuild grant reads.
	if err := oldGrant.CheckAndConsumeRequestAt(now); err != nil {
		t.Fatalf("second consume on pre-rebuild grant: %v", err)
	}

	// A request using the post-rebuild grant must now see the quota exhausted.
	newGrant, ok := manager.AuthenticateRequest(req)
	if !ok {
		t.Fatal("authenticate runner (after rebuild) failed")
	}
	if err := newGrant.CheckAndConsumeRequestAt(now); err == nil {
		t.Fatal("in-flight increment on pre-rebuild grant was lost; expected shared counters to reflect it")
	}
}
