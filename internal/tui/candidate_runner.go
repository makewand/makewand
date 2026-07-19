package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/model"
)

type candidateAttempt struct {
	index        int
	requested    string
	provider     string
	content      string
	files        []engine.ExtractedFile
	usage        model.Usage
	verification engine.CandidateVerification
	err          error
}

type candidateSelection struct {
	content       string
	provider      string
	usage         model.Usage
	verified      bool
	selectionNote string
	// err carries a fail-closed sentinel from the engine when every candidate
	// failed for that reason (currently model.ErrNoUntrustedSafeProvider, set when
	// untrusted-repo mode had no untrusted-repo-safe provider). Callers use
	// contentError to surface the actionable message instead of a generic failure.
	err error
}

// contentError returns the error to report when a candidate selection produced
// no usable content. When the selection failed closed in untrusted-repo mode it
// returns the sentinel so handleAIResponse's chatErrorContent presents the
// actionable untrusted-mode message; otherwise it returns the generic fallback.
func (s candidateSelection) contentError(generic error) error {
	if errors.Is(s.err, model.ErrNoUntrustedSafeProvider) {
		return s.err
	}
	return generic
}

type candidateProgressStage = engine.CandidateProgressStage

const (
	candidateProgressRunning   = engine.CandidateProgressRunning
	candidateProgressVerifying = engine.CandidateProgressVerifying
	candidateProgressPassed    = engine.CandidateProgressPassed
	candidateProgressRejected  = engine.CandidateProgressRejected
	candidateProgressFailed    = engine.CandidateProgressFailed
	candidateProgressCanceled  = engine.CandidateProgressCanceled
)

type candidateProgressReporter struct {
	mu       sync.Mutex
	activity *chatActivityState
	order    []string
	stages   map[string]candidateProgressStage
	closed   bool
}

func (a App) shouldUseAutopilotCandidates() bool {
	return a.currentApprovalMode() == config.ApprovalModeAuto
}

func orderedCandidateProviders(router *model.Router, phase model.BuildPhase, exclude ...string) []string {
	return engine.OrderedCandidateProviders(router, phase, exclude...)
}

func runCandidateSelection(
	ctx context.Context,
	router *model.Router,
	project *engine.Project,
	phase model.BuildPhase,
	messages []model.Message,
	system string,
	exclude ...string,
) candidateSelection {
	return runCandidateSelectionWithActivity(ctx, nil, router, project, phase, messages, system, exclude...)
}

func newCandidateProgressReporter(activity *chatActivityState, providers []string) *candidateProgressReporter {
	if activity == nil || len(providers) == 0 {
		return nil
	}
	return &candidateProgressReporter{
		activity: activity,
		order:    append([]string(nil), providers...),
		stages:   make(map[string]candidateProgressStage, len(providers)),
	}
}

func (r *candidateProgressReporter) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
}

func (r *candidateProgressReporter) Set(provider string, stage candidateProgressStage) {
	if r == nil || strings.TrimSpace(provider) == "" {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.stages[provider] = stage
	detail := r.summaryLocked()
	activity := r.activity
	r.mu.Unlock()
	activity.SetPhase(chatActivityWaiting, "", "", false, detail)
}

func (r *candidateProgressReporter) summaryLocked() string {
	parts := make([]string, 0, len(r.order))
	for _, provider := range r.order {
		stage, ok := r.stages[provider]
		if !ok {
			continue
		}
		parts = append(parts, formatCandidateProgress(provider, stage))
	}
	return strings.Join(parts, " | ")
}

func formatCandidateProgress(provider string, stage candidateProgressStage) string {
	msg := i18n.Msg()
	switch stage {
	case candidateProgressVerifying:
		return fmt.Sprintf(msg.AutomationCandidateVerifying, provider)
	case candidateProgressPassed:
		return fmt.Sprintf(msg.AutomationCandidatePassed, provider)
	case candidateProgressRejected:
		return fmt.Sprintf(msg.AutomationCandidateRejected, provider)
	case candidateProgressFailed:
		return fmt.Sprintf(msg.AutomationCandidateFailed, provider)
	case candidateProgressCanceled:
		return fmt.Sprintf(msg.AutomationCandidateCanceled, provider)
	default:
		return fmt.Sprintf(msg.AutomationCandidateRunning, provider)
	}
}

func runCandidateSelectionWithActivity(
	ctx context.Context,
	activity *chatActivityState,
	router *model.Router,
	project *engine.Project,
	phase model.BuildPhase,
	messages []model.Message,
	system string,
	exclude ...string,
) candidateSelection {
	providers := engine.OrderedCandidateProviders(router, phase, exclude...)
	reporter := newCandidateProgressReporter(activity, providers)
	if reporter != nil {
		defer reporter.Stop()
	}

	selection := engine.RunCandidateSelection(ctx, router, project, phase, messages, system, func(provider string, stage engine.CandidateProgressStage) {
		if reporter != nil {
			reporter.Set(provider, stage)
		}
	}, exclude...)

	msg := i18n.Msg()
	provider := selection.Provider
	if provider == "" {
		provider = selection.Usage.Provider
	}
	local := candidateSelection{
		content:  selection.Content,
		provider: provider,
		usage:    selection.Usage,
		verified: selection.Verified,
		err:      selection.Err,
	}
	var notes []string
	switch {
	case selection.Verified && selection.Provider != "":
		notes = append(notes, fmt.Sprintf(msg.AutomationCandidateSelected, selection.Provider, selection.PassedCount, selection.TotalCandidates))
	case selection.NotVerifiedReason != "":
		notes = append(notes, fmt.Sprintf(msg.AutomationCandidateIsolationUnavailable, selection.NotVerifiedReason))
	case strings.TrimSpace(selection.Content) != "" && selection.Strength > 0:
		notes = append(notes, msg.AutomationCandidateWeakVerification)
	case strings.TrimSpace(selection.Content) != "":
		notes = append(notes, msg.AutomationCandidateFallback)
	}
	if len(selection.DeletedFiles) > 0 {
		notes = append(notes, fmt.Sprintf(msg.AutomationCandidateDeletions, strings.Join(selection.DeletedFiles, ", ")))
	}
	local.selectionNote = strings.Join(notes, "\n")
	return local
}

func shouldRecordCandidateQuality(attempt candidateAttempt) bool {
	return engine.ShouldRecordCandidateQuality(engine.CandidateAttempt{
		Index:        attempt.index,
		Requested:    attempt.requested,
		Provider:     attempt.provider,
		Content:      attempt.content,
		Files:        attempt.files,
		Usage:        attempt.usage,
		Verification: attempt.verification,
		Err:          attempt.err,
	})
}
