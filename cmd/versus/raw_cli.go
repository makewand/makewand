package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/model"
	"github.com/makewand/makewand/internal/processjob"
)

// rawCLIProvider performs one raw baseline process. ChatProvider supplies the
// same admission, accounting and private metadata events as the routed arm.
type rawCLIProvider struct {
	name string
	path string
	args []string
	env  []string
}

func (p *rawCLIProvider) Name() string      { return p.name }
func (p *rawCLIProvider) IsAvailable() bool { return true }
func (p *rawCLIProvider) ChatStream(context.Context, []model.Message, string, int) (<-chan model.StreamChunk, error) {
	return nil, fmt.Errorf("raw comparison does not support streaming")
}

func (p *rawCLIProvider) Chat(ctx context.Context, _ []model.Message, _ string, _ int) (string, model.Usage, error) {
	// #nosec G204 -- this explicit comparison command resolves the operator-selected executable before admission; argv is passed without a shell.
	cmd := exec.CommandContext(ctx, p.path, p.args...)
	cmd.Env = p.env
	setProcessGroup(cmd)
	cmd.Cancel = func() error {
		terminateProcessGroup(cmd)
		return nil
	}
	cmd.WaitDelay = 100 * time.Millisecond
	capture := processjob.NewCapture(processjob.MaxOutputBytes, func() { killProcessGroup(cmd) })
	cmd.Stdout, cmd.Stderr = capture.Stdout(), capture.Stderr()
	usage := model.Usage{Provider: p.name}
	started := time.Now()
	cleanupProcess, startErr := processjob.Start(cmd)
	if startErr != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return "", usage, model.ClassifyCLIExecutionError(p.name, capture.StderrString(), startErr, errors.Join(contextErr, startErr), time.Since(started))
		}
		return "", usage, fmt.Errorf("start raw CLI: %w", startErr)
	}
	defer cleanupProcess()
	defer killProcessGroup(cmd)
	waitErr := cmd.Wait()
	if capture.Exceeded() {
		return "", usage, &execution.UnknownOutcomeError{Err: processjob.OutputLimitError()}
	}
	if err := waitErr; err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return "", usage, model.ClassifyCLIExecutionError(p.name, capture.StderrString(), err, contextErr, time.Since(started))
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return "", usage, &execution.UnknownOutcomeError{Err: fmt.Errorf("wait raw CLI: %w", err)}
		}
		if exitErr.ExitCode() < 0 {
			return "", usage, &execution.UnknownOutcomeError{Err: fmt.Errorf("raw CLI terminated without an exit result: %w", err)}
		}
		return "", usage, model.ClassifyCLIExecutionError(p.name, capture.StderrString(), err, nil, time.Since(started))
	}
	return capture.StdoutString(), usage, nil
}

func runCLIWithTimeout(parent context.Context, name, bin string, args []string, timeout time.Duration) result {
	return runCLIWithTimeoutAndEnv(parent, name, bin, args, timeout, os.Environ())
}

func runCLIWithTimeoutAndEnv(parent context.Context, name, bin string, args []string, timeout time.Duration, customEnv []string) result {
	r := result{name: name}
	// A missing executable or invalid command configuration never reserves a
	// provider slot. Races after admission conservatively keep their reservation.
	binPath, err := exec.LookPath(bin)
	if err != nil {
		r.err = fmt.Errorf("%s not found: %w", bin, err)
		fmt.Printf("  ✗ %s not found\n\n", bin)
		return r
	}
	if timeout <= 0 {
		r.err = &execution.StatusError{Status: execution.InvalidRequest, OutcomeKnown: true, Err: fmt.Errorf("raw CLI timeout must be positive")}
		return r
	}
	for _, arg := range args {
		if strings.ContainsRune(arg, '\x00') {
			r.err = &execution.StatusError{Status: execution.InvalidRequest, OutcomeKnown: true, Err: fmt.Errorf("raw CLI argument contains NUL")}
			return r
		}
	}
	if err := parent.Err(); err != nil {
		r.err = err
		return r
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	ctx = execution.ContextWithStage(ctx, "generation")
	// The historical raw commands can write in their working directory. Record
	// that capability instead of labeling them as the routed read-only CLI mode.
	ctx = model.ContextWithWorkDir(ctx, ".")
	env := customEnv
	if env == nil {
		env = os.Environ()
	}
	provider := &rawCLIProvider{name: filepath.Base(bin), path: binPath, args: args, env: env}
	if bin == "claude" {
		filtered := make([]string, 0, len(provider.env))
		for _, entry := range provider.env {
			if !strings.HasPrefix(entry, "CLAUDECODE=") {
				filtered = append(filtered, entry)
			}
		}
		provider.env = filtered
	}
	if bin == "gemini" {
		provider.env = ensureNoProxyCopy(provider.env)
	}
	fmt.Printf("  Running %s... ", binPath)
	start := time.Now()
	content, _, err := model.ChatProvider(ctx, provider, nil, "", 0)
	r.elapsed = time.Since(start)
	if err != nil {
		r.err = err
		message := err.Error()
		fmt.Printf("✗ (%.0fs) %s\n\n", r.elapsed.Seconds(), message[:min(len(message), 100)])
		return r
	}
	r.output = stripANSI(content)
	r.files = engine.ParseFilesBestEffort(r.output).Files
	for _, file := range r.files {
		r.totalLines += strings.Count(file.Content, "\n") + 1
	}
	fmt.Printf("✓ %.0fs, %d files, %d lines\n\n", r.elapsed.Seconds(), len(r.files), r.totalLines)
	return r
}

// ParallelSpec defines a contestant configuration for concurrent dispatch.
type ParallelSpec struct {
	Name    string
	Bin     string
	Args    []string
	Timeout time.Duration
	Env     []string
}

// VerificationGate checks whether a contestant result passes verification.
type VerificationGate func(r result) bool

// DefaultVerificationGate accepts results with no error and at least one extracted file.
func DefaultVerificationGate(r result) bool {
	return r.err == nil && len(r.files) > 0
}

// RunParallelRace executes contestant specs concurrently in a Winner-Take-All race.
// As soon as any candidate completes successfully and passes the verification gate,
// cancellation (SIGTERM) is immediately sent to all remaining running contestants.
func RunParallelRace(parent context.Context, specs []ParallelSpec, gate VerificationGate) ([]result, *result) {
	if gate == nil {
		gate = DefaultVerificationGate
	}
	raceCtx, cancelAll := context.WithCancel(parent)
	defer cancelAll()

	results := make([]result, len(specs))
	type contestantOutcome struct {
		index int
		res   result
	}
	ch := make(chan contestantOutcome, len(specs))

	for i, spec := range specs {
		go func(idx int, sp ParallelSpec) {
			r := runCLIWithTimeoutAndEnv(raceCtx, sp.Name, sp.Bin, sp.Args, sp.Timeout, sp.Env)
			ch <- contestantOutcome{index: idx, res: r}
		}(i, spec)
	}

	var winner *result
	received := 0
	for received < len(specs) {
		out := <-ch
		results[out.index] = out.res
		received++

		if winner == nil && gate(out.res) {
			// Winning candidate completed and passed all verification gates!
			w := out.res
			winner = &w
			// Winner-Take-All: immediately send SIGTERM / cancellation to remaining running contestants!
			cancelAll()
		}
	}

	return results, winner
}

