package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitCommit_NeutralizesInfoAttributesCleanFilter(t *testing.T) {
	p, err := NewProject("git-audit-defense", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(p.Path, "marker")
	for _, args := range [][]string{
		{"init"},
		{"config", "user.name", "Audit Fixture"},
		{"config", "user.email", "fixture@example.invalid"},
		{"config", "filter.audit.clean", "cat; printf filter-ran > " + markerPath},
	} {
		r, err := p.Exec(context.Background(), "git", args...)
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("setup: %v %+v", err, r)
		}
	}

	infoDir := filepath.Join(p.Path, ".git", "info")
	if err := os.MkdirAll(infoDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(infoDir, "attributes"), []byte("*.txt filter=audit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteFile("payload.txt", "fixture"); err != nil {
		t.Fatal(err)
	}

	commitErr := p.GitCommit(context.Background(), "test commit")
	if commitErr != nil {
		t.Fatalf("GitCommit failed: %v", commitErr)
	}

	if _, err := os.Stat(markerPath); err == nil {
		t.Fatal("info/attributes clean filter executed on host! GitCommit failed to neutralize it")
	}
}

func TestGitCommit_ConcurrentShieldAttributes(t *testing.T) {
	p, err := NewProject("git-concurrent-shield", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(p.Path, "marker")
	for _, args := range [][]string{
		{"init"},
		{"config", "user.name", "Concurrent Fixture"},
		{"config", "user.email", "concurrent@example.invalid"},
		{"config", "filter.audit.clean", "cat; printf filter-ran > " + markerPath},
	} {
		r, err := p.Exec(context.Background(), "git", args...)
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("setup: %v %+v", err, r)
		}
	}

	infoDir := filepath.Join(p.Path, ".git", "info")
	if err := os.MkdirAll(infoDir, 0700); err != nil {
		t.Fatal(err)
	}
	attrContent := []byte("*.txt filter=audit\n")
	attrFile := filepath.Join(infoDir, "attributes")
	if err := os.WriteFile(attrFile, attrContent, 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteFile("payload.txt", "fixture"); err != nil {
		t.Fatal(err)
	}

	const goroutines = 8
	errCh := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			if idx%2 == 0 {
				_, statusErr := p.GitStatus(context.Background())
				errCh <- statusErr
			} else {
				_, diffErr := p.GitDiff(context.Background())
				errCh <- diffErr
			}
		}(i)
	}

	for i := 0; i < goroutines; i++ {
		if opErr := <-errCh; opErr != nil {
			t.Errorf("concurrent git operation failed: %v", opErr)
		}
	}

	if _, err := os.Stat(markerPath); err == nil {
		t.Fatal("info/attributes clean filter executed on host during concurrent operations!")
	}

	// Verify that the attributes file is preserved after all operations finish
	content, err := os.ReadFile(attrFile)
	if err != nil {
		t.Fatalf("attributes file missing after concurrent runs: %v", err)
	}
	if string(content) != string(attrContent) {
		t.Fatalf("attributes file content corrupted: got %q, want %q", string(content), string(attrContent))
	}
}

func TestGitCommit_WorktreeCommondirDeduplication(t *testing.T) {
	tempDir := t.TempDir()
	mainRepo := filepath.Join(tempDir, "main")
	wtDir := filepath.Join(tempDir, "worktree")

	p, err := NewProject("git-main", mainRepo)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init"},
		{"config", "user.name", "Worktree Fixture"},
		{"config", "user.email", "worktree@example.invalid"},
	} {
		r, err := p.Exec(context.Background(), "git", args...)
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("setup main: %v %+v", err, r)
		}
	}
	if err := p.WriteFile("init.txt", "base"); err != nil {
		t.Fatal(err)
	}
	if err := p.GitCommit(context.Background(), "init"); err != nil {
		t.Fatalf("main init commit: %v", err)
	}

	// Create linked worktree
	r, err := p.Exec(context.Background(), "git", "worktree", "add", wtDir, "-b", "wt-branch")
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("git worktree add: %v %+v", err, r)
	}

	wtProject, err := NewProject("git-worktree", wtDir)
	if err != nil {
		t.Fatal(err)
	}

	// In both main repo and worktree, simulate attributes shielding
	mainInfo := filepath.Join(mainRepo, ".git", "info")
	if err := os.MkdirAll(mainInfo, 0700); err != nil {
		t.Fatal(err)
	}
	mainAttr := filepath.Join(mainInfo, "attributes")
	if err := os.WriteFile(mainAttr, []byte("*.txt -text\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Verify that GitStatus and GitDiff run smoothly on linked worktree without deadlock
	status, err := wtProject.GitStatus(context.Background())
	if err != nil {
		t.Fatalf("worktree GitStatus failed: %v", err)
	}
	_ = status

	// Verify main repo attributes file remained intact
	data, err := os.ReadFile(mainAttr)
	if err != nil || string(data) != "*.txt -text\n" {
		t.Fatalf("main attributes corrupted or missing: %v", err)
	}
}

func TestGitCommit_OrphanShieldSelfHealing(t *testing.T) {
	p, err := NewProject("git-orphan-heal", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init"},
		{"config", "user.name", "Orphan Fixture"},
		{"config", "user.email", "orphan@example.invalid"},
	} {
		r, err := p.Exec(context.Background(), "git", args...)
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("setup: %v %+v", err, r)
		}
	}

	infoDir := filepath.Join(p.Path, ".git", "info")
	if err := os.MkdirAll(infoDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Simulate crashed process leaving orphan shield file
	orphanFile := filepath.Join(infoDir, "attributes.mw_shield_99999_0")
	expectedContent := "*.dat filter=special\n"
	if err := os.WriteFile(orphanFile, []byte(expectedContent), 0600); err != nil {
		t.Fatal(err)
	}

	// GitStatus should self-heal the orphan into .git/info/attributes
	_, err = p.GitStatus(context.Background())
	if err != nil {
		t.Fatalf("GitStatus failed during orphan recovery: %v", err)
	}

	attrFile := filepath.Join(infoDir, "attributes")
	content, err := os.ReadFile(attrFile)
	if err != nil {
		t.Fatalf("attributes file was not restored from orphan: %v", err)
	}
	if string(content) != expectedContent {
		t.Fatalf("restored content mismatch: got %q, want %q", string(content), expectedContent)
	}
	if _, err := os.Stat(orphanFile); err == nil {
		t.Fatal("orphan shield file still exists after healing!")
	}
}

func TestGitInit_PreservesExistingGitignore(t *testing.T) {
	// Case 1: Fresh repository with no .gitignore creates default
	p1, err := NewProject("git-fresh", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := p1.GitInit(context.Background()); err != nil {
		t.Fatalf("GitInit fresh: %v", err)
	}
	content1, err := p1.ReadFile(".gitignore")
	if err != nil {
		t.Fatalf("ReadFile(.gitignore): %v", err)
	}
	if !strings.Contains(content1, "node_modules/") || !strings.Contains(content1, ".env") {
		t.Fatalf("default .gitignore missing expected entries: %s", content1)
	}

	// Case 2: Existing .gitignore with custom user entries preserves them and appends missing defaults
	p2, err := NewProject("git-existing", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	customContent := "# Custom user ignores\nmy_secret_data/\n*.myextension\n.env\n"
	if err := p2.WriteFile(".gitignore", customContent); err != nil {
		t.Fatal(err)
	}
	if err := p2.GitInit(context.Background()); err != nil {
		t.Fatalf("GitInit existing: %v", err)
	}
	content2, err := p2.ReadFile(".gitignore")
	if err != nil {
		t.Fatalf("ReadFile(.gitignore): %v", err)
	}
	if !strings.HasPrefix(content2, customContent) {
		t.Fatalf("existing custom content was overwritten or altered: got %q, want prefix %q", content2, customContent)
	}
	if !strings.Contains(content2, "node_modules/") || !strings.Contains(content2, "credentials.json") {
		t.Fatalf("missing defaults were not appended: %s", content2)
	}
	// Verify .env was not duplicated
	if strings.Count(content2, ".env") != 1 {
		t.Fatalf(".env was duplicated in .gitignore: %s", content2)
	}

	// Case 3: Calling GitInit when all defaults are present does not mutate the file
	p3, err := NewProject("git-complete", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := p3.GitInit(context.Background()); err != nil {
		t.Fatal(err)
	}
	beforeContent, err := p3.ReadFile(".gitignore")
	if err != nil {
		t.Fatal(err)
	}
	if err := p3.GitInit(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterContent, err := p3.ReadFile(".gitignore")
	if err != nil {
		t.Fatal(err)
	}
	if beforeContent != afterContent {
		t.Fatalf("second GitInit mutated complete .gitignore: got %q, want %q", afterContent, beforeContent)
	}
}

func TestGitCommit_SHA256RepositoryTakesItsOwnEmptyTree(t *testing.T) {
	// A branch named like the SHA-1 empty tree carries a .gitattributes selecting a filter:
	// the SHA-1 id, a ref name in a SHA-256 repository, would read attributes from its tree.
	p, err := NewProject("git-sha256", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(t.TempDir(), "marker")
	git := func(args ...string) {
		t.Helper()
		r, err := p.Exec(context.Background(), "git", args...)
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("setup git %v: %v %+v", args, err, r)
		}
	}
	git("init", "--object-format=sha256")
	git("config", "user.name", "Audit Fixture")
	git("config", "user.email", "fixture@example.invalid")
	if err := p.WriteFile(".gitattributes", "* filter=audit\n"); err != nil {
		t.Fatal(err)
	}
	git("add", ".gitattributes")
	git("commit", "-m", "attributes")
	git("branch", "4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	git("rm", "-q", ".gitattributes")
	git("commit", "-m", "no attributes")
	git("config", "filter.audit.clean", "cat; printf filter-ran >> "+markerPath)
	git("config", "filter.audit.smudge", "cat; printf filter-ran >> "+markerPath)
	if err := p.WriteFile("payload.txt", "fixture"); err != nil {
		t.Fatal(err)
	}

	if err := p.GitCommit(context.Background(), "test commit"); err != nil {
		t.Fatalf("GitCommit failed: %v", err)
	}
	if _, err := p.GitDiff(context.Background()); err != nil {
		t.Fatalf("GitDiff failed: %v", err)
	}
	if status, err := p.GitStatus(context.Background()); err != nil || status != "" {
		t.Fatalf("GitStatus = %q, %v", status, err)
	}
	if _, err := os.Stat(markerPath); err == nil {
		t.Fatal("the filter of a branch named like the SHA-1 empty tree ran on the host")
	}
}
