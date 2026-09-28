package router

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestLocalReviewTaskKeepsCodexReviewUncommitted guards the local (non-HTTP)
// contract: a TUI/headless review still uses `codex review --uncommitted`.
func TestLocalReviewTaskKeepsCodexReviewUncommitted(t *testing.T) {
	p := NewCodexCLI("/usr/bin/codex")
	cmd := p.buildCmd(ContextWithTask(context.Background(), TaskReview), "review this code")
	if len(cmd.Args) < 3 || cmd.Args[1] != "review" || cmd.Args[2] != "--uncommitted" {
		t.Fatalf("local review argv = %v, want codex review --uncommitted", cmd.Args)
	}

	remote := ContextWithRemoteOrigin(ContextWithTask(context.Background(), TaskReview))
	cmd = p.buildCmd(remote, "review this code")
	if len(cmd.Args) < 2 || cmd.Args[1] != "exec" || cmd.Args[len(cmd.Args)-1] != "review this code" {
		t.Fatalf("remote review argv = %v, want codex exec ... <prompt>", cmd.Args)
	}
	cmd = p.buildStreamCmd(remote, "review this code")
	if len(cmd.Args) < 2 || cmd.Args[1] != "exec" || cmd.Args[len(cmd.Args)-1] != "review this code" {
		t.Fatalf("remote review stream argv = %v, want codex exec ... <prompt>", cmd.Args)
	}
}

// TestRemoteOriginCLIHonorsExplicitWorkDir keeps an embedder-provided WorkDir:
// only requests WITHOUT an explicit WorkDir get the per-request scratch dir.
func TestRemoteOriginCLIHonorsExplicitWorkDir(t *testing.T) {
	logDir := t.TempDir()
	codex := writeRecordingCodexCLI(t, logDir)
	work := t.TempDir()
	p := NewCodexCLI(codex)
	ctx := ContextWithWorkDir(ContextWithRemoteOrigin(context.Background()), work)
	if _, _, err := p.Chat(ctx, []Message{{Role: "user", Content: "hello"}}, "", 256); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	calls := readFakeCodexInvocations(t, logDir)
	if len(calls) != 1 {
		t.Fatalf("invocations = %d, want 1", len(calls))
	}
	want, _ := filepath.EvalSymlinks(work)
	if calls[0].dir != want {
		t.Fatalf("dir = %q, want explicit WorkDir %q", calls[0].dir, want)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("explicit WorkDir must not be removed: %v", err)
	}
}
