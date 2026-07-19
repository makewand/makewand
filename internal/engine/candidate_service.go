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

	"github.com/makewand/makewand/internal/model"
)

type CandidateAttempt struct {
	Index        int
	Requested    string
	Provider     string
	Content      string
	Files        []ExtractedFile
	DeletedFiles []string
	Usage        model.Usage
	Verification CandidateVerification
	Err          error
}

type CandidateSelection struct {
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
	Err error
}

type CandidateProgressStage string

const (
	CandidateProgressRunning   CandidateProgressStage = "running"
	CandidateProgressVerifying CandidateProgressStage = "verifying"
	CandidateProgressPassed    CandidateProgressStage = "passed"
	CandidateProgressRejected  CandidateProgressStage = "rejected"
	CandidateProgressFailed    CandidateProgressStage = "failed"
	CandidateProgressCanceled  CandidateProgressStage = "canceled"
)

const maxVerifiedCandidateStrength = 2

// autopilotMinCandidateStrength is the acceptance floor for automatic
// application: real (baseline) tests must have run and passed. Candidates that
// merely compile or install dependencies (Strength 1) require user approval.
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
) CandidateSelection {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	providers := OrderedCandidateProviders(router, phase, exclude...)
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
	var wg sync.WaitGroup
	for i, name := range providers {
		wg.Add(1)
		go func(idx int, providerName string) {
			defer wg.Done()
			reportProgress(progress, providerName, CandidateProgressRunning)
			attemptCtx := ctx
			candidateProject := project
			if project != nil {
				cloned, cloneErr := project.CloneToTemp()
				if cloneErr != nil {
					reportProgress(progress, providerName, CandidateProgressFailed)
					results <- CandidateAttempt{
						Index:     idx,
						Requested: providerName,
						Provider:  providerName,
						Err:       cloneErr,
					}
					return
				}
				defer os.RemoveAll(cloned.Path)
				candidateProject = cloned
				attemptCtx = model.ContextWithWorkDir(ctx, cloned.Path)
			}

			attemptExclude := isolatedCandidateExcludes(router, providerName, exclude...)
			content, usage, route, err := router.ChatWith(attemptCtx, providerName, phase, messages, system, attemptExclude...)
			attempt := CandidateAttempt{
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
					changedFiles, deletedFiles, diffErr := candidateProject.ChangedFilesAgainstWithDeletions(project)
					if diffErr != nil {
						attempt.Err = diffErr
					} else {
						attempt.DeletedFiles = deletedFiles
						merged := mergeCandidateFiles(attempt.Files, changedFiles)
						if len(changedFiles) > 0 && !extractedFilesEqual(merged, attempt.Files) {
							attempt.Files = merged
							attempt.Content = RenderExtractedFiles(merged)
						}
					}
				}
				if attempt.Err == nil && project != nil && len(attempt.Files) > 0 {
					reportProgress(progress, providerName, CandidateProgressVerifying)
					verification, verifyErr := project.EvaluateCandidateFiles(ctx, attempt.Files)
					if verifyErr != nil {
						attempt.Err = verifyErr
					} else {
						attempt.Verification = verification
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

// attemptOutranks reports whether a beats b as the fallback candidate: prefer
// weak verification passes over unverified output, then higher strength, then
// the earlier provider in routing order.
func attemptOutranks(a, b CandidateAttempt) bool {
	if a.Verification.Passed != b.Verification.Passed {
		return a.Verification.Passed
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
	return CandidateProgressRejected
}

func ShouldRecordCandidateQuality(attempt CandidateAttempt) bool {
	if strings.TrimSpace(attempt.Provider) == "" {
		return false
	}
	if attempt.Err != nil {
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
