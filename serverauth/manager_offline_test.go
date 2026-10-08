package serverauth

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManager_OfflineRevoke_IsDetectedOnNextRequest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server_auth.json")

	cfg := Config{
		Tokens: []TokenRule{
			{
				ID:     "admin-id",
				Token:  "admin-secret-token",
				UserID: "admin",
				Scopes: AllScopes(),
			},
			{
				ID:     "user-id",
				Token:  "user-secret-token",
				UserID: "user",
				Scopes: AllClientScopes(),
			},
		},
	}
	if err := SaveConfigFile(path, cfg); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}

	mgr, err := LoadManager(path)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}

	// 1. Both tokens authenticate successfully on initial manager load
	reqAdmin := httptest.NewRequest("GET", "/api/chat", nil)
	reqAdmin.Header.Set("Authorization", "Bearer admin-secret-token")
	grantAdmin, ok := mgr.AuthenticateRequest(reqAdmin)
	if !ok || grantAdmin == nil {
		t.Fatalf("expected admin token to authenticate, got ok=%v", ok)
	}

	reqUser := httptest.NewRequest("GET", "/api/chat", nil)
	reqUser.Header.Set("Authorization", "Bearer user-secret-token")
	grantUser, ok := mgr.AuthenticateRequest(reqUser)
	if !ok || grantUser == nil {
		t.Fatalf("expected user token to authenticate, got ok=%v", ok)
	}

	// Wait slightly to ensure filesystem timestamp ticks if mtime is used
	time.Sleep(15 * time.Millisecond)

	// 2. Simulate offline revocation via CLI or file editing
	unlock, err := LockConfigFile(path, true)
	if err != nil {
		t.Fatalf("LockConfigFile: %v", err)
	}
	diskCfg, err := LoadConfigFile(path)
	if err != nil {
		unlock()
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if err := RevokeTokenRule(&diskCfg, "user-id"); err != nil {
		unlock()
		t.Fatalf("RevokeTokenRule: %v", err)
	}
	if err := SaveConfigFile(path, diskCfg); err != nil {
		unlock()
		t.Fatalf("SaveConfigFile: %v", err)
	}
	unlock()

	// 3. The running server Manager must dynamically reload and reject the revoked user token
	grantUser2, ok2 := mgr.AuthenticateRequest(reqUser)
	if ok2 || grantUser2 != nil {
		t.Fatalf("expected offline revoked token to be rejected on next request, but got ok=%v grant=%+v", ok2, grantUser2)
	}

	// 4. Admin token should continue to be accepted
	grantAdmin2, okAdmin2 := mgr.AuthenticateRequest(reqAdmin)
	if !okAdmin2 || grantAdmin2 == nil {
		t.Fatalf("expected admin token to remain valid, got ok=%v", okAdmin2)
	}
}

func TestManager_ServerIssueDoesNotResurrectOfflineRevokedToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server_auth.json")

	cfg := Config{
		Tokens: []TokenRule{
			{
				ID:     "tok-1",
				Token:  "secret-token-1",
				UserID: "user1",
				Scopes: AllClientScopes(),
			},
		},
	}
	if err := SaveConfigFile(path, cfg); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}

	mgr, err := LoadManager(path)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}

	time.Sleep(15 * time.Millisecond)

	// 1. Offline revoke tok-1 on disk
	unlock, err := LockConfigFile(path, true)
	if err != nil {
		t.Fatalf("LockConfigFile: %v", err)
	}
	diskCfg, err := LoadConfigFile(path)
	if err != nil {
		unlock()
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if err := RevokeTokenRule(&diskCfg, "tok-1"); err != nil {
		unlock()
		t.Fatalf("RevokeTokenRule: %v", err)
	}
	if err := SaveConfigFile(path, diskCfg); err != nil {
		unlock()
		t.Fatalf("SaveConfigFile: %v", err)
	}
	unlock()

	time.Sleep(15 * time.Millisecond)

	// 2. Server issues a new token tok-2 via Manager API while running
	view, tokVal, err := mgr.Issue(TokenRule{
		ID:     "tok-2",
		Token:  "secret-token-2",
		UserID: "user2",
		Scopes: AllClientScopes(),
	})
	if err != nil {
		t.Fatalf("Manager.Issue: %v", err)
	}
	if view.ID != "tok-2" || tokVal != "secret-token-2" {
		t.Fatalf("unexpected issue result: %+v, %s", view, tokVal)
	}

	// 3. Inspect disk config directly: tok-1 MUST remain revoked! It must NOT be resurrected
	finalDiskCfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	var foundTok1, foundTok2 *TokenRule
	for i := range finalDiskCfg.Tokens {
		if finalDiskCfg.Tokens[i].ID == "tok-1" {
			foundTok1 = &finalDiskCfg.Tokens[i]
		}
		if finalDiskCfg.Tokens[i].ID == "tok-2" {
			foundTok2 = &finalDiskCfg.Tokens[i]
		}
	}
	if foundTok1 == nil || !foundTok1.Revoked {
		t.Fatalf("tok-1 was resurrected by server issue! foundTok1=%+v", foundTok1)
	}
	if foundTok2 == nil || foundTok2.Revoked {
		t.Fatalf("tok-2 missing or revoked in final disk config: %+v", foundTok2)
	}

	// 4. Test authentication: tok-1 rejected, tok-2 accepted
	req1 := httptest.NewRequest("GET", "/", nil)
	req1.Header.Set("Authorization", "Bearer secret-token-1")
	if _, ok := mgr.AuthenticateRequest(req1); ok {
		t.Fatal("expected tok-1 to be rejected, got accepted")
	}

	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Header.Set("Authorization", "Bearer secret-token-2")
	if _, ok := mgr.AuthenticateRequest(req2); !ok {
		t.Fatal("expected tok-2 to be accepted, got rejected")
	}
}

func TestManager_ServerRevokeByUserIDDoesNotResurrectOfflineRevokedToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server_auth.json")

	cfg := Config{
		Tokens: []TokenRule{
			{
				ID:     "u1-tok",
				Token:  "u1-secret",
				UserID: "user-alpha",
				Scopes: AllClientScopes(),
			},
			{
				ID:     "u2-tok",
				Token:  "u2-secret",
				UserID: "user-beta",
				Scopes: AllClientScopes(),
			},
			{
				ID:     "offline-target",
				Token:  "offline-secret",
				UserID: "user-gamma",
				Scopes: AllClientScopes(),
			},
		},
	}
	if err := SaveConfigFile(path, cfg); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}

	mgr, err := LoadManager(path)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}

	time.Sleep(15 * time.Millisecond)

	// Offline revoke offline-target
	unlock, err := LockConfigFile(path, true)
	if err != nil {
		t.Fatalf("LockConfigFile: %v", err)
	}
	diskCfg, err := LoadConfigFile(path)
	if err != nil {
		unlock()
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if err := RevokeTokenRule(&diskCfg, "offline-target"); err != nil {
		unlock()
		t.Fatalf("RevokeTokenRule: %v", err)
	}
	if err := SaveConfigFile(path, diskCfg); err != nil {
		unlock()
		t.Fatalf("SaveConfigFile: %v", err)
	}
	unlock()

	time.Sleep(15 * time.Millisecond)

	// Server manager revokes user-alpha
	if err := mgr.RevokeByUserID("user-alpha"); err != nil {
		t.Fatalf("RevokeByUserID: %v", err)
	}

	// Verify on disk:
	// - u1-tok revoked
	// - offline-target STILL revoked (not resurrected)
	// - u2-tok NOT revoked
	diskAfter, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	status := make(map[string]bool)
	for _, tok := range diskAfter.Tokens {
		status[tok.ID] = tok.Revoked
	}
	if !status["u1-tok"] {
		t.Error("u1-tok should be revoked by RevokeByUserID")
	}
	if !status["offline-target"] {
		t.Error("offline-target should still be revoked (resurrection defect!)")
	}
	if status["u2-tok"] {
		t.Error("u2-tok should not be revoked")
	}
}

func TestSaveConfigFile_IsAtomicAndMode0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server_auth.json")

	cfg := Config{
		Tokens: []TokenRule{
			{
				ID:     "test-tok",
				Token:  "test-secret",
				Scopes: AllClientScopes(),
			},
		},
	}
	if err := SaveConfigFile(path, cfg); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("expected 0600 file permissions, got %o", fi.Mode().Perm())
	}

	// Check directory: should only contain server_auth.json (and optional .lock), no .auth-config-* temp files
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Base(e.Name()) != "server_auth.json" && filepath.Base(e.Name()) != "server_auth.json.lock" {
			t.Errorf("unexpected file in directory: %s", e.Name())
		}
	}
}
