package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/config"
)

// An arbitrary CLI exit cannot prove the remote outcome. Keep its charge
// and stop automatic replay, even when its text resembles a quota refusal.
func TestE2ENativeUnverifiedProviderFailureExit17(t *testing.T) {
	if testing.Short() || runtime.GOOS == "windows" {
		t.Skip("POSIX offline native CLI fixture")
	}
	for _, name := range []string{"MAKEWAND_CALL_BUDGET_FILE", "MAKEWAND_MAX_MODEL_CALLS", "MAKEWAND_EXECUTION_EVENTS_FILE", "MAKEWAND_TASK_ID"} {
		t.Setenv(name, "")
	}
	bin := buildMakewandBinary(t)
	for _, message := range []string{"offline quota exceeded", "synthetic provider refusal"} {
		t.Run(message, func(t *testing.T) {
			cfgDir := t.TempDir()
			script := filepath.Join(cfgDir, "reject.sh")
			body := "#!/bin/sh\necho '" + message + "' >&2\nexit 1\n"
			if err := os.WriteFile(script, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(script, 0700); err != nil {
				t.Fatal(err)
			}
			cfg := config.DefaultConfig()
			cfg.DefaultModel, cfg.CodingModel, cfg.ReviewModel = "synthetic", "synthetic", "synthetic"
			cfg.CustomProviders = []config.CustomProvider{{Name: "synthetic", Command: script, Access: "subscription", PromptMode: config.CustomPromptModeStdin}}
			writeTestConfig(t, cfgDir, cfg)
			ledgerPath := filepath.Join(cfgDir, "ledger.json")
			stdout, stderr, err := runMakewand(t, bin, cfgDir, "--print", "--timeout=5s", "--max-model-calls=10", "--call-budget-file="+ledgerPath, "explain this synthetic fixture")
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 17 {
				t.Fatalf("native exit=%v, want 17; stdout=%s stderr=%s", err, stdout, stderr)
			}
			if !strings.Contains(stderr, message) || strings.TrimSpace(stdout) != "" {
				t.Fatalf("CLI diagnostic lost: stdout=%q stderr=%q", stdout, stderr)
			}
			data, readErr := os.ReadFile(ledgerPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var ledger struct {
				Attempts []struct {
					Status string `json:"result_status"`
					Known  bool   `json:"outcome_known"`
				} `json:"attempts"`
			}
			if err := json.Unmarshal(data, &ledger); err != nil || len(ledger.Attempts) != 1 {
				t.Fatalf("missing accounted provider rejection: %s; %v", data, err)
			}
			for _, attempt := range ledger.Attempts {
				if attempt.Status != "UNKNOWN" || attempt.Known {
					t.Fatalf("unknown outcome accounting=%+v", attempt)
				}
			}
		})
	}
}
