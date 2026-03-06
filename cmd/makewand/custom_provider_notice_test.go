package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/config"
)

func TestCustomProviderSafetyWarning_ShellAdapterLegacy(t *testing.T) {
	cp := config.CustomProvider{
		Name:    "shelly",
		Command: "/bin/sh",
		Args:    []string{"-c", "printf ok\n", "{{prompt}}"},
	}

	got := customProviderSafetyWarning(cp)
	if got == "" || !containsAllStrings(got, "shell adapter", "prompt_mode=\"stdin\"") {
		t.Fatalf("customProviderSafetyWarning(shell adapter) = %q", got)
	}
}

func TestCustomProviderDoctorCheck_PassForStdin(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "provider.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.CustomProviders = []config.CustomProvider{
		{
			Name:       "private",
			Command:    script,
			PromptMode: config.CustomPromptModeStdin,
		},
	}

	check, ok := customProviderDoctorCheck(cfg)
	if !ok {
		t.Fatal("customProviderDoctorCheck() ok = false, want true")
	}
	if check.Status != doctorPass {
		t.Fatalf("status = %q, want %q", check.Status, doctorPass)
	}
	if !strings.Contains(check.Details, "stdin") {
		t.Fatalf("details = %q, want stdin guidance", check.Details)
	}
}

func TestCustomProviderDoctorCheck_WarnsForArgMode(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "provider.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.CustomProviders = []config.CustomProvider{
		{
			Name:       "private",
			Command:    script,
			PromptMode: config.CustomPromptModeArg,
		},
	}

	check, ok := customProviderDoctorCheck(cfg)
	if !ok {
		t.Fatal("customProviderDoctorCheck() ok = false, want true")
	}
	if check.Status != doctorWarn {
		t.Fatalf("status = %q, want %q", check.Status, doctorWarn)
	}
	if !containsAllStrings(check.Details, "argv", "prompt_mode=\"stdin\"") {
		t.Fatalf("details = %q", check.Details)
	}
}
