package execution

import (
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"
)

func capacityMissingHomeFixture(t *testing.T) string {
	t.Helper()
	_ = layeredFixture(t)
	for _, key := range []string{"HOME", "USERPROFILE", "MAKEWAND_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(key, "")
	}
	home := t.TempDir()
	original := capacityCurrentUser
	capacityCurrentUser = func() (*user.User, error) { return &user.User{HomeDir: home}, nil }
	t.Cleanup(func() { capacityCurrentUser = original })
	return home
}

func TestSharedCapacityMissingHomeKeepsLegacyAdmissions(t *testing.T) {
	for _, private := range []bool{false, true} {
		name := "unbudgeted"
		if private {
			name = "private"
		}
		t.Run(name, func(t *testing.T) {
			home := capacityMissingHomeFixture(t)
			cfg := Config{TaskID: "no-home", MaxCalls: 3}
			if private {
				cfg.LedgerPath = filepath.Join(t.TempDir(), "budget.json")
			} else {
				cfg.MaxCalls = 0
				for _, key := range []string{"MAKEWAND_LAYERED_BUDGET", "MAKEWAND_MACHINE_DAILY_CALLS", "MAKEWAND_PROJECT_DAILY_CALLS"} {
					t.Setenv(key, "")
				}
			}
			a, err := Reserve(ContextWithConfig(t.Context(), cfg), Metadata{Engine: "codex"})
			if err != nil {
				t.Fatal("optional policy broke legacy admission", err)
			}
			if err := a.Complete(Outcome{Status: Passed, Known: true}); err != nil {
				t.Fatal(err)
			}
			if refs := capacityReferencedAttempts(); refs == nil || len(refs) != 0 {
				t.Fatal("absent coordinator blocked normal compaction", refs)
			}
			if _, err := os.Stat(filepath.Join(home, ".config")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("optional reads created a default config directory", err)
			}
		})
	}
}

func TestSharedCapacityMissingHomeFindsPolicyAndRetainsReceipts(t *testing.T) {
	home := capacityMissingHomeFixture(t)
	dir := filepath.Join(home, ".config", "makewand")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	policy := hostCapacityPolicy{Schema: 1, GlobalMax: 3, ProjectMax: 1, ToolMax: 2, WaitSeconds: 300, Providers: map[string]int{"codex": 1}}
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "capacity-policy.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "private.json")
	cfg := Config{LedgerPath: path, TaskID: "no-home", MaxCalls: 3}
	a, err := Reserve(ContextWithConfig(t.Context(), cfg), Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Complete(Outcome{Status: Passed, Known: true}); err != nil {
		t.Fatal(err)
	}
	if !capacityReferencedAttempts()[a.ID] {
		t.Fatal("home fallback lost cached settlement reference")
	}
	data := readTestLedger(t, path)
	data["attempts"].([]any)[0].(map[string]any)["finished_at"] = float64(time.Now().Add(-49*time.Hour).UnixNano()) / 1e9
	raw, err = json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := InitializeBudget(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(readTestLedger(t, path)["attempts"].([]any)) != 1 {
		t.Fatal("compaction discarded a referenced receipt without HOME")
	}
	if err := os.WriteFile(filepath.Join(dir, "capacity-policy.json"), []byte(`{"schema":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCapacityPolicy(); err == nil {
		t.Fatal("malformed policy at fallback home was ignored")
	}
}

func TestSharedCapacityUnknownHomeKeepsLegacyAndConservativeHistory(t *testing.T) {
	_ = capacityMissingHomeFixture(t)
	capacityCurrentUser = func() (*user.User, error) { return nil, errors.New("fixture user database unavailable") }
	path := filepath.Join(t.TempDir(), "private.json")
	a, err := Reserve(ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "unknown-home", MaxCalls: 3}), Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal("unresolvable optional directory blocked a private ledger", err)
	}
	if err := a.Complete(Outcome{Status: Passed, Known: true}); err != nil {
		t.Fatal(err)
	}
	if capacityReferencedAttempts() != nil {
		t.Fatal("unresolvable coordinator must retain possible references")
	}
}

func TestSharedCapacityMissingHomeExpandsExplicitConfigAliases(t *testing.T) {
	for _, key := range []string{"MAKEWAND_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Run(key, func(t *testing.T) {
			home := capacityMissingHomeFixture(t)
			t.Setenv(key, "~/operator-config")
			expected := filepath.Join(home, "operator-config")
			if key == "XDG_CONFIG_HOME" {
				expected = filepath.Join(expected, "makewand")
			}
			path, err := capacityPath("capacity-policy.json")
			if err != nil || path != filepath.Join(expected, "capacity-policy.json") {
				t.Fatalf("home-less config alias: %s %v", path, err)
			}
			if policy, err := loadCapacityPolicy(); err != nil || policy != nil {
				t.Fatalf("absent optional policy: %v %v", policy, err)
			}
			capacityCurrentUser = func() (*user.User, error) { return nil, errors.New("fixture home unavailable") }
			if _, err := loadCapacityPolicy(); err == nil {
				t.Fatal("unresolvable explicit config alias was ignored")
			}
		})
	}
}
