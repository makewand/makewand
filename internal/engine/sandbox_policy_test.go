package engine

import (
	"context"
	"strings"
	"testing"
)

func TestDetectPlans_FromPackageJSON(t *testing.T) {
	proj, err := NewProject("sandbox-policy", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := proj.WriteFile("package.json", `{"name":"x","scripts":{"test":"echo ok"}}`); err != nil {
		t.Fatalf("WriteFile(package.json): %v", err)
	}

	depsPlan, err := proj.DetectInstallPlan()
	if err != nil {
		t.Fatalf("DetectInstallPlan: %v", err)
	}
	if depsPlan == nil {
		t.Fatal("DetectInstallPlan: got nil plan")
	}
	if depsPlan.Command != "npm" || strings.Join(depsPlan.Args, " ") != "install --ignore-scripts" {
		t.Fatalf("deps plan = %+v, want npm install --ignore-scripts", depsPlan)
	}
	if got := depsPlan.DisplayCommand(); got != "npm install --ignore-scripts" {
		t.Fatalf("deps DisplayCommand = %q, want %q", got, "npm install --ignore-scripts")
	}

	testsPlan, err := proj.DetectTestPlan()
	if err != nil {
		t.Fatalf("DetectTestPlan: %v", err)
	}
	if testsPlan == nil {
		t.Fatal("DetectTestPlan: got nil plan")
	}
	if testsPlan.Command != "npm" || strings.Join(testsPlan.Args, " ") != "test" {
		t.Fatalf("tests plan = %+v, want npm test", testsPlan)
	}
}

func TestExecRestricted_BlocksDisallowedCommand(t *testing.T) {
	proj, err := NewProject("sandbox-policy", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	_, err = proj.ExecRestricted(context.Background(), "sh", "-c", "echo hi")
	if err == nil {
		t.Fatal("ExecRestricted should reject non-allowlisted command")
	}
	if !strings.Contains(err.Error(), "blocked by execution policy") {
		t.Fatalf("ExecRestricted error = %q, want blocked-by-policy message", err.Error())
	}
}

func TestSanitizeExecEnv_RemovesSensitiveVars(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"OPENAI_API_KEY=secret",
		"NPM_TOKEN=secret2",
		"CI=true",
	}
	out := sanitizeExecEnv(in)
	joined := strings.Join(out, "\n")

	if strings.Contains(joined, "OPENAI_API_KEY=") {
		t.Fatal("sanitizeExecEnv should remove OPENAI_API_KEY")
	}
	if strings.Contains(joined, "NPM_TOKEN=") {
		t.Fatal("sanitizeExecEnv should remove NPM_TOKEN")
	}
	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Fatal("sanitizeExecEnv should keep PATH")
	}
	if !strings.Contains(joined, "MAKEWAND_SANDBOX=1") {
		t.Fatal("sanitizeExecEnv should append MAKEWAND_SANDBOX=1")
	}
}
