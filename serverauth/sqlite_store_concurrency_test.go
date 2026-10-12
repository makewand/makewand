package serverauth

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSQLiteStore_AuthenticateRejectsDeactivatedUserInOneSnapshot(t *testing.T) {
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	defer store.Close()
	_, secret, err := store.Issue(TokenRule{ID: "u1tok", UserID: "u1", Scopes: AllClientScopes()})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Without a users table the store keeps token-only semantics.
	if _, ok := store.AuthenticateHeader("Bearer " + secret); !ok {
		t.Fatal("token rejected when users table is absent")
	}
	if _, err := store.db.Exec(`CREATE TABLE users (id TEXT PRIMARY KEY, is_active INTEGER NOT NULL, role TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatalf("create users: %v", err)
	}
	if _, ok := store.AuthenticateHeader("Bearer " + secret); !ok {
		t.Fatal("token rejected when user row is absent (historical behavior)")
	}
	if _, err := store.db.Exec(`INSERT INTO users (id, is_active) VALUES ('u1', 1)`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, ok := store.AuthenticateHeader("Bearer " + secret); !ok {
		t.Fatal("active user token rejected")
	}
	if _, err := store.db.Exec(`UPDATE users SET is_active = 0 WHERE id = 'u1'`); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, ok := store.AuthenticateHeader("Bearer " + secret); ok {
		t.Fatal("deactivated user token accepted")
	}
}

// Once Revoke returns, no concurrent or later authentication may succeed.
func TestSQLiteStore_ConcurrentRevokeAndAuthenticate(t *testing.T) {
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	defer store.Close()
	_, secret, err := store.Issue(TokenRule{ID: "tok", Scopes: AllClientScopes()})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	var revoked atomic.Bool
	var lateSuccess atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				wasRevoked := revoked.Load()
				if _, ok := store.AuthenticateHeader("Bearer " + secret); ok && wasRevoked {
					lateSuccess.Add(1)
				}
			}
		}()
	}
	if err := store.Revoke("tok"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	revoked.Store(true)
	for i := 0; i < 200; i++ {
		if _, ok := store.AuthenticateHeader("Bearer " + secret); ok {
			lateSuccess.Add(1)
		}
	}
	close(stop)
	wg.Wait()
	if n := lateSuccess.Load(); n != 0 {
		t.Fatalf("%d authentications succeeded after Revoke returned", n)
	}
}
