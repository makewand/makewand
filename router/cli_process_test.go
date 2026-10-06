package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/processjob"
)

const cliProcessFixtureStartupTimeout = 15 * time.Second

func TestCLIProcessChild(t *testing.T) {
	mode := os.Getenv("MAKEWAND_CLI_PROCESS_CHILD")
	if mode == "" {
		return
	}
	marker := os.Getenv("MAKEWAND_CLI_PROCESS_MARKER")
	if mode == "marker" {
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if err := os.WriteFile(marker+".ready", []byte("ready"), 0o600); err != nil {
			os.Exit(3)
		}
		time.Sleep(2 * time.Second)
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if err := os.WriteFile(marker, []byte("escaped"), 0o600); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	if mode == "quiet-only" {
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	if mode == "quiet" || mode == "leader" {
		// #nosec G204 G702 -- parent-controlled Go test executable and fixed helper selector.
		child := exec.Command(os.Args[0], "-test.run=^TestCLIProcessChild$")
		child.Env = append(os.Environ(), "MAKEWAND_CLI_PROCESS_CHILD=marker")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if runtime.GOOS == "windows" {
			setCLIProcessGroup(child) // A new Windows group still belongs to the Job.
		}
		if err := child.Start(); err != nil {
			os.Exit(4)
		}
		deadline := time.Now().Add(cliProcessFixtureStartupTimeout)
		for time.Now().Before(deadline) {
			// #nosec G703 -- marker is in the parent-created temporary fixture directory.
			if _, err := os.Stat(marker + ".ready"); err == nil {
				if mode == "quiet" {
					time.Sleep(10 * time.Second)
				} else {
					fmt.Print("leader\n")
				}
				os.Exit(0)
			}
			time.Sleep(5 * time.Millisecond)
		}
		os.Exit(5)
	}
	if mode == "long-line" {
		// Transfer a real unterminated prefix just below the Scanner limit.
		// The parent starts the cleanup clock only after this prefix and
		// runtime initialization are ready, then releases the oversized tail.
		const prefixBytes = (1 << 20) - 1
		if _, err := os.Stdout.Write(bytes.Repeat([]byte("x"), prefixBytes)); err != nil {
			os.Exit(6)
		}
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if err := os.WriteFile(marker+".ready", []byte("prefix ready"), 0o600); err != nil {
			os.Exit(6)
		}
		deadline := time.Now().Add(cliProcessFixtureStartupTimeout)
		armed := false
		for time.Now().Before(deadline) {
			// #nosec G703 -- marker is in the parent-created temporary fixture directory.
			if _, err := os.Stat(marker + ".armed"); err == nil {
				armed = true
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !armed {
			os.Exit(6)
		}
		if _, err := os.Stdout.Write(bytes.Repeat([]byte("x"), (2<<20)-prefixBytes)); err != nil {
			os.Exit(6)
		}
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	block := append(bytes.Repeat([]byte("x"), 4095), '\n')
	size := 6 << 20
	if mode == "healthy" {
		size = 256 << 10
	}
	for _, stream := range []*os.File{os.Stdout, os.Stderr} {
		for written := 0; written < size; written += len(block) {
			if _, err := stream.Write(block); err != nil {
				os.Exit(6)
			}
		}
	}
	if mode == "healthy" {
		fmt.Println("tail")
	}
	os.Exit(0)
}

func TestCLIProcessOutputOverflowIsUnknown(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			p := cliProcessProvider(t, "flood", "")
			// This guard bounds helper initialization and transferring 12 MiB.
			// The output limit must trigger UNKNOWN before the guard expires;
			// processjob separately checks cleanup from the actual stop event.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var err error
			if streaming {
				_, err = collectCLIProcessStream(ctx, p)
			} else {
				_, _, err = p.chatReservedAttempt(ctx, "fixture", "fixture", "")
			}
			if !errors.Is(err, execution.ErrUnknownOutcome) || execution.ErrorStatus(err) != execution.Unknown || ctx.Err() != nil {
				t.Fatalf("overflow was not a bounded UNKNOWN: err=%v ctx=%v", err, ctx.Err())
			}
		})
	}
}

func TestCLIProcessStreamingPreservesCompleteOutput(t *testing.T) {
	p := cliProcessProvider(t, "healthy", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	content, err := collectCLIProcessStream(ctx, p)
	if err != nil || len(content) != (256<<10)+5 || !strings.HasSuffix(content, "tail\n") {
		t.Fatalf("concurrent Wait lost output: bytes=%d err=%v", len(content), err)
	}
}

func TestCLIProcessHealthProbeRejectsOutputOverflow(t *testing.T) {
	p := cliProcessProvider(t, "flood", "")
	p.binPath = os.Args[0]
	p.checkCmd = func(ctx context.Context) *exec.Cmd { return p.buildCmd(ctx, "") }
	if p.healthCheck() {
		t.Fatal("an output-flooding health probe was marked available")
	}
}

func TestCLIProcessOversizedLineCannotBlockWait(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "oversized-line")
	p := cliProcessProvider(t, "long-line", marker)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	completed := make(chan struct{})
	var err error
	go func() {
		_, err = collectCLIProcessStream(ctx, p)
		close(completed)
	}()
	defer func() {
		cancel()
		select {
		case <-completed:
		case <-time.After(2 * time.Second):
			t.Error("oversized-line fixture did not finish after cancellation")
		}
	}()
	if readyErr := waitCLIProcessFixtureReady(ctx, marker, completed); readyErr != nil {
		t.Fatalf("real unterminated prefix was not ready: %v", readyErr)
	}
	started := time.Now()
	if armErr := os.WriteFile(marker+".armed", []byte("release oversized tail"), 0o600); armErr != nil {
		t.Fatal(armErr)
	}
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("scanner failure blocked process cleanup after the prefix was ready")
	}
	if !errors.Is(err, execution.ErrUnknownOutcome) || !strings.Contains(err.Error(), "token too long") || ctx.Err() != nil || time.Since(started) > time.Second {
		t.Fatalf("scanner failure blocked process cleanup: err=%v elapsed=%v", err, time.Since(started))
	}
}

func TestCLIProcessStreamingQuietCancellationAndLeaderExit(t *testing.T) {
	for _, mode := range []string{"quiet", "leader"} {
		t.Run(mode, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "descendant")
			p := cliProcessProvider(t, mode, marker)
			ctx, cancel := context.WithTimeout(context.Background(), cliProcessFixtureStartupTimeout)
			completed := make(chan struct{})
			var err error
			go func() {
				_, err = collectCLIProcessStream(ctx, p)
				close(completed)
			}()
			defer func() {
				cancel()
				select {
				case <-completed:
				case <-time.After(2 * time.Second):
					t.Error("fixture stream did not complete after cancellation")
				}
			}()
			if readyErr := waitCLIProcessFixtureReady(ctx, marker, completed); readyErr != nil {
				t.Fatalf("real descendant was not started: %v", readyErr)
			}
			// Race-instrumented parent and child startup is outside the cleanup
			// bound. The marker proves a real descendant is now running.
			started := time.Now()
			if mode == "quiet" {
				cancel()
			}
			select {
			case <-completed:
			case <-time.After(1800 * time.Millisecond):
				t.Fatal("inherited pipes exceeded wait bound after readiness")
			}
			if time.Since(started) > 1800*time.Millisecond {
				t.Fatalf("inherited pipes exceeded wait bound: %v", err)
			}
			// Explicit cancellation closes this stream without an error chunk;
			// deadlines are surfaced separately by the quiet-only regression.
			if mode == "quiet" && (ctx.Err() != context.Canceled || err != nil) {
				t.Fatalf("silent stream lost its cancellation: err=%v ctx=%v", err, ctx.Err())
			}
			if mode == "leader" && err != nil && !errors.Is(err, execution.ErrUnknownOutcome) {
				t.Fatalf("incomplete descendant stdio was not UNKNOWN: %v", err)
			}
			remaining := time.Until(started.Add(2300 * time.Millisecond))
			if remaining > 0 {
				time.Sleep(remaining)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("descendant survived command completion: %v", err)
			}
		})
	}
}

func TestCLIProcessStreamingQuietDeadline(t *testing.T) {
	// One silent helper covers deadline propagation without requiring two
	// race-instrumented executables to initialize inside a 500ms deadline.
	p := cliProcessProvider(t, "quiet-only", "")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := collectCLIProcessStream(ctx, p)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 1800*time.Millisecond {
		t.Fatalf("silent stream lost its bounded deadline: err=%v elapsed=%v", err, time.Since(started))
	}
}

func TestCLIProcessStartFailureHonorsContextAndBudget(t *testing.T) {
	// These cases exercise the direct exec Start boundary. A bwrap wrapper
	// would start successfully and report its missing inner executable at Wait.
	t.Setenv("MAKEWAND_NO_BWRAP", "1")
	t.Setenv("MAKEWAND_RESTRICTED", "")
	t.Setenv("MAKEWAND_REQUIRE_BWRAP", "")
	for _, streaming := range []bool{false, true} {
		for _, mode := range []string{"canceled", "deadline", "missing"} {
			t.Run(fmt.Sprintf("stream=%t/%s", streaming, mode), func(t *testing.T) {
				base, ledger := budgetTestContext(t, 1)
				ctx, cancel := context.WithCancel(base)
				if mode == "deadline" {
					cancel()
					// Admission happens before the real deadline. The builder then
					// waits for expiry, separating Start failure from child startup.
					ctx, cancel = context.WithTimeout(base, 10*time.Second)
				}
				defer cancel()
				var command *exec.Cmd
				var commandContext context.Context
				builds := 0
				p := &CLIProvider{provider: "offline-fixture"}
				p.buildCmd = func(buildCtx context.Context, _ string) *exec.Cmd {
					builds++
					if attempts := budgetTestAttempts(t, ledger); len(attempts) != 1 || buildCtx.Err() != nil {
						t.Fatalf("context expired before its real reservation: attempts=%+v ctx=%v", attempts, buildCtx.Err())
					}
					executable := os.Args[0]
					switch mode {
					case "canceled":
						cancel()
					case "deadline":
						<-buildCtx.Done()
					case "missing":
						executable = filepath.Join(t.TempDir(), "missing-fixture.exe")
					}
					commandContext = buildCtx
					// #nosec G204 G702 -- executable is the trusted test binary or an intentionally absent private fixture; fixed child selector.
					command = exec.CommandContext(buildCtx, executable, "-test.run=^TestCLIProcessChild$")
					return command
				}
				p.buildStreamCmd = p.buildCmd
				var err error
				if streaming {
					var stream <-chan StreamChunk
					stream, err = p.ChatStream(ctx, []Message{{Role: "user", Content: "fixture"}}, "", 0)
					if stream != nil {
						t.Fatal("failed Start returned a live stream")
					}
				} else {
					_, _, err = p.Chat(ctx, []Message{{Role: "user", Content: "fixture"}}, "", 0)
				}
				status, kind, known := execution.InvalidRequest, ErrorKindConfig, true
				// The attempt ledger records a known provider failure; the
				// returned config error has the public INVALID_REQUEST status.
				ledgerStatus := execution.Failed
				var contextErr error
				switch mode {
				case "canceled":
					status, kind, known, contextErr = execution.Cancelled, ErrorKindCanceled, false, context.Canceled
					ledgerStatus = status
				case "deadline":
					status, kind, known, contextErr = execution.Timeout, ErrorKindTimeout, false, context.DeadlineExceeded
					ledgerStatus = status
				}
				attempts := budgetTestAttempts(t, ledger)
				if err == nil || ErrorKindOf(err) != kind || execution.ErrorStatus(err) != status || builds != 1 || command == nil || command.Process != nil || len(attempts) != 1 || attempts[0]["result_status"] != string(ledgerStatus) || attempts[0]["outcome_known"] != known {
					t.Fatalf("Start failure lost classification or charged twice: error=%v builds=%d command=%+v attempts=%+v", err, builds, command, attempts)
				}
				if contextErr != nil {
					if !errors.Is(err, contextErr) || !errors.Is(err, execution.ErrUnknownOutcome) {
						t.Fatalf("interrupted Start lost its context or conservative unknown: %v", err)
					}
				} else {
					if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, execution.ErrUnknownOutcome) || !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("ordinary missing executable became cancellation or unknown: %v", err)
					}
					if streaming && commandContext.Err() != context.Canceled {
						t.Fatal("missing stream fixture did not exercise its own cleanup cancellation")
					}
				}
			})
		}
	}
}

func waitCLIProcessFixtureReady(ctx context.Context, marker string, completed <-chan struct{}) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		// #nosec G703 -- parent-created temporary fixture path.
		if _, err := os.Stat(marker + ".ready"); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-completed:
			// Completion can race the initial Stat. Recheck so a fast leader
			// exit cannot hide the descendant's already-written ready marker.
			// #nosec G703 -- parent-created temporary fixture path.
			_, err := os.Stat(marker + ".ready")
			return err
		case <-ticker.C:
		}
	}
}

func cliProcessProvider(t *testing.T, mode, marker string) *CLIProvider {
	t.Helper()
	return &CLIProvider{provider: "offline-fixture", buildCmd: func(ctx context.Context, _ string) *exec.Cmd {
		// #nosec G204 G702 -- parent-controlled Go test executable and fixed helper selector.
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIProcessChild$")
		cmd.Env = append(os.Environ(), "MAKEWAND_CLI_PROCESS_CHILD="+mode, "MAKEWAND_CLI_PROCESS_MARKER="+marker, "GORACE=atexit_sleep_ms=0")
		return cmd
	}}
}

func collectCLIProcessStream(ctx context.Context, p *CLIProvider) (string, error) {
	stream, err := p.chatStreamUnaccounted(ctx, []Message{{Role: "user", Content: "fixture"}}, "", 0)
	if err != nil {
		return "", err
	}
	var content strings.Builder
	var streamErr error
	for chunk := range stream {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
		if content.Len()+len(chunk.Content) <= processjob.MaxOutputBytes+4096 {
			content.WriteString(chunk.Content)
		}
	}
	return content.String(), streamErr
}
