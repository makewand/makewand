package engine

import "testing"

func TestNewBuildPipeline_StartsIdle(t *testing.T) {
	p := NewBuildPipeline()
	if p.Phase() != PhaseIdle {
		t.Fatalf("Phase() = %v, want PhaseIdle", p.Phase())
	}
	if p.AutoFixAttempt() != 0 {
		t.Fatalf("AutoFixAttempt() = %d, want 0", p.AutoFixAttempt())
	}
	if p.CodeProvider() != "" {
		t.Fatalf("CodeProvider() = %q, want empty", p.CodeProvider())
	}
}

func TestOnCodeWritten_SingleProvider_SkipsReview(t *testing.T) {
	p := NewBuildPipeline()
	p.SetAvailableProviders(1)
	p.SetCodeProvider("gemini")

	action := p.OnCodeWritten()

	if action.Kind != ActionSkipReview {
		t.Fatalf("Kind = %v, want ActionSkipReview", action.Kind)
	}
	if action.SkipReason != "single provider" {
		t.Fatalf("SkipReason = %q, want 'single provider'", action.SkipReason)
	}
	if p.Phase() != PhaseDeps {
		t.Fatalf("Phase() = %v, want PhaseDeps", p.Phase())
	}
}

func TestOnCodeWritten_MultipleProviders_StartsReview(t *testing.T) {
	p := NewBuildPipeline()
	p.SetAvailableProviders(3)
	p.SetCodeProvider("claude")

	action := p.OnCodeWritten()

	if action.Kind != ActionStartReview {
		t.Fatalf("Kind = %v, want ActionStartReview", action.Kind)
	}
	if p.Phase() != PhaseReview {
		t.Fatalf("Phase() = %v, want PhaseReview", p.Phase())
	}
}

func TestOnReviewComplete_LGTM_MovesDeps(t *testing.T) {
	p := NewBuildPipeline()
	p.SetPhase(PhaseReview)

	action := p.OnReviewComplete(ReviewLGTM)

	if action.Kind != ActionStartDeps {
		t.Fatalf("Kind = %v, want ActionStartDeps", action.Kind)
	}
	if p.Phase() != PhaseDeps {
		t.Fatalf("Phase() = %v, want PhaseDeps", p.Phase())
	}
}

func TestOnReviewComplete_Error_NonFatal(t *testing.T) {
	p := NewBuildPipeline()
	p.SetPhase(PhaseReview)

	action := p.OnReviewComplete(ReviewError)

	if action.Kind != ActionStartDeps {
		t.Fatalf("Kind = %v, want ActionStartDeps", action.Kind)
	}
}

func TestOnReviewComplete_HasIssues_ReturnsNone(t *testing.T) {
	p := NewBuildPipeline()
	p.SetPhase(PhaseReview)

	action := p.OnReviewComplete(ReviewHasIssues)

	if action.Kind != ActionNone {
		t.Fatalf("Kind = %v, want ActionNone (TUI handles file writing)", action.Kind)
	}
}

func TestOnReviewComplete_NoFixFiles_MovesDeps(t *testing.T) {
	p := NewBuildPipeline()
	p.SetPhase(PhaseReview)

	action := p.OnReviewComplete(ReviewNoFixFiles)

	if action.Kind != ActionStartDeps {
		t.Fatalf("Kind = %v, want ActionStartDeps", action.Kind)
	}
}

func TestOnReviewFixesWritten_MovesDeps(t *testing.T) {
	p := NewBuildPipeline()
	p.SetPhase(PhaseReview)

	action := p.OnReviewFixesWritten()

	if action.Kind != ActionStartDeps {
		t.Fatalf("Kind = %v, want ActionStartDeps", action.Kind)
	}
	if p.Phase() != PhaseDeps {
		t.Fatalf("Phase() = %v, want PhaseDeps", p.Phase())
	}
}

func TestOnDepsComplete_Success_MovesTests(t *testing.T) {
	p := NewBuildPipeline()
	p.SetPhase(PhaseDeps)

	action := p.OnDepsComplete(false, "")

	if action.Kind != ActionStartTests {
		t.Fatalf("Kind = %v, want ActionStartTests", action.Kind)
	}
	if p.Phase() != PhaseTests {
		t.Fatalf("Phase() = %v, want PhaseTests", p.Phase())
	}
}

func TestOnDepsComplete_Failure_TriggersAutoFix(t *testing.T) {
	p := NewBuildPipeline()
	p.SetPhase(PhaseDeps)

	action := p.OnDepsComplete(true, "npm ERR! missing peer dep")

	if action.Kind != ActionStartAutoFix {
		t.Fatalf("Kind = %v, want ActionStartAutoFix", action.Kind)
	}
	if action.AutoFixAttempt != 1 {
		t.Fatalf("AutoFixAttempt = %d, want 1", action.AutoFixAttempt)
	}
	if p.Phase() != PhaseAutoFix {
		t.Fatalf("Phase() = %v, want PhaseAutoFix", p.Phase())
	}
}

func TestOnDepsDeclined_CompletesBuild(t *testing.T) {
	p := NewBuildPipeline()
	p.SetPhase(PhaseDeps)

	action := p.OnDepsDeclined()

	if action.Kind != ActionBuildComplete {
		t.Fatalf("Kind = %v, want ActionBuildComplete", action.Kind)
	}
	if p.Phase() != PhaseDone {
		t.Fatalf("Phase() = %v, want PhaseDone", p.Phase())
	}
}

func TestOnDepsSkipped_MovesTests(t *testing.T) {
	p := NewBuildPipeline()
	p.SetPhase(PhaseDeps)

	action := p.OnDepsSkipped()

	if action.Kind != ActionStartTests {
		t.Fatalf("Kind = %v, want ActionStartTests", action.Kind)
	}
}

func TestOnTestsComplete_Success_CompletesBuild(t *testing.T) {
	p := NewBuildPipeline()
	p.SetPhase(PhaseTests)

	action := p.OnTestsComplete(false, "")

	if action.Kind != ActionBuildComplete {
		t.Fatalf("Kind = %v, want ActionBuildComplete", action.Kind)
	}
	if p.Phase() != PhaseDone {
		t.Fatalf("Phase() = %v, want PhaseDone", p.Phase())
	}
}

func TestOnTestsComplete_Failure_TriggersAutoFix(t *testing.T) {
	p := NewBuildPipeline()
	p.SetPhase(PhaseTests)
	p.SetCodeProvider("claude")

	action := p.OnTestsComplete(true, "FAIL main_test.go:12")

	if action.Kind != ActionStartAutoFix {
		t.Fatalf("Kind = %v, want ActionStartAutoFix", action.Kind)
	}
	if action.AutoFixAttempt != 1 {
		t.Fatalf("AutoFixAttempt = %d, want 1", action.AutoFixAttempt)
	}
}

func TestOnTestsSkipped_CompletesBuild(t *testing.T) {
	p := NewBuildPipeline()
	p.SetPhase(PhaseTests)

	action := p.OnTestsSkipped()

	if action.Kind != ActionBuildComplete {
		t.Fatalf("Kind = %v, want ActionBuildComplete", action.Kind)
	}
}

func TestOnAutoFixAttempt_WithinLimit(t *testing.T) {
	p := NewBuildPipeline()

	for attempt := 1; attempt <= MaxAutoFixRetries; attempt++ {
		action := p.OnAutoFixAttempt(attempt)
		if action.Kind != ActionStartAutoFix {
			t.Fatalf("attempt %d: Kind = %v, want ActionStartAutoFix", attempt, action.Kind)
		}
		if action.AutoFixAttempt != attempt {
			t.Fatalf("attempt %d: AutoFixAttempt = %d", attempt, action.AutoFixAttempt)
		}
		if p.AutoFixAttempt() != attempt {
			t.Fatalf("attempt %d: AutoFixAttempt() = %d", attempt, p.AutoFixAttempt())
		}
	}
}

func TestOnAutoFixAttempt_ExceedsLimit(t *testing.T) {
	p := NewBuildPipeline()

	action := p.OnAutoFixAttempt(MaxAutoFixRetries + 1)

	if action.Kind != ActionMaxRetriesReached {
		t.Fatalf("Kind = %v, want ActionMaxRetriesReached", action.Kind)
	}
	if p.Phase() != PhaseDone {
		t.Fatalf("Phase() = %v, want PhaseDone", p.Phase())
	}
}

func TestOnAutoFixResponse_WithFiles(t *testing.T) {
	p := NewBuildPipeline()
	p.SetPhase(PhaseAutoFix)
	p.autoFixAttempt = 2

	action := p.OnAutoFixResponse(true, 2)

	if action.Kind != ActionNone {
		t.Fatalf("Kind = %v, want ActionNone (TUI writes files first)", action.Kind)
	}
	if p.AutoFixRetryAttempt() != 2 {
		t.Fatalf("AutoFixRetryAttempt() = %d, want 2", p.AutoFixRetryAttempt())
	}
}

func TestOnAutoFixResponse_NoFiles_GivesUp(t *testing.T) {
	p := NewBuildPipeline()
	p.SetPhase(PhaseAutoFix)

	action := p.OnAutoFixResponse(false, 1)

	if action.Kind != ActionMaxRetriesReached {
		t.Fatalf("Kind = %v, want ActionMaxRetriesReached", action.Kind)
	}
	if p.Phase() != PhaseDone {
		t.Fatalf("Phase() = %v, want PhaseDone", p.Phase())
	}
}

func TestOnAutoFixFilesWritten_ReturnsRetry(t *testing.T) {
	p := NewBuildPipeline()
	p.autoFixRetryAttempt = 2

	action := p.OnAutoFixFilesWritten()

	if action.Kind != ActionAutoFixRetry {
		t.Fatalf("Kind = %v, want ActionAutoFixRetry", action.Kind)
	}
	if action.AutoFixAttempt != 2 {
		t.Fatalf("AutoFixAttempt = %d, want 2", action.AutoFixAttempt)
	}
}

func TestShouldRecordCodeQuality_NoAutoFix(t *testing.T) {
	p := NewBuildPipeline()
	p.SetCodeProvider("claude")

	if !p.ShouldRecordCodeQuality() {
		t.Fatal("ShouldRecordCodeQuality() = false, want true when no auto-fix needed")
	}
}

func TestShouldRecordCodeQuality_AfterAutoFix(t *testing.T) {
	p := NewBuildPipeline()
	p.SetCodeProvider("claude")
	p.OnAutoFixAttempt(1)

	if p.ShouldRecordCodeQuality() {
		t.Fatal("ShouldRecordCodeQuality() = true, want false when auto-fix was needed")
	}
}

func TestShouldRecordCodeQuality_NoProvider(t *testing.T) {
	p := NewBuildPipeline()

	if p.ShouldRecordCodeQuality() {
		t.Fatal("ShouldRecordCodeQuality() = true, want false when no code provider")
	}
}

func TestProviderTracking(t *testing.T) {
	p := NewBuildPipeline()

	p.SetCodeProvider("gemini")
	if p.CodeProvider() != "gemini" {
		t.Fatalf("CodeProvider() = %q, want gemini", p.CodeProvider())
	}

	p.SetReviewProvider("claude")
	if p.ReviewProvider() != "claude" {
		t.Fatalf("ReviewProvider() = %q, want claude", p.ReviewProvider())
	}
}

func TestApprovalTracking(t *testing.T) {
	p := NewBuildPipeline()

	if p.DepsApproved() {
		t.Fatal("DepsApproved() should start false")
	}
	if p.TestsApproved() {
		t.Fatal("TestsApproved() should start false")
	}

	p.SetDepsApproved(true)
	if !p.DepsApproved() {
		t.Fatal("DepsApproved() should be true after SetDepsApproved(true)")
	}

	p.SetTestsApproved(true)
	if !p.TestsApproved() {
		t.Fatal("TestsApproved() should be true after SetTestsApproved(true)")
	}
}

func TestPhaseString(t *testing.T) {
	tests := []struct {
		phase BuildPhase
		want  string
	}{
		{PhaseIdle, "Idle"},
		{PhaseCode, "Code"},
		{PhaseReview, "Review"},
		{PhaseDeps, "Deps"},
		{PhaseTests, "Tests"},
		{PhaseAutoFix, "AutoFix"},
		{PhaseDone, "Done"},
		{BuildPhase(99), "Unknown"},
	}
	for _, tt := range tests {
		if got := tt.phase.String(); got != tt.want {
			t.Errorf("BuildPhase(%d).String() = %q, want %q", tt.phase, got, tt.want)
		}
	}
}

func TestActionKindString(t *testing.T) {
	tests := []struct {
		kind ActionKind
		want string
	}{
		{ActionNone, "None"},
		{ActionStartReview, "StartReview"},
		{ActionSkipReview, "SkipReview"},
		{ActionStartDeps, "StartDeps"},
		{ActionStartTests, "StartTests"},
		{ActionStartAutoFix, "StartAutoFix"},
		{ActionAutoFixRetry, "AutoFixRetry"},
		{ActionBuildComplete, "BuildComplete"},
		{ActionMaxRetriesReached, "MaxRetriesReached"},
		{ActionKind(99), "Unknown"},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.want {
			t.Errorf("ActionKind(%d).String() = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

// TestFullPipelineFlow simulates the happy path: code → review (LGTM) → deps → tests → done.
func TestFullPipelineFlow(t *testing.T) {
	p := NewBuildPipeline()
	p.SetAvailableProviders(2)
	p.SetCodeProvider("gemini")

	// Code written → start review
	a := p.OnCodeWritten()
	if a.Kind != ActionStartReview {
		t.Fatalf("after code: Kind = %v, want ActionStartReview", a.Kind)
	}

	// Review LGTM → start deps
	a = p.OnReviewComplete(ReviewLGTM)
	if a.Kind != ActionStartDeps {
		t.Fatalf("after review: Kind = %v, want ActionStartDeps", a.Kind)
	}

	// Deps OK → start tests
	a = p.OnDepsComplete(false, "")
	if a.Kind != ActionStartTests {
		t.Fatalf("after deps: Kind = %v, want ActionStartTests", a.Kind)
	}

	// Tests pass → build complete
	a = p.OnTestsComplete(false, "")
	if a.Kind != ActionBuildComplete {
		t.Fatalf("after tests: Kind = %v, want ActionBuildComplete", a.Kind)
	}
	if p.Phase() != PhaseDone {
		t.Fatalf("Phase() = %v, want PhaseDone", p.Phase())
	}

	// Quality should be recorded since no auto-fix
	if !p.ShouldRecordCodeQuality() {
		t.Fatal("ShouldRecordCodeQuality() should be true for clean build")
	}
}

// TestAutoFixFlow simulates: code → review → deps fail → auto-fix → retry → tests pass.
func TestAutoFixFlow(t *testing.T) {
	p := NewBuildPipeline()
	p.SetAvailableProviders(2)
	p.SetCodeProvider("claude")

	p.OnCodeWritten()
	p.OnReviewComplete(ReviewLGTM)

	// Deps fail → auto-fix
	a := p.OnDepsComplete(true, "npm ERR!")
	if a.Kind != ActionStartAutoFix {
		t.Fatalf("after deps fail: Kind = %v, want ActionStartAutoFix", a.Kind)
	}
	if a.AutoFixAttempt != 1 {
		t.Fatalf("AutoFixAttempt = %d, want 1", a.AutoFixAttempt)
	}

	// AI responds with fix files
	a = p.OnAutoFixResponse(true, 1)
	if a.Kind != ActionNone {
		t.Fatalf("after fix response: Kind = %v, want ActionNone", a.Kind)
	}

	// Files written → retry
	a = p.OnAutoFixFilesWritten()
	if a.Kind != ActionAutoFixRetry {
		t.Fatalf("after fix files: Kind = %v, want ActionAutoFixRetry", a.Kind)
	}

	// Quality should NOT be recorded since auto-fix was needed
	if p.ShouldRecordCodeQuality() {
		t.Fatal("ShouldRecordCodeQuality() should be false after auto-fix")
	}
}
