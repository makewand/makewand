//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/model"
)

func TestE2_CommandUnknownOutcomeErrorClassification(t *testing.T) {
	preStart := &commandUnknownOutcomeError{
		Err:     fmt.Errorf("exec go interrupted: %w", context.DeadlineExceeded),
		Started: false,
	}
	if !errors.Is(preStart, execution.ErrUnknownOutcome) {
		t.Fatalf("preStart must match execution.ErrUnknownOutcome")
	}
	if isStartedCommandUnknownOutcome(preStart) {
		t.Fatalf("preStart must not be classified as a started command unknown outcome")
	}
	if !isEnvironmentExecError(preStart) {
		t.Fatalf("preStart interruption must remain an environment error")
	}

	postStartWaitDelay := &commandUnknownOutcomeError{
		Err:     fmt.Errorf("exec go inherited output pipes did not close: %w", exec.ErrWaitDelay),
		Started: true,
	}
	if !errors.Is(postStartWaitDelay, execution.ErrUnknownOutcome) || !errors.Is(postStartWaitDelay, exec.ErrWaitDelay) {
		t.Fatalf("postStartWaitDelay must preserve both ErrUnknownOutcome and ErrWaitDelay")
	}
	if !isStartedCommandUnknownOutcome(postStartWaitDelay) {
		t.Fatalf("postStartWaitDelay must be classified as a started command unknown outcome")
	}
	if isEnvironmentExecError(postStartWaitDelay) {
		t.Fatalf("postStartWaitDelay must not be classified as an environment error")
	}

	joined := fmt.Errorf("%w; %w", ErrProtectedPathsModified, postStartWaitDelay)
	if !errors.Is(joined, ErrProtectedPathsModified) || !errors.Is(joined, execution.ErrUnknownOutcome) {
		t.Fatalf("joined error must match both ErrProtectedPathsModified and ErrUnknownOutcome: %v", joined)
	}
	if !isStartedCommandUnknownOutcome(joined) || isEnvironmentExecError(joined) {
		t.Fatalf("joined error must remain candidate-attributable and incomplete")
	}
}

func TestE2_CandidateTestHangAndOutputFloodAttributedToCandidate(t *testing.T) {
	if testing.Short() {
		t.Skip("sandbox verification test")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_SANDBOX_MEMORY_MAX", "1G")
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}

	project, err := NewProject("e2-attr", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WriteFiles([]ExtractedFile{
		{Path: "go.mod", Content: "module example.com/e2attr\n\ngo 1.22\n"},
		{Path: "app.go", Content: "package e2attr\n\nfunc Answer() int { return 42 }\n"},
		{Path: "app_test.go", Content: "package e2attr\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) {\n\tif Answer() != 42 {\n\t\tt.Fatal(\"wrong answer\")\n\t}\n}\n"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := project.ScanFiles(); err != nil {
		t.Fatal(err)
	}

	t.Run("hanging candidate times out as candidate failure, not environment error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
		defer cancel()
		report, err := project.EvaluateCandidateFiles(ctx, []ExtractedFile{
			{Path: "app.go", Content: "package e2attr\n\nimport \"time\"\n\nfunc Answer() int {\n\tfor {\n\t\ttime.Sleep(100 * time.Millisecond)\n\t}\n}\n"},
		})
		if err != nil {
			t.Fatalf("EvaluateCandidateFiles err = %v", err)
		}
		if report.Passed || report.Strength != 0 {
			t.Fatalf("hanging candidate passed or got positive strength: %+v", report)
		}
		if report.EnvironmentError {
			t.Fatalf("hanging candidate must not set EnvironmentError: %+v", report)
		}
		if !report.ExecutionIncomplete {
			t.Fatalf("hanging candidate must set ExecutionIncomplete: %+v", report)
		}
		if got := verificationRank(report); got != 0 {
			t.Fatalf("verificationRank(hanging) = %d, want 0", got)
		}
		attempt := CandidateAttempt{
			Provider:     "looping-provider",
			Content:      "patched",
			Files:        []ExtractedFile{{Path: "app.go", Content: "package e2attr"}},
			Verification: report,
		}
		if verificationUnattributable(report) || !ShouldRecordCandidateQuality(attempt) {
			t.Fatalf("hanging candidate must be attributable and record quality failure: %+v", report)
		}
		if stage := CandidateAttemptStage(context.Background(), attempt); stage != CandidateProgressRejected {
			t.Fatalf("CandidateAttemptStage = %q, want %q", stage, CandidateProgressRejected)
		}
	})

	t.Run("output flooding candidate fails as candidate failure, not environment error", func(t *testing.T) {
		report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{
			{Path: "app.go", Content: "package e2attr\n\nimport (\n\t\"bytes\"\n\t\"os\"\n)\n\nfunc Answer() int {\n\tchunk := bytes.Repeat([]byte(\"x\"), 65536)\n\tfor {\n\t\t_, _ = os.Stderr.Write(chunk)\n\t}\n}\n"},
		})
		if err != nil {
			t.Fatalf("EvaluateCandidateFiles err = %v", err)
		}
		if report.Passed || report.Strength != 0 {
			t.Fatalf("flooding candidate passed or got positive strength: %+v", report)
		}
		if report.EnvironmentError {
			t.Fatalf("flooding candidate must not set EnvironmentError: %+v", report)
		}
		if !report.ExecutionIncomplete {
			t.Fatalf("flooding candidate must set ExecutionIncomplete: %+v", report)
		}
		if got := verificationRank(report); got != 0 {
			t.Fatalf("verificationRank(flooding) = %d, want 0", got)
		}
		attempt := CandidateAttempt{
			Provider:     "flood-provider",
			Content:      "patched",
			Files:        []ExtractedFile{{Path: "app.go", Content: "package e2attr"}},
			Verification: report,
		}
		if verificationUnattributable(report) || !ShouldRecordCandidateQuality(attempt) {
			t.Fatalf("flooding candidate must be attributable and record quality failure: %+v", report)
		}
	})
}

func TestE2_TrustedAcceptanceHangAndFloodAttributedToCandidate(t *testing.T) {
	if testing.Short() {
		t.Skip("sandbox verification test")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_SANDBOX_MEMORY_MAX", "1G")
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}

	project, err := NewProject("e2-trusted-acc", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WriteFiles([]ExtractedFile{
		{Path: "app.py", Content: "import sys\nprint(int(sys.stdin.read()) * 2)\n"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := project.ScanFiles(); err != nil {
		t.Fatal(err)
	}

	ctx := ContextWithTrustedAcceptance(context.Background(), TrustedAcceptanceSpec{
		Schema:         1,
		Command:        "python3",
		Args:           []string{"-I", "app.py"},
		Timeout:        "200ms",
		MaxOutputBytes: 4096,
		Cases: []TrustedAcceptanceCase{
			{Name: "double", Stdin: "2\n", Stdout: "4\n", Stderr: "", ExitCode: 0},
		},
	})

	t.Run("trusted acceptance timeout is candidate failure even without baseline test plan", func(t *testing.T) {
		report, err := project.EvaluateCandidateFiles(ctx, []ExtractedFile{
			{Path: "app.py", Content: "import time\ntime.sleep(5)\n"},
		})
		if err != nil {
			t.Fatalf("EvaluateCandidateFiles err = %v", err)
		}
		if report.Passed || report.EnvironmentError || !report.ExecutionIncomplete {
			t.Fatalf("unexpected report for hanging acceptance candidate: %+v", report)
		}
		if !strings.Contains(report.AcceptanceError, "did not complete") {
			t.Fatalf("AcceptanceError = %q, want 'did not complete'", report.AcceptanceError)
		}
		if verificationRank(report) != 0 || verificationUnattributable(report) {
			t.Fatalf("hanging acceptance candidate must have rank 0 and be attributable: %+v", report)
		}
	})

	t.Run("trusted acceptance output flood is candidate failure", func(t *testing.T) {
		report, err := project.EvaluateCandidateFiles(ctx, []ExtractedFile{
			{Path: "app.py", Content: "import os\nos.write(1, b'x' * 32768)\n"},
		})
		if err != nil {
			t.Fatalf("EvaluateCandidateFiles err = %v", err)
		}
		if report.Passed || report.EnvironmentError || !report.ExecutionIncomplete {
			t.Fatalf("unexpected report for flooding acceptance candidate: %+v", report)
		}
		if verificationRank(report) != 0 || verificationUnattributable(report) {
			t.Fatalf("flooding acceptance candidate must have rank 0 and be attributable: %+v", report)
		}
	})
}

func TestE2_RunCandidateSelectionRanksHonestFailureAboveIncompleteAndPenalizesQuality(t *testing.T) {
	if testing.Short() {
		t.Skip("sandbox verification test")
	}
	cfgDir := t.TempDir()
	t.Setenv("HOME", cfgDir)
	t.Setenv("MAKEWAND_CONFIG_DIR", cfgDir)
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_SANDBOX_MEMORY_MAX", "1G")
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}

	project, err := NewProject("e2-rank", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WriteFiles([]ExtractedFile{
		{Path: "go.mod", Content: "module example.com/e2rank\n\ngo 1.22\n"},
		{Path: "app.go", Content: "package e2rank\n\nfunc Answer() int { return 42 }\n"},
		{Path: "app_test.go", Content: "package e2rank\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) {\n\tif Answer() != 42 {\n\t\tt.Fatal(\"wrong answer\")\n\t}\n}\n"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := project.ScanFiles(); err != nil {
		t.Fatal(err)
	}

	floodContent := RenderExtractedFiles([]ExtractedFile{
		{Path: "app.go", Content: "package e2rank\n\nimport (\n\t\"bytes\"\n\t\"os\"\n)\n\nfunc Answer() int {\n\tchunk := bytes.Repeat([]byte(\"y\"), 65536)\n\tfor {\n\t\t_, _ = os.Stderr.Write(chunk)\n\t}\n}\n"},
	})
	honestFailContent := RenderExtractedFiles([]ExtractedFile{
		{Path: "app.go", Content: "package e2rank\n\nfunc Answer() int { return 0 }\n"},
	})

	router, err := model.NewRouterFromConfig(model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"claude": {Provider: &fixedContentProvider{name: "claude", content: floodContent}},
			"codex":  {Provider: &fixedContentProvider{name: "codex", content: honestFailContent}},
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
		project,
		model.PhaseCode,
		[]model.Message{{Role: "user", Content: "update app.py"}},
		"system",
		nil,
	)
	if selection.Provider != "codex" {
		t.Fatalf("selected provider = %q, want codex (completed test failure must outrank output-flooding candidate; selection=%+v)", selection.Provider, selection)
	}

	// Real environment failure still outranks both failed checks and records no quality penalty.
	envAttempt := CandidateAttempt{
		Provider:     "env_blocked",
		Index:        2,
		Content:      "patched",
		Verification: CandidateVerification{EnvironmentError: true, TestsError: "sandbox unavailable"},
	}
	honestFailAttempt := CandidateAttempt{
		Provider:     "codex",
		Index:        1,
		Content:      "patched",
		Verification: CandidateVerification{TestsError: "wrong answer"},
	}
	if !attemptOutranks(envAttempt, honestFailAttempt) {
		t.Fatalf("real EnvironmentError (rank 1) must still outrank failed checks (rank 0)")
	}
	if ShouldRecordCandidateQuality(envAttempt) {
		t.Fatalf("real EnvironmentError must not record a provider quality penalty")
	}

	if err := router.SaveStats(cfgDir); err != nil {
		t.Fatalf("SaveStats: %v", err)
	}
	statsPath := filepath.Join(cfgDir, "routing_stats.json")
	rawStats, err := os.ReadFile(statsPath)
	if err != nil {
		t.Fatalf("read routing_stats.json: %v", err)
	}
	var stats struct {
		Quality map[string]struct {
			Failures int `json:"failures"`
		} `json:"quality"`
	}
	if err := json.Unmarshal(rawStats, &stats); err != nil {
		t.Fatalf("unmarshal routing_stats.json: %v", err)
	}
	if got := stats.Quality["1:claude"].Failures; got != 1 {
		t.Fatalf("routing_stats.json quality[1:claude].failures = %d, want 1 (raw=%s)", got, string(rawStats))
	}
	if got := stats.Quality["1:codex"].Failures; got != 1 {
		t.Fatalf("routing_stats.json quality[1:codex].failures = %d, want 1 (raw=%s)", got, string(rawStats))
	}
}
