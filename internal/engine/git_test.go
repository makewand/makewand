package engine

import (
	"context"
	"os"
	"path/filepath"
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
