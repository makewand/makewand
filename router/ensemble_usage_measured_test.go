package router

import (
	"context"
	"errors"
	"testing"

	"github.com/makewand/makewand/execution"
)

// spendThenFailProvider consumes measurable spend and then reports an error.
type spendThenFailProvider struct {
	ensembleCaptureProvider
	err error
}

func (p *spendThenFailProvider) Chat(context.Context, []Message, string, int) (string, Usage, error) {
	return "", p.usage, p.err
}

func measuredUsage(cost float64, in, out int) Usage {
	return Usage{Cost: cost, InputTokens: in, OutputTokens: out, MeasuredCost: true, MeasuredTokens: true}
}

func TestAccumulateAttemptUsageANDsMeasuredFlags(t *testing.T) {
	var total Usage
	total = accumulateAttemptUsage(total, measuredUsage(1, 10, 1))
	if !total.MeasuredCost || !total.MeasuredTokens {
		t.Fatalf("first measured part must seed measured flags: %+v", total)
	}
	total = accumulateAttemptUsage(total, Usage{}) // zero spend carries no information
	if !total.MeasuredCost || !total.MeasuredTokens {
		t.Fatalf("zero-spend part must not clear measured flags: %+v", total)
	}
	total = accumulateAttemptUsage(total, Usage{Cost: 2, InputTokens: 5})
	if total.MeasuredCost || total.MeasuredTokens {
		t.Fatalf("estimated part must clear measured flags: %+v", total)
	}
	if total.Cost != 3 || total.InputTokens != 15 || total.OutputTokens != 1 {
		t.Fatalf("totals = %+v, want sum of all parts", total)
	}
	keep := accumulateAttemptUsage(Usage{Provider: "ensemble"}, measuredUsage(1, 1, 1))
	if keep.Provider != "ensemble" {
		t.Fatalf("aggregate provider attribution lost: %+v", keep)
	}
}

func TestChatBestPowerFailedPlusSuccessfulUsageMeasuredSemantics(t *testing.T) {
	for _, tc := range []struct {
		name         string
		okUsage      Usage
		wantMeasured bool
	}{
		{"all measured", measuredUsage(2, 20, 2), true},
		{"one estimated", Usage{Cost: 2, InputTokens: 20, OutputTokens: 2}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failed := &spendThenFailProvider{
				ensembleCaptureProvider: ensembleCaptureProvider{name: "gemini", available: true, usage: measuredUsage(1, 10, 1)},
				err:                     errors.New("provider failed after spend"),
			}
			ok := &ensembleCaptureProvider{name: "claude", available: true, content: "usable", usage: tc.okUsage}
			judge := &ensembleCaptureProvider{name: "codex", available: false}
			r := powerReviewRouter(failed, ok, judge)

			_, total, _, err := r.ChatBest(context.Background(), PhaseReview, []Message{{Role: "user", Content: "review"}}, "")
			if err != nil {
				t.Fatalf("ChatBest: %v", err)
			}
			if total.Cost != 3 || total.InputTokens != 30 || total.OutputTokens != 3 {
				t.Fatalf("total = %+v, want failed + successful attempts", total)
			}
			if total.MeasuredCost != tc.wantMeasured || total.MeasuredTokens != tc.wantMeasured {
				t.Fatalf("measured flags = cost:%v tokens:%v, want %v", total.MeasuredCost, total.MeasuredTokens, tc.wantMeasured)
			}
		})
	}
}

func TestChatBestPowerTerminalErrorKeepsUsageAndMeasuredSemantics(t *testing.T) {
	for _, tc := range []struct {
		name         string
		okUsage      Usage
		wantMeasured bool
	}{
		{"all measured", measuredUsage(2, 20, 2), true},
		{"one estimated", Usage{Cost: 2, InputTokens: 20, OutputTokens: 2}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failed := &spendThenFailProvider{
				ensembleCaptureProvider: ensembleCaptureProvider{name: "gemini", available: true, usage: measuredUsage(1, 10, 1)},
				err:                     &execution.UnknownOutcomeError{Err: errors.New("stream cut")},
			}
			ok := &ensembleCaptureProvider{name: "claude", available: true, content: "usable", usage: tc.okUsage}
			judge := &ensembleCaptureProvider{name: "codex", available: false}
			r := powerReviewRouter(failed, ok, judge)

			_, total, _, err := r.ChatBest(context.Background(), PhaseReview, []Message{{Role: "user", Content: "review"}}, "")
			if !errors.Is(err, execution.ErrUnknownOutcome) {
				t.Fatalf("err = %v, want unknown outcome", err)
			}
			if total.Cost != 3 || total.InputTokens != 30 || total.OutputTokens != 3 {
				t.Fatalf("total = %+v, want failed + successful attempts", total)
			}
			if total.MeasuredCost != tc.wantMeasured || total.MeasuredTokens != tc.wantMeasured {
				t.Fatalf("measured flags = cost:%v tokens:%v, want %v", total.MeasuredCost, total.MeasuredTokens, tc.wantMeasured)
			}
		})
	}
}
