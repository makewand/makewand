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
