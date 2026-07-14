package router

import (
	"path/filepath"
	"testing"
)

func TestSQLiteUserStore_CreateListAndMutate(t *testing.T) {
	store, err := OpenSQLiteUserStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteUserStore: %v", err)
	}
	defer store.Close()

	user, err := store.CreateUser("Person@Example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if user.Email != "person@example.com" {
		t.Fatalf("Email = %q, want lowercase", user.Email)
	}
	if _, err := store.SetUserRole(user.ID, UserRoleAdmin); err != nil {
		t.Fatalf("SetUserRole: %v", err)
	}
	if _, err := store.SetUserActive(user.ID, false); err != nil {
		t.Fatalf("SetUserActive: %v", err)
	}

	loaded, err := store.GetUserByEmail("PERSON@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if loaded.Role != UserRoleAdmin {
		t.Fatalf("Role = %q, want %q", loaded.Role, UserRoleAdmin)
	}
	if loaded.IsActive {
		t.Fatal("IsActive = true, want false")
	}

	views, err := store.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(views) != 1 || views[0].ID != user.ID {
		t.Fatalf("ListUsers = %+v, want user %s", views, user.ID)
	}
}
