package model

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/processjob"
	"github.com/makewand/makewand/internal/testfixture"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/router"
)

func writeFixtureCLI(t *testing.T, name string) string {
	t.Helper()
	bin := testfixture.WriteCLI(t, name, testfixture.Spec{Stdout: "fixture\n"})
	// Load the owned fixture once before production's bounded availability probe.
	// This reports real startup/version errors without changing policy or probe
	// timeouts; a first Windows PE load under race can include scanner work.
	warmCtx, warmCancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer warmCancel()
	warmCmd := exec.CommandContext(warmCtx, bin, "--version") // #nosec G204 G702 -- fixed argument to the test-owned native fixture.
	warmCapture := processjob.NewCapture(4096, func() { _ = processjob.Kill(warmCmd) })
	warmCmd.Stdout = warmCapture.Stdout()
	warmCmd.Stderr = warmCapture.Stderr()
	warmStarted := time.Now()
	warmCleanup, err := processjob.Start(warmCmd)
	if err != nil {
		t.Fatalf("owned fixture version start failed after %v: %v (context=%v)", time.Since(warmStarted), err, warmCtx.Err())
	}
	defer warmCleanup()
	warmErr := warmCmd.Wait()
	warmElapsed := time.Since(warmStarted)
	warmStdout, warmStderr := warmCapture.StdoutString(), warmCapture.StderrString()
	if warmErr != nil || warmCtx.Err() != nil || warmCapture.Exceeded() || warmStdout != "fixture\n" || warmStderr != "" {
		t.Fatalf("owned fixture version failed after %v: error=%v context=%v exceeded=%t stdout=%q stderr=%q", warmElapsed, warmErr, warmCtx.Err(), warmCapture.Exceeded(), warmStdout, warmStderr)
	}
	t.Logf("owned fixture --version exit=0 elapsed=%v stdout=%q stderr=%q; production availability probe unchanged", warmElapsed, warmStdout, warmStderr)
	warmCancel()
	return bin
}

// TestExplicitAPIAccessIsHonoredByDynamicFactories reproduces go-router#2: with
// claude_access/gemini_access = "api" (allow_paid + key), the adapter registered
// the API provider but the per-instance factories still returned the
// subscription CLI, so every mode-routed path (Route/RouteProvider/Chat/ChatBest)
// silently ran the CLI while accounting it as API.
func TestExplicitAPIAccessIsHonoredByDynamicFactories(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		cliName  string
		setup    func(cfg *config.Config)
	}{
		{
			name:     "claude_access=api with claude CLI installed",
			provider: "claude",
			cliName:  "claude",
			setup: func(cfg *config.Config) {
				cfg.ClaudeAccess = "api"
			},
		},
		{
			name:     "gemini_access=api with agy CLI installed",
			provider: "gemini",
			cliName:  "agy",
			setup: func(cfg *config.Config) {
				cfg.GeminiAccess = "api"
			},
		},
		{
			name:     "gemini_access=api with gemini CLI installed",
			provider: "gemini",
			cliName:  "gemini",
			setup: func(cfg *config.Config) {
				cfg.GeminiAccess = "api"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MAKEWAND_API_POLICY", "allow_paid")
			cfg := policyConfig(t)
			cfg.OpenAIAPIKey = ""
			cfg.UsageMode = "balanced"
			cfg.CLIs = []config.CLITool{{Name: tc.cliName, BinPath: writeFixtureCLI(t, tc.cliName)}}
			tc.setup(cfg)

			r, err := NewRouter(cfg)
			if err != nil {
				t.Fatal(err)
			}
			registered, err := r.Get(tc.provider)
			if err != nil {
				t.Fatalf("Get(%s): %v", tc.provider, err)
			}
			if _, isCLI := registered.(*router.CLIProvider); isCLI {
				t.Fatalf("Get(%s) = CLI provider, want the explicitly configured API provider", tc.provider)
			}

			for _, phase := range []router.BuildPhase{router.PhasePlan, router.PhaseCode, router.PhaseReview, router.PhaseFix} {
				res, err := r.RouteProvider(tc.provider, phase)
				if err != nil {
					t.Fatalf("RouteProvider(%s, %v): %v", tc.provider, phase, err)
				}
				if _, isCLI := res.Provider.(*router.CLIProvider); isCLI {
					t.Fatalf("RouteProvider(%s, %v) resolved %T (model %q); explicit %s_access=api must not run the subscription CLI",
						tc.provider, phase, res.Provider, res.ModelID, tc.provider)
				}
			}
			for _, mode := range []UsageMode{ModeFast, ModeBalanced, ModePower} {
				r.SetMode(mode)
				for _, task := range []router.TaskType{router.TaskAnalyze, router.TaskCode, router.TaskReview, router.TaskExplain, router.TaskFix} {
					res, err := r.Route(task)
					if err != nil {
						continue // a strategy may not include this provider at all
					}
					if res.Actual != tc.provider {
						continue
					}
					if _, isCLI := res.Provider.(*router.CLIProvider); isCLI {
						t.Fatalf("mode %v task %v routed %s to %T; explicit api access must be honored", mode, task, tc.provider, res.Provider)
					}
				}
			}
		})
	}
}

// TestSubscriptionAccessStillPrefersCLIInFactories guards the default: without an
// explicit api access setting the factories keep returning the subscription CLI.
func TestSubscriptionAccessStillPrefersCLIInFactories(t *testing.T) {
	t.Setenv("MAKEWAND_API_POLICY", "allow_paid")
	cfg := policyConfig(t)
	cfg.UsageMode = "balanced"
	cfg.CLIs = []config.CLITool{{Name: "claude", BinPath: writeFixtureCLI(t, "claude")}}
	r, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.RouteProvider("claude", router.PhaseCode)
	if err != nil {
		t.Fatalf("RouteProvider: %v", err)
	}
	if _, isCLI := res.Provider.(*router.CLIProvider); !isCLI {
		t.Fatalf("default access resolved %T, want the subscription CLI", res.Provider)
	}
}
