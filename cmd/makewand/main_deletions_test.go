package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/model"
)

type deletingCandidateProvider struct {
	name   string
	mutate func(context.Context) error
}

func (p *deletingCandidateProvider) Name() string      { return p.name }
func (p *deletingCandidateProvider) IsAvailable() bool { return true }
func (p *deletingCandidateProvider) Chat(ctx context.Context, _ []model.Message, _ string, _ int) (string, model.Usage, error) {
	if p.mutate != nil {
		if err := p.mutate(ctx); err != nil {
			return "", model.Usage{}, err
		}
	}
	// delete-only candidate: no writable content.
	return "", model.Usage{Provider: p.name}, nil
}
func (p *deletingCandidateProvider) ChatStream(context.Context, []model.Message, string, int) (<-chan model.StreamChunk, error) {
	ch := make(chan model.StreamChunk)
	close(ch)
	return ch, nil
}

// TestHeadlessDeleteOnlyCandidateSurfacesDeletionWarning verifies the headless
// path surfaces a delete-only candidate's deletions: the candidate removes a
// baseline file and returns empty content, and the headless deletion notice must
// still report it rather than dropping it on the empty-content early return.
func TestHeadlessDeleteOnlyCandidateSurfacesDeletionWarning(t *testing.T) {
	// The delete-only path runs no external commands, but opt into host exec so
	// the test never depends on bubblewrap being present.
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "1")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/headlessdel\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(go.mod): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package headlessdel\n\nfunc Mul(a, b int) int { return a + b }\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(calc.go): %v", err)
	}
	project, err := engine.OpenProject(dir)
	if err != nil {
		t.Fatalf("OpenProject: %v", err)
	}

	deleter := &deletingCandidateProvider{
		name: "deleter",
		mutate: func(ctx context.Context) error {
			wd, ok := model.WorkDirFromContext(ctx)
			if !ok {
				return context.Canceled
			}
			return os.Remove(filepath.Join(wd, "calc.go"))
		},
	}
	router, err := model.NewRouterFromConfig(model.RouterConfig{
		Providers: map[string]model.ProviderEntry{"deleter": {Provider: deleter}},
		UsageMode: "balanced",
	})
	if err != nil {
		t.Fatalf("NewRouterFromConfig: %v", err)
	}
	router.SetMode(model.ModeBalanced)

	selection := engine.RunCandidateSelection(
		context.Background(),
		router,
		project,
		model.PhaseCode,
		[]model.Message{{Role: "user", Content: "trim the project"}},
		"system",
		nil,
	)

	if strings.TrimSpace(selection.Content) != "" {
		t.Fatalf("selection.Content = %q, want empty for a delete-only candidate", selection.Content)
	}
	if len(selection.DeletedFiles) == 0 {
		t.Fatalf("selection.DeletedFiles empty, want it to include calc.go")
	}

	notice := headlessDeletionNotice(selection.DeletedFiles)
	wantFragment := fmt.Sprintf(i18n.Msg().AutomationCandidateDeletions, "calc.go")
	if !strings.Contains(notice, wantFragment) {
		t.Fatalf("headlessDeletionNotice = %q, want it to contain %q", notice, wantFragment)
	}
	if !strings.HasPrefix(notice, "[makewand] ") {
		t.Fatalf("headlessDeletionNotice = %q, want the [makewand] stderr prefix", notice)
	}
}
