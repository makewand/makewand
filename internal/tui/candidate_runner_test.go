package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/model"
)

type fixedCandidateProvider struct {
	name      string
	content   string
	chatErr   error
	chatCalls int
	mutate    func(context.Context) error
}

type usageAfterCancelProvider struct {
	name  string
	usage model.Usage
}

func (p *usageAfterCancelProvider) Name() string      { return p.name }
func (p *usageAfterCancelProvider) IsAvailable() bool { return true }
func (p *usageAfterCancelProvider) Chat(ctx context.Context, _ []model.Message, _ string, _ int) (string, model.Usage, error) {
	<-ctx.Done()
	return "", p.usage, nil
}
func (p *usageAfterCancelProvider) ChatStream(context.Context, []model.Message, string, int) (<-chan model.StreamChunk, error) {
	ch := make(chan model.StreamChunk)
	close(ch)
	return ch, nil
}

func (p *fixedCandidateProvider) Name() string      { return p.name }
func (p *fixedCandidateProvider) IsAvailable() bool { return true }
func (p *fixedCandidateProvider) Chat(ctx context.Context, messages []model.Message, system string, maxTokens int) (string, model.Usage, error) {
	p.chatCalls++
	if p.chatErr != nil {
		return "", model.Usage{}, p.chatErr
	}
	if p.mutate != nil {
		if err := p.mutate(ctx); err != nil {
			return "", model.Usage{}, err
		}
	}
	return p.content, model.Usage{Provider: "stub", InputTokens: 10, OutputTokens: 20, Cost: 0.1}, nil
}
func (p *fixedCandidateProvider) ChatStream(ctx context.Context, messages []model.Message, system string, maxTokens int) (<-chan model.StreamChunk, error) {
	ch := make(chan model.StreamChunk, 1)
	ch <- model.StreamChunk{Content: p.content, Done: true}
	close(ch)
	return ch, nil
}

// allowHostExecForTest opts candidate verification into direct host execution
// via the documented escape hatch so unit tests do not depend on bubblewrap
// being installed. The fail-closed path is covered in internal/engine.
func allowHostExecForTest(t *testing.T) {
	t.Helper()
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "1")
}

func mustNewRouterFromConfig(t *testing.T, rc model.RouterConfig) *model.Router {
	t.Helper()
	router, err := model.NewRouterFromConfig(rc)
	if err != nil {
		t.Fatalf("NewRouterFromConfig: %v", err)
	}
	return router
}

func newCandidateProject(t *testing.T) *engine.Project {
	t.Helper()
	allowHostExecForTest(t)
	project, err := engine.NewProject("candidate-project", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := project.WriteFiles([]engine.ExtractedFile{
		{
			Path: "go.mod",
			Content: `module example.com/candidate

go 1.22
`,
		},
		{
			Path: "calc.go",
			Content: `package candidate

func Mul(a, b int) int {
	return a * b
}
`,
		},
		{
			Path: "calc_test.go",
			Content: `package candidate

import "testing"

func TestMul(t *testing.T) {
	if got := Mul(3, 4); got != 12 {
		t.Fatalf("Mul(3,4) = %d, want 12", got)
	}
}
`,
		},
	}); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}
	if err := project.ScanFiles(); err != nil {
		t.Fatalf("ScanFiles: %v", err)
	}
	return project
}

func TestRunCandidateSelection_PrefersVerifiedCandidate(t *testing.T) {
	project := newCandidateProject(t)
	candidateFiles := engine.ParseFilesBestEffort("--- FILE: calc.go ---\n```\npackage candidate\n\nfunc Mul(a, b int) int {\n\treturn a * b\n}\n```\n").Files
	if len(candidateFiles) != 1 {
		t.Fatalf("ParseFilesBestEffort returned %d files, want 1", len(candidateFiles))
	}
	report, err := project.EvaluateCandidateFiles(context.Background(), candidateFiles)
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if !report.Passed {
		t.Fatalf("direct candidate verification failed: %+v", report)
	}

	router := mustNewRouterFromConfig(t, model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"alpha": {Provider: &fixedCandidateProvider{name: "alpha", content: "--- FILE: calc.go ---\n```\npackage candidate\n\nfunc Mul(a, b int) int {\n\treturn a + b\n}\n```\n"}},
			"bravo": {Provider: &fixedCandidateProvider{name: "bravo", content: "--- FILE: calc.go ---\n```\npackage candidate\n\nfunc Mul(a, b int) int {\n\treturn a * b\n}\n```\n"}},
		},
		UsageMode: "balanced",
	})
	router.SetMode(model.ModeBalanced)

	selection := runCandidateSelection(
		context.Background(),
		router,
		project,
		model.PhaseCode,
		[]model.Message{{Role: "user", Content: "fix multiplication"}},
		"system",
	)

	if !selection.verified {
		t.Fatal("selection.verified = false, want true")
	}
	if selection.provider != "bravo" {
		t.Fatalf("selection.provider = %q, want %q", selection.provider, "bravo")
	}
	if selection.content == "" {
		t.Fatal("selection.content = empty, want chosen candidate output")
	}
}

func TestAutopilotApprovalModeUsesCandidateWorkflow(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ApprovalMode = config.ApprovalModeAuto
	app := *NewApp(ModeChat, cfg, "")
	if !app.shouldUseAutopilotCandidates() {
		t.Fatal("shouldUseAutopilotCandidates() = false, want true")
	}
}

func TestRunCandidateSelection_PreservesUsageWhenNoCandidateHasContent(t *testing.T) {
	alpha := &fixedCandidateProvider{name: "alpha"}
	bravo := &fixedCandidateProvider{name: "bravo"}
	router := mustNewRouterFromConfig(t, model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"alpha": {Provider: alpha},
			"bravo": {Provider: bravo},
		},
		UsageMode: "balanced",
	})

	selection := runCandidateSelection(
		context.Background(),
		router,
		nil,
		model.PhaseCode,
		[]model.Message{{Role: "user", Content: "build something"}},
		"system",
	)

	if selection.content != "" {
		t.Fatalf("selection content = %q, want empty", selection.content)
	}
	if selection.provider != "ensemble" {
		t.Fatalf("selection provider = %q, want ensemble usage attribution", selection.provider)
	}
	if selection.usage.InputTokens != 20 || selection.usage.OutputTokens != 40 || selection.usage.Cost != 0.2 {
		t.Fatalf("selection usage = %+v, want both empty attempts", selection.usage)
	}
}

func TestRunCandidateSelection_IsolatesProvidersFromFallback(t *testing.T) {
	project := newCandidateProject(t)
	alpha := &fixedCandidateProvider{name: "alpha", chatErr: context.DeadlineExceeded}
	bravo := &fixedCandidateProvider{name: "bravo", content: "--- FILE: calc.go ---\n```\npackage candidate\n\nfunc Mul(a, b int) int {\n\treturn a * b\n}\n```\n"}
	charlie := &fixedCandidateProvider{name: "charlie", content: "--- FILE: calc.go ---\n```\npackage candidate\n\nfunc Mul(a, b int) int {\n\treturn a * b\n}\n```\n"}

	router := mustNewRouterFromConfig(t, model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"alpha":   {Provider: alpha},
			"bravo":   {Provider: bravo},
			"charlie": {Provider: charlie},
		},
		UsageMode: "balanced",
	})
	router.SetMode(model.ModeBalanced)

	selection := runCandidateSelection(
		context.Background(),
		router,
		project,
		model.PhaseCode,
		[]model.Message{{Role: "user", Content: "fix multiplication"}},
		"system",
	)

	if !selection.verified {
		t.Fatal("selection.verified = false, want true")
	}
	if alpha.chatCalls != 1 || bravo.chatCalls != 1 || charlie.chatCalls != 1 {
		t.Fatalf("chatCalls = (%d, %d, %d), want (1, 1, 1)", alpha.chatCalls, bravo.chatCalls, charlie.chatCalls)
	}
}

func TestRunCandidateSelection_CapturesAgentWorkspaceChanges(t *testing.T) {
	project := newCandidateProject(t)
	if err := project.WriteFiles([]engine.ExtractedFile{{
		Path: "calc.go",
		Content: `package candidate

func Mul(a, b int) int {
	return a + b
}
`,
	}}); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}
	if err := project.ScanFiles(); err != nil {
		t.Fatalf("ScanFiles: %v", err)
	}

	mutator := &fixedCandidateProvider{
		name:    "mutator",
		content: "I fixed the bug.",
		mutate: func(ctx context.Context) error {
			dir, ok := model.WorkDirFromContext(ctx)
			if !ok {
				return context.Canceled
			}
			return os.WriteFile(filepath.Join(dir, "calc.go"), []byte(`package candidate

func Mul(a, b int) int {
	return a * b
}
`), 0o600)
		},
	}

	router := mustNewRouterFromConfig(t, model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"mutator": {Provider: mutator},
		},
		UsageMode: "balanced",
	})
	router.SetMode(model.ModeBalanced)

	selection := runCandidateSelection(
		context.Background(),
		router,
		project,
		model.PhaseCode,
		[]model.Message{{Role: "user", Content: "fix multiplication"}},
		"system",
	)

	if !selection.verified {
		t.Fatal("selection.verified = false, want true")
	}
	if !strings.Contains(selection.content, "--- FILE: calc.go ---") {
		t.Fatalf("selection.content = %q, want synthesized file block", selection.content)
	}
	content, err := project.ReadFile("calc.go")
	if err != nil {
		t.Fatalf("ReadFile(calc.go): %v", err)
	}
	if !strings.Contains(content, "return a + b") {
		t.Fatalf("project calc.go was modified in place: %q", content)
	}
}

func newTestlessCandidateProject(t *testing.T) *engine.Project {
	t.Helper()
	allowHostExecForTest(t)
	project, err := engine.NewProject("candidate-notests", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := project.WriteFiles([]engine.ExtractedFile{
		{
			Path: "go.mod",
			Content: `module example.com/candidatenotests

go 1.22
`,
		},
		{
			Path: "calc.go",
			Content: `package candidatenotests

func Mul(a, b int) int {
	return a + b
}
`,
		},
	}); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}
	if err := project.ScanFiles(); err != nil {
		t.Fatalf("ScanFiles: %v", err)
	}
	return project
}

func TestRunCandidateSelection_StrengthOneCandidateIsNotAutoVerified(t *testing.T) {
	project := newTestlessCandidateProject(t)

	// The candidate compiles and "go test ./..." exits 0, but no tests exist,
	// so verification strength caps at 1 and autopilot must not auto-apply it.
	router := mustNewRouterFromConfig(t, model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"alpha": {Provider: &fixedCandidateProvider{name: "alpha", content: "--- FILE: calc.go ---\n```\npackage candidatenotests\n\nfunc Mul(a, b int) int {\n\treturn a * b\n}\n```\n"}},
		},
		UsageMode: "balanced",
	})
	router.SetMode(model.ModeBalanced)

	selection := runCandidateSelection(
		context.Background(),
		router,
		project,
		model.PhaseCode,
		[]model.Message{{Role: "user", Content: "fix multiplication"}},
		"system",
	)

	if selection.verified {
		t.Fatal("selection.verified = true, want false for Strength 1 candidate")
	}
	if strings.TrimSpace(selection.content) == "" {
		t.Fatal("selection.content = empty, want fallback content for manual approval")
	}
	if selection.selectionNote != i18n.Msg().AutomationCandidateWeakVerification {
		t.Fatalf("selectionNote = %q, want weak verification notice", selection.selectionNote)
	}
}

func TestRunCandidateSelection_UnreportedCloneEditJoinsChangedSet(t *testing.T) {
	project := newCandidateProject(t)

	// The provider reports a FILE block for calc.go but also writes an
	// unreported helper file into its workspace clone. The authoritative
	// changed set is the union, so both must be verified and applied.
	mutator := &fixedCandidateProvider{
		name:    "mutator",
		content: "--- FILE: calc.go ---\n```\npackage candidate\n\nfunc Mul(a, b int) int {\n\treturn a * b\n}\n```\n",
		mutate: func(ctx context.Context) error {
			dir, ok := model.WorkDirFromContext(ctx)
			if !ok {
				return context.Canceled
			}
			return os.WriteFile(filepath.Join(dir, "helper.go"), []byte(`package candidate

func Twice(a int) int {
	return Mul(a, 2)
}
`), 0o600)
		},
	}

	router := mustNewRouterFromConfig(t, model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"mutator": {Provider: mutator},
		},
		UsageMode: "balanced",
	})
	router.SetMode(model.ModeBalanced)

	selection := runCandidateSelection(
		context.Background(),
		router,
		project,
		model.PhaseCode,
		[]model.Message{{Role: "user", Content: "fix multiplication"}},
		"system",
	)

	if !selection.verified {
		t.Fatal("selection.verified = false, want true")
	}
	if !strings.Contains(selection.content, "--- FILE: calc.go ---") {
		t.Fatalf("selection.content = %q, want reported calc.go block", selection.content)
	}
	if !strings.Contains(selection.content, "--- FILE: helper.go ---") {
		t.Fatalf("selection.content = %q, want unreported helper.go clone edit included", selection.content)
	}
}

func TestOrderedCandidateProviders_FixLimitsToTwoProviders(t *testing.T) {
	router := mustNewRouterFromConfig(t, model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"claude": {Provider: &fixedCandidateProvider{name: "claude"}},
			"codex":  {Provider: &fixedCandidateProvider{name: "codex"}},
			"gemini": {Provider: &fixedCandidateProvider{name: "gemini"}},
		},
		UsageMode: "balanced",
	})
	router.SetMode(model.ModeBalanced)

	providers := orderedCandidateProviders(router, model.PhaseFix)
	if len(providers) != 2 {
		t.Fatalf("len(providers) = %d, want 2", len(providers))
	}
}

func TestOrderedCandidateProviders_FixLearnsFromCandidateVerification(t *testing.T) {
	router := mustNewRouterFromConfig(t, model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"claude": {Provider: &fixedCandidateProvider{name: "claude", content: "--- FILE: calc.go ---\n```\npackage candidate\n\nfunc Mul(a, b int) int {\n\treturn a * b\n}\n```\n"}},
			"codex":  {Provider: &fixedCandidateProvider{name: "codex", content: "--- FILE: calc.go ---\n```\npackage candidate\n\nfunc Mul(a, b int) int {\n\treturn a + b\n}\n```\n"}},
		},
		UsageMode: "balanced",
	})
	router.SetMode(model.ModeBalanced)

	for i := 0; i < 40; i++ {
		router.RecordQualityOutcome(model.PhaseFix, "claude", true)
		router.RecordQualityOutcome(model.PhaseFix, "codex", false)
	}

	providers := orderedCandidateProviders(router, model.PhaseFix)
	if len(providers) == 0 {
		t.Fatal("orderedCandidateProviders returned no providers")
	}
	if providers[0] != "claude" {
		t.Fatalf("providers[0] = %q, want %q after repeated codex failures", providers[0], "claude")
	}
}

func TestShouldRecordCandidateQuality(t *testing.T) {
	tests := []struct {
		name    string
		attempt candidateAttempt
		want    bool
	}{
		{
			name: "verified candidate",
			attempt: candidateAttempt{
				provider: "claude",
				content:  "--- FILE: calc.go ---",
				verification: engine.CandidateVerification{
					Passed: true,
				},
			},
			want: true,
		},
		{
			name: "rejected writable candidate",
			attempt: candidateAttempt{
				provider: "codex",
				content:  "--- FILE: calc.go ---",
			},
			want: true,
		},
		{
			name: "provider error",
			attempt: candidateAttempt{
				provider: "codex",
				err:      context.DeadlineExceeded,
			},
			want: false,
		},
		{
			name: "empty response",
			attempt: candidateAttempt{
				provider: "codex",
			},
			want: false,
		},
	}

	for _, tt := range tests {
		if got := shouldRecordCandidateQuality(tt.attempt); got != tt.want {
			t.Fatalf("%s: shouldRecordCandidateQuality() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestRunCandidateSelection_CancelsSlowCandidatesAfterVerifiedWinner(t *testing.T) {
	project := newCandidateProject(t)
	slowCanceled := make(chan struct{}, 2)
	slowProvider := func(name string) *fixedCandidateProvider {
		return &fixedCandidateProvider{
			name: name,
			mutate: func(ctx context.Context) error {
				select {
				case <-ctx.Done():
					select {
					case slowCanceled <- struct{}{}:
					default:
					}
					return ctx.Err()
				case <-time.After(2 * time.Second):
					return nil
				}
			},
		}
	}

	router := mustNewRouterFromConfig(t, model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"alpha":   {Provider: slowProvider("alpha")},
			"bravo":   {Provider: &fixedCandidateProvider{name: "bravo", content: "--- FILE: calc.go ---\n```\npackage candidate\n\nfunc Mul(a, b int) int {\n\treturn a * b\n}\n```\n"}},
			"charlie": {Provider: slowProvider("charlie")},
		},
		UsageMode: "balanced",
	})
	router.SetMode(model.ModeBalanced)

	start := time.Now()
	selection := runCandidateSelection(
		context.Background(),
		router,
		project,
		model.PhaseCode,
		[]model.Message{{Role: "user", Content: "fix multiplication"}},
		"system",
	)
	elapsed := time.Since(start)

	if !selection.verified {
		t.Fatal("selection.verified = false, want true")
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("runCandidateSelection elapsed = %s, want early cancellation before 2s", elapsed)
	}

	canceled := 0
	timeout := time.After(500 * time.Millisecond)
	for canceled < 2 {
		select {
		case <-slowCanceled:
			canceled++
		case <-timeout:
			t.Fatalf("slow candidates canceled = %d, want 2", canceled)
		}
	}
}

func TestRunCandidateSelection_DrainsUsageAfterEarlyWinnerCancellation(t *testing.T) {
	project := newCandidateProject(t)
	router := mustNewRouterFromConfig(t, model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"alpha": {
				Provider: &usageAfterCancelProvider{
					name:  "alpha",
					usage: model.Usage{Provider: "alpha", InputTokens: 7, OutputTokens: 5, Cost: 0.4},
				},
			},
			"bravo": {
				Provider: &fixedCandidateProvider{
					name:    "bravo",
					content: "--- FILE: calc.go ---\n```\npackage candidate\n\nfunc Mul(a, b int) int {\n\treturn a * b\n}\n```\n",
				},
			},
		},
		UsageMode: "balanced",
	})

	selection := runCandidateSelection(
		context.Background(),
		router,
		project,
		model.PhaseCode,
		[]model.Message{{Role: "user", Content: "fix multiplication"}},
		"system",
	)

	if !selection.verified || selection.provider != "bravo" {
		t.Fatalf("selection = %+v, want verified bravo winner", selection)
	}
	if selection.usage.InputTokens != 17 || selection.usage.OutputTokens != 25 || selection.usage.Cost != 0.5 {
		t.Fatalf("selection usage = %+v, want winner plus canceled in-flight attempt", selection.usage)
	}
}

func TestRunCandidateSelectionWithActivity_ShowsProviderProgress(t *testing.T) {
	project := newCandidateProject(t)
	activity := newChatActivityState()
	started := make(chan struct{})
	release := make(chan struct{})

	provider := &fixedCandidateProvider{
		name:    "alpha",
		content: "--- FILE: calc.go ---\n```\npackage candidate\n\nfunc Mul(a, b int) int {\n\treturn a * b\n}\n```\n",
		mutate: func(ctx context.Context) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}

	router := mustNewRouterFromConfig(t, model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"alpha": {Provider: provider},
		},
		UsageMode: "balanced",
	})
	router.SetMode(model.ModeBalanced)

	done := make(chan candidateSelection, 1)
	go func() {
		done <- runCandidateSelectionWithActivity(
			context.Background(),
			activity,
			router,
			project,
			model.PhaseCode,
			[]model.Message{{Role: "user", Content: "fix multiplication"}},
			"system",
		)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}

	deadline := time.Now().Add(time.Second)
	for !strings.Contains(activity.Snapshot().Detail, "alpha generating") {
		if time.Now().After(deadline) {
			t.Fatalf("activity detail = %q, want generating status", activity.Snapshot().Detail)
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(release)
	selection := <-done
	if !selection.verified {
		t.Fatal("selection.verified = false, want true")
	}
	if !strings.Contains(activity.Snapshot().Detail, "alpha passed") {
		t.Fatalf("activity detail = %q, want passed status", activity.Snapshot().Detail)
	}
}

func TestCandidateProgressReporter_StopPreventsLateActivityReactivation(t *testing.T) {
	activity := newChatActivityState()
	activity.Start()
	reporter := newCandidateProgressReporter(activity, []string{"alpha"})

	reporter.Set("alpha", candidateProgressRunning)
	reporter.Stop()
	activity.Reset()
	reporter.Set("alpha", candidateProgressCanceled)

	snapshot := activity.Snapshot()
	if snapshot.Active {
		t.Fatalf("activity should stay inactive after reporter.Stop, snapshot=%+v", snapshot)
	}
}
