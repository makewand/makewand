package execution

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAccountingDirectorySwapCannotOverwriteOutside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permits renaming a directory while its rooted sidecar is open; resolved-parent replacement is tested on all platforms")
	}
	base := t.TempDir()
	parent := filepath.Join(base, "selected")
	outside := filepath.Join(base, "outside")
	for _, path := range []string{parent, outside} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(parent, "calls.json")
	sentinel := []byte("outside must remain untouched\n")
	outsideFile := filepath.Join(outside, "calls.json")
	if err := os.WriteFile(outsideFile, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	err := withLockedFile(context.Background(), path, nil, func(pinned *accountingPath) error {
		if err := os.Rename(parent, parent+"-moved"); err != nil {
			return err
		}
		if err := os.Symlink(outside, parent); err != nil {
			return err
		}
		return saveLedger(pinned, map[string]any{"schema": 1, "maximum": 1, "attempts": []any{}})
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outsideFile)
	if err != nil || !bytes.Equal(got, sentinel) {
		t.Fatalf("accounting followed the exchanged parent and overwrote an outside file: %q; %v", got, err)
	}
	if ledger := readTestLedger(t, filepath.Join(parent+"-moved", "calls.json")); ledger["maximum"] != float64(1) {
		t.Fatal("replacement did not commit to the original pinned directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 1 {
		t.Fatalf("outside directory received accounting temporary/lock files: %v; %v", entries, err)
	}
}

func clearAccountingEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"MAKEWAND_CALL_BUDGET_FILE", "MAKEWAND_EXECUTION_EVENTS_FILE", "MAKEWAND_MAX_MODEL_CALLS", "MAKEWAND_CALL_BUDGET_LEASE_ID"} {
		t.Setenv(key, "")
	}
}

func accountingSymlink(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows symlink creation is unavailable: %v", err)
		}
		t.Fatal(err)
	}
}

func TestAccountingAliasesKeepCanonicalBudget(t *testing.T) {
	clearAccountingEnvironment(t)
	base := t.TempDir()
	parent := filepath.Join(base, "real")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "parent-alias")
	accountingSymlink(t, parent, alias)
	target := filepath.Join(parent, "calls.json")
	first, err := Reserve(ContextWithConfig(context.Background(), Config{LedgerPath: target, MaxCalls: 3}), Metadata{Engine: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Complete(Outcome{Status: Unknown}); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(base, "leaf-alias.json")
	accountingSymlink(t, target, leaf)
	for _, path := range []string{filepath.Join(alias, "calls.json"), leaf} {
		t.Setenv("MAKEWAND_CALL_BUDGET_FILE", path)
		ctx, cfg, err := EnsureContext(ContextWithConfig(context.Background(), Config{LedgerPath: target, MaxCalls: 3}))
		if err != nil {
			t.Fatalf("legitimate alias broke inherited canonical ledger identity: %v", err)
		}
		original, statErr := os.Stat(target)
		resolved, resolvedErr := os.Stat(cfg.LedgerPath)
		if statErr != nil || resolvedErr != nil || !os.SameFile(original, resolved) {
			t.Fatalf("alias selected a different ledger: %v / %v", statErr, resolvedErr)
		}
		attempt, err := Reserve(ctx, Metadata{Engine: "synthetic"})
		if err != nil {
			t.Fatal(err)
		}
		if err := attempt.Complete(Outcome{Status: Unknown}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Reserve(ContextWithConfig(context.Background(), Config{LedgerPath: leaf, MaxCalls: 99}), Metadata{Engine: "synthetic"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("leaf alias escaped the original maximum: %v", err)
	}
	if got := len(readTestLedger(t, target)["attempts"].([]any)); got != 3 {
		t.Fatalf("aliases produced separate budgets: %d attempts", got)
	}
	if _, err := os.Stat(leaf + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("alias received a separate sidecar: %v", err)
	}
}

func TestAccountingResolvedAliasCannotRedirectAdmission(t *testing.T) {
	clearAccountingEnvironment(t)
	base := t.TempDir()
	selected := filepath.Join(base, "selected")
	outside := filepath.Join(base, "outside")
	for _, path := range []string{selected, outside} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(base, "alias")
	accountingSymlink(t, selected, alias)
	ctx, cfg, err := EnsureContext(ContextWithConfig(context.Background(), Config{LedgerPath: filepath.Join(alias, "new", "calls.json"), MaxCalls: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	accountingSymlink(t, outside, alias)
	attempt, err := Reserve(ctx, Metadata{Engine: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.Complete(Outcome{Status: Unknown}); err != nil {
		t.Fatal(err)
	}
	if got := len(readTestLedger(t, cfg.LedgerPath)["attempts"].([]any)); got != 1 {
		t.Fatalf("resolved admission did not retain the selected ledger: %d", got)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("changed alias received accounting writes: %v; %v", entries, err)
	}
}

func TestAccountingResolvedParentReplacementRejectsAdmissionAndCompletion(t *testing.T) {
	clearAccountingEnvironment(t)
	for _, replacement := range []string{"directory", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			base := t.TempDir()
			selected := filepath.Join(base, "selected")
			outside := filepath.Join(base, "outside")
			for _, path := range []string{selected, outside} {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			ledgerPath := filepath.Join(selected, "calls.json")
			ctx, _, err := EnsureContext(ContextWithConfig(context.Background(), Config{LedgerPath: ledgerPath, MaxCalls: 2}))
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := Reserve(ctx, Metadata{Engine: "synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			moved := selected + "-moved"
			if err := os.Rename(selected, moved); err != nil {
				t.Fatal(err)
			}
			if replacement == "symlink" {
				accountingSymlink(t, outside, selected)
			} else if err := os.Mkdir(selected, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := Reserve(ctx, Metadata{Engine: "synthetic"}); err == nil {
				t.Fatal("replaced canonical parent admitted another dispatch")
			}
			if err := attempt.Complete(Outcome{Status: Unknown}); err == nil {
				t.Fatal("completion wrote through a replaced canonical parent")
			}
			attempts := readTestLedger(t, filepath.Join(moved, "calls.json"))["attempts"].([]any)
			if len(attempts) != 1 || attempts[0].(map[string]any)["status"] != "started" {
				t.Fatal("rejected replacement refunded or completed the counted attempt")
			}
			for _, path := range []string{selected, outside} {
				entries, err := os.ReadDir(path)
				if err != nil || len(entries) != 0 {
					t.Fatalf("replacement received ledger/sidecar/temp writes: %s: %v; %v", path, entries, err)
				}
			}
		})
	}
}

func TestAccountingEventParentReplacementStaysOptionalAndCannotRedirect(t *testing.T) {
	clearAccountingEnvironment(t)
	base := t.TempDir()
	parent := filepath.Join(base, "events")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	var warnings int
	ctx := ContextWithConfig(context.Background(), Config{
		LedgerPath: filepath.Join(base, "calls.json"), MaxCalls: 1,
		EventsPath: filepath.Join(parent, "events.jsonl"), EventError: func(error) { warnings++ },
	})
	attempt, err := Reserve(ctx, Metadata{Engine: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parent, parent+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := attempt.Complete(Outcome{Status: Unknown}); err != nil {
		t.Fatalf("optional recorder failure changed the completion: %v", err)
	}
	if warnings != 1 {
		t.Fatalf("replaced recorder directory must report one error: %d", warnings)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("replacement recorder directory received writes: %v; %v", entries, err)
	}
	ledger := readTestLedger(t, filepath.Join(base, "calls.json"))
	if ledger["attempts"].([]any)[0].(map[string]any)["status"] != "completed" {
		t.Fatal("recorder failure changed the counted terminal outcome")
	}
	data, err := os.ReadFile(filepath.Join(parent+"-moved", "events.jsonl"))
	if err != nil || bytes.Count(data, []byte("\n")) != 1 || !bytes.Contains(data, []byte(`"event":"start"`)) {
		t.Fatalf("replaced recorder modified its original start event: %v", err)
	}
}

func TestAccountingLeafExchangeDoesNotFollowRelativeSymlink(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "calls.json")
	target := filepath.Join(base, "target.json")
	sentinel := []byte("target must remain untouched\n")
	if err := os.WriteFile(target, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	err := withLockedFile(context.Background(), path, nil, func(pinned *accountingPath) error {
		accountingSymlink(t, "target.json", path)
		_, err := readLedger(pinned, 1)
		return err
	})
	if err == nil {
		t.Fatal("root-local relative leaf symlink was followed")
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, sentinel) {
		t.Fatalf("exchanged leaf changed the target: %q; %v", got, err)
	}
}

func TestAccountingRejectsHardlinksWithoutChangingTarget(t *testing.T) {
	clearAccountingEnvironment(t)
	for _, kind := range []string{"ledger", "lock", "events"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			target := filepath.Join(base, "target")
			sentinel := []byte("not an accounting file\n")
			if err := os.WriteFile(target, sentinel, 0o600); err != nil {
				t.Fatal(err)
			}
			// Deliberately non-private: rejection must precede any permission
			// normalization through the hardlink.
			if err := os.Chmod(target, 0o644); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(base, "calls.json")
			link := path
			if kind == "lock" {
				link += ".lock"
			}
			if err := os.Link(target, link); err != nil {
				t.Fatal(err)
			}
			cfg := Config{LedgerPath: path, MaxCalls: 1}
			var warnings int
			if kind == "events" {
				cfg = Config{EventsPath: path, EventError: func(error) { warnings++ }}
			}
			_, err = Reserve(ContextWithConfig(context.Background(), cfg), Metadata{Engine: "synthetic"})
			if kind == "events" {
				if err != nil || warnings != 1 {
					t.Fatalf("optional hardlinked telemetry changed admission: %v, warnings=%d", err, warnings)
				}
			} else if err == nil {
				t.Fatal("hardlinked accounting file admitted a dispatch")
			}
			got, err := os.ReadFile(target)
			after, statErr := os.Stat(target)
			if err != nil || statErr != nil || !bytes.Equal(got, sentinel) || after.Mode() != before.Mode() {
				t.Fatalf("hardlinked target content or permissions changed: %q; %v / %v", got, err, statErr)
			}
		})
	}
}

func TestAccountingExplicitPathKeepsSpacesUnicodeAndRelativeLocation(t *testing.T) {
	clearAccountingEnvironment(t)
	base := t.TempDir()
	t.Chdir(base)
	path := filepath.Join("budget %#", "nested", "预算 %#.json")
	attempt, err := Reserve(ContextWithConfig(context.Background(), Config{LedgerPath: path, MaxCalls: 1}), Metadata{Engine: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.Complete(Outcome{Status: Unknown}); err != nil {
		t.Fatal(err)
	}
	ledger := readTestLedger(t, filepath.Join(base, path))
	if len(ledger["attempts"].([]any)) != 1 || ledger["attempts"].([]any)[0].(map[string]any)["status"] != "completed" {
		t.Fatal("explicit relative path did not persist the complete attempt")
	}
	info, err := os.Stat(filepath.Join(base, path))
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger is not private: %v / %v", info, err)
	}
}
