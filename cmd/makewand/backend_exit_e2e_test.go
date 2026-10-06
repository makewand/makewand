package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/makewand/makewand/execution"
)

func TestE2ENativeNoBackendReportsExecutionFailure(t *testing.T) {
	if testing.Short() || runtime.GOOS == "windows" {
		t.Skip("POSIX offline native CLI fixture")
	}
	bin := buildMakewandBinary(t)
	for _, test := range []struct {
		name, configuration, message string
		arguments                    []string
		status                       execution.Status
	}{
		{"missing configuration", "", "no AI models", []string{"--print", "explain this fixture"}, execution.Unverified},
		{"disabled providers", `{"active_providers":[]}`, "no AI models", []string{"--print", "explain this fixture"}, execution.Unverified},
		{"implicit headless task", `{"active_providers":[]}`, "no AI models", []string{"explain this fixture"}, execution.Unverified},
		{"interactive chat unavailable", `{"active_providers":[]}`, "no AI models", []string{"chat"}, execution.Unverified},
		{"guided new project unavailable", `{"active_providers":[]}`, "no AI models", []string{"new"}, execution.Unverified},
		{"subscription policy excludes API", `{"claude_api_key":"fixture-key","api_policy":"subscription_only"}`, "no AI models", []string{"--print", "explain this fixture"}, execution.Unverified},
		{"invalid mode precedes backend check", `{"active_providers":[]}`, "invalid mode", []string{"--print", "--mode=nonsense", "explain this fixture"}, execution.InvalidRequest},
		{"invalid tier precedes backend check", `{"active_providers":[]}`, "invalid mode", []string{"--print", "--tier=nonsense", "explain this fixture"}, execution.InvalidRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfgDir := t.TempDir()
			if test.configuration != "" {
				if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(test.configuration), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(bin, test.arguments...) //nolint:gosec // G204: test-built binary with fixed fixture arguments.
			cmd.Env = backendFixtureEnv(cfgDir)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != test.status.ExitCode() {
				t.Fatalf("exit=%v, want %d; stdout=%q stderr=%q", err, test.status.ExitCode(), stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), test.message) {
				t.Fatalf("failure output: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if test.arguments[0] != "new" && strings.TrimSpace(stdout.String()) != "" {
				t.Fatalf("unexpected success output: %q", stdout.String())
			}
		})
	}
}

func TestE2ENativeCorruptDisabledPolicyNeverExecutesProvider(t *testing.T) {
	if testing.Short() || runtime.GOOS == "windows" {
		t.Skip("POSIX offline native CLI fixture")
	}
	bin := buildMakewandBinary(t)
	cfgDir := t.TempDir()
	sentinel := filepath.Join(cfgDir, "provider-executed")
	stub := filepath.Join(cfgDir, "claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf 'unexpected call\\n' >> \"$MAKEWAND_TEST_SENTINEL\"\nprintf 'unexpected response\\n'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stub, 0700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, configuration string
		status              execution.Status
	}{
		{"valid disabled policy", `{"active_providers":[],"enabled_providers":{"claude":false},"local_model_enabled":false}`, execution.Unverified},
		{"same policy corrupted", `{"active_providers":[],"enabled_providers":{"claude":false},"local_model_enabled":false}X`, execution.InvalidRequest},
		{"invalid enablement value", `{"enabled_providers":{"claude":"false"}}`, execution.InvalidRequest},
		{"invalid whitelist entry", `{"active_providers":["claude",3]}`, execution.InvalidRequest},
		{"invalid local authorization", `{"local_model_enabled":"false"}`, execution.InvalidRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(test.configuration), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin, "--print", "Explain this isolated fixture")
			cmd.Env = append(backendFixtureEnv(cfgDir), "MAKEWAND_TEST_SENTINEL="+sentinel)
			output, err := cmd.CombinedOutput()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != test.status.ExitCode() {
				t.Fatalf("exit=%v, want %d; output=%s", err, test.status.ExitCode(), output)
			}
			if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
				t.Fatalf("disabled provider was probed or dispatched: %v; output=%s", err, output)
			}
		})
	}
}

func backendFixtureEnv(cfgDir string) []string {
	var environment []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "MAKEWAND_") || strings.HasPrefix(key, "GIT_") ||
			strings.HasPrefix(key, "XDG_") || key == "HOME" || key == "PATH" ||
			strings.HasSuffix(key, "_API_KEY") || strings.HasSuffix(key, "_AUTH_TOKEN") {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, "HOME="+cfgDir, "MAKEWAND_CONFIG_DIR="+cfgDir, "PATH="+cfgDir)
}
