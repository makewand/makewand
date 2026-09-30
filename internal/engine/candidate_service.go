package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/makewand/makewand/execution"

	"github.com/makewand/makewand/internal/model"
)

type CandidateAttempt struct {
	TaskID       string
	ID           string
	Status       execution.Status
	Index        int
	Requested    string
	Provider     string
	Content      string
	Files        []ExtractedFile
	DeletedFiles []string
	// LargeFiles are files over the verified-change size limit that the
	// candidate added or modified; they are reported, never applied.
	LargeFiles []string
	// UnchangedLargeFiles are oversized baseline files the candidate left
	// alone; they are skipped by the diff and recorded here.
	UnchangedLargeFiles []string
	Usage               model.Usage
	Verification        CandidateVerification
	Err                 error
}

type CandidateSelection struct {
	TaskID          string
	Status          execution.Status
	Attempts        []CandidateState
	Content         string
	Provider        string
	Usage           model.Usage
	Verified        bool
	Strength        int
	PassedCount     int
	TotalCandidates int
	// DeletedFiles lists baseline files the selected candidate removed in its
	// workspace. Deletions are not auto-applied; callers must surface them.
	DeletedFiles []string
	// NotVerifiedReason explains why candidates could not be verified at all
	// (e.g. sandbox isolation unavailable, fail closed).
	NotVerifiedReason string
	// Err carries a fail-closed sentinel from the underlying attempts when every
	// candidate failed for that reason (currently model.ErrNoUntrustedSafeProvider,
	// surfaced when untrusted-repo mode had no direct-API provider). Callers can
	// errors.Is against it to present the actionable message instead of a generic
	// "no candidate produced a response".
	Err            error
	VerifiedFiles  []ExtractedFile
	VerifiedDigest string
	// RestoredTests lists the selected candidate's edits to pre-existing test
	// files (and package.json scripts.test) that were discarded: verification
	// keeps the baseline tests, and the delivered file set (VerifiedFiles)
	// excludes those edits. Callers must tell the user.
	RestoredTests []string
	// NoTestsExecuted reports that the selected candidate was checked without
	// running any test (no baseline test plan, or the plan ran zero tests).
	NoTestsExecuted bool
	// LargeFiles lists files over the size limit the selected candidate
	// changed; they are not part of the delivered change set.
	LargeFiles []string
}

type CandidateProgressStage string

// CandidateState keeps each outcome without retaining discarded candidate payloads.
type CandidateState struct {
	TaskID         string
	ID             string
	Requested      string
	Provider       string
	Status         execution.Status
	Usage          model.Usage
	Strength       int
	ArtifactDigest string
	ErrorKind      *string
}

const (
	CandidateProgressRunning   CandidateProgressStage = "running"
	CandidateProgressVerifying CandidateProgressStage = "verifying"
	CandidateProgressPassed    CandidateProgressStage = "passed"
	CandidateProgressRejected  CandidateProgressStage = "rejected"
	CandidateProgressFailed    CandidateProgressStage = "failed"
	CandidateProgressCanceled  CandidateProgressStage = "canceled"
	// CandidateProgressUnverified: the candidate produced a change but could
	// not be verified (no baseline tests, sandbox or toolchain unavailable).
	CandidateProgressUnverified CandidateProgressStage = "unverified"
)

const maxVerifiedCandidateStrength = 2

// autopilotMinCandidateStrength is the acceptance floor for automatic
// application: an independent trusted acceptance driver must attest the result.
// Candidate-controlled local test output is Strength 1 and requires approval.
const autopilotMinCandidateStrength = 2

type CandidateProgressFunc func(provider string, stage CandidateProgressStage)

func OrderedCandidateProviders(router *model.Router, phase model.BuildPhase, exclude ...string) []string {
	if router == nil {
		return nil
	}

	available := router.Available()
	if len(available) == 0 {
		return nil
	}

	limit := 3
	switch phase {
	case model.PhaseFix, model.PhaseReview:
		limit = 2
	}
	if adaptive := router.BuildProvidersForAdaptive(phase, limit, exclude...); len(adaptive) > 0 {
		return adaptive
	}

	excluded := make(map[string]bool, len(exclude))
	for _, name := range exclude {
		excluded[name] = true
	}

	var ordered []string
	primary := router.BuildProviderFor(phase)
	if primary != "" && !excluded[primary] && slices.Contains(available, primary) {
		ordered = append(ordered, primary)
	}

	for _, preferred := range []string{"claude", "codex", "gemini"} {
		if excluded[preferred] || preferred == primary || !slices.Contains(available, preferred) {
			continue
		}
		ordered = append(ordered, preferred)
	}

	var extras []string
	for _, name := range available {
		if excluded[name] || slices.Contains(ordered, name) {
			continue
		}
		extras = append(extras, name)
	}
	sort.Strings(extras)
	ordered = append(ordered, extras...)

	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	return ordered
}

func RenderExtractedFiles(files []ExtractedFile) string {
	var b strings.Builder
	for i, f := range files {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "--- FILE: %s ---\n```\n%s\n```\n", f.Path, f.Content)
	}
	return strings.TrimSpace(b.String())
}

func RunCandidateSelection(
	ctx context.Context,
	router *model.Router,
	project *Project,
	phase model.BuildPhase,
	messages []model.Message,
	system string,
	progress CandidateProgressFunc,
	exclude ...string,
) (selection CandidateSelection) {
	var executionErr error
	var executionConfig execution.Config
	ctx, executionConfig, executionErr = execution.EnsureContext(ctx)
	if executionErr != nil {
		return CandidateSelection{Err: executionErr, Status: execution.InvalidRequest}
	}
	options, err := candidateSelectionOptions(ctx)
	if err != nil {
		return CandidateSelection{TaskID: executionConfig.TaskID, Err: err, Status: execution.InvalidRequest}
	}
	if options.Timeout > 0 {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, options.Timeout)
		defer cancelTimeout()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	selectionStage, executionErr := execution.StartStage(ctx, "selection", "go", false)
	if executionErr != nil {
		return CandidateSelection{TaskID: executionConfig.TaskID, Err: executionErr, Status: execution.InternalError}
	}
	var allAttempts []CandidateState
	defer func() {
		selection.TaskID = executionConfig.TaskID
		selection.Attempts = allAttempts
		switch {
		case selection.Err != nil:
			selection.Status = candidateErrorStatus(selection.Err)
		case selection.Verified:
			selection.Status = execution.Passed
		case selection.Content != "":
			selection.Status = execution.Unverified
		case ctx.Err() != nil:
			selection.Status = candidateErrorStatus(ctx.Err())
		default:
			selection.Status = execution.Failed
		}
		outcome := execution.Outcome{Status: selection.Status, Known: selection.Status != execution.Unknown && selection.Status != execution.Timeout && selection.Status != execution.Cancelled}
		if selection.VerifiedDigest != "" {
			outcome.ArtifactDigest = &selection.VerifiedDigest
		}
		if err := selectionStage.Complete(outcome); err != nil {
			selection.Err = err
			selection.Status = execution.InternalError
		}
	}()

	providers := OrderedCandidateProviders(router, phase, exclude...)
	if options.MaxCandidates > 0 && len(providers) > options.MaxCandidates {
		providers = providers[:options.MaxCandidates]
	}
	if len(providers) == 0 {
		// In untrusted mode the candidate provider set is filtered to
		// untrusted-repo-safe (direct API) providers; an empty set is the
		// fail-closed case, so surface the sentinel instead of a silent empty
		// selection that the caller would report as a generic failure.
		if router != nil && router.RepoTrust() == model.RepoTrustUntrusted {
			return CandidateSelection{Err: model.ErrNoUntrustedSafeProvider}
		}
		return CandidateSelection{}
	}

	results := make(chan CandidateAttempt, len(providers))
	concurrency := len(providers)
	if options.MaxConcurrent > 0 && options.MaxConcurrent < concurrency {
		concurrency = options.MaxConcurrent
	}
	slots := make(chan struct{}, concurrency)
	turns := make([]chan struct{}, len(providers)+1)
	if concurrency == 1 {
		for i := range turns {
			turns[i] = make(chan struct{})
		}
		close(turns[0])
	}
	budget := &candidateCostBudget{limit: options.MaxCostUSD}
	var wg sync.WaitGroup
	for i, name := range providers {
		wg.Add(1)
		go func(idx int, providerName string) {
			defer wg.Done()
			if concurrency == 1 {
				defer close(turns[idx+1])
				select {
				case <-turns[idx]:
				case <-ctx.Done():
					results <- CandidateAttempt{Index: idx, Requested: providerName, Provider: providerName, Err: ctx.Err()}
					reportProgress(progress, providerName, CandidateProgressCanceled)
					return
				}
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				results <- CandidateAttempt{Index: idx, Requested: providerName, Provider: providerName, Err: ctx.Err()}
				reportProgress(progress, providerName, CandidateProgressCanceled)
				return
			}
			if ctx.Err() != nil || !budget.admit() {
				results <- CandidateAttempt{Index: idx, Requested: providerName, Provider: providerName, Err: context.Canceled}
				reportProgress(progress, providerName, CandidateProgressCanceled)
				return
			}
			reportProgress(progress, providerName, CandidateProgressRunning)
			candidateID := execution.NewID()
			attemptCtx := execution.ContextWithStage(ctx, "generation")
			candidateProject := project
			if project != nil {
				copyStage, stageErr := execution.StartStageForAttempt(attemptCtx, "copy", providerName, false, candidateID)
				if stageErr != nil {
					results <- CandidateAttempt{Index: idx, Requested: providerName, Provider: providerName, ID: candidateID, Err: stageErr}
					return
				}
				cloned, cloneErr := project.cloneToTempContext(ctx, nil)
				if eventErr := copyStage.Complete(execution.Outcome{Status: candidateErrorStatus(cloneErr), Known: true}); eventErr != nil {
					cloneErr = errors.Join(cloneErr, eventErr)
				}
				if cloneErr != nil {
					if cloned != nil {
						_ = os.RemoveAll(cloned.Path)
					}
					reportProgress(progress, providerName, CandidateProgressFailed)
					results <- CandidateAttempt{
						Index:     idx,
						ID:        candidateID,
						Requested: providerName,
						Provider:  providerName,
						Err:       cloneErr,
					}
					return
				}
				defer os.RemoveAll(cloned.Path)
				candidateProject = cloned
				attemptCtx = model.ContextWithWorkDir(attemptCtx, cloned.Path)
			}

			attemptExclude := isolatedCandidateExcludes(router, providerName, exclude...)
			generationStage, stageErr := execution.StartStageForAttempt(attemptCtx, "generation", providerName, false, candidateID)
			if stageErr != nil {
				results <- CandidateAttempt{Index: idx, Requested: providerName, Provider: providerName, ID: candidateID, Err: stageErr}
				return
			}
			content, usage, route, err := router.ChatWith(attemptCtx, providerName, phase, messages, system, attemptExclude...)
			if eventErr := generationStage.Complete(execution.Outcome{Status: candidateErrorStatus(err), Known: err == nil || !errors.Is(err, execution.ErrUnknownOutcome)}); eventErr != nil {
				err = errors.Join(err, eventErr)
			}
			budget.record(usage.Cost)
			attempt := CandidateAttempt{
				ID:        candidateID,
				Index:     idx,
				Requested: providerName,
				Content:   content,
				Usage:     usage,
				Err:       err,
			}
			switch {
			case route.Actual != "":
				attempt.Provider = route.Actual
			case usage.Provider != "":
				attempt.Provider = usage.Provider
			default:
				attempt.Provider = providerName
			}
			if err == nil {
				attempt.Files = ParseFilesBestEffort(content).Files
				if project != nil && candidateProject != nil && candidateProject != project {
					// The clone diff is the authoritative changed-file set: the
					// union of what the model reported and what it actually
					// edited on disk. Unreported clone edits are verified and
					// applied like reported ones; deletions are surfaced.
					diff, diffErr := candidateProject.DiffAgainst(project)
					changedFiles := diff.Files
					if diffErr != nil {
						attempt.Err = diffErr
					} else {
						attempt.DeletedFiles = diff.Deleted
						attempt.LargeFiles = diff.LargeChanged
						attempt.UnchangedLargeFiles = diff.LargeUnchanged
						merged := mergeCandidateFiles(attempt.Files, changedFiles)
						if len(changedFiles) > 0 && !extractedFilesEqual(merged, attempt.Files) {
							attempt.Files = merged
							attempt.Content = RenderExtractedFiles(merged)
						}
					}
				}
				if attempt.Err == nil && project != nil && len(attempt.Files) > 0 {
					reportProgress(progress, providerName, CandidateProgressVerifying)
					verificationStage, stageErr := execution.StartStageForAttempt(attemptCtx, "verification", providerName, true, candidateID)
					if stageErr != nil {
						attempt.Err = stageErr
						results <- attempt
						return
					}
					verification, verifyErr := project.EvaluateCandidateFiles(attemptCtx, attempt.Files)
					verificationStatus := candidateErrorStatus(verifyErr)
					if verifyErr == nil && !verification.Passed {
						verificationStatus = execution.Unverified
						if verification.IsolationError != "" {
							verificationStatus = execution.SandboxUnavailable
						}
					}
					verificationOutcome := execution.Outcome{Status: verificationStatus, Known: true}
					if verification.VerifiedDigest != "" {
						verificationOutcome.ArtifactDigest = &verification.VerifiedDigest
					}
					if eventErr := verificationStage.Complete(verificationOutcome); eventErr != nil {
						verifyErr = errors.Join(verifyErr, eventErr)
					}
					if verifyErr != nil {
						attempt.Err = verifyErr
					} else {
						attempt.Verification = verification
						if verification.Passed {
							attempt.Files = verification.VerifiedFiles
							attempt.Content = verification.VerifiedContent
						}
					}
				}
			}
			reportProgress(progress, providerName, CandidateAttemptStage(ctx, attempt))
			results <- attempt
		}(i, name)
	}

	totalUsage := model.Usage{}
	var (
		bestVerified           *CandidateAttempt
		bestSuccessful         *CandidateAttempt
		fallbackDeletions      []string
		fallbackDeletionsIndex = -1
		passedCount            int
		notVerifiedReason      string
		untrustedSafeErr       error
	)

	for completed := 0; completed < len(providers); completed++ {
		attempt := <-results
		attempt.TaskID = executionConfig.TaskID
		if attempt.ID == "" {
			attempt.ID = execution.NewID()
		}
		attempt.Status = candidateErrorStatus(attempt.Err)
		if attempt.Err == nil && !attempt.Verification.Passed {
			attempt.Status = execution.Unverified
		}
		state := CandidateState{TaskID: attempt.TaskID, ID: attempt.ID, Requested: attempt.Requested, Provider: attempt.Provider, Status: attempt.Status, Usage: attempt.Usage, Strength: attempt.Verification.Strength, ArtifactDigest: attempt.Verification.VerifiedDigest}
		if attempt.Err != nil {
			kind := string(model.ErrorKindOf(attempt.Err))
			state.ErrorKind = &kind
		}
		allAttempts = append(allAttempts, state)
		totalUsage.InputTokens += attempt.Usage.InputTokens
		totalUsage.OutputTokens += attempt.Usage.OutputTokens
		totalUsage.Cost += attempt.Usage.Cost
		if router != nil && ShouldRecordCandidateQuality(attempt) {
			router.RecordQualityOutcome(phase, attempt.Provider, attempt.Verification.Passed)
		}
		if notVerifiedReason == "" && attempt.Verification.IsolationError != "" {
			notVerifiedReason = attempt.Verification.IsolationError
		}
		// Preserve the fail-closed untrusted-mode sentinel instead of discarding it
		// with the rest of the per-attempt error: when every candidate fails this
		// way the all-failure return below surfaces it so callers can present the
		// actionable message rather than a generic failure.
		if untrustedSafeErr == nil && errors.Is(attempt.Err, model.ErrNoUntrustedSafeProvider) {
			untrustedSafeErr = attempt.Err
		}
		if untrustedSafeErr == nil && (errors.Is(attempt.Err, execution.ErrBudgetExhausted) || errors.Is(attempt.Err, execution.ErrUnknownOutcome)) {
			untrustedSafeErr = attempt.Err
		}
		// Track deletions even from a delete-only candidate that returned empty
		// content: when no candidate content is selected below, these must still
		// be surfaced to the user rather than silently dropped.
		if len(attempt.DeletedFiles) > 0 && (fallbackDeletionsIndex == -1 || attempt.Index < fallbackDeletionsIndex) {
			fallbackDeletions = attempt.DeletedFiles
			fallbackDeletionsIndex = attempt.Index
		}

		if attempt.Err == nil && strings.TrimSpace(attempt.Content) != "" {
			if bestSuccessful == nil || attemptOutranks(attempt, *bestSuccessful) {
				attemptCopy := attempt
				bestSuccessful = &attemptCopy
			}
		}
		// Only candidates at or above the autopilot strength floor count as
		// verified: real baseline tests ran and passed. Weaker passes fall back
		// to bestSuccessful and require user approval.
		if attempt.Verification.Passed && attempt.Verification.Strength >= autopilotMinCandidateStrength {
			passedCount++
			if bestVerified == nil ||
				attempt.Verification.Strength > bestVerified.Verification.Strength ||
				(attempt.Verification.Strength == bestVerified.Verification.Strength && attempt.Index < bestVerified.Index) {
				attemptCopy := attempt
				bestVerified = &attemptCopy
			}
			if bestVerified != nil && bestVerified.Verification.Strength >= maxVerifiedCandidateStrength {
				// Stop outstanding provider work, but keep draining one result from
				// every already-started attempt. A provider may finish at the same
				// time as cancellation and return real usage; returning immediately
				// would make that consumed request disappear from the aggregate.
				cancel()
			}
		}
	}
	wg.Wait()
	close(results)

	if bestVerified != nil {
		totalUsage.Provider = bestVerified.Provider
		return CandidateSelection{
			Content:         bestVerified.Content,
			Provider:        bestVerified.Provider,
			Usage:           totalUsage,
			Verified:        true,
			Strength:        bestVerified.Verification.Strength,
			PassedCount:     passedCount,
			TotalCandidates: len(providers),
			DeletedFiles:    bestVerified.DeletedFiles,
			VerifiedFiles:   bestVerified.Verification.VerifiedFiles,
			VerifiedDigest:  bestVerified.Verification.VerifiedDigest,
			RestoredTests:   discardedTestEdits(*bestVerified),
			NoTestsExecuted: noTestsExecuted(*bestVerified),
			LargeFiles:      bestVerified.LargeFiles,
		}
	}

	if bestSuccessful != nil {
		totalUsage.Provider = bestSuccessful.Provider
		return CandidateSelection{
			Content:           bestSuccessful.Content,
			Provider:          bestSuccessful.Provider,
			Usage:             totalUsage,
			Verified:          false,
			Strength:          bestSuccessful.Verification.Strength,
			PassedCount:       passedCount,
			TotalCandidates:   len(providers),
			DeletedFiles:      bestSuccessful.DeletedFiles,
			NotVerifiedReason: notVerifiedReason,
			VerifiedFiles:     bestSuccessful.Verification.VerifiedFiles,
			VerifiedDigest:    bestSuccessful.Verification.VerifiedDigest,
			RestoredTests:     discardedTestEdits(*bestSuccessful),
			NoTestsExecuted:   noTestsExecuted(*bestSuccessful),
			LargeFiles:        bestSuccessful.LargeFiles,
		}
	}

	if totalUsage.InputTokens > 0 || totalUsage.OutputTokens > 0 || totalUsage.Cost > 0 {
		totalUsage.Provider = "ensemble"
	}
	return CandidateSelection{
		Usage:             totalUsage,
		TotalCandidates:   len(providers),
		DeletedFiles:      fallbackDeletions,
		NotVerifiedReason: notVerifiedReason,
		Err:               untrustedSafeErr,
	}
}

// discardedTestEdits returns the attempt's test-file edits that are NOT part
// of its delivered content. Only a passing verification replaces the delivered
// files with the verified set (which keeps baseline tests); otherwise the raw
// candidate content, test edits included, goes to manual approval.
func candidateErrorStatus(err error) execution.Status {
	switch {
	case err == nil:
		return execution.Passed
	case errors.Is(err, execution.ErrBudgetExhausted):
		return execution.BudgetExhausted
	case errors.Is(err, context.Canceled):
		return execution.Cancelled
	case errors.Is(err, context.DeadlineExceeded):
		return execution.Timeout
	case errors.Is(err, execution.ErrUnknownOutcome):
		return execution.Unknown
	default:
		return execution.Failed
	}
}

func discardedTestEdits(attempt CandidateAttempt) []string {
	if !attempt.Verification.Passed || len(attempt.Verification.RestoredTests) == 0 {
		return nil
	}
	return append([]string(nil), attempt.Verification.RestoredTests...)
}

// noTestsExecuted reports whether the attempt's checks ran no test at all.
func noTestsExecuted(attempt CandidateAttempt) bool {
	v := attempt.Verification
	return v.NoTestPlan || (v.Passed && v.NoTestsRan)
}

// verificationRank orders verification outcomes: a (weak) pass, then
// unverified output whose checks did not fail (no test plan, sandbox
// unavailable), then output that failed a check.
func verificationRank(v CandidateVerification) int {
	switch {
	case v.Passed:
		return 2
	case v.IntegrityError != "":
		return 0
	case v.EnvironmentError || v.IsolationError != "":
		return 1
	case v.QuickCheckError != "" || v.DepsError != "" || v.TestsError != "":
		return 0
	default:
		return 1
	}
}

// attemptOutranks reports whether a beats b as the fallback candidate: prefer
// weak verification passes over unverified output and unverified output over
// failed checks, then higher strength, then the earlier provider in routing
// order.
func attemptOutranks(a, b CandidateAttempt) bool {
	if ra, rb := verificationRank(a.Verification), verificationRank(b.Verification); ra != rb {
		return ra > rb
	}
	if a.Verification.Strength != b.Verification.Strength {
		return a.Verification.Strength > b.Verification.Strength
	}
	return a.Index < b.Index
}

func CandidateAttemptStage(ctx context.Context, attempt CandidateAttempt) CandidateProgressStage {
	if attempt.Err != nil {
		if errors.Is(attempt.Err, context.Canceled) || errors.Is(attempt.Err, context.DeadlineExceeded) {
			return CandidateProgressCanceled
		}
		if ctx != nil {
			if err := ctx.Err(); errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return CandidateProgressCanceled
			}
		}
		return CandidateProgressFailed
	}
	if attempt.Verification.Passed {
		return CandidateProgressPassed
	}
	if verificationUnattributable(attempt.Verification) {
		return CandidateProgressUnverified
	}
	return CandidateProgressRejected
}

// verificationUnattributable reports whether a non-passing verification says
// nothing about the candidate's quality: there was nothing to verify against
// (no baseline test plan) or the environment could not run the checks.
func verificationUnattributable(v CandidateVerification) bool {
	if v.Passed {
		return false
	}
	return v.NoTestPlan || v.IsolationError != "" || v.EnvironmentError
}

func ShouldRecordCandidateQuality(attempt CandidateAttempt) bool {
	if strings.TrimSpace(attempt.Provider) == "" {
		return false
	}
	if attempt.Err != nil {
		return false
	}
	// Unverifiable outcomes must not count against (or for) the provider.
	if verificationUnattributable(attempt.Verification) {
		return false
	}
	return attempt.Verification.Passed || len(attempt.Files) > 0 || strings.TrimSpace(attempt.Content) != ""
}

func isolatedCandidateExcludes(router *model.Router, providerName string, exclude ...string) []string {
	if router == nil {
		return append([]string(nil), exclude...)
	}

	excluded := make(map[string]struct{}, len(exclude)+4)
	for _, name := range exclude {
		name = strings.TrimSpace(name)
		if name == "" || name == providerName {
			continue
		}
		excluded[name] = struct{}{}
	}
	for _, name := range router.Available() {
		if name == providerName {
			continue
		}
		excluded[name] = struct{}{}
	}

	out := make([]string, 0, len(excluded))
	for name := range excluded {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func reportProgress(progress CandidateProgressFunc, provider string, stage CandidateProgressStage) {
	if progress != nil && strings.TrimSpace(provider) != "" {
		progress(provider, stage)
	}
}
