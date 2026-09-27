package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCandidateCannotReplaceBaselineRunner(t *testing.T) {
	p := newVerificationProject(t)
	report, err := p.EvaluateCandidateFiles(context.Background(), []ExtractedFile{
		{Path: "math.go", Content: "package verify\nfunc Add(a,b int) int { return 0 }\n"},
		{Path: "package.json", Content: `{"scripts":{"test":"true"}}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.TestsPlan == nil || report.TestsPlan.Command != "go" {
		t.Fatalf("baseline runner replaced: %+v", report.TestsPlan)
	}
	if report.DepsPlan == nil || report.DepsPlan.Command != "go" {
		t.Fatalf("baseline dependency runner replaced: %+v", report.DepsPlan)
	}
	if report.Passed || report.Strength != 0 {
		t.Fatalf("incorrect candidate accepted: %+v", report)
	}
}

func TestVerificationRejectsInputMutation(t *testing.T) {
	for _, change := range []struct{ name, source string }{
		{"source", `os.WriteFile("math.go", []byte("invalid Go after test"), 0600)`},
		{"permissions", `os.Chmod("math.go", 0755)`},
		{"new source", `os.WriteFile("unreviewed.go", []byte("package verify"), 0600)`},
		{"baseline test", `os.WriteFile("math_test.go", []byte("package verify"), 0600)`},
	} {
		t.Run(change.name, func(t *testing.T) {
			p := newVerificationProject(t)
			report, err := p.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{
				Path: "math.go", Content: "package verify\nimport \"os\"\nfunc Add(a,b int) int { _ = " + change.source + "; return a+b }\n",
			}})
			if err != nil {
				t.Fatal(err)
			}
			if report.Passed || report.Strength != 0 || report.IntegrityError == "" {
				t.Fatalf("mutated inputs accepted: %+v", report)
			}
			if len(report.VerifiedFiles) != 0 || report.VerifiedDigest != "" {
				t.Fatal("mutated inputs received a sealed payload")
			}
		})
	}
}

func TestCandidateOutputCannotAuthorizeAutomaticApplication(t *testing.T) {
	for _, source := range []string{
		// The original init/stdout bypass.
		`package verify
import ("fmt"; "os")
func Add(a,b int) int { return 0 }
func init() { fmt.Println("--- PASS: TestAdd (0.00s)"); os.Exit(0) }
`,
		// Output forgery from an ordinary function after the real test's run event.
		// test2json's own framing is also controlled by the candidate test process.
		`package verify
import ("fmt"; "os")
func Add(a,b int) int { fmt.Print("\x16--- PASS: TestAdd (0.00s)\n\x16PASS\n"); os.Exit(0); return 0 }
`,
	} {
		p := newVerificationProject(t)
		report, err := p.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{Path: "math.go", Content: source}})
		if err != nil {
			t.Fatal(err)
		}
		if report.Strength >= autopilotMinCandidateStrength {
			t.Fatalf("forged output authorized apply: %+v", report)
		}
	}
}

func TestGoEvidenceRequiresEveryBaselineTestAndPackageCompletion(t *testing.T) {
	expected := map[string]bool{"example/p\x00TestOne": true, "example/p\x00TestTwo": true}
	events := "{\"Action\":\"run\",\"Package\":\"example/p\",\"Test\":\"TestOne\"}\n" +
		"{\"Action\":\"pass\",\"Package\":\"example/p\",\"Test\":\"TestOne\"}\n"
	if goBaselineTestsPassed(expected, &ExecResult{Stdout: events}) {
		t.Fatal("partial baseline accepted")
	}
	events += strings.ReplaceAll(events, "TestOne", "TestTwo")
	if goBaselineTestsPassed(expected, &ExecResult{Stdout: events}) {
		t.Fatal("missing package completion accepted")
	}
	events += "{\"Action\":\"pass\",\"Package\":\"example/p\"}\n"
	if !goBaselineTestsPassed(expected, &ExecResult{Stdout: events}) {
		t.Fatal("complete diagnostic evidence rejected")
	}
}

func TestCandidateSealedPermissionsSurviveApplyAndRollback(t *testing.T) {
	p := newVerificationProject(t)
	path := filepath.Join(p.Path, "run.sh")
	//nolint:gosec // G306: this fixture must be executable to verify permission preservation or CLI availability.
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntrue\n"), 0755); err != nil {
		t.Fatal(err)
	}
	files := []ExtractedFile{{Path: "run.sh", Content: "#!/bin/sh\necho changed\n"}}
	report, err := p.EvaluateCandidateFiles(context.Background(), files)
	if err != nil || !report.Passed {
		t.Fatalf("verification: %v %+v", err, report)
	}
	if !report.VerifiedFiles[0].ModeKnown || report.VerifiedFiles[0].Mode != 0755 {
		t.Fatalf("mode not sealed: %+v", report.VerifiedFiles)
	}
	checkpoint, err := p.CheckpointFiles(report.VerifiedFiles)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpoint.Cleanup()
	if err := p.WriteFiles(report.VerifiedFiles); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0755 {
		t.Fatalf("apply mode=%o", info.Mode().Perm())
	}
	tampered := append([]ExtractedFile(nil), report.VerifiedFiles...)
	tampered[0].Mode = 0600
	if VerifiedFilesMatch(tampered, report.VerifiedDigest) {
		t.Fatal("permission mutation preserved attestation")
	}
	if err := checkpoint.Restore(); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(path)
	data, _ := os.ReadFile(path)
	if info.Mode().Perm() != 0755 || string(data) != "#!/bin/sh\ntrue\n" {
		t.Fatal("rollback did not restore content and mode")
	}
}
