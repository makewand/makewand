package main

import (
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
)

func TestParseDoctorModes_All(t *testing.T) {
	got, err := parseDoctorModes("all")
	if err != nil {
		t.Fatalf("parseDoctorModes(all) error = %v", err)
	}
	want := []model.UsageMode{model.ModeFree, model.ModeEconomy, model.ModeBalanced, model.ModePower}
	if len(got) != len(want) {
		t.Fatalf("len(modes) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mode[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestParseDoctorModes_CommaListDeduped(t *testing.T) {
	got, err := parseDoctorModes("balanced,power,balanced")
	if err != nil {
		t.Fatalf("parseDoctorModes(list) error = %v", err)
	}
	want := []model.UsageMode{model.ModeBalanced, model.ModePower}
	if len(got) != len(want) {
		t.Fatalf("len(modes) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mode[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestParseDoctorModes_Invalid(t *testing.T) {
	if _, err := parseDoctorModes("balanced,invalid"); err == nil {
		t.Fatal("parseDoctorModes(invalid) error = nil, want non-nil")
	}
}

func TestDetectConfiguredProviders(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CLIs = []config.CLITool{
		{Name: "claude"},
		{Name: "codex"},
	}
	cfg.GeminiAPIKey = "test-key"
	cfg.OllamaURL = "http://localhost:11434"

	got := detectConfiguredProviders(cfg)
	if len(got) == 0 {
		t.Fatal("detectConfiguredProviders() returned empty list")
	}
	assertContains(t, got, "claude (cli)")
	assertContains(t, got, "codex (cli)")
	assertContains(t, got, "gemini (api)")
	assertContains(t, got, "ollama")
}

func TestUniqueProbeProviders(t *testing.T) {
	routes := []doctorTaskRoute{
		{Task: "analyze", Provider: "claude"},
		{Task: "code", Provider: "codex"},
		{Task: "review", Provider: "claude"},
		{Task: "fix", Provider: ""},
	}
	got := uniqueProbeProviders(routes)
	want := []string{"claude", "codex"}
	if len(got) != len(want) {
		t.Fatalf("len(uniqueProbeProviders) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("provider[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func assertContains(t *testing.T, list []string, want string) {
	t.Helper()
	for _, v := range list {
		if v == want {
			return
		}
	}
	t.Fatalf("list %v does not contain %q", list, want)
}
