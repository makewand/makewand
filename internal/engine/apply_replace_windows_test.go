//go:build windows

package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestApplyWindowsReadonlyReplacementAndACL(t *testing.T) {
	p := applyFixture(t)
	path := filepath.Join(p.Path, "readonly.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	security := "D:P(A;;FA;;;" + user.User.Sid.String() + ")"
	if !user.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		security += "(A;;FA;;;SY)"
	}
	if err := applySetOpenFileSecurity(file, security); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	_ = file.Close()
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o666) })
	beforeSecurity, err := applyFileSecurity(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyFilesTransactional(context.Background(), []ExtractedFile{{Path: "readonly.txt", Content: "after"}}, nil); err != nil {
		t.Fatal(err)
	}
	requireApplyContent(t, p, "readonly.txt", "after")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("readonly attribute changed: %v, %v", info, err)
	}
	afterSecurity, err := applyFileSecurity(path)
	if err != nil || afterSecurity != beforeSecurity {
		t.Fatalf("ACL changed: %q -> %q, %v", beforeSecurity, afterSecurity, err)
	}
}

func TestApplyWindowsReadonlyRecoveryKeepsACL(t *testing.T) {
	p := applyFixture(t)
	path := filepath.Join(p.Path, "readonly.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o666) })
	beforeSecurity, err := applyFileSecurity(path)
	if err != nil {
		t.Fatal(err)
	}
	state, err := p.openApplyState(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	files := []ExtractedFile{{Path: "readonly.txt", Content: "after"}}
	journal, err := p.prepareApplyJournal(context.Background(), state, files)
	if err != nil {
		state.close()
		t.Fatal(err)
	}
	root, err := os.OpenRoot(p.Path)
	if err != nil {
		state.close()
		t.Fatal(err)
	}
	err = writeApplyEntry(root, state, journal, 0, "after")
	_ = root.Close()
	state.close()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RecoverInterruptedApply(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireApplyContent(t, p, "readonly.txt", "before")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("readonly preimage not restored: %v, %v", info, err)
	}
	afterSecurity, err := applyFileSecurity(path)
	if err != nil || afterSecurity != beforeSecurity {
		t.Fatalf("restored ACL changed: %q -> %q, %v", beforeSecurity, afterSecurity, err)
	}
}

func TestApplyWindowsNewReadonlyFileRecoveryRemovesOnlyTheCreatedName(t *testing.T) {
	p := applyFixture(t)
	state, err := p.openApplyState(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	files := []ExtractedFile{{Path: "new.txt", Content: "generated", ModeKnown: true, Mode: 0o444}}
	journal, err := p.prepareApplyJournal(context.Background(), state, files)
	if err != nil {
		state.close()
		t.Fatal(err)
	}
	root, err := os.OpenRoot(p.Path)
	if err != nil {
		state.close()
		t.Fatal(err)
	}
	err = writeApplyEntry(root, state, journal, 0, "generated")
	_ = root.Close()
	state.close()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.Path, "new.txt")
	otherName := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.Link(path, otherName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(otherName, 0o666) })
	if err := p.RecoverInterruptedApply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("created file was not removed: %v", err)
	}
	info, err := os.Stat(otherName)
	if err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("removing the new name altered the outside hardlink: %v, %v", info, err)
	}
}

func TestApplyWindowsJunctionCannotRedirectReplacementOrACL(t *testing.T) {
	p := applyFixture(t)
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(p.Path, "junction")
	command := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, outside)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v, %s", err, output)
	}
	t.Cleanup(func() { _ = os.Remove(junction) })
	beforeSecurity, err := applyFileSecurity(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if err := privateApplyDirectory(junction); err == nil {
		t.Fatal("private state accepted a junction")
	}
	if _, err := applyFileSecurity(filepath.Join(junction, "sentinel.txt")); err == nil {
		t.Fatal("ACL capture followed a junction")
	}
	if err := os.WriteFile(filepath.Join(p.Path, ".source.txt"), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := replaceApplyFile(root, ".source.txt", filepath.Join("junction", "sentinel.txt")); err == nil {
		t.Fatal("replacement followed a junction")
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "outside" {
		t.Fatalf("outside content changed: %q, %v", data, err)
	}
	afterSecurity, err := applyFileSecurity(sentinel)
	if err != nil || afterSecurity != beforeSecurity {
		t.Fatalf("outside ACL changed: %q -> %q, %v", beforeSecurity, afterSecurity, err)
	}
}

func TestApplyWindowsDirectoryPinPreventsRename(t *testing.T) {
	p := applyFixture(t)
	pins, err := pinApplyWindowsDirectories(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.Path, p.Path+"-moved"); err == nil {
		pins.close()
		t.Fatal("directory was replaced while pinned")
	}
	pins.close()
	if err := os.Rename(p.Path, p.Path+"-moved"); err != nil {
		t.Fatal(err)
	}
}

func requireWindowsInterruptedImages(t *testing.T, p *Project) {
	t.Helper()
	requireApplyContent(t, p, "a.txt", "new-a")
	requireApplyContent(t, p, "b.txt", "old-b")
	requireApplyContent(t, p, "nested/new.txt", "created")
}

func setWindowsUserOnlyACL(t *testing.T, path string) string {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = applySetOpenFileSecurity(file, "D:P(A;;FA;;;"+user.User.Sid.String()+")")
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	security, err := applyFileSecurity(path)
	if err != nil {
		t.Fatal(err)
	}
	return security
}

func TestApplyWindowsACLOnlyConflictPreventsWholeBatchRecovery(t *testing.T) {
	for _, name := range []string{"a.txt", "nested/new.txt"} {
		t.Run(name, func(t *testing.T) {
			p := applyFixture(t)
			state, journal := preparePartialApply(t, p)
			for _, index := range []int{0, 2} {
				if journal.Entries[index].AfterSecurity == "" {
					state.close()
					t.Fatal("published postimage lacks a durable ACL seal")
				}
			}
			pending := filepath.Join(state.path, "pending", "journal.json")
			state.close()
			path := filepath.Join(p.Path, filepath.FromSlash(name))
			before, err := applyFileSecurity(path)
			if err != nil {
				t.Fatal(err)
			}
			edited := setWindowsUserOnlyACL(t, path)
			if edited == before {
				t.Fatal("fixture must change only the file DACL")
			}
			requireWindowsInterruptedImages(t, p)
			if err := p.RecoverInterruptedApply(context.Background()); !errors.Is(err, ErrStaleApply) {
				t.Fatalf("ACL conflict accepted: %v", err)
			}
			requireWindowsInterruptedImages(t, p)
			current, err := applyFileSecurity(path)
			if err != nil || current != edited {
				t.Fatalf("external ACL edit lost: %q -> %q, %v", edited, current, err)
			}
			if _, err := os.Stat(pending); err != nil {
				t.Fatalf("conflict evidence was removed: %v", err)
			}
		})
	}
}

func TestApplyWindowsNewFileWithoutAfterSecurityFailsClosed(t *testing.T) {
	p := applyFixture(t)
	state, journal := preparePartialApply(t, p)
	if !journal.Entries[2].Existed && journal.Entries[2].AfterSecurity != "" {
		journal.Entries[2].AfterSecurity = ""
	} else {
		state.close()
		t.Fatal("fixture must publish and seal a new file")
	}
	if err := state.write(journal); err != nil {
		state.close()
		t.Fatal(err)
	}
	pending := filepath.Join(state.path, "pending", "journal.json")
	state.close()
	if err := p.RecoverInterruptedApply(context.Background()); !errors.Is(err, ErrStaleApply) {
		t.Fatalf("new postimage without its ACL seal accepted: %v", err)
	}
	requireWindowsInterruptedImages(t, p)
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("invalid recovery evidence was removed: %v", err)
	}
}

func TestApplyWindowsHeldRootIdentityMustMatchJournal(t *testing.T) {
	p := applyFixture(t)
	state, journal := preparePartialApply(t, p)
	state.close()
	other := t.TempDir()
	root, err := os.OpenRoot(other)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := checkApplyRoot(root, journal.RootID); !errors.Is(err, ErrStaleApply) {
		t.Fatalf("unrelated held directory accepted the journal root identity: %v", err)
	}
	bound, err := openBoundApplyRoot(other, journal.RootID)
	if bound != nil {
		_ = bound.Close()
		t.Fatal("opened a root with the wrong directory identity")
	}
	if !errors.Is(err, ErrStaleApply) {
		t.Fatalf("root binding failed open: %v", err)
	}
	requireWindowsInterruptedImages(t, p)
	if files, err := os.ReadDir(other); err != nil || len(files) != 0 {
		t.Fatalf("unrelated held directory changed: %v, %v", files, err)
	}
}
