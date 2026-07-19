package tui

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/model"
)

// TestRunCandidateSelection_UntrustedSentinelMapsToActionableMessage verifies the
// TUI candidate path fails closed with the actionable untrusted-mode message: an
// untrusted router whose only provider is not untrusted-repo-safe yields a
// candidateSelection carrying ErrNoUntrustedSafeProvider on its err field,
// contentError preserves the sentinel over the generic fallback, and
// chatErrorContent renders the RepoTrustNoSafeProvider message.
func TestRunCandidateSelection_UntrustedSentinelMapsToActionableMessage(t *testing.T) {
	router := mustNewRouterFromConfig(t, model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			// fixedCandidateProvider does not implement UntrustedRepoCapable, so the
			// router treats it as unsafe in untrusted mode and fails closed.
			"claude": {Provider: &fixedCandidateProvider{name: "claude", content: "should-not-run"}},
		},
		UsageMode: "balanced",
	})
	router.SetMode(model.ModeBalanced)
	router.SetRepoTrust(model.RepoTrustUntrusted)

	// project == nil keeps the attempt free of cloning/verification: it fails
	// closed the moment the router rejects the unsafe provider.
	selection := runCandidateSelection(
		context.Background(),
		router,
		nil,
		model.PhaseCode,
		[]model.Message{{Role: "user", Content: "add a feature"}},
		"system",
	)

	if selection.content != "" {
		t.Fatalf("selection.content = %q, want empty (fail closed)", selection.content)
	}
	if !errors.Is(selection.err, model.ErrNoUntrustedSafeProvider) {
		t.Fatalf("selection.err = %v, want errors.Is ErrNoUntrustedSafeProvider", selection.err)
	}

	generic := fmt.Errorf("no candidate provider produced a response")
	surfaced := selection.contentError(generic)
	if !errors.Is(surfaced, model.ErrNoUntrustedSafeProvider) {
		t.Fatalf("contentError = %v, want the sentinel (not the generic error)", surfaced)
	}
	if got := chatErrorContent(surfaced); got != i18n.Msg().RepoTrustNoSafeProvider {
		t.Fatalf("chatErrorContent = %q, want the actionable untrusted-mode message %q", got, i18n.Msg().RepoTrustNoSafeProvider)
	}

	// A non-sentinel selection keeps the generic fallback unchanged.
	if got := (candidateSelection{}).contentError(generic); got != generic {
		t.Fatalf("contentError(no sentinel) = %v, want the generic fallback", got)
	}
}
