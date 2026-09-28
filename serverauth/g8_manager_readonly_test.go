package serverauth

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// go-server#7: when the auth config cannot be written (for example a unit that
// keeps it under a read-only /etc), a failed revoke must neither report the
// token as revoked while it still authenticates, nor turn a retry of a
// user-wide revocation into a silent success.
func TestG8_ManagerFailedPersistenceLeavesStateConsistent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "etc")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "server_auth.json")
	if err := SaveConfigFile(path, Config{Tokens: []TokenRule{
		{ID: "svc_legacy", Token: "legacy-secret", Scopes: AllClientScopes()},
		{ID: "bob_file", Token: "bob-secret", UserID: "usr_bob", Scopes: AllClientScopes()},
	}}); err != nil {
		t.Fatal(err)
	}
	manager, err := LoadManager(path)
	if err != nil {
		t.Fatal(err)
	}
	// Make the file itself read-only (and its directory, so it cannot be
	// replaced) to mimic ProtectSystem=strict without a ReadWritePaths entry.
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o700) //nolint:gosec // G302: restoring a private test directory's owner-only permissions.
		_ = os.Chmod(path, 0o600)
	})

	authenticates := func(secret string) bool {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		_, ok := manager.AuthenticateRequest(req)
		return ok
	}
	listedRevoked := func(id string) bool {
		for _, view := range manager.TokenRules() {
			if view.ID == id {
				return view.Revoked
			}
		}
		t.Fatalf("token %s not listed", id)
		return false
	}

	if err := manager.Revoke("svc_legacy"); err == nil {
		t.Fatal("revoke succeeded although the auth config is read-only")
	}
	if listedRevoked("svc_legacy") != !authenticates("legacy-secret") {
		t.Fatalf("listing says revoked=%v but token authenticates=%v", listedRevoked("svc_legacy"), authenticates("legacy-secret"))
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if err := manager.RevokeByUserID("usr_bob"); err == nil {
			t.Fatalf("attempt %d: user-wide revocation reported success without persisting", attempt)
		}
	}
	if listedRevoked("bob_file") != !authenticates("bob-secret") {
		t.Fatal("user token listing and authentication disagree after failed revocation")
	}

	// Once the file is writable again the same calls succeed and persist.
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: owner-only test directory.
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Revoke("svc_legacy"); err != nil {
		t.Fatal(err)
	}
	if err := manager.RevokeByUserID("usr_bob"); err != nil {
		t.Fatal(err)
	}
	if authenticates("legacy-secret") || authenticates("bob-secret") {
		t.Fatal("revoked tokens still authenticate")
	}
	reloaded, err := LoadManager(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range reloaded.TokenRules() {
		if !view.Revoked {
			t.Fatalf("revocation not persisted: %+v", view)
		}
	}
}
