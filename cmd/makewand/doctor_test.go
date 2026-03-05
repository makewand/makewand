package main

import (
	"errors"
	"strings"
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

func TestClassifyProbeError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want doctorProbeClassification
	}{
		{
			name: "environment timeout",
			err:  errors.New("request timed out after 45s"),
			want: probeClassEnvironment,
		},
		{
			name: "environment permission",
			err:  errors.New("proxyconnect tcp: socket: operation not permitted"),
			want: probeClassEnvironment,
		},
		{
			name: "configuration missing",
			err:  errors.New("model provider \"codex\" is not available"),
			want: probeClassConfiguration,
		},
		{
			name: "provider internal",
			err:  errors.New("internal server error"),
			want: probeClassProvider,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyProbeError(tc.err)
			if got != tc.want {
				t.Fatalf("classifyProbeError(%q) = %q, want %q", tc.err.Error(), got, tc.want)
			}
		})
	}
}

func TestEvaluateProbeFailures_DowngradeEnvironmentToWarn(t *testing.T) {
	failures := []doctorProbeFailure{
		{
			provider: "codex",
			attempt:  1,
			class:    probeClassEnvironment,
			err:      errors.New("proxyconnect tcp: connection refused"),
		},
		{
			provider: "claude",
			attempt:  2,
			class:    probeClassEnvironment,
			err:      errors.New("deadline exceeded"),
		},
	}

	status, class, msg := evaluateProbeFailures(failures)
	if status != doctorWarn {
		t.Fatalf("status = %q, want %q", status, doctorWarn)
	}
	if class != probeClassEnvironment {
		t.Fatalf("class = %q, want %q", class, probeClassEnvironment)
	}
	assertContainsSubstring(t, msg, "environment issue")
	assertContainsSubstring(t, msg, "codex")
	assertContainsSubstring(t, msg, "claude")
}

func TestEvaluateProbeFailures_ProviderRemainsFail(t *testing.T) {
	failures := []doctorProbeFailure{
		{
			provider: "codex",
			attempt:  1,
			class:    probeClassProvider,
			err:      errors.New("unexpected malformed provider response"),
		},
	}

	status, class, msg := evaluateProbeFailures(failures)
	if status != doctorFail {
		t.Fatalf("status = %q, want %q", status, doctorFail)
	}
	if class != probeClassProvider {
		t.Fatalf("class = %q, want %q", class, probeClassProvider)
	}
	assertContainsSubstring(t, msg, "provider issue")
}

func TestCompactProbeError_ShortensMultilineNoise(t *testing.T) {
	err := errors.New("WARNING: stale tmp cleanup failed\nERROR: stream disconnected before completion\nstacktrace ...")
	got := compactProbeError(err)
	if got != "stream disconnected" {
		t.Fatalf("compactProbeError() = %q, want %q", got, "stream disconnected")
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

func assertContainsSubstring(t *testing.T, got string, wantSubstr string) {
	t.Helper()
	if !strings.Contains(got, wantSubstr) {
		t.Fatalf("string %q does not contain %q", got, wantSubstr)
	}
}
