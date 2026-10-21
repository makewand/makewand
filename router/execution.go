package router

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/processjob"
)

var errExecutionAccounting = errors.New("execution accounting failed")
var errExecutionAdmission = errors.New("execution admission interrupted")

type providerAttemptKey struct{}
type providerLifecycleKey struct{}
type providerAttemptSlot struct {
	mu      sync.Mutex
	attempt *execution.Attempt
}

// Private transport handoff prevents external callers from forging a budget
// voucher while still avoiding duplicate admission at the Router/builtin seam.
func contextWithProviderAttempt(ctx context.Context, attempt *execution.Attempt) context.Context {
	ctx = context.WithValue(ctx, providerLifecycleKey{}, attempt)
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
	ctx, cfg, err := execution.EnsureContext(ctx)
	if err != nil {
		return ctx, nil, fmt.Errorf("%w: %w", errExecutionAccounting, err)
	}
	if inherited := takeProviderAttempt(ctx); inherited != nil {
		return context.WithValue(ctx, providerLifecycleKey{}, inherited), inherited, nil
	}
	switch provider.(type) {
	case *OpenAI, *Claude, *Gemini:
		// API credentials do not identify the subscription login. Tag the
		// actual transport before admission so native Gemini competes with
		// Python agy API calls in the same canonical API namespace.
		account := provider.Name() + ":api:unknown"
		cfg.AccountRef = &account
		ctx = execution.ContextWithConfig(ctx, cfg)
	default:
		ctx, err = bindCredentialAdmission(ctx, provider.Name())
		if err != nil {
			return ctx, nil, fmt.Errorf("%w: %w", errExecutionAccounting, err)
		}
		if provider.Name() == "codex" || provider.Name() == "claude" {
			identity := readQuotaIdentity(provider.Name(), selectedQuotaRoot(provider.Name()))
			if cfg.AccountRef != nil && identity.IdentityKnown && *cfg.AccountRef != identity.AccountRef {
				return ctx, nil, errors.New("admission account differs from selected login")
			}
			if cfg.AccountRef == nil {
				cfg.AccountRef = &identity.AccountRef
				ctx = execution.ContextWithConfig(ctx, cfg)
			}
		}
		if err := verifyCredentialBinding(ctx, provider.Name()); err != nil {
			return ctx, nil, err
		}
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
	attempt, err := execution.Reserve(ctx, execution.Metadata{Engine: provider.Name(), Tier: tier, Model: requestedModel, Readonly: !workspace, Pending: true})
	if err != nil {
		if errors.Is(err, execution.ErrConcurrencyBusy) {
			return ctx, nil, err
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ctx, nil, fmt.Errorf("%w: %w", errExecutionAdmission, err)
		}
		return ctx, nil, fmt.Errorf("%w: %w", errExecutionAccounting, err)
	}
	if err := ctx.Err(); err != nil {
		admissionErr := fmt.Errorf("%w: %w", errExecutionAdmission, err)
		if releaseErr := attempt.ReleaseUnstarted(); releaseErr != nil {
			return ctx, nil, errors.Join(admissionErr, fmt.Errorf("%w: %w", errExecutionAccounting, releaseErr))
		}
		return ctx, nil, admissionErr
	}
	// CLI account validation/copy happens before its launch boundary.
	_, cli := provider.(*CLIProvider)
	if !cli {
		if err := attempt.Start(); err != nil {
			releaseErr := attempt.ReleaseUnstarted()
			return ctx, nil, fmt.Errorf("%w: %w", errExecutionAccounting, errors.Join(err, releaseErr))
		}
	}
	return context.WithValue(ctx, providerLifecycleKey{}, attempt), attempt, nil
}

func providerOutcome(err error, usage Usage) execution.Outcome {
	var outcome execution.Outcome
	if err == nil {
		outcome = execution.Outcome{Status: execution.Passed, Known: true}
	} else {
		kind := string(ErrorKindOf(err))
		outcome = execution.Outcome{Status: execution.Failed, Known: true, ErrorKind: &kind}
		switch {
		case errors.Is(err, execution.ErrConcurrencyBusy):
			kind = "concurrency_busy"
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
			if errors.As(err, &providerErr) && (strings.Contains(providerErr.Op, "response") || strings.Contains(providerErr.Op, "parse") || strings.Contains(providerErr.Op, "stream")) && providerErr.Kind == ErrorKindProvider {
				outcome.Status, outcome.Known = execution.Unknown, false
			}
			// An ordinary CLI exit only proves the local process stopped. It
			// does not prove whether the upstream model finished the request.
			// The CLI's own refusal of the invocation is no ordinary exit: it
			// has its own kind (cliInvocationRefusal) and stays known.
			if errors.As(err, &providerErr) && providerErr.Op == "CLI" && providerErr.Kind == ErrorKindProvider {
				outcome.Status, outcome.Known = execution.Unknown, false
			}
			if ErrorKindOf(err) == ErrorKindTimeout {
				outcome.Status, outcome.Known = execution.Timeout, false
			}
			if ErrorKindOf(err) == ErrorKindNetwork {
				outcome.Status, outcome.Known = execution.Unknown, false
			}
		}
	}

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

func completeProvider(attempt *execution.Attempt, usage Usage, callErr error) error {
	outcome := providerOutcome(callErr, usage)
	if err := attempt.Complete(outcome); err != nil {
		accountingErr := fmt.Errorf("%w: %w", errExecutionAccounting, err)
		if callErr != nil {
			accountingErr = errors.Join(accountingErr, callErr)
		}
		if !outcome.Known && !errors.Is(accountingErr, execution.ErrUnknownOutcome) {
			return &execution.UnknownOutcomeError{Err: accountingErr}
		}
		return accountingErr
	}
	if callErr != nil && !outcome.Known && !errors.Is(callErr, execution.ErrUnknownOutcome) {
		return &execution.UnknownOutcomeError{Err: callErr}
	}
	return callErr
}

func stopExecutionReplay(err error) bool {
	return replayStoppingUnknown(err) || errors.Is(err, errExecutionAccounting) || errors.Is(err, execution.ErrBudgetExhausted)
}

// onlyUnknownOutcome reports whether err stops replay only because a call's
// outcome is unknown, not because admission or accounting refused a call.
func onlyUnknownOutcome(err error) bool {
	return errors.Is(err, execution.ErrUnknownOutcome) && !errors.Is(err, errExecutionAccounting) && !errors.Is(err, execution.ErrBudgetExhausted)
}

// readOnlyUnknownError is the unknown outcome of a CLI exit in a read-only run
// (readOnlyExitError). It is UNKNOWN to accounting and to callers, but it does
// not stop fallback: the run could not write the workspace, so no edit of the
// next provider can stack on one of it.
type readOnlyUnknownError struct{ err error }

func (e *readOnlyUnknownError) Error() string {
	return "model call outcome unknown (read-only run): " + e.err.Error()
}
func (e *readOnlyUnknownError) Unwrap() error        { return e.err }
func (e *readOnlyUnknownError) Is(target error) bool { return target == execution.ErrUnknownOutcome }

// readOnlyExitError marks err, the error of a finished CLI run, as a read-only
// run's unknown outcome when the run was read-only (cliReadOnlyRun) and its
// exit says nothing the catalog recognizes: an ordinary exit (ErrorKindProvider)
// or one that looks transient (ErrorKindNetwork). A refusal, a quota limit, a
// timeout and a run without a complete exit result are left as they are.
func readOnlyExitError(ctx context.Context, err error) error {
	var providerErr *ProviderError
	if err == nil || !cliReadOnlyRun(ctx) || errors.Is(err, execution.ErrUnknownOutcome) ||
		!errors.As(err, &providerErr) || providerErr.Op != "CLI" ||
		(providerErr.Kind != ErrorKindProvider && providerErr.Kind != ErrorKindNetwork) {
		return err
	}
	return &readOnlyUnknownError{err: err}
}

// replayStoppingUnknown reports whether err carries an unknown outcome that
// stops automatic replay: errors.Is(err, execution.ErrUnknownOutcome) without
// what a readOnlyUnknownError wraps.
func replayStoppingUnknown(err error) bool {
	for err != nil {
		switch e := err.(type) {
		case *readOnlyUnknownError:
			return false
		case interface{ Unwrap() []error }:
			return slices.ContainsFunc(e.Unwrap(), replayStoppingUnknown)
		}
		if err == execution.ErrUnknownOutcome {
			return true
		}
		if is, ok := err.(interface{ Is(error) bool }); ok && is.Is(execution.ErrUnknownOutcome) {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
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
	return observeDispatchStream(ctx, source, attempt, streamDrainWaitFor(p)), nil
}

// observeDispatchStream forwards source and completes attempt with its outcome.
// A cancellation of ctx ends the stream with ctx's error; with drainWait > 0
// only once source has ended too, waiting at most drainWait for it
// (drainStreamSource).
func observeDispatchStream(ctx context.Context, source <-chan StreamChunk, attempt *execution.Attempt, drainWait time.Duration) <-chan StreamChunk {
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
				drainStreamSource(source, drainWait)
				chunk = StreamChunk{Done: true, Error: ctx.Err()}
			}
			if chunk.Done || chunk.Error != nil {
				chunk.Error = completeProvider(attempt, Usage{}, chunk.Error)
				_ = forward(chunk)
				return
			}
			if !forward(chunk) {
				if ctx.Err() != nil {
					drainStreamSource(source, drainWait)
				}
				_ = completeProvider(attempt, Usage{}, &execution.UnknownOutcomeError{Err: fmt.Errorf("stream consumer stopped reading")})
				return
			}
		}
	}()
	return out
}

// streamEndWaiter is a provider whose stream, once a cancellation has ended
// it, still does work a caller must not outrun: a provider CLI's stream ends
// only after the credential write-back of its sandbox's private home
// (cliAfterWait). streamEndWait bounds how long a wrapper ending such a stream
// on a cancellation waits for it to end first.
type streamEndWaiter interface {
	streamEndWait() time.Duration
}

// Cancellation first kills and drains the CLI, then cliAfterWait starts its
// write-back timer. Cover both intervals from the wrapper's cancellation time.
func (c *CLIProvider) streamEndWait() time.Duration {
	return processjob.DefaultWaitDelay + cliWriteBackWait()
}

// streamDrainWaitFor returns how long a wrapper of p's streams waits for the
// source to end before it ends its own stream on a cancellation: 0, at once,
// for a provider whose stream does nothing after a cancellation that a caller
// could outrun (an API provider), which is any but a streamEndWaiter.
func streamDrainWaitFor(p Provider) time.Duration {
	if w, ok := p.(streamEndWaiter); ok {
		return w.streamEndWait()
	}
	return 0
}

// drainStreamSource discards what source still sends until it sends its
// terminal chunk or closes, waiting at most wait (not at all for wait <= 0).
// A wrapper that ends its own stream on a cancellation calls it before its
// terminal chunk and close: the source is still ending then - a provider CLI
// is being killed, and its sandbox's credential write-back runs before its
// stream ends (cliAfterWait) - and a caller that returns, or a process that
// exits, once the stream has ended would otherwise lose a token the provider
// refreshed in the sandbox and rotated on its server. A source that takes
// longer than wait (a stuck private home, see cliWriteBackWait) cannot hang
// the cancellation.
func drainStreamSource(source <-chan StreamChunk, wait time.Duration) {
	if wait <= 0 {
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case chunk, ok := <-source:
			if !ok || chunk.Done || chunk.Error != nil {
				return
			}
		case <-timer.C:
			return
		}
	}
}
