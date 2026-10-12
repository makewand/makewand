package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setApplyGuardHook(t *testing.T, hook func(string)) {
	t.Helper()
	previous := applyBeforeGuardedReplace
	applyBeforeGuardedReplace = hook
	t.Cleanup(func() { applyBeforeGuardedReplace = previous })
}

func forceApplyRenameFallback(t *testing.T) {
	t.Helper()
	exchange, noReplace := applyRenameExchange, applyRenameNoReplace
	unsupported := func(*os.Root, string, string) error { return errApplyAtomicUnsupported }
	applyRenameExchange, applyRenameNoReplace = unsupported, unsupported
	t.Cleanup(func() { applyRenameExchange, applyRenameNoReplace = exchange, noReplace })
}

func requireApplyConflict(t *testing.T, p *Project, err error, target, external, candidate string) {
	t.Helper()
	var conflict *ApplyConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, ErrApplyConflict) || !errors.Is(err, ErrStaleApply) {
		t.Fatalf("want ApplyConflictError, got %v", err)
	}
	requireApplyContent(t, p, target, external)
	if conflict.ConflictPath == "" {
		t.Fatalf("candidate was not preserved at a conflict path: %v", err)
	}
	requireApplyContent(t, p, conflict.ConflictPath, candidate)
	if !strings.Contains(err.Error(), conflict.ConflictPath) {
		t.Fatalf("error does not report conflict path: %v", err)
	}
}

func applyGuardModes(t *testing.T, run func(t *testing.T)) {
	t.Run("atomic", run)
	t.Run("verify-then-rename", func(t *testing.T) {
		forceApplyRenameFallback(t)
		run(t)
	})
}

func TestApplyConcurrentEditorSaveIsNotOverwritten(t *testing.T) {
	applyGuardModes(t, func(t *testing.T) {
		p := applyFixture(t)
		if err := p.WriteFile("main.go", "before"); err != nil {
			t.Fatal(err)
		}
		fired := false
		setApplyGuardHook(t, func(dst string) {
			if dst != "main.go" || fired {
				return
			}
			fired = true
			// An editor's atomic save lands after the last preimage check.
			if err := os.WriteFile(filepath.Join(p.Path, "main.go"), []byte("editor save"), 0o600); err != nil {
				t.Error(err)
			}
		})
		err := p.ApplyFilesTransactional(context.Background(), []ExtractedFile{{Path: "main.go", Content: "candidate"}}, nil)
		if !fired {
			t.Fatal("hook not reached")
		}
		requireApplyConflict(t, p, err, "main.go", "editor save", "candidate")
	})
}

func TestApplyConcurrentCreateIsNotClobbered(t *testing.T) {
	applyGuardModes(t, func(t *testing.T) {
		p := applyFixture(t)
		setApplyGuardHook(t, func(dst string) {
			if dst == "new.txt" {
				if err := os.WriteFile(filepath.Join(p.Path, "new.txt"), []byte("created by editor"), 0o600); err != nil {
					t.Error(err)
				}
			}
		})
		err := p.ApplyFilesTransactional(context.Background(), []ExtractedFile{{Path: "new.txt", Content: "candidate"}}, nil)
		requireApplyConflict(t, p, err, "new.txt", "created by editor", "candidate")
	})
}

func TestApplyGuardedReplaceSucceedsWithoutRace(t *testing.T) {
	applyGuardModes(t, func(t *testing.T) {
		p := applyFixture(t)
		if err := p.WriteFile("a.txt", "before"); err != nil {
			t.Fatal(err)
		}
		files := []ExtractedFile{{Path: "a.txt", Content: "after"}, {Path: "b.txt", Content: "new"}}
		if err := p.ApplyFilesTransactional(context.Background(), files, nil); err != nil {
			t.Fatal(err)
		}
		requireApplyContent(t, p, "a.txt", "after")
		requireApplyContent(t, p, "b.txt", "new")
		entries, err := os.ReadDir(p.Path)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Fatalf("leftover scratch files: %v", entries)
		}
	})
}

// Rollback restoration must not clobber an editor save made to an already
// applied file while a later entry fails.
func TestApplyRollbackDoesNotOverwriteConcurrentSave(t *testing.T) {
	applyGuardModes(t, func(t *testing.T) {
		p := applyFixture(t)
		if err := p.WriteFile("a.txt", "before"); err != nil {
			t.Fatal(err)
		}
		state, err := p.openApplyState(context.Background(), true)
		if err != nil {
			t.Fatal(err)
		}
		defer state.close()
		journal, err := p.prepareApplyJournal(context.Background(), state, []ExtractedFile{{Path: "a.txt", Content: "after"}})
		if err != nil {
			t.Fatal(err)
		}
		root, err := openBoundApplyRoot(p.Path, journal.RootID)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		if err := writeApplyEntry(root, state, journal, 0, "after"); err != nil {
			t.Fatal(err)
		}
		setApplyGuardHook(t, func(dst string) {
			if dst == "a.txt" {
				_ = os.WriteFile(filepath.Join(p.Path, "a.txt"), []byte("editor save"), 0o600)
			}
		})
		err = p.recoverApplyLocked(state)
		requireApplyConflict(t, p, err, "a.txt", "editor save", "before")
	})
}

func TestApplyRenameExchangeSwapsOnLinux(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for name, data := range map[string]string{"a": "A", "b": "B"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err = applyRenameExchange(root, "a", "b")
	if errors.Is(err, errApplyAtomicUnsupported) {
		t.Skip("platform or filesystem lacks atomic exchange")
	}
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := os.ReadFile(filepath.Join(dir, "a")); string(a) != "B" {
		t.Fatalf("exchange did not swap: a=%q", a)
	}
	if err := applyRenameNoReplace(root, "a", "b"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("RENAME_NOREPLACE clobbered or failed oddly: %v", err)
	}
}
