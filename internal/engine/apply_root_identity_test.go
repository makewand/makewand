//go:build !windows

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyBindsNewRootAcrossRealRenameABA(t *testing.T) {
	for _, phase := range []string{"prepare", "initial_apply", "recovery"} {
		t.Run(phase, func(t *testing.T) {
			p := applyFixture(t)
			if err := p.WriteFile("old.txt", "workspace-a"); err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(filepath.Dir(p.Path), "workspace-b")
			if err := os.Mkdir(other, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(other, "old.txt"), []byte("workspace-b"), 0o600); err != nil {
				t.Fatal(err)
			}
			if phase == "recovery" {
				state, err := p.openApplyState(context.Background(), true)
				if err != nil {
					t.Fatal(err)
				}
				_, err = p.prepareApplyJournal(context.Background(), state, []ExtractedFile{{Path: "old.txt", Content: "candidate"}})
				state.close()
				if err != nil {
					t.Fatal(err)
				}
			}
			original := openApplyWorkspaceRoot
			defer func() { openApplyWorkspaceRoot = original }()
			calls, wanted := 0, 1
			if phase == "initial_apply" {
				wanted = 2
			}
			openApplyWorkspaceRoot = func(path string) (*os.Root, error) {
				calls++
				if calls != wanted {
					return original(path)
				}
				moved := p.Path + "-moved"
				if err := os.Rename(p.Path, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(other, p.Path); err != nil {
					t.Fatal(err)
				}
				captured, err := original(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(p.Path, other); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(moved, p.Path); err != nil {
					t.Fatal(err)
				}
				return captured, nil
			}
			var err error
			if phase == "recovery" {
				err = p.RecoverInterruptedApply(context.Background())
			} else {
				err = p.ApplyFilesTransactional(context.Background(), []ExtractedFile{{Path: "old.txt", Content: "candidate"}}, nil)
			}
			if !errors.Is(err, ErrStaleApply) || calls != wanted {
				t.Fatalf("ABA root was admitted: phase=%s calls=%d error=%v", phase, calls, err)
			}
			requireApplyContent(t, p, "old.txt", "workspace-a")
			data, err := os.ReadFile(filepath.Join(other, "old.txt"))
			if err != nil || string(data) != "workspace-b" {
				t.Fatalf("captured replacement workspace changed: %q, %v", data, err)
			}
			if phase == "recovery" {
				statePath, _, err := p.applyStatePath()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(statePath, "pending", "journal.json")); err != nil {
					t.Fatalf("recovery discarded conflict journal: %v", err)
				}
			}
		})
	}
}
