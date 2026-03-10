package engine

// BuildPipeline is a pure domain state machine that manages the build wizard's
// phase transitions, auto-fix retry counting, and provider tracking. It has no
// dependency on any TUI or model package — the TUI layer feeds it events and
// reads back action descriptors.
//
// Usage flow:
//   1. TUI creates a pipeline via NewBuildPipeline.
//   2. After code generation completes, TUI calls OnCodeWritten.
//   3. Pipeline returns a BuildAction telling the TUI what to do next.
//   4. TUI performs the action (e.g. start review, start deps, complete build)
//      and feeds results back to the pipeline via the appropriate On* method.

// BuildPhase represents the current stage of the build pipeline.
type BuildPhase int

const (
	PhaseIdle     BuildPhase = iota // not started
	PhaseCode                       // generating code
	PhaseReview                     // cross-model code review
	PhaseDeps                       // dependency installation
	PhaseTests                      // running tests
	PhaseAutoFix                    // auto-fixing failures
	PhaseDone                       // build complete
)

// String returns a human-readable name for the phase.
func (p BuildPhase) String() string {
	switch p {
	case PhaseIdle:
		return "Idle"
	case PhaseCode:
		return "Code"
	case PhaseReview:
		return "Review"
	case PhaseDeps:
		return "Deps"
	case PhaseTests:
		return "Tests"
	case PhaseAutoFix:
		return "AutoFix"
	case PhaseDone:
		return "Done"
	default:
		return "Unknown"
	}
}

// MaxAutoFixRetries is the maximum number of auto-fix attempts before giving up.
const MaxAutoFixRetries = 3

// ActionKind describes what the TUI should do next.
type ActionKind int

const (
	ActionNone              ActionKind = iota
	ActionStartReview                  // begin cross-model code review
	ActionSkipReview                   // review skipped (single provider or error)
	ActionStartDeps                    // begin dependency installation phase
	ActionStartTests                   // begin test execution phase
	ActionStartAutoFix                 // begin auto-fix attempt
	ActionAutoFixRetry                 // re-run deps+tests after fix files written
	ActionBuildComplete                // the entire build pipeline is done
	ActionMaxRetriesReached            // auto-fix retries exhausted, build done
)

// String returns a human-readable name for the action.
func (a ActionKind) String() string {
	switch a {
	case ActionNone:
		return "None"
	case ActionStartReview:
		return "StartReview"
	case ActionSkipReview:
		return "SkipReview"
	case ActionStartDeps:
		return "StartDeps"
	case ActionStartTests:
		return "StartTests"
	case ActionStartAutoFix:
		return "StartAutoFix"
	case ActionAutoFixRetry:
		return "AutoFixRetry"
	case ActionBuildComplete:
		return "BuildComplete"
	case ActionMaxRetriesReached:
		return "MaxRetriesReached"
	default:
		return "Unknown"
	}
}

// BuildAction is the descriptor returned by pipeline transitions. The TUI reads
// the Kind field and acts accordingly.
type BuildAction struct {
	Kind ActionKind

	// SkipReason is set when Kind is ActionSkipReview to explain why.
	SkipReason string

	// AutoFixAttempt is the current attempt number (1-based) when Kind is
	// ActionStartAutoFix or ActionAutoFixRetry.
	AutoFixAttempt int
}

// ReviewResult describes the outcome of a code review.
type ReviewResult int

const (
	ReviewLGTM       ReviewResult = iota // no issues found
	ReviewHasIssues                      // issues found with fix files
	ReviewNoFixFiles                     // issues found but no parseable fix files
	ReviewError                          // review failed (non-fatal)
)

// BuildPipeline manages build wizard state transitions.
type BuildPipeline struct {
	phase BuildPhase

	// Provider tracking
	codeProvider   string
	reviewProvider string

	// Auto-fix state
	autoFixAttempt      int
	autoFixRetryAttempt int

	// Approval state
	depsApproved  bool
	testsApproved bool

	// Available provider count (set by TUI so pipeline can decide on review skip)
	availableProviders int
}

// NewBuildPipeline creates a new pipeline in the idle state.
func NewBuildPipeline() *BuildPipeline {
	return &BuildPipeline{
		phase: PhaseIdle,
	}
}

// Phase returns the current build phase.
func (p *BuildPipeline) Phase() BuildPhase {
	return p.phase
}

// SetPhase sets the phase directly. Used by the TUI to sync initial state.
func (p *BuildPipeline) SetPhase(phase BuildPhase) {
	p.phase = phase
}

// CodeProvider returns the provider that generated the code.
func (p *BuildPipeline) CodeProvider() string {
	return p.codeProvider
}

// SetCodeProvider records which provider generated the code.
func (p *BuildPipeline) SetCodeProvider(provider string) {
	p.codeProvider = provider
}

// ReviewProvider returns the provider used for code review.
func (p *BuildPipeline) ReviewProvider() string {
	return p.reviewProvider
}

// SetReviewProvider records which provider will review the code.
func (p *BuildPipeline) SetReviewProvider(provider string) {
	p.reviewProvider = provider
}

// AutoFixAttempt returns the current auto-fix attempt number (0 = none).
func (p *BuildPipeline) AutoFixAttempt() int {
	return p.autoFixAttempt
}

// AutoFixRetryAttempt returns the auto-fix retry attempt saved for post-file-write.
func (p *BuildPipeline) AutoFixRetryAttempt() int {
	return p.autoFixRetryAttempt
}

// DepsApproved returns whether dependency installation has been approved.
func (p *BuildPipeline) DepsApproved() bool {
	return p.depsApproved
}

// SetDepsApproved records user approval for dependency installation.
func (p *BuildPipeline) SetDepsApproved(approved bool) {
	p.depsApproved = approved
}

// TestsApproved returns whether test execution has been approved.
func (p *BuildPipeline) TestsApproved() bool {
	return p.testsApproved
}

// SetTestsApproved records user approval for test execution.
func (p *BuildPipeline) SetTestsApproved(approved bool) {
	p.testsApproved = approved
}

// SetAvailableProviders tells the pipeline how many providers are available.
// This drives the review skip decision.
func (p *BuildPipeline) SetAvailableProviders(count int) {
	p.availableProviders = count
}

// --- Phase transition methods ---

// OnCodeWritten is called after code files have been written to disk.
// It decides whether to proceed to review or skip it.
func (p *BuildPipeline) OnCodeWritten() BuildAction {
	if p.availableProviders <= 1 {
		p.phase = PhaseDeps
		return BuildAction{Kind: ActionSkipReview, SkipReason: "single provider"}
	}
	p.phase = PhaseReview
	return BuildAction{Kind: ActionStartReview}
}

// OnReviewComplete is called after the code review finishes.
func (p *BuildPipeline) OnReviewComplete(result ReviewResult) BuildAction {
	switch result {
	case ReviewLGTM, ReviewNoFixFiles, ReviewError:
		// Move to deps regardless — review is non-blocking
		p.phase = PhaseDeps
		return BuildAction{Kind: ActionStartDeps}
	case ReviewHasIssues:
		// TUI will handle writing fix files, then call OnReviewFixesWritten
		return BuildAction{Kind: ActionNone}
	default:
		p.phase = PhaseDeps
		return BuildAction{Kind: ActionStartDeps}
	}
}

// OnReviewFixesWritten is called after review fix files have been written.
func (p *BuildPipeline) OnReviewFixesWritten() BuildAction {
	p.phase = PhaseDeps
	return BuildAction{Kind: ActionStartDeps}
}

// OnDepsComplete is called after dependency installation finishes.
// failed indicates the deps command exited non-zero.
func (p *BuildPipeline) OnDepsComplete(failed bool, errOutput string) BuildAction {
	if failed {
		return p.triggerAutoFix(errOutput, 1)
	}
	p.phase = PhaseTests
	return BuildAction{Kind: ActionStartTests}
}

// OnDepsSkipped is called when there are no deps to install or user skipped.
func (p *BuildPipeline) OnDepsSkipped() BuildAction {
	p.phase = PhaseTests
	return BuildAction{Kind: ActionStartTests}
}

// OnDepsDeclined is called when the user declines deps installation.
// This also skips tests.
func (p *BuildPipeline) OnDepsDeclined() BuildAction {
	p.phase = PhaseDone
	return BuildAction{Kind: ActionBuildComplete}
}

// OnTestsComplete is called after tests finish.
func (p *BuildPipeline) OnTestsComplete(failed bool, errOutput string) BuildAction {
	if failed {
		return p.triggerAutoFix(errOutput, 1)
	}
	p.phase = PhaseDone
	return BuildAction{Kind: ActionBuildComplete}
}

// OnTestsSkipped is called when there are no tests or user skipped them.
func (p *BuildPipeline) OnTestsSkipped() BuildAction {
	p.phase = PhaseDone
	return BuildAction{Kind: ActionBuildComplete}
}

// OnAutoFixAttempt is called when an auto-fix is triggered (e.g. from test/deps failure).
// attempt is 1-based. Returns the action to take.
func (p *BuildPipeline) OnAutoFixAttempt(attempt int) BuildAction {
	if attempt > MaxAutoFixRetries {
		p.phase = PhaseDone
		return BuildAction{Kind: ActionMaxRetriesReached, AutoFixAttempt: attempt}
	}
	p.phase = PhaseAutoFix
	p.autoFixAttempt = attempt
	return BuildAction{Kind: ActionStartAutoFix, AutoFixAttempt: attempt}
}

// OnAutoFixResponse is called after the AI responds with fix code.
// hasFiles indicates whether the response contained parseable file blocks.
func (p *BuildPipeline) OnAutoFixResponse(hasFiles bool, attempt int) BuildAction {
	if !hasFiles {
		p.phase = PhaseDone
		return BuildAction{Kind: ActionMaxRetriesReached, AutoFixAttempt: attempt}
	}
	p.autoFixRetryAttempt = attempt
	// TUI will write files, then call OnAutoFixFilesWritten
	return BuildAction{Kind: ActionNone}
}

// OnAutoFixFilesWritten is called after auto-fix files have been written.
// The TUI should re-run deps+tests.
func (p *BuildPipeline) OnAutoFixFilesWritten() BuildAction {
	return BuildAction{Kind: ActionAutoFixRetry, AutoFixAttempt: p.autoFixRetryAttempt}
}

// triggerAutoFix initiates an auto-fix cycle.
func (p *BuildPipeline) triggerAutoFix(errOutput string, attempt int) BuildAction {
	return p.OnAutoFixAttempt(attempt)
}

// ShouldRecordCodeQuality returns true if the code provider should receive a
// quality reward (tests passed without any auto-fix).
func (p *BuildPipeline) ShouldRecordCodeQuality() bool {
	return p.codeProvider != "" && p.autoFixAttempt == 0
}

// Complete marks the pipeline as done.
func (p *BuildPipeline) Complete() {
	p.phase = PhaseDone
}
