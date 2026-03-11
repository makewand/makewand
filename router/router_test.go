package router

import (
	"context"
	"testing"
)

// stubProvider is a minimal Provider for testing.
type stubProvider struct {
	name      string
	available bool
	response  string
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) IsAvailable() bool { return s.available }
func (s *stubProvider) Chat(_ context.Context, _ []Message, _ string, _ int) (string, Usage, error) {
	return s.response, Usage{Provider: s.name, Model: s.name}, nil
}
func (s *stubProvider) ChatStream(_ context.Context, _ []Message, _ string, _ int) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Content: s.response, Done: true}
	close(ch)
	return ch, nil
}

func TestNewRouterFromConfig_ConfigFree(t *testing.T) {
	// Verify that a Router can be created without any config package dependency.
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"test": {Provider: &stubProvider{name: "test", available: true, response: "hello"}, Access: AccessSubscription},
		},
		UsageMode: "balanced",
	})

	if !r.ModeSet() {
		t.Fatal("expected mode to be set")
	}
	if r.Mode() != ModeBalanced {
		t.Fatalf("Mode() = %v, want ModeBalanced", r.Mode())
	}

	avail := r.Available()
	if len(avail) != 1 || avail[0] != "test" {
		t.Fatalf("Available() = %v, want [test]", avail)
	}
}

func TestNewRouterFromConfig_LegacyRouting(t *testing.T) {
	stub := &stubProvider{name: "mymodel", available: true, response: "ok"}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"mymodel": {Provider: stub, Access: AccessFree},
		},
		DefaultModel:  "mymodel",
		CodingModel:   "mymodel",
		AnalysisModel: "mymodel",
		ReviewModel:   "mymodel",
	})

	// Legacy routing (no mode set)
	result, err := r.Route(TaskCode)
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if result.Actual != "mymodel" {
		t.Fatalf("Route().Actual = %q, want mymodel", result.Actual)
	}
}

func TestNewRouterFromConfig_Chat(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: "test response"}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	content, _, _, err := r.Chat(context.Background(), TaskCode,
		[]Message{{Role: "user", Content: "hello"}}, "")
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if content != "test response" {
		t.Fatalf("Chat() = %q, want %q", content, "test response")
	}
}

func TestRegisterProvider_Runtime(t *testing.T) {
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{},
		UsageMode: "fast",
	})

	stub := &stubProvider{name: "custom", available: true, response: "custom ok"}
	if err := r.RegisterProvider("custom", stub, AccessFree); err != nil {
		t.Fatalf("RegisterProvider() error = %v", err)
	}

	avail := r.Available()
	if len(avail) != 1 || avail[0] != "custom" {
		t.Fatalf("Available() = %v, want [custom]", avail)
	}
}

func TestClassifyTask_Library(t *testing.T) {
	if got := ClassifyTask("/review check this code"); got != TaskReview {
		t.Fatalf("ClassifyTask(/review) = %v, want TaskReview", got)
	}
	if got := ClassifyTask("fix the bug"); got != TaskFix {
		t.Fatalf("ClassifyTask(fix) = %v, want TaskFix", got)
	}
}

func TestEstimateCost_Library(t *testing.T) {
	// codex-cli is free (subscription), should return 0
	cost := EstimateCost("codex-cli", 1000, 500)
	if cost != 0 {
		t.Fatalf("EstimateCost(codex-cli) = %f, want 0", cost)
	}
}
