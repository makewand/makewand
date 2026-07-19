package router

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type ensembleCaptureProvider struct {
	mu            sync.Mutex
	name          string
	available     bool
	content       string
	usage         Usage
	model         string
	mode          UsageMode
	modeSet       bool
	task          TaskType
	taskSet       bool
	messages      []Message
	system        string
	calls         int
	errAfterFirst error
}

func (p *ensembleCaptureProvider) Name() string      { return p.name }
func (p *ensembleCaptureProvider) IsAvailable() bool { return p.available }

func (p *ensembleCaptureProvider) Chat(ctx context.Context, messages []Message, system string, _ int) (string, Usage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls > 1 && p.errAfterFirst != nil {
		return "", Usage{}, p.errAfterFirst
	}
	p.model, _ = ModelFromContext(ctx)
	p.mode, p.modeSet = UsageModeFromContext(ctx)
	p.task, p.taskSet = TaskFromContext(ctx)
	p.messages = append([]Message(nil), messages...)
	p.system = system
	return p.content, p.usage, nil
}

func newAllEmptyPowerErrorRouter() *Router {
	fallbackErr := errors.New("adaptive fallback failed")
	gemini := &ensembleCaptureProvider{
		name:          "gemini",
		available:     true,
		usage:         Usage{Provider: "gemini", Cost: 1.25, InputTokens: 10, OutputTokens: 1},
		errAfterFirst: fallbackErr,
	}
	claude := &ensembleCaptureProvider{
		name:          "claude",
		available:     true,
		usage:         Usage{Provider: "claude", Cost: 2.50, InputTokens: 20, OutputTokens: 2},
		errAfterFirst: fallbackErr,
	}
	judge := &ensembleCaptureProvider{name: "codex", available: false}
	return powerReviewRouter(gemini, claude, judge)
}

func (p *ensembleCaptureProvider) ChatStream(context.Context, []Message, string, int) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

func powerReviewRouter(gemini, claude, codex Provider) *Router {
	providers := map[string]Provider{
		"gemini": gemini,
		"claude": claude,
		"codex":  codex,
	}
	cache := make(map[providerKey]Provider, len(providers))
	for name, provider := range providers {
		cache[providerKey{name: name, modelID: testModelID(name, TierPremium)}] = provider
	}
	return &Router{
		providers:     providers,
		providerCache: cache,
		accessTypes: map[string]AccessType{
			"gemini": AccessSubscription,
			"claude": AccessSubscription,
			"codex":  AccessSubscription,
		},
		usage:   newSessionUsage(),
		mode:    ModePower,
		modeSet: true,
	}
}

func TestChatBestPowerPropagatesRequestAndExecutionContext(t *testing.T) {
	gemini := &ensembleCaptureProvider{
		name:      "gemini",
		available: true,
		content:   "candidate from gemini",
		usage:     Usage{Cost: 1, InputTokens: 10, OutputTokens: 11},
	}
	claude := &ensembleCaptureProvider{
		name:      "claude",
		available: true,
		content:   "candidate from claude",
		usage:     Usage{Cost: 2, InputTokens: 20, OutputTokens: 21},
	}
	codex := &ensembleCaptureProvider{
		name:      "codex",
		available: true,
		content:   "WINNER: 2\nThe second candidate follows the request.",
		usage:     Usage{Cost: 3, InputTokens: 30, OutputTokens: 31},
	}
	r := powerReviewRouter(gemini, claude, codex)

	content, total, route, err := r.ChatBest(
		context.Background(),
		PhaseReview,
		[]Message{{Role: "user", Content: "ORIGINAL_TASK_SENTINEL"}},
		"ORIGINAL_SYSTEM_SENTINEL",
	)
	if err != nil {
		t.Fatalf("ChatBest: %v", err)
	}
	if content != claude.content || route.Actual != "claude" {
		t.Fatalf("selected content/provider = %q/%q, want claude candidate", content, route.Actual)
	}
	if total.Cost != 6 || total.InputTokens != 60 || total.OutputTokens != 63 {
		t.Fatalf("total usage = %+v, want all generators and judge exactly once", total)
	}

	for _, generator := range []*ensembleCaptureProvider{gemini, claude} {
		generator.mu.Lock()
		if !generator.modeSet || generator.mode != ModePower {
			t.Errorf("%s mode = %v/%v, want power/set", generator.name, generator.mode, generator.modeSet)
		}
		if !generator.taskSet || generator.task != TaskReview {
			t.Errorf("%s task = %v/%v, want review/set", generator.name, generator.task, generator.taskSet)
		}
		if generator.model != testModelID(generator.name, TierPremium) {
			t.Errorf("%s model = %q, want premium model %q", generator.name, generator.model, testModelID(generator.name, TierPremium))
		}
		generator.mu.Unlock()
	}

	codex.mu.Lock()
	if !codex.modeSet || codex.mode != ModePower || !codex.taskSet || codex.task != TaskAnalyze {
		t.Errorf("judge context mode/task = %v/%v and %v/%v, want power and prompt-driven analyze", codex.mode, codex.modeSet, codex.task, codex.taskSet)
	}
	if codex.model != testModelID("codex", TierPremium) {
		t.Errorf("judge model = %q, want %q", codex.model, testModelID("codex", TierPremium))
	}
	if len(codex.messages) != 1 {
		t.Fatalf("judge messages = %d, want 1", len(codex.messages))
	}
	judgePrompt := codex.messages[0].Content
	codex.mu.Unlock()
	for _, want := range []string{"ORIGINAL_TASK_SENTINEL", "ORIGINAL_SYSTEM_SENTINEL", gemini.content, claude.content} {
		if !strings.Contains(judgePrompt, want) {
			t.Errorf("judge prompt does not contain %q", want)
		}
	}

	r.usage.mu.Lock()
	winnerQuality := r.usage.quality[qualityKey{PhaseReview, "claude"}]
	loserQuality := r.usage.quality[qualityKey{PhaseReview, "gemini"}]
	r.usage.mu.Unlock()
	if winnerQuality == nil || winnerQuality.Successes != 1 {
		t.Fatalf("winner quality = %+v, want one success", winnerQuality)
	}
	if loserQuality == nil || loserQuality.Failures != 1 {
		t.Fatalf("loser quality = %+v, want one failure", loserQuality)
	}
}

func TestChatBestPowerSingleResultDoesNotDoubleCountOrReward(t *testing.T) {
	gemini := &ensembleCaptureProvider{
		name:      "gemini",
		available: true,
		content:   "only candidate",
		usage:     Usage{Cost: 1.25, InputTokens: 12, OutputTokens: 8},
	}
	claude := &ensembleCaptureProvider{name: "claude", available: false}
	codex := &ensembleCaptureProvider{name: "codex", available: true, content: "WINNER: 1"}
	r := powerReviewRouter(gemini, claude, codex)

	_, total, _, err := r.ChatBest(context.Background(), PhaseReview, []Message{{Role: "user", Content: "review"}}, "")
	if err != nil {
		t.Fatalf("ChatBest: %v", err)
	}
	if total.Cost != 1.25 || total.InputTokens != 12 || total.OutputTokens != 8 {
		t.Fatalf("total usage = %+v, want the sole generator exactly once", total)
	}
	if got := r.usage.QualitySampleCount(PhaseReview, "gemini"); got != 0 {
		t.Fatalf("single unjudged result quality samples = %d, want 0", got)
	}
}

func TestParseWinnerIndexAcceptsSingleLine(t *testing.T) {
	if got, ok := parseWinnerIndex("WINNER: 2", 2); !ok || got != 1 {
		t.Fatalf("parseWinnerIndex = %d/%v, want 1/true", got, ok)
	}
	if _, ok := parseWinnerIndex("WINNER: 3", 2); ok {
		t.Fatal("out-of-range winner was accepted")
	}
}

func TestPowerReviewJudgeUsesPromptDrivenCodexCommand(t *testing.T) {
	provider := NewCodexCLI("codex")
	ctx := ContextWithTask(context.Background(), taskForJudge())
	cmd := provider.buildCmd(ctx, "JUDGE_PROMPT_SENTINEL")
	if len(cmd.Args) < 2 || cmd.Args[1] != "exec" {
		t.Fatalf("Codex judge argv = %v, want prompt-driven exec command", cmd.Args)
	}
	if strings.Join(cmd.Args, "\n") == "" || !strings.Contains(strings.Join(cmd.Args, "\n"), "JUDGE_PROMPT_SENTINEL") {
		t.Fatalf("Codex judge argv does not contain judge prompt: %v", cmd.Args)
	}
}

func TestChatBestPowerAccountsForEmptyGeneratorUsage(t *testing.T) {
	gemini := &ensembleCaptureProvider{
		name:      "gemini",
		available: true,
		content:   "",
		usage:     Usage{Cost: 1.25, InputTokens: 10, OutputTokens: 1},
	}
	claude := &ensembleCaptureProvider{
		name:      "claude",
		available: true,
		content:   "usable candidate",
		usage:     Usage{Cost: 2.50, InputTokens: 20, OutputTokens: 2},
	}
	judge := &ensembleCaptureProvider{name: "codex", available: true, content: "WINNER: 1"}
	r := powerReviewRouter(gemini, claude, judge)

	content, total, result, err := r.ChatBest(context.Background(), PhaseReview, []Message{{Role: "user", Content: "review"}}, "")
	if err != nil {
		t.Fatalf("ChatBest: %v", err)
	}
	if content != "usable candidate" || result.Actual != "claude" {
		t.Fatalf("selected content/provider = %q/%q, want sole usable candidate", content, result.Actual)
	}
	if total.Cost != 3.75 || total.InputTokens != 30 || total.OutputTokens != 3 {
		t.Fatalf("total usage = %+v, want empty and usable generator usage", total)
	}
	judge.mu.Lock()
	judgeCalls := judge.calls
	judge.mu.Unlock()
	if judgeCalls != 0 {
		t.Fatalf("judge calls = %d, want 0 for one usable candidate", judgeCalls)
	}
}

func TestChatBestPowerReturnsUsageWhenAllGeneratorsEmptyAndFallbackFails(t *testing.T) {
	r := newAllEmptyPowerErrorRouter()

	content, total, _, err := r.ChatBest(context.Background(), PhaseReview, []Message{{Role: "user", Content: "review"}}, "")
	if err == nil {
		t.Fatal("ChatBest error = nil, want adaptive fallback failure")
	}
	if content != "" {
		t.Fatalf("content = %q, want empty", content)
	}
	if total.Cost != 3.75 || total.InputTokens != 30 || total.OutputTokens != 3 {
		t.Fatalf("total usage = %+v, want both empty generator attempts", total)
	}
	if total.Provider != "ensemble" {
		t.Fatalf("usage provider = %q, want ensemble for an unselected aggregate", total.Provider)
	}
}
