package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/config"
)

func TestE2EPrintRoutesByClassifier(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("shell-based E2E fixtures are Unix-only")
	}

	bin := buildMakewandBinary(t)
	cfgDir := t.TempDir()
	script := writeProviderScript(t)

	cfg := config.DefaultConfig()
	cfg.DefaultModel = "default-provider"
	cfg.AnalysisModel = "planner"
	cfg.CodingModel = "coder"
	cfg.ReviewModel = "reviewer"
	cfg.CustomProviders = []config.CustomProvider{
		{Name: "default-provider", Command: script, Args: []string{"default-provider"}, Access: "subscription", PromptMode: config.CustomPromptModeStdin},
		{Name: "planner", Command: script, Args: []string{"planner"}, Access: "subscription", PromptMode: config.CustomPromptModeStdin},
		{Name: "coder", Command: script, Args: []string{"coder"}, Access: "subscription", PromptMode: config.CustomPromptModeStdin},
		{Name: "reviewer", Command: script, Args: []string{"reviewer"}, Access: "subscription", PromptMode: config.CustomPromptModeStdin},
	}
	writeTestConfig(t, cfgDir, cfg)

	tests := []struct {
		name         string
		prompt       string
		wantProvider string
	}{
		{name: "review with punctuation", prompt: "please review.", wantProvider: "reviewer"},
		{name: "fix with punctuation", prompt: "fix, please", wantProvider: "coder"},
		{name: "checkout stays code", prompt: "checkout the repo", wantProvider: "coder"},
		{name: "explain phrase", prompt: "how does this work", wantProvider: "planner"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, err := runMakewand(t, bin, cfgDir, "--print", "--timeout=5s", tt.prompt)
			if err != nil {
				t.Fatalf("runMakewand(%q) error = %v\nstdout:\n%s\nstderr:\n%s", tt.prompt, err, stdout, stderr)
			}
			if got := strings.TrimSpace(stdout); got != "provider:"+tt.wantProvider {
				t.Fatalf("stdout = %q, want %q", got, "provider:"+tt.wantProvider)
			}
			if !strings.Contains(stderr, "[makewand] provider="+tt.wantProvider) {
				t.Fatalf("stderr = %q, want provider marker for %q", stderr, tt.wantProvider)
			}
		})
	}
}

func TestE2EDoctorJSONIncludesPolicyChecks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("shell-based E2E fixtures are Unix-only")
	}

	bin := buildMakewandBinary(t)
	cfgDir := t.TempDir()
	script := writeProviderScript(t)

	cfg := config.DefaultConfig()
	cfg.CustomProviders = []config.CustomProvider{
		{Name: "custom", Command: script, Args: []string{"custom"}, Access: "subscription", PromptMode: config.CustomPromptModeStdin},
	}
	writeTestConfig(t, cfgDir, cfg)

	stdout, stderr, err := runMakewand(t, bin, cfgDir, "doctor", "--json", "--modes", "balanced")
	if err != nil {
		t.Fatalf("runMakewand(doctor --json) error = %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	report := mustParseDoctorJSON(t, stdout)
	assertContains(t, report.DetectedProviders, "custom (custom)")

	customCheck := requireDoctorCheck(t, report, "custom provider prompt safety")
	if customCheck.Status != string(doctorPass) {
		t.Fatalf("custom provider prompt safety status = %q, want %q", customCheck.Status, doctorPass)
	}

	if len(report.ModeCoverage) != 1 || report.ModeCoverage[0].Mode != "balanced" {
		t.Fatalf("mode coverage = %+v, want single balanced report", report.ModeCoverage)
	}
}

func TestE2ESetupAndDoctorWarnOnShellAdapterCustomProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("shell-based E2E fixtures are Unix-only")
	}

	bin := buildMakewandBinary(t)
	cfgDir := t.TempDir()

	cfg := config.DefaultConfig()
	cfg.CustomProviders = []config.CustomProvider{
		{
			Name:    "shelly",
			Command: "/bin/sh",
			Args:    []string{"-c", "printf 'ok\\n'", "{{prompt}}"},
			Access:  "subscription",
		},
	}
	writeTestConfig(t, cfgDir, cfg)

	stdout, stderr, err := runMakewand(t, bin, cfgDir, "setup")
	if err != nil {
		t.Fatalf("runMakewand(setup shell adapter) error = %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, `warning: shelly uses shell adapter "sh" with legacy prompt delivery; prefer prompt_mode="stdin" or remove the shell wrapper`) {
		t.Fatalf("setup stdout missing shell adapter warning:\n%s", stdout)
	}

	stdout, stderr, err = runMakewand(t, bin, cfgDir, "doctor", "--modes", "balanced")
	if err != nil {
		t.Fatalf("runMakewand(doctor shell adapter) error = %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, `[WARN] custom provider prompt safety - shelly uses shell adapter "sh" with legacy prompt delivery; prefer prompt_mode="stdin" or remove the shell wrapper`) {
		t.Fatalf("doctor stdout missing custom provider safety warning:\n%s", stdout)
	}
}

func TestE2EDoctorJSONWarnsOnShellAdapterCustomProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("shell-based E2E fixtures are Unix-only")
	}

	bin := buildMakewandBinary(t)
	cfgDir := t.TempDir()

	cfg := config.DefaultConfig()
	cfg.CustomProviders = []config.CustomProvider{
		{
			Name:    "shelly",
			Command: "/bin/sh",
			Args:    []string{"-c", "printf 'ok\\n'", "{{prompt}}"},
			Access:  "subscription",
		},
	}
	writeTestConfig(t, cfgDir, cfg)

	stdout, stderr, err := runMakewand(t, bin, cfgDir, "doctor", "--json", "--modes", "balanced")
	if err != nil {
		t.Fatalf("runMakewand(doctor --json shell adapter) error = %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	report := mustParseDoctorJSON(t, stdout)
	customCheck := requireDoctorCheck(t, report, "custom provider prompt safety")
	if customCheck.Status != string(doctorWarn) {
		t.Fatalf("custom provider prompt safety status = %q, want %q", customCheck.Status, doctorWarn)
	}
	if !containsAllStrings(customCheck.Details, `shell adapter "sh"`, `prompt_mode="stdin"`) {
		t.Fatalf("custom provider prompt safety details = %q", customCheck.Details)
	}
}

func buildMakewandBinary(t *testing.T) string {
	t.Helper()

	repoRoot := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "makewand")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/makewand")
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build makewand failed: %v\n%s", err, string(output))
	}
	return bin
}

func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func writeProviderScript(t *testing.T) string {
	t.Helper()

	script := filepath.Join(t.TempDir(), "provider.sh")
	body := "#!/bin/sh\n" +
		"provider=\"$1\"\n" +
		"printf 'provider:%s\\n' \"$provider\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(provider script): %v", err)
	}
	return script
}

func writeTestConfig(t *testing.T, cfgDir string, cfg *config.Config) {
	t.Helper()

	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(config dir): %v", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent(config): %v", err)
	}
	path := filepath.Join(cfgDir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(config): %v", err)
	}
}

func runMakewand(t *testing.T, bin, cfgDir string, args ...string) (string, string, error) {
	t.Helper()

	cmd := exec.Command(bin, args...)
	cmd.Env = isolatedCLIEnv(cfgDir)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func isolatedCLIEnv(cfgDir string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PATH=") || strings.HasPrefix(entry, "MAKEWAND_CONFIG_DIR=") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env,
		"MAKEWAND_CONFIG_DIR="+cfgDir,
		"PATH="+cfgDir,
	)
	return env
}

func stripANSIForTest(s string) string {
	var b strings.Builder
	inEscape := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !inEscape && ch == 0x1b {
			inEscape = true
			continue
		}
		if inEscape {
			if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || ch == '\\' {
				inEscape = false
			}
			continue
		}
		if ch == '\r' {
			continue
		}
		if ch < 0x20 && ch != '\n' && ch != '\t' {
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

type doctorJSONReport struct {
	DetectedProviders []string               `json:"detected_providers"`
	Checks            []doctorJSONCheck      `json:"checks"`
	ModeCoverage      []doctorJSONModeReport `json:"mode_coverage"`
}

type doctorJSONCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Details string `json:"details"`
}

type doctorJSONModeReport struct {
	Mode   string `json:"mode"`
	Status string `json:"status"`
}

func mustParseDoctorJSON(t *testing.T, stdout string) doctorJSONReport {
	t.Helper()

	var report doctorJSONReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("json.Unmarshal(doctor stdout) error = %v\nstdout:\n%s", err, stdout)
	}
	return report
}

func requireDoctorCheck(t *testing.T, report doctorJSONReport, name string) doctorJSONCheck {
	t.Helper()

	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("doctor report checks %+v do not contain %q", report.Checks, name)
	return doctorJSONCheck{}
}

func containsAllStrings(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
