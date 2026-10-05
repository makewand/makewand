package main

import (
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/testfixture"
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
	script := testfixture.WriteCLI(t, "provider", testfixture.Spec{Mode: "echo-stdin", Stdout: "ok\n"})

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
	script := testfixture.WriteCLI(t, "provider", testfixture.Spec{Mode: "output", Stdout: "ok\n"})

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
