package engine

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// TestRunRestrictedPlan_FailsClosedWithoutIsolation mirrors the verification
// path: with no bubblewrap isolation and no MAKEWAND_UNSAFE_HOST_EXEC opt-in,
// RunRestrictedPlan must NOT execute the command on the host.
func TestRunRestrictedPlan_FailsClosedWithoutIsolation(t *testing.T) {
	fakeMissingBwrap(t) // linux, no bwrap, unsafe opt-in disabled

	project, err := NewProject("restricted-failclosed", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	plan := ExecPlan{Kind: "tests", Command: "go", Args: []string{"test", "./..."}}
	result, err := project.RunRestrictedPlan(context.Background(), plan)
	if err == nil {
		t.Fatal("RunRestrictedPlan should fail closed without isolation or opt-in")
	}
	if result != nil {
		t.Fatalf("RunRestrictedPlan result = %+v, want nil (no host execution)", result)
	}
	if !strings.Contains(err.Error(), "MAKEWAND_UNSAFE_HOST_EXEC") {
		t.Fatalf("error = %q, want unsafe opt-in hint", err.Error())
	}
	if RestrictedExecAutoApprovable() {
		t.Fatal("RestrictedExecAutoApprovable() = true, want false without isolation")
	}
}

// TestRunRestrictedPlan_RunsOnHostWithOptIn confirms the explicit escape hatch
// still permits host execution, so the e2e/real-project paths keep working.
func TestRunRestrictedPlan_RunsOnHostWithOptIn(t *testing.T) {
	project := newVerificationProject(t) // sets MAKEWAND_UNSAFE_HOST_EXEC=1

	plan := ExecPlan{Kind: "tests", Command: "go", Args: []string{"test", "./..."}}
	result, err := project.RunRestrictedPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("RunRestrictedPlan with opt-in: %v", err)
	}
	if result == nil {
		t.Fatal("RunRestrictedPlan result = nil, want host execution result")
	}
	if result.ExitCode != 0 {
		t.Fatalf("RunRestrictedPlan exit = %d (stderr=%s), want 0", result.ExitCode, result.Stderr)
	}
}

func TestIsTrivialTestScript(t *testing.T) {
	tests := []struct {
		script string
		want   bool
	}{
		{"", true},
		{"   ", true},
		{"true", true},
		{"TRUE", true},
		{":", true},
		{"exit 0", true},
		{"/bin/true", true},
		{`echo "Error: no test specified" && exit 1`, true},
		{"echo no tests", true},
		{"true && true", true},
		{"jest", false},
		{"mocha", false},
		{"vitest run", false},
		{"npm run test:unit", false},
		{"echo running && jest", false},
		{"true && jest", false},
		{"node --test", false},
		// Control-flow aware: a real runner that never actually executes is trivial.
		{"true || jest", true},       // || short-circuits, jest never runs
		{"exit 0 && jest", true},     // exit terminates the script first
		{"exit 1 || jest", true},     // exit terminates regardless of status
		{"jest || true", false},      // jest runs first
		{"jest && echo done", false}, // jest runs first
		{"false || jest", false},     // false fails, || runs jest
		{"false && jest", true},      // false fails, && skips jest
		// Quoted operators are not control flow.
		{`echo "a && b"`, true},         // just an echo
		{`jest --grep "a || b"`, false}, // jest runs; || is quoted
	}
	for _, tt := range tests {
		if got := isTrivialTestScript(tt.script); got != tt.want {
			t.Errorf("isTrivialTestScript(%q) = %v, want %v", tt.script, got, tt.want)
		}
	}
}

func TestBaselineTrustedTests_NpmScript(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   bool
	}{
		{name: "trivial true script is not trusted", script: "true", want: false},
		{name: "echo placeholder is not trusted", script: `echo "Error: no test specified" && exit 1`, want: false},
		{name: "real runner is trusted", script: "jest", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project, err := NewProject("npm-trust", t.TempDir())
			if err != nil {
				t.Fatalf("NewProject: %v", err)
			}
			pkg := `{"name":"x","version":"1.0.0","scripts":{"test":"` + tc.script + `"}}`
			if err := project.WriteFile("package.json", pkg); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if err := project.ScanFiles(); err != nil {
				t.Fatalf("ScanFiles: %v", err)
			}
			if got := project.baselineTrustedTests(); got != tc.want {
				t.Fatalf("baselineTrustedTests() = %v, want %v for script %q", got, tc.want, tc.script)
			}
		})
	}
}

func TestRestoreBaselineNpmTestScript(t *testing.T) {
	baseline, err := NewProject("npm-baseline", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject(baseline): %v", err)
	}
	if err := baseline.WriteFile("package.json",
		`{"name":"x","version":"1.0.0","scripts":{"test":"jest"},"dependencies":{"left-pad":"1.0.0"}}`); err != nil {
		t.Fatalf("WriteFile(baseline): %v", err)
	}

	// The candidate weakens the test script but also adds a legitimate dependency.
	clone, err := NewProject("npm-clone", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject(clone): %v", err)
	}
	if err := clone.WriteFile("package.json",
		`{"name":"x","version":"1.0.0","scripts":{"test":"true"},"dependencies":{"left-pad":"1.0.0","lodash":"4.0.0"}}`); err != nil {
		t.Fatalf("WriteFile(clone): %v", err)
	}

	restored, err := baseline.restoreBaselineNpmTestScript(clone)
	if err != nil {
		t.Fatalf("restoreBaselineNpmTestScript: %v", err)
	}
	if restored != "package.json" {
		t.Fatalf("restored = %q, want package.json", restored)
	}

	patched, err := clone.ReadFile("package.json")
	if err != nil {
		t.Fatalf("ReadFile(clone): %v", err)
	}
	if got := packageJSONTestScript(patched); got != "jest" {
		t.Fatalf("clone test script = %q, want jest (baseline restored)", got)
	}
	// The candidate's other change (added dependency) must be preserved.
	if !strings.Contains(patched, "lodash") {
		t.Fatalf("patched package.json lost candidate dependency: %s", patched)
	}

	// A candidate that left the test script alone triggers no restore.
	same, err := NewProject("npm-same", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject(same): %v", err)
	}
	if err := same.WriteFile("package.json", `{"name":"x","scripts":{"test":"jest"}}`); err != nil {
		t.Fatalf("WriteFile(same): %v", err)
	}
	if restored, err := baseline.restoreBaselineNpmTestScript(same); err != nil || restored != "" {
		t.Fatalf("restoreBaselineNpmTestScript(unchanged) = (%q, %v), want (\"\", nil)", restored, err)
	}
}

// TestEvaluateCandidateFiles_NpmTrivialTestScriptCapsStrength exercises the full
// verification path end-to-end: a baseline whose only "test" is the always-true
// script `true` must not earn a Strength-2 pass.
func TestEvaluateCandidateFiles_NpmTrivialTestScriptCapsStrength(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not installed")
	}
	allowHostExecForTest(t)

	project, err := NewProject("npm-trivial", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := project.WriteFiles([]ExtractedFile{
		{Path: "package.json", Content: `{"name":"x","version":"1.0.0","scripts":{"test":"true"}}`},
		{Path: "index.js", Content: "module.exports = 1;\n"},
	}); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}
	if err := project.ScanFiles(); err != nil {
		t.Fatalf("ScanFiles: %v", err)
	}

	report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{
		Path:    "index.js",
		Content: "module.exports = 42;\n",
	}})
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if report.DepsError != "" {
		t.Skipf("npm install unavailable in this environment: %s", report.DepsError)
	}
	if !report.Passed {
		t.Fatalf("report.Passed = false, want true (tests error: %q)", report.TestsError)
	}
	if report.BaselineTests {
		t.Fatal("report.BaselineTests = true, want false for a trivial npm test script")
	}
	if report.Strength != 1 {
		t.Fatalf("report.Strength = %d, want 1 for a trivial npm test script", report.Strength)
	}
}
