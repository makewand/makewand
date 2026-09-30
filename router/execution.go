package router

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/makewand/makewand/execution"
)

var errExecutionAccounting = errors.New("execution accounting failed")

type providerAttemptKey struct{}
type providerAttemptSlot struct {
	mu      sync.Mutex
	attempt *execution.Attempt
}

// Private transport handoff prevents external callers from forging a budget
// voucher while still avoiding duplicate admission at the Router/builtin seam.
func contextWithProviderAttempt(ctx context.Context, attempt *execution.Attempt) context.Context {
	return context.WithValue(ctx, providerAttemptKey{}, &providerAttemptSlot{attempt: attempt})
}
func takeProviderAttempt(ctx context.Context) *execution.Attempt {
	slot, _ := ctx.Value(providerAttemptKey{}).(*providerAttemptSlot)
	if slot == nil {
		return nil
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	attempt := slot.attempt
	slot.attempt = nil
	return attempt
}

func (r *Router) executionContext(ctx context.Context) (context.Context, error) {
	ctx, cfg, err := execution.EnsureContext(ctx)
	if err != nil {
		return ctx, err
	}
	if r.executionAPIPolicy != "" {
		cfg.APIPolicy = r.executionAPIPolicy
	}
	return execution.ContextWithConfig(ctx, cfg), nil
}

func reserveProvider(ctx context.Context, provider Provider) (context.Context, *execution.Attempt, error) {
	ctx, _, err := execution.EnsureContext(ctx)
	if err != nil {
		return ctx, nil, fmt.Errorf("%w: %w", errExecutionAccounting, err)
	}
	if inherited := takeProviderAttempt(ctx); inherited != nil {
		return ctx, inherited, nil
	}
	modelID, _ := ModelFromContext(ctx)
	var requestedModel *string
	if modelID != "" {
		requestedModel = &modelID
	}
	_, workspace := WorkDirFromContext(ctx)
	tier := "standard"
	if mode, ok := UsageModeFromContext(ctx); ok {
		tier = UsageModeToPythonTier(mode)
	}
	attempt, err := execution.Reserve(ctx, execution.Metadata{Engine: provider.Name(), Tier: tier, Model: requestedModel, Readonly: !workspace})
	if err != nil {
		return ctx, nil, fmt.Errorf("%w: %w", errExecutionAccounting, err)
	}
	return ctx, attempt, nil
}

func providerOutcome(err error, usage Usage) execution.Outcome {
	if err == nil {
		outcome := execution.Outcome{Status: execution.Passed, Known: true}
		if usage.MeasuredTokens && usage.InputTokens >= 0 && usage.OutputTokens >= 0 {
			tokens := int64(usage.InputTokens) + int64(usage.OutputTokens)
			outcome.Tokens = &tokens
		}
		if usage.MeasuredCost && usage.Cost >= 0 && !math.IsNaN(usage.Cost) && !math.IsInf(usage.Cost, 0) {
			cost := usage.Cost
			outcome.MonetaryCost = &cost
		}
		return outcome
	}
	kind := string(ErrorKindOf(err))
	outcome := execution.Outcome{Status: execution.Failed, Known: true, ErrorKind: &kind}
	switch {
	case errors.Is(err, execution.ErrBudgetExhausted):
		outcome.Status = execution.BudgetExhausted
	case errors.Is(err, context.Canceled):
		outcome.Status, outcome.Known = execution.Cancelled, false
	case errors.Is(err, context.DeadlineExceeded):
		outcome.Status, outcome.Known = execution.Timeout, false
	case errors.Is(err, execution.ErrUnknownOutcome):
		outcome.Status, outcome.Known = execution.Unknown, false
	default:
		var providerErr *ProviderError
		if errors.As(err, &providerErr) && providerErr.StatusCode != 0 {
			return outcome
		}
		if errors.As(err, &providerErr) && (strings.Contains(providerErr.Op, "response") || strings.Contains(providerErr.Op, "parse") || strings.Contains(providerErr.Op, "stream")) && providerErr.Kind == ErrorKindProvider {
			outcome.Status, outcome.Known = execution.Unknown, false
		}
		if ErrorKindOf(err) == ErrorKindTimeout {
			outcome.Status, outcome.Known = execution.Timeout, false
		}
		if ErrorKindOf(err) == ErrorKindNetwork {
			outcome.Status, outcome.Known = execution.Unknown, false
		}
	}
	return outcome
}

func completeProvider(attempt *execution.Attempt, usage Usage, callErr error) error {
	outcome := providerOutcome(callErr, usage)
	if err := attempt.Complete(outcome); err != nil {
		return fmt.Errorf("%w: %w", errExecutionAccounting, err)
	}
	if callErr != nil && !outcome.Known && !errors.Is(callErr, execution.ErrUnknownOutcome) {
		return &execution.UnknownOutcomeError{Err: callErr}
	}
	return callErr
}

func stopExecutionReplay(err error) bool {
	return errors.Is(err, execution.ErrUnknownOutcome) || errors.Is(err, errExecutionAccounting) || errors.Is(err, execution.ErrBudgetExhausted)
}

// ChatProvider applies the same atomic admission used by Router to direct
// provider users (including doctor and CLI test tools).
func ChatProvider(ctx context.Context, p Provider, messages []Message, system string, maxTokens int) (string, Usage, error) {
	return dispatchChat(ctx, p, func(attemptCtx context.Context) (string, Usage, error) {
		return p.Chat(attemptCtx, messages, system, maxTokens)
	})
}

func dispatchChat(ctx context.Context, p Provider, call func(context.Context) (string, Usage, error)) (string, Usage, error) {
	ctx, attempt, err := reserveProvider(ctx, p)
	if err != nil {
		return "", Usage{}, err
	}
	content, usage, callErr := call(contextWithProviderAttempt(ctx, attempt))
	return content, usage, completeProvider(attempt, usage, callErr)
}

func ChatStreamProvider(ctx context.Context, p Provider, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	return dispatchStream(ctx, p, func(attemptCtx context.Context) (<-chan StreamChunk, error) {
		return p.ChatStream(attemptCtx, messages, system, maxTokens)
	})
}

func dispatchStream(ctx context.Context, p Provider, call func(context.Context) (<-chan StreamChunk, error)) (<-chan StreamChunk, error) {
	ctx, attempt, err := reserveProvider(ctx, p)
	if err != nil {
		return nil, err
	}
	source, callErr := call(contextWithProviderAttempt(ctx, attempt))
	if callErr != nil {
		return nil, completeProvider(attempt, Usage{}, callErr)
	}
	if source == nil {
		return nil, completeProvider(attempt, Usage{}, &execution.UnknownOutcomeError{Err: fmt.Errorf("provider returned a nil stream")})
	}
	return observeDispatchStream(ctx, source, attempt), nil
}

func observeDispatchStream(ctx context.Context, source <-chan StreamChunk, attempt *execution.Attempt) <-chan StreamChunk {
	out := make(chan StreamChunk, 1)
	go func() {
		defer close(out)
		forward := func(chunk StreamChunk) bool {
			if chunk.Done || chunk.Error != nil {
				select {
				case out <- chunk:
					return true
				default:
				}
			}
			timer := time.NewTimer(streamForwardTimeout)
			defer timer.Stop()
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				return false
			case <-timer.C:
				return false
			}
		}
		for {
			var chunk StreamChunk
			select {
			case value, ok := <-source:
				if !ok {
					chunk = StreamChunk{Done: true, Error: &execution.UnknownOutcomeError{Err: fmt.Errorf("provider stream closed before completion")}}
					if ctx.Err() != nil {
						chunk.Error = ctx.Err()
					}
				} else {
					chunk = value
				}
			case <-ctx.Done():
				chunk = StreamChunk{Done: true, Error: ctx.Err()}
			}
			if chunk.Done || chunk.Error != nil {
				chunk.Error = completeProvider(attempt, Usage{}, chunk.Error)
				_ = forward(chunk)
				return
			}
			if !forward(chunk) {
				_ = completeProvider(attempt, Usage{}, &execution.UnknownOutcomeError{Err: fmt.Errorf("stream consumer stopped reading")})
				return
			}
		}
	}()
	return out
}
