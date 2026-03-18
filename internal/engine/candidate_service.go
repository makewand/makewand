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
	Usage        model.Usage
	Verification CandidateVerification
	Err          error
}

type CandidateSelection struct {
	Content         string
	Provider        string
	Usage           model.Usage
	Verified        bool
	PassedCount     int
	TotalCandidates int
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
		b.WriteString(fmt.Sprintf("--- FILE: %s ---\n```\n%s\n```\n", f.Path, f.Content))
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
			if route.Actual != "" {
				attempt.Provider = route.Actual
			} else if usage.Provider != "" {
				attempt.Provider = usage.Provider
			} else {
				attempt.Provider = providerName
			}
			if err == nil {
				attempt.Files = ParseFilesBestEffort(content).Files
				if len(attempt.Files) == 0 && project != nil && candidateProject != nil {
					changedFiles, diffErr := candidateProject.ChangedFilesAgainst(project)
					if diffErr != nil {
						attempt.Err = diffErr
					} else if len(changedFiles) > 0 {
						attempt.Files = changedFiles
						attempt.Content = RenderExtractedFiles(changedFiles)
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
		bestVerified   *CandidateAttempt
		bestSuccessful *CandidateAttempt
		passedCount    int
	)

	for completed := 0; completed < len(providers); completed++ {
		attempt := <-results
		totalUsage.InputTokens += attempt.Usage.InputTokens
		totalUsage.OutputTokens += attempt.Usage.OutputTokens
		totalUsage.Cost += attempt.Usage.Cost
		if router != nil && ShouldRecordCandidateQuality(attempt) {
			router.RecordQualityOutcome(phase, attempt.Provider, attempt.Verification.Passed)
		}

		if attempt.Err == nil && bestSuccessful == nil && strings.TrimSpace(attempt.Content) != "" {
			attemptCopy := attempt
			bestSuccessful = &attemptCopy
		}
		if attempt.Verification.Passed {
			passedCount++
			if bestVerified == nil ||
				attempt.Verification.Strength > bestVerified.Verification.Strength ||
				(attempt.Verification.Strength == bestVerified.Verification.Strength && attempt.Index < bestVerified.Index) {
				attemptCopy := attempt
				bestVerified = &attemptCopy
			}
			if bestVerified != nil && bestVerified.Verification.Strength >= maxVerifiedCandidateStrength {
				cancel()
				break
			}
		}
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	if bestVerified != nil {
		totalUsage.Provider = bestVerified.Provider
		return CandidateSelection{
			Content:         bestVerified.Content,
			Provider:        bestVerified.Provider,
			Usage:           totalUsage,
			Verified:        true,
			PassedCount:     passedCount,
			TotalCandidates: len(providers),
		}
	}

	if bestSuccessful != nil {
		totalUsage.Provider = bestSuccessful.Provider
		return CandidateSelection{
			Content:         bestSuccessful.Content,
			Provider:        bestSuccessful.Provider,
			Usage:           totalUsage,
			Verified:        false,
			PassedCount:     passedCount,
			TotalCandidates: len(providers),
		}
	}

	return CandidateSelection{TotalCandidates: len(providers)}
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
