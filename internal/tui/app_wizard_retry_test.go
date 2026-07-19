package tui

import (
	"context"
	"sync"
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
)

type wizardRetryStubProvider struct {
	name string

	mu        sync.Mutex
	calls     int
	responses []string
	usages    []model.Usage
	errs      []error
}

const wizardRetryProviderName = "wizard-retry"

func (p *wizardRetryStubProvider) Name() string { return p.name }

func (p *wizardRetryStubProvider) IsAvailable() bool { return true }

func (p *wizardRetryStubProvider) Chat(context.Context, []model.Message, string, int) (string, model.Usage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := p.calls
	p.calls++

	var content string
	if idx < len(p.responses) {
		content = p.responses[idx]
	}
	var usage model.Usage
	if idx < len(p.usages) {
		usage = p.usages[idx]
	}
	if usage.Provider == "" {
		usage.Provider = p.name
	}
	var err error
	if idx < len(p.errs) {
		err = p.errs[idx]
	}
	return content, usage, err
}

func (p *wizardRetryStubProvider) ChatStream(context.Context, []model.Message, string, int) (<-chan model.StreamChunk, error) {
	ch := make(chan model.StreamChunk)
	close(ch)
	return ch, nil
}

func newWizardRetryRouter(t *testing.T, provider model.Provider) *model.Router {
	t.Helper()
	return mustNewRouterFromConfig(t, model.RouterConfig{
		UsageMode: config.UsageModeBalanced,
		Providers: map[string]model.ProviderEntry{
			wizardRetryProviderName: {Provider: provider, Access: model.AccessSubscription},
		},
	})
}

func TestRetryWizardBuildForMissingFiles_SkipsRetryWhenFilesAlreadyPresent(t *testing.T) {
	stub := &wizardRetryStubProvider{name: wizardRetryProviderName}
	router := newWizardRetryRouter(t, stub)

	content := "--- FILE: index.html ---\n```\n<h1>ok</h1>\n```"
	usage := model.Usage{Provider: wizardRetryProviderName, InputTokens: 10, OutputTokens: 20, Cost: 0.1}

	gotContent, gotUsage := retryWizardBuildForMissingFiles(context.Background(), router, "build site", content, usage)
	if gotContent != content {
		t.Fatalf("content changed unexpectedly")
	}
	if gotUsage != usage {
		t.Fatalf("usage changed unexpectedly: got %+v want %+v", gotUsage, usage)
	}
	if stub.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", stub.calls)
	}
}

func TestRetryWizardBuildForMissingFiles_RecoversWithStrictRetry(t *testing.T) {
	stub := &wizardRetryStubProvider{
		name: wizardRetryProviderName,
		responses: []string{
			"--- FILE: index.html ---\n```\n<h1>recovered</h1>\n```",
		},
		usages: []model.Usage{
			{Provider: wizardRetryProviderName, InputTokens: 3, OutputTokens: 7, Cost: 0.2},
		},
	}
	router := newWizardRetryRouter(t, stub)

	original := "The project is already complete. Run open index.html."
	usage := model.Usage{Provider: wizardRetryProviderName, InputTokens: 11, OutputTokens: 13, Cost: 0.5}

	gotContent, gotUsage := retryWizardBuildForMissingFiles(context.Background(), router, "build site", original, usage)
	if gotContent == original {
		t.Fatalf("expected recovered content, got original")
	}
	if gotUsage.InputTokens != 14 || gotUsage.OutputTokens != 20 {
		t.Fatalf("usage tokens not accumulated: %+v", gotUsage)
	}
	if gotUsage.Cost != 0.7 {
		t.Fatalf("usage cost = %.3f, want 0.7", gotUsage.Cost)
	}
	if gotUsage.Provider != wizardRetryProviderName {
		t.Fatalf("usage provider = %q, want %s", gotUsage.Provider, wizardRetryProviderName)
	}
	if stub.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", stub.calls)
	}
}

func TestRetryWizardBuildForMissingFiles_LeavesOriginalWhenRetryStillNonFile(t *testing.T) {
	stub := &wizardRetryStubProvider{
		name: wizardRetryProviderName,
		responses: []string{
			"```bash\nopen password-strength-checker/index.html\n```",
		},
		usages: []model.Usage{
			{Provider: "claude", InputTokens: 2, OutputTokens: 2},
		},
	}
	router := newWizardRetryRouter(t, stub)

	original := "Project already exists."
	usage := model.Usage{Provider: wizardRetryProviderName, InputTokens: 7, OutputTokens: 9, Cost: 0.1}

	gotContent, gotUsage := retryWizardBuildForMissingFiles(context.Background(), router, "build site", original, usage)
	if gotContent != original {
		t.Fatalf("content changed unexpectedly: %q", gotContent)
	}
	if gotUsage.InputTokens != 9 || gotUsage.OutputTokens != 11 || gotUsage.Cost != usage.Cost {
		t.Fatalf("invalid retry usage was not accumulated: got %+v", gotUsage)
	}
	if gotUsage.Provider != usage.Provider {
		t.Fatalf("original content provider = %q, want %q", gotUsage.Provider, usage.Provider)
	}
	if stub.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", stub.calls)
	}
}

func TestBuildWizardCodeFormatRetryPrompt_IncludesStrictRules(t *testing.T) {
	prompt := buildWizardCodeFormatRetryPrompt("build a todo app", "project already exists")
	if prompt == "" {
		t.Fatal("prompt should not be empty")
	}
	for _, want := range []string{
		"Original project request",
		"Previous response",
		"Output ONLY file blocks",
		"--- FILE: path/to/file ---",
	} {
		if !containsSubstring(prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
}

func containsSubstring(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
