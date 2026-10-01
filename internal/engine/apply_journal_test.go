package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func applyFixture(t *testing.T) *Project {
	t.Helper()
	root := t.TempDir()
	t.Setenv("MAKEWAND_APPLY_STATE_DIR", filepath.Join(root, "private"))
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	return &Project{Path: workspace}
}

func requireApplyContent(t *testing.T, p *Project, path, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(p.Path, path))
	if err != nil || string(data) != want {
		t.Fatalf("%s=%q, err=%v; want %q", path, data, err, want)
	}
}

func TestApplyFilesTransactionalCommitsAndPreservesMode(t *testing.T) {
	p := applyFixture(t)
	if err := p.WriteFile("old.txt", "before"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(p.Path, "old.txt"))
	if err != nil {
		t.Fatal(err)
	}
	files := []ExtractedFile{{Path: "old.txt", Content: "after"}, {Path: "new/deep.txt", Content: "new"}}
	if err := p.ApplyFilesTransactional(context.Background(), files, nil); err != nil {
		t.Fatal(err)
	}
	requireApplyContent(t, p, "old.txt", "after")
	requireApplyContent(t, p, "new/deep.txt", "new")
	got, err := os.Stat(filepath.Join(p.Path, "old.txt"))
	if err != nil || got.Mode().Perm() != info.Mode().Perm() {
		t.Fatalf("mode changed: %v %v", got, err)
	}
	path, _, err := p.applyStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, "pending")); !os.IsNotExist(err) {
		t.Fatalf("journal not cleaned: %v", err)
	}
}

func TestApplyValidationAndProtectedPathsWriteNothing(t *testing.T) {
	p := applyFixture(t)
	if err := p.WriteFile("old.txt", "before"); err != nil {
		t.Fatal(err)
	}
	veto := errors.New("baseline changed")
	err := p.ApplyFilesTransactional(context.Background(), []ExtractedFile{{Path: "old.txt", Content: "after"}}, func() error { return veto })
	if !errors.Is(err, veto) {
		t.Fatal(err)
	}
	requireApplyContent(t, p, "old.txt", "before")
	err = p.ApplyFilesTransactional(context.Background(), []ExtractedFile{{Path: "old.txt", Content: "after"}, {Path: ".git/config", Content: "bad"}}, nil)
	if err == nil {
		t.Fatal("protected path accepted")
	}
	requireApplyContent(t, p, "old.txt", "before")
}

type applyEditingContext struct {
	context.Context
	p      *Project
	edited bool
	err    error
}

func (c *applyEditingContext) Err() error {
	if !c.edited {
		// Simulate an editor acting after the first replacement, before the
		// next application step. The editor does not use the makewand lock.
		data, err := os.ReadFile(filepath.Join(c.p.Path, "a.txt"))
		if err == nil && string(data) == "new-a" {
			c.edited = true
			c.err = c.p.WriteFile("b.txt", "external editor")
		}
	}
	return c.err
}

func TestApplyPreservesEditBetweenReplacements(t *testing.T) {
	p := applyFixture(t)
	if err := p.WriteFile("a.txt", "old-a"); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteFile("b.txt", "old-b"); err != nil {
		t.Fatal(err)
	}
	ctx := &applyEditingContext{Context: context.Background(), p: p}
	err := p.ApplyFilesTransactional(ctx, []ExtractedFile{{Path: "a.txt", Content: "new-a"}, {Path: "b.txt", Content: "new-b"}}, nil)
	if !ctx.edited || !errors.Is(err, ErrStaleApply) {
		t.Fatalf("external edit not detected: edited=%v err=%v", ctx.edited, err)
	}
	requireApplyContent(t, p, "a.txt", "new-a")
	requireApplyContent(t, p, "b.txt", "external editor")
	if err := p.RecoverInterruptedApply(context.Background()); !errors.Is(err, ErrStaleApply) {
		t.Fatalf("conflicting journal was not preserved: %v", err)
	}
}

func TestApplyRecoveryCleanupUsesHeldRoot(t *testing.T) {
	p := applyFixture(t)
	if err := os.Mkdir(filepath.Join(p.Path, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	moved := p.Path + "-original"
	if runtime.GOOS == "windows" {
		// Windows holds this directory against rename while os.Root is open.
		// Exercise that real kernel protection, cleanup through the held root,
		// and a separate intact tree instead of requiring a POSIX rename.
		external := p.Path + "-external"
		if err := os.MkdirAll(filepath.Join(external, "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(p.Path, moved); err == nil {
			t.Fatal("workspace was renamed while its root handle was open")
		}
		removeApplyCreatedDirs(root, []string{"nested"})
		if _, err := os.Stat(filepath.Join(p.Path, "nested")); !os.IsNotExist(err) {
			t.Fatalf("held root's directory was not removed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(external, "nested")); err != nil {
			t.Fatalf("external tree changed: %v", err)
		}
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(p.Path, moved); err != nil {
			t.Fatalf("workspace remained pinned after closing the root: %v", err)
		}
		return
	}
	if err := os.Rename(p.Path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p.Path, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	removeApplyCreatedDirs(root, []string{"nested"})
	if _, err := os.Stat(filepath.Join(p.Path, "nested")); err != nil {
		t.Fatalf("replacement workspace changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, "nested")); !os.IsNotExist(err) {
		t.Fatalf("original root's directory not removed: %v", err)
	}
}

func TestApplyPublishedPreparationErrorPreservesRecoveryImages(t *testing.T) {
	p := applyFixture(t)
	if err := p.WriteFile("old.txt", "before"); err != nil {
		t.Fatal(err)
	}
	state, err := p.openApplyState(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	fault := errors.New("directory flush failed after journal publication")
	state.syncDirectory = func(string) error { return fault }
	_, err = p.prepareApplyJournal(context.Background(), state, []ExtractedFile{{Path: "old.txt", Content: "after"}})
	if !errors.Is(err, fault) {
		t.Fatalf("expected directory flush failure, got %v", err)
	}
	requireApplyContent(t, p, "old.txt", "before")
	for _, name := range []string{"journal.json", "000000.before", "000000.after"} {
		if _, err := state.root.Stat(filepath.Join("pending", name)); err != nil {
			t.Fatalf("published journal lost %s: %v", name, err)
		}
	}
	state.syncDirectory = nil
	if err := p.recoverApplyLocked(state); err != nil {
		t.Fatalf("recover prepared journal: %v", err)
	}
	requireApplyContent(t, p, "old.txt", "before")
}

func preparePartialApply(t *testing.T, p *Project) (*applyJournalState, *applyJournal) {
	t.Helper()
	if err := p.WriteFile("a.txt", "old-a"); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteFile("b.txt", "old-b"); err != nil {
		t.Fatal(err)
	}
	state, err := p.openApplyState(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	files := []ExtractedFile{{Path: "a.txt", Content: "new-a"}, {Path: "b.txt", Content: "new-b"}, {Path: "nested/new.txt", Content: "created"}}
	journal, err := p.prepareApplyJournal(context.Background(), state, files)
	if err != nil {
		state.close()
		t.Fatal(err)
	}
	root, err := openBoundApplyRoot(p.Path, journal.RootID)
	if err != nil {
		state.close()
		t.Fatal(err)
	}
	defer root.Close()
	for _, index := range []int{0, 2} {
		if err := writeApplyEntry(root, state, journal, index, files[index].Content); err != nil {
			state.close()
			t.Fatal(err)
		}
	}
	return state, journal
}

func TestApplyRecoveryRestoresWholeBatchAndRemovesCreatedPaths(t *testing.T) {
	p := applyFixture(t)
	state, _ := preparePartialApply(t, p)
	state.close()
	reopened, err := OpenProject(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	requireApplyContent(t, reopened, "a.txt", "old-a")
	requireApplyContent(t, reopened, "b.txt", "old-b")
	if _, err := os.Stat(filepath.Join(p.Path, "nested")); !os.IsNotExist(err) {
		t.Fatalf("created directory remains: %v", err)
	}
	if err := p.RecoverInterruptedApply(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRecoveryPreservesEveryFileOnExternalConflict(t *testing.T) {
	p := applyFixture(t)
	state, _ := preparePartialApply(t, p)
	state.close()
	if err := p.WriteFile("b.txt", "external editor"); err != nil {
		t.Fatal(err)
	}
	err := p.RecoverInterruptedApply(context.Background())
	if !errors.Is(err, ErrStaleApply) {
		t.Fatalf("expected conflict, got %v", err)
	}
	requireApplyContent(t, p, "a.txt", "new-a")
	requireApplyContent(t, p, "b.txt", "external editor")
	requireApplyContent(t, p, "nested/new.txt", "created")
	path, _, _ := p.applyStatePath()
	if _, err := os.Stat(filepath.Join(path, "pending", "journal.json")); err != nil {
		t.Fatal("lost journal:", err)
	}
}

func TestApplyRecoveryCorruptBlobWritesNothing(t *testing.T) {
	p := applyFixture(t)
	state, _ := preparePartialApply(t, p)
	if err := os.WriteFile(filepath.Join(state.path, "pending", "000001.before"), []byte("tampered"), 0o600); err != nil {
		state.close()
		t.Fatal(err)
	}
	state.close()
	if err := p.RecoverInterruptedApply(context.Background()); err == nil {
		t.Fatal("corrupt preimage accepted")
	}
	requireApplyContent(t, p, "a.txt", "new-a")
	requireApplyContent(t, p, "b.txt", "old-b")
}

func TestApplyWriterLockHonorsCancellation(t *testing.T) {
	p := applyFixture(t)
	state, err := p.openApplyState(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err = p.ApplyFilesTransactional(ctx, []ExtractedFile{{Path: "new.txt", Content: "new"}}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock ignored context: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.Path, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("wrote while locked: %v", err)
	}
}

func TestApplyRecoveryCompletedMarkerSurvivesPartialCleanup(t *testing.T) {
	for _, status := range []string{"committed", "rolled_back"} {
		t.Run(status, func(t *testing.T) {
			p := applyFixture(t)
			state, journal := preparePartialApply(t, p)
			journal.State = status
			if err := state.write(journal); err != nil {
				state.close()
				t.Fatal(err)
			}
			if err := state.root.Remove(filepath.Join("pending", "000000.before")); err != nil {
				state.close()
				t.Fatal(err)
			}
			state.close()
			if err := p.RecoverInterruptedApply(context.Background()); err != nil {
				t.Fatal(err)
			}
			requireApplyContent(t, p, "a.txt", "new-a")
		})
	}
}

func TestApplyHardProcessInterruption(t *testing.T) {
	if os.Getenv("MAKEWAND_APPLY_CRASH_HELPER") == "1" {
		p := &Project{Path: os.Getenv("MAKEWAND_APPLY_CRASH_WORKSPACE")}
		state, journal := preparePartialApply(t, p)
		defer state.close()
		// The scratch path comes from this test helper's private generated journal.
		if err := os.WriteFile(filepath.Join(p.Path, journal.Entries[1].Temp), []byte("new-"), 0o600); err != nil { //nolint:gosec
			t.Fatal(err)
		}
		fmt.Println("APPLY-PARTIAL-READY")
		for {
			time.Sleep(time.Hour)
		}
	}
	p := applyFixture(t)
	// The executable is this running test binary; no candidate controls argv.
	command := exec.Command(os.Args[0], "-test.run=^TestApplyHardProcessInterruption$") //nolint:gosec
	command.Env = append(os.Environ(), "MAKEWAND_APPLY_CRASH_HELPER=1", "MAKEWAND_APPLY_CRASH_WORKSPACE="+p.Path)
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "APPLY-PARTIAL-READY") {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("helper exited before partial apply")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("helper did not reach crash barrier")
	}
	requireApplyContent(t, p, "a.txt", "new-a")
	requireApplyContent(t, p, "b.txt", "old-b")
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	reopened, err := OpenProject(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	requireApplyContent(t, reopened, "a.txt", "old-a")
	requireApplyContent(t, reopened, "b.txt", "old-b")
	if _, err := os.Stat(filepath.Join(p.Path, "nested", "new.txt")); !os.IsNotExist(err) {
		t.Fatal("crash-created file remains", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(p.Path, ".makewand-apply-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("crashed replacement left scratch files: %v %v", leftovers, err)
	}
}

func TestApplyRecoveryRejectsReplacedWorkspaceIdentity(t *testing.T) {
	p := applyFixture(t)
	state, _ := preparePartialApply(t, p)
	state.close()
	if err := os.Rename(p.Path, p.Path+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteFile("a.txt", "external root"); err != nil {
		t.Fatal(err)
	}
	err := p.RecoverInterruptedApply(context.Background())
	if !errors.Is(err, ErrStaleApply) {
		t.Fatal("accepted replaced workspace", err)
	}
	requireApplyContent(t, p, "a.txt", "external root")
}

func TestApplyRecoveryForeignScratchWritesNothing(t *testing.T) {
	for _, kind := range []string{"foreign-content", "directory"} {
		t.Run(kind, func(t *testing.T) {
			p := applyFixture(t)
			state, journal := preparePartialApply(t, p)
			temp := filepath.Join(p.Path, journal.Entries[1].Temp)
			if kind == "directory" {
				if err := os.Mkdir(temp, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(temp, []byte("external scratch"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			state.close()
			if err := p.RecoverInterruptedApply(context.Background()); !errors.Is(err, ErrStaleApply) {
				t.Fatal("scratch conflict ignored", err)
			}
			requireApplyContent(t, p, "a.txt", "new-a")
			requireApplyContent(t, p, "b.txt", "old-b")
			if _, err := os.Stat(temp); err != nil {
				t.Fatal("foreign scratch deleted", err)
			}
		})
	}
}
