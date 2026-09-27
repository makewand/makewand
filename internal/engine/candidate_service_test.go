package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/model"
)

// unsafeCandidateProvider does NOT implement router.UntrustedRepoCapable, so the
// router treats it as unsafe in untrusted-repo mode and fails closed.
type unsafeCandidateProvider struct {
	name string
}

func (p *unsafeCandidateProvider) Name() string      { return p.name }
func (p *unsafeCandidateProvider) IsAvailable() bool { return true }
func (p *unsafeCandidateProvider) Chat(context.Context, []model.Message, string, int) (string, model.Usage, error) {
	// Must never be called: untrusted mode fails closed before executing.
	return "should-not-run", model.Usage{Provider: p.name}, nil
}
func (p *unsafeCandidateProvider) ChatStream(context.Context, []model.Message, string, int) (<-chan model.StreamChunk, error) {
	ch := make(chan model.StreamChunk)
	close(ch)
	return ch, nil
}

// TestRunCandidateSelection_SurfacesUntrustedSafeSentinel verifies that when every
// candidate attempt fails closed with model.ErrNoUntrustedSafeProvider (untrusted
// mode, no direct-API provider), the selection carries the sentinel on its Err
// field instead of swallowing it into a generic failure.
func TestRunCandidateSelection_SurfacesUntrustedSafeSentinel(t *testing.T) {
	router, err := model.NewRouterFromConfig(model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"claude": {Provider: &unsafeCandidateProvider{name: "claude"}},
		},
		UsageMode: "balanced",
	})
	if err != nil {
		t.Fatalf("NewRouterFromConfig: %v", err)
	}
	router.SetMode(model.ModeBalanced)
	router.SetRepoTrust(model.RepoTrustUntrusted)

	// project == nil keeps the attempt path free of cloning/verification: the
	// attempt fails closed the moment router.ChatWith rejects the unsafe provider.
	selection := RunCandidateSelection(
		context.Background(),
		router,
		nil,
		model.PhaseCode,
		[]model.Message{{Role: "user", Content: "do something"}},
		"system",
		nil,
	)

	if selection.Content != "" {
		t.Fatalf("selection.Content = %q, want empty (all attempts failed closed)", selection.Content)
	}
	if selection.Err == nil {
		t.Fatal("selection.Err is nil, want the fail-closed sentinel surfaced")
	}
	if !errors.Is(selection.Err, model.ErrNoUntrustedSafeProvider) {
		t.Fatalf("selection.Err = %v, want errors.Is ErrNoUntrustedSafeProvider", selection.Err)
	}
}

// TestRunCandidateSelection_OrdinaryFailureLeavesErrNil verifies the sentinel
// capture does not disturb ordinary failures: a provider that simply returns a
// non-sentinel error yields a selection with a nil Err (unchanged behavior).
func TestRunCandidateSelection_OrdinaryFailureLeavesErrNil(t *testing.T) {
	router, err := model.NewRouterFromConfig(model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"claude": {Provider: &failingCandidateProvider{name: "claude"}},
		},
		UsageMode: "balanced",
	})
	if err != nil {
		t.Fatalf("NewRouterFromConfig: %v", err)
	}
	router.SetMode(model.ModeBalanced)

	selection := RunCandidateSelection(
		context.Background(),
		router,
		nil,
		model.PhaseCode,
		[]model.Message{{Role: "user", Content: "do something"}},
		"system",
		nil,
	)

	if selection.Err != nil {
		t.Fatalf("selection.Err = %v, want nil for an ordinary non-sentinel failure", selection.Err)
	}
}

// failingCandidateProvider returns an ordinary (non-sentinel) error from Chat.
type failingCandidateProvider struct {
	name string
}

func (p *failingCandidateProvider) Name() string      { return p.name }
func (p *failingCandidateProvider) IsAvailable() bool { return true }
func (p *failingCandidateProvider) Chat(context.Context, []model.Message, string, int) (string, model.Usage, error) {
	return "", model.Usage{Provider: p.name}, errors.New("boom")
}
func (p *failingCandidateProvider) ChatStream(context.Context, []model.Message, string, int) (<-chan model.StreamChunk, error) {
	ch := make(chan model.StreamChunk)
	close(ch)
	return ch, nil
}

type fixedContentProvider struct {
	name    string
	content string
}

func (p *fixedContentProvider) Name() string      { return p.name }
func (p *fixedContentProvider) IsAvailable() bool { return true }
func (p *fixedContentProvider) Chat(context.Context, []model.Message, string, int) (string, model.Usage, error) {
	return p.content, model.Usage{Provider: p.name}, nil
}
func (p *fixedContentProvider) ChatStream(context.Context, []model.Message, string, int) (<-chan model.StreamChunk, error) {
	ch := make(chan model.StreamChunk)
	close(ch)
	return ch, nil
}

func TestRunCandidateSelection_DeliversExactVerifiedPayload(t *testing.T) {
	allowHostExecForTest(t)
	p, err := NewProject("candidate-verified-payload", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.SetUnsafeHostExecAuthorization(UnsafeHostExecAuthorization{Acknowledged: true, Source: "test"})
	err = p.WriteFiles([]ExtractedFile{
		{Path: "go.mod", Content: "module example.com/audit\n\ngo 1.22\n"},
		{Path: "math.go", Content: "package audit\nfunc Add(a, b int) int { return a + b }\n"},
		{Path: "math_test.go", Content: "package audit\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(2, 2) != 4 { t.Fatal(\"wrong\") } }\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.ScanFiles(); err != nil {
		t.Fatal(err)
	}

	files := []ExtractedFile{
		{Path: "math.go", Content: "package audit\nfunc Add(a, b int) int { return b + a }\n"},
		{Path: "math_test.go", Content: "package audit\nTHIS IS INVALID GO\n"},
	}
	provider := &fixedContentProvider{name: "claude", content: RenderExtractedFiles(files)}
	r, err := model.NewRouterFromConfig(model.RouterConfig{
		Providers: map[string]model.ProviderEntry{"claude": {Provider: provider}},
		UsageMode: "balanced",
	})
	if err != nil {
		t.Fatal(err)
	}

	selection := RunCandidateSelection(context.Background(), r, p, model.PhaseCode, []model.Message{{Role: "user", Content: "implement addition"}}, "", nil)
	if selection.Verified {
		t.Fatalf("candidate-controlled evidence must require approval, got %+v", selection)
	}
	if strings.Contains(selection.Content, "THIS IS INVALID GO") {
		t.Fatal("unverified test file was delivered in selection.Content")
	}
	if len(selection.VerifiedFiles) != 1 || selection.VerifiedFiles[0].Path != "math.go" {
		t.Fatalf("unexpected VerifiedFiles: %+v", selection.VerifiedFiles)
	}
	if selection.VerifiedDigest == "" {
		t.Fatal("expected non-empty VerifiedDigest")
	}
	// Applying delivered selection to baseline project must succeed and pass verification
	if err = p.WriteFiles(ParseFilesBestEffort(selection.Content).Files); err != nil {
		t.Fatal(err)
	}
	plan, err := p.DetectTestPlan()
	if err != nil || plan == nil {
		t.Fatalf("DetectTestPlan: %v", err)
	}
	result, err := p.RunVerificationPlan(context.Background(), *plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("delivered content failed verification: %s", result.Stderr)
	}
}
