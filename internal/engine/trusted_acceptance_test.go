package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/model"
)

func stdioAcceptanceSpec() TrustedAcceptanceSpec {
	command := "python3"
	if runtime.GOOS == "windows" {
		command = "python"
	}
	return TrustedAcceptanceSpec{Schema: 1, Command: command, Args: []string{"-I", "main.py"}, Cases: []TrustedAcceptanceCase{
		{Name: "positive", Stdin: "2 3\n", Stdout: "5\n"},
		{Name: "negative", Stdin: "-7 2\n", Stdout: "-5\n"},
	}}
}

func acceptanceProject(t *testing.T) *Project {
	t.Helper()
	project, err := NewProject("acceptance", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(project.Path, "main.py"), "print('baseline')\n")
	writeFile(t, filepath.Join(project.Path, "unchanged.txt"), "baseline execution input\n")
	return project
}

func writeAcceptancePolicy(t *testing.T, directory string, spec TrustedAcceptanceSpec) string {
	t.Helper()
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "acceptance.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func requireAcceptancePython(t *testing.T) {
	t.Helper()
	if testing.Short() && os.Getenv("MAKEWAND_REQUIRE_BWRAP") == "1" {
		t.Fatal("required trusted acceptance isolation cannot be skipped with -short")
	}
	requireLiveBwrap(t)
	if _, err := exec.LookPath("python3"); err != nil {
		if os.Getenv("MAKEWAND_REQUIRE_BWRAP") == "1" {
			t.Fatal("required trusted acceptance runtime python3 is not installed")
		}
		t.Skip("python3 is not installed")
	}
}

func TestTrustedAcceptanceSDKFreezesPolicySlices(t *testing.T) {
	spec := stdioAcceptanceSpec()
	ctx := ContextWithTrustedAcceptance(context.Background(), spec)
	spec.Args[1] = "attacker.py"
	spec.Cases[0].Stdout = "forged"
	state := acceptancePolicy(ctx)
	if state == nil || state.spec.Args[1] != "main.py" || state.spec.Cases[0].Stdout != "5\n" {
		t.Fatalf("SDK policy was not frozen: %+v", state)
	}
	ctx, err := prepareTrustedAcceptance(ctx, nil)
	if err != nil || acceptancePolicy(ctx) != state {
		t.Fatalf("frozen SDK state changed: %v", err)
	}
}

func TestTrustedAcceptanceMalformedSpecsAreRejected(t *testing.T) {
	cases := []struct {
		name   string
		change func(*TrustedAcceptanceSpec)
	}{
		{"unknown schema", func(s *TrustedAcceptanceSpec) { s.Schema = 2 }},
		{"command path", func(s *TrustedAcceptanceSpec) { s.Command = "/usr/bin/python3" }},
		{"unknown command", func(s *TrustedAcceptanceSpec) { s.Command = "sh" }},
		{"no cases", func(s *TrustedAcceptanceSpec) { s.Cases = nil }},
		{"duplicate cases", func(s *TrustedAcceptanceSpec) { s.Cases[1].Name = s.Cases[0].Name }},
		{"empty name", func(s *TrustedAcceptanceSpec) { s.Cases[0].Name = "" }},
		{"invalid timeout", func(s *TrustedAcceptanceSpec) { s.Timeout = "-1s" }},
		{"excessive timeout", func(s *TrustedAcceptanceSpec) { s.Timeout = "24h" }},
		{"negative output limit", func(s *TrustedAcceptanceSpec) { s.MaxOutputBytes = -1 }},
		{"expected output too big", func(s *TrustedAcceptanceSpec) { s.MaxOutputBytes = 1 }},
		{"invalid exit code", func(s *TrustedAcceptanceSpec) { s.Cases[0].ExitCode = -1 }},
		{"oversized policy", func(s *TrustedAcceptanceSpec) { s.Cases[0].Stdin = strings.Repeat("x", trustedAcceptanceMaxSpecBytes) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := stdioAcceptanceSpec()
			tc.change(&spec)
			ctx := ContextWithTrustedAcceptance(context.Background(), spec)
			if _, err := prepareTrustedAcceptance(ctx, nil); err == nil {
				t.Fatal("malformed policy was accepted")
			}
		})
	}
}

func TestTrustedAcceptanceEnvironmentMustBeOutsideProject(t *testing.T) {
	project := acceptanceProject(t)
	path := writeAcceptancePolicy(t, project.Path, stdioAcceptanceSpec())
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", path)
	if _, err := prepareTrustedAcceptance(context.Background(), project); err == nil || !strings.Contains(err.Error(), "outside the project") {
		t.Fatalf("project-controlled policy was accepted: %v", err)
	}
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", "acceptance.json")
	if _, err := prepareTrustedAcceptance(context.Background(), project); err == nil {
		t.Fatal("relative policy path was accepted")
	}
}

func TestTrustedAcceptanceEnvironmentRejectsMalformedAndSymlinkFiles(t *testing.T) {
	project := acceptanceProject(t)
	outside := t.TempDir()
	path := writeAcceptancePolicy(t, outside, stdioAcceptanceSpec())
	for _, suffix := range []string{" {}", "\nnull", ",", "\ntrue"} {
		t.Run(fmt.Sprintf("trailing %q", suffix), func(t *testing.T) {
			data, err := json.Marshal(stdioAcceptanceSpec())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(data, []byte(suffix)...), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", path)
			if _, err := prepareTrustedAcceptance(context.Background(), project); err == nil {
				t.Fatal("trailing policy object was accepted")
			}
		})
	}
	link := filepath.Join(outside, "link.json")
	if err := os.Symlink(path, link); err == nil {
		t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", link)
		if _, err := prepareTrustedAcceptance(context.Background(), project); err == nil {
			t.Fatal("symlink policy was accepted")
		}
	}
}

func TestTrustedAcceptanceAbsentPolicyDoesNotBecomeCandidateEnvironmentPolicy(t *testing.T) {
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", "")
	ctx, err := prepareTrustedAcceptance(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// A candidate changing the process environment after admission cannot
	// activate its own policy for a later verification step.
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", filepath.Join(t.TempDir(), "attacker.json"))
	ctx, err = prepareTrustedAcceptance(ctx, nil)
	if err != nil || acceptancePolicy(ctx) != nil {
		t.Fatalf("candidate environment activated policy: %v", err)
	}
}

func TestTrustedAcceptanceStrictInputsIncludeCacheDirectories(t *testing.T) {
	project := acceptanceProject(t)
	for _, name := range []string{"__pycache__", "node_modules", "target", ".pytest_cache"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(project.Path, name, "source.py")
			writeFile(t, path, "first")
			first, err := AcceptanceInputDigest(project.Path)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, path, "second")
			second, err := AcceptanceInputDigest(project.Path)
			if err != nil || first == second {
				t.Fatalf("execution input %q was ignored: %v", path, err)
			}
		})
	}
}

func TestTrustedAcceptanceInputSymlinksFailClosed(t *testing.T) {
	project := acceptanceProject(t)
	if err := os.Symlink(filepath.Join(project.Path, "main.py"), filepath.Join(project.Path, "link.py")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := AcceptanceInputDigest(project.Path); err == nil {
		t.Fatal("unsealed symlink input was accepted")
	}
}

func TestTrustedAcceptanceWrapperProtectsAssetsAndWorkspace(t *testing.T) {
	policy, err := freezeTrustedAcceptance(stdioAcceptanceSpec(), "/opt/private/acceptance.json")
	if err != nil {
		t.Fatal(err)
	}
	command, args := wrapTrustedAcceptanceCommand("/usr/bin/bwrap", "/tmp/candidate", policy)
	if command != "/usr/bin/bwrap" || !containsArg(args, "--unshare-pid") || !containsArg(args, "--unshare-net") || !containsArg(args, "--new-session") {
		t.Fatalf("independent namespaces missing: %v", args)
	}
	if !containsArgPair(args, "--ro-bind", "/tmp/candidate") {
		t.Fatalf("candidate input tree is not read-only: %v", args)
	}
	if !containsArgPair(args, "--ro-bind", os.DevNull) || !containsArg(args, "/opt/private/acceptance.json") {
		t.Fatalf("trusted acceptance file is visible: %v", args)
	}
	for _, forbidden := range []string{"5\n", "-5\n", "MAKEWAND_TRUSTED_ACCEPTANCE_FILE"} {
		if containsArg(args, forbidden) {
			t.Fatalf("oracle policy leaked to the child: %q", forbidden)
		}
	}
}

func TestTrustedAcceptanceUnsafeHostNeverGrantsStrengthTwo(t *testing.T) {
	project := acceptanceProject(t)
	allowHostExecForTest(t)
	project.SetUnsafeHostExecAuthorization(UnsafeHostExecAuthorization{Acknowledged: true, Source: "test"})
	ctx := ContextWithTrustedAcceptance(context.Background(), stdioAcceptanceSpec())
	// Resolve is instructed to use acknowledged unsafe host execution for local
	// diagnostics. Independent acceptance must reject that authorization.
	before, err := AcceptanceInputDigest(project.Path)
	if err != nil {
		t.Fatal(err)
	}
	originalGOOS := verifyGOOS
	verifyGOOS = "windows"
	t.Cleanup(func() { verifyGOOS = originalGOOS })
	report := CandidateVerification{Passed: true, Strength: 1}
	project.applyTrustedAcceptance(ctx, &report, before, before, []ExtractedFile{{Path: "main.py", Content: "print(1)"}})
	if report.Passed || report.Strength == 2 || report.Acceptance != nil || report.AcceptanceError == "" || !report.EnvironmentError {
		t.Fatalf("unsafe host obtained independent authority: %+v", report)
	}
}

func TestLiveTrustedAcceptanceIndependentBehaviorAndBinding(t *testing.T) {
	requireAcceptancePython(t)
	project := acceptanceProject(t)
	ctx := ContextWithTrustedAcceptance(context.Background(), stdioAcceptanceSpec())
	candidate := []ExtractedFile{{Path: "main.py", Content: "import sys\nprint(sum(map(int, sys.stdin.read().split())))\n"}}
	report, err := project.EvaluateCandidateFiles(ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.Strength != 2 || report.Acceptance == nil || !report.Isolated || report.NoTestsRan {
		t.Fatalf("independent behavior did not pass: %+v", report)
	}
	if report.Acceptance.CasesPassed != 2 || report.Acceptance.ArtifactDigest != report.VerifiedDigest || report.Acceptance.WorkspaceDigest == "" || report.Acceptance.BaselineDigest == "" {
		t.Fatalf("incomplete acceptance binding: %+v", report.Acceptance)
	}
	if err := project.ValidateTrustedAcceptance(ctx, report.VerifiedFiles, report.Acceptance); err != nil {
		t.Fatalf("valid approval cannot be applied: %v", err)
	}
	if noTestsExecuted(CandidateAttempt{Verification: report}) {
		t.Fatal("independent behavioral cases were reported as no tests")
	}
	writeFile(t, filepath.Join(project.Path, "unchanged.txt"), "unreviewed outside-patch mutation")
	if err := project.ValidateTrustedAcceptance(ctx, report.VerifiedFiles, report.Acceptance); err == nil {
		t.Fatal("changed full baseline retained automatic approval")
	}
}

func TestLiveTrustedAcceptanceCannotImportOrMutateAuthority(t *testing.T) {
	requireAcceptancePython(t)
	project := acceptanceProject(t)
	ctx := ContextWithTrustedAcceptance(context.Background(), stdioAcceptanceSpec())
	report, err := project.EvaluateCandidateFiles(ctx, []ExtractedFile{{Path: "main.py", Content: "import sys\nprint(sum(map(int, sys.stdin.read().split())))\n"}})
	if err != nil || report.Acceptance == nil {
		t.Fatalf("valid acceptance unavailable: %v %+v", err, report)
	}
	data, err := json.Marshal(report.Acceptance)
	if err != nil {
		t.Fatal(err)
	}
	var imported TrustedAcceptanceRecord
	if err := json.Unmarshal(data, &imported); err != nil {
		t.Fatal(err)
	}
	if err := project.ValidateTrustedAcceptance(ctx, report.VerifiedFiles, &imported); err == nil {
		t.Fatal("JSON/self-signed certificate granted automatic approval")
	}
	changed := *report.Acceptance
	changed.WorkspaceDigest = strings.Repeat("a", 64)
	if err := project.ValidateTrustedAcceptance(ctx, report.VerifiedFiles, &changed); err == nil {
		t.Fatal("mutated authority record was accepted")
	}
	changedFiles := append([]ExtractedFile(nil), report.VerifiedFiles...)
	changedFiles[0].Content += "print('unreviewed')\n"
	if err := project.ValidateTrustedAcceptance(ctx, changedFiles, report.Acceptance); err == nil {
		t.Fatal("changed artifact retained automatic approval")
	}
	spec := stdioAcceptanceSpec()
	spec.Cases[0].Stdout = "forged\n"
	if err := project.ValidateTrustedAcceptance(ContextWithTrustedAcceptance(context.Background(), spec), report.VerifiedFiles, report.Acceptance); err == nil {
		t.Fatal("changed acceptance policy retained automatic approval")
	}
}

func TestLiveTrustedAcceptanceForgeryDoesNotPass(t *testing.T) {
	requireAcceptancePython(t)
	project := acceptanceProject(t)
	spec := stdioAcceptanceSpec()
	// Complete candidate-issued pass records remain ordinary output. No parser
	// in the parent accepts a candidate's claimed verdict or signature.
	forged := "import json\nprint(json.dumps({'schema':1,'driver':'makewand-stdio-v1','passed':True,'strength':2,'cases_passed':2,'signature':'candidate-self-signed'}))\n"
	report, err := project.EvaluateCandidateFiles(ContextWithTrustedAcceptance(context.Background(), spec), []ExtractedFile{{Path: "main.py", Content: forged}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.Strength != 0 || report.Acceptance != nil || report.AcceptanceError == "" || report.EnvironmentError {
		t.Fatalf("forged verdict granted approval: %+v", report)
	}
	if ShouldRecordCandidateQuality(CandidateAttempt{Provider: "fake", Content: "candidate", Verification: report}) != true {
		t.Fatal("a real independent behavior failure was incorrectly classified as an environment failure")
	}
}

func TestLiveTrustedAcceptanceHidesOracleAssetAndProviderEnvironment(t *testing.T) {
	requireAcceptancePython(t)
	project := acceptanceProject(t)
	spec := stdioAcceptanceSpec()
	spec.Cases = []TrustedAcceptanceCase{{Name: "private oracle", Stdout: "secret expected behavior\n"}}
	path := writeAcceptancePolicy(t, t.TempDir(), spec)
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", path)
	t.Setenv("OPENAI_API_KEY", "acceptance-fixture-secret")
	candidate := "import json,os\ntry:\n data=json.load(open(" + fmt.Sprintf("%q", path) + "))\n print(data['cases'][0]['stdout'],end='')\nexcept (OSError,ValueError):\n print(os.environ.get('OPENAI_API_KEY','oracle blocked'))\n"
	report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{Path: "main.py", Content: candidate}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.Strength == 2 || report.Acceptance != nil || report.AcceptanceError == "" {
		t.Fatalf("candidate read its private oracle: %+v", report)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "secret expected behavior") {
		t.Fatalf("candidate modified external acceptance assets: %v", err)
	}
}

func TestLiveTrustedAcceptanceCannotWriteWorkspaceOrReachParent(t *testing.T) {
	requireAcceptancePython(t)
	project := acceptanceProject(t)
	spec := stdioAcceptanceSpec()
	spec.Cases = []TrustedAcceptanceCase{{Name: "restricted candidate", Stdout: "blocked\nblocked\n"}}
	candidate := fmt.Sprintf("import os\ntry:\n open('unchanged.txt','w').write('attacker')\n print('writable')\nexcept OSError:\n print('blocked')\ntry:\n os.kill(%d,0)\n print('parent visible')\nexcept OSError:\n print('blocked')\n", os.Getpid())
	report, err := project.EvaluateCandidateFiles(ContextWithTrustedAcceptance(context.Background(), spec), []ExtractedFile{{Path: "main.py", Content: candidate}})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.Strength != 2 {
		t.Fatalf("namespace/workspace boundary did not hold: %+v", report)
	}
	content, err := os.ReadFile(filepath.Join(project.Path, "unchanged.txt"))
	if err != nil || string(content) != "baseline execution input\n" {
		t.Fatalf("candidate changed original project: %v %q", err, content)
	}
}

func TestLiveTrustedAcceptanceDeadlineAndOutputLimit(t *testing.T) {
	requireAcceptancePython(t)
	for _, tc := range []struct {
		name   string
		source string
		limit  int
	}{
		{"deadline", "import time\ntime.sleep(60)\n", 64},
		{"output limit", "print('x'*1000)\n", 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := acceptanceProject(t)
			spec := stdioAcceptanceSpec()
			spec.Timeout = "300ms"
			spec.MaxOutputBytes = tc.limit
			started := time.Now()
			report, err := project.EvaluateCandidateFiles(ContextWithTrustedAcceptance(context.Background(), spec), []ExtractedFile{{Path: "main.py", Content: tc.source}})
			if err != nil {
				t.Fatal(err)
			}
			if report.Passed || report.Strength == 2 || report.Acceptance != nil || report.AcceptanceError == "" {
				t.Fatalf("incomplete execution became acceptance: %+v", report)
			}
			if time.Since(started) > 10*time.Second {
				t.Fatal("deadline did not terminate isolated candidate")
			}
		})
	}
}

func TestTrustedAcceptanceRejectsUndeliveredExecutionInputs(t *testing.T) {
	project := acceptanceProject(t)
	ctx := ContextWithTrustedAcceptance(context.Background(), stdioAcceptanceSpec())
	before, err := AcceptanceInputDigest(project.Path)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(project.Path, "__pycache__", "actual_source.py"), "print('hidden test-only behavior')")
	report := CandidateVerification{Passed: true, Strength: 1, Isolated: true}
	project.applyTrustedAcceptance(ctx, &report, before, before, []ExtractedFile{{Path: "main.py", Content: "print('candidate')"}})
	if report.Passed || report.Strength == 2 || report.Acceptance != nil || !strings.Contains(report.AcceptanceError, "execution inputs") {
		t.Fatalf("undeclared generated source became accepted: %+v", report)
	}
}

type acceptanceFixtureProvider struct {
	calls  int
	before func()
}

func (p *acceptanceFixtureProvider) Name() string      { return "claude" }
func (p *acceptanceFixtureProvider) IsAvailable() bool { return true }
func (p *acceptanceFixtureProvider) Chat(context.Context, []model.Message, string, int) (string, model.Usage, error) {
	p.calls++
	if p.before != nil {
		p.before()
	}
	return RenderExtractedFiles([]ExtractedFile{{Path: "main.py", Content: "import sys\nprint(sum(map(int, sys.stdin.read().split())))\n"}}), model.Usage{Provider: p.Name()}, nil
}
func (p *acceptanceFixtureProvider) ChatStream(context.Context, []model.Message, string, int) (<-chan model.StreamChunk, error) {
	return nil, errors.New("fixture streaming is not used")
}

func acceptanceFixtureRouter(t *testing.T, provider *acceptanceFixtureProvider) *model.Router {
	t.Helper()
	router, err := model.NewRouterFromConfig(model.RouterConfig{Providers: map[string]model.ProviderEntry{"claude": {Provider: provider}}, UsageMode: "balanced"})
	if err != nil {
		t.Fatal(err)
	}
	router.SetMode(model.ModeBalanced)
	return router
}

func TestTrustedAcceptanceInvalidPolicyStopsBeforeProviderCall(t *testing.T) {
	project := acceptanceProject(t)
	path := writeAcceptancePolicy(t, project.Path, stdioAcceptanceSpec())
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", path)
	provider := &acceptanceFixtureProvider{}
	selection := RunCandidateSelection(context.Background(), acceptanceFixtureRouter(t, provider), project, model.PhaseCode, []model.Message{{Role: "user", Content: "fixture"}}, "fixture", nil)
	if provider.calls != 0 || selection.Err == nil || selection.Status != execution.InvalidRequest || selection.Verified {
		t.Fatalf("invalid policy consumed a provider call: calls=%d selection=%+v", provider.calls, selection)
	}
}

func TestLiveTrustedAcceptanceCandidateSelectionFreezesBeforeGeneration(t *testing.T) {
	requireAcceptancePython(t)
	project := acceptanceProject(t)
	path := writeAcceptancePolicy(t, t.TempDir(), stdioAcceptanceSpec())
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", path)
	provider := &acceptanceFixtureProvider{before: func() {
		// Even a trusted provider adapter accidentally rewriting policy cannot
		// replace the pre-generation contract in the parent context.
		spec := stdioAcceptanceSpec()
		spec.Cases[0].Stdout = "candidate replacement oracle\n"
		writeAcceptancePolicy(t, filepath.Dir(path), spec)
	}}
	selection := RunCandidateSelection(context.Background(), acceptanceFixtureRouter(t, provider), project, model.PhaseCode, []model.Message{{Role: "user", Content: "fixture"}}, "fixture", nil)
	if provider.calls != 1 || !selection.Verified || selection.Strength != 2 || selection.Acceptance == nil || selection.Status != execution.Passed || selection.NoTestsExecuted {
		t.Fatalf("trusted selection did not use pre-generation policy: calls=%d selection=%+v", provider.calls, selection)
	}
	if len(selection.Attempts) != 1 || selection.Attempts[0].Acceptance == nil {
		t.Fatalf("attempt lost the artifact-bound acceptance record: %+v", selection.Attempts)
	}
	// Application independently reloads the user's active policy and detects
	// its later modification rather than blindly trusting an old certificate.
	if err := project.ValidateTrustedAcceptance(context.Background(), selection.VerifiedFiles, selection.Acceptance); err == nil {
		t.Fatal("changed policy remained auto-applicable")
	}
}

func TestTrustedAcceptanceEnvironmentRejectsHardlinkedOracle(t *testing.T) {
	project := acceptanceProject(t)
	path := writeAcceptancePolicy(t, t.TempDir(), stdioAcceptanceSpec())
	if err := os.Link(path, filepath.Join(project.Path, "candidate-readable-oracle.json")); err != nil {
		t.Skipf("hardlink unavailable: %v", err)
	}
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", path)
	if _, err := prepareTrustedAcceptance(context.Background(), project); err == nil {
		t.Fatal("a workspace hardlink made the private oracle candidate-readable")
	}
}

func TestLiveTrustedAcceptanceDirectVerificationSealsActualDiskBytes(t *testing.T) {
	requireAcceptancePython(t)
	project := acceptanceProject(t)
	actual := "import sys\nprint(sum(map(int, sys.stdin.read().split())))\n"
	writeFile(t, filepath.Join(project.Path, "main.py"), actual)
	ctx := ContextWithTrustedAcceptance(context.Background(), stdioAcceptanceSpec())
	// The SDK's caller-provided Content field is diagnostic input, never proof
	// that those bytes were actually executed from disk.
	report := project.VerifyRestrictedWorkspace(ctx, []ExtractedFile{{Path: "main.py", Content: "print('unverified caller content')\n"}})
	if !report.Passed || report.Strength != 2 || report.Acceptance == nil || len(report.VerifiedFiles) != 1 || report.VerifiedFiles[0].Content != actual {
		t.Fatalf("direct SDK path sealed unexecuted bytes: %+v", report)
	}
	if err := project.ValidateTrustedAcceptance(ctx, report.VerifiedFiles, report.Acceptance); err != nil {
		t.Fatalf("direct SDK acceptance cannot be validated: %v", err)
	}
}

func TestLiveTrustedAcceptanceCannotReplaceRuntimeFromCandidateCwd(t *testing.T) {
	requireAcceptancePython(t)
	project := acceptanceProject(t)
	spec := stdioAcceptanceSpec()
	ctx := ContextWithTrustedAcceptance(context.Background(), spec)
	// A PATH search in the candidate cwd would pick this executable and
	// incorrectly pass even though main.py has the wrong behavior.
	t.Setenv("PATH", "."+string(os.PathListSeparator)+os.Getenv("PATH"))
	fakeRuntime := "#!/bin/sh\nread first second\necho $((first + second))\n"
	report, err := project.EvaluateCandidateFiles(ctx, []ExtractedFile{
		{Path: "main.py", Content: "print('broken behavior')\n"},
		{Path: "python3", Content: fakeRuntime, ModeKnown: true, Mode: 0o755},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.Strength == 2 || report.Acceptance != nil || report.AcceptanceError == "" {
		t.Fatalf("candidate-selected runtime granted approval: %+v", report)
	}
}

func TestTrustedAcceptanceRuntimeInsideProjectIsRejected(t *testing.T) {
	project := acceptanceProject(t)
	name := stdioAcceptanceSpec().Command
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	fake := filepath.Join(project.Path, name)
	writeFile(t, fake, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	t.Setenv("PATH", project.Path+string(os.PathListSeparator)+path)
	ctx := ContextWithTrustedAcceptance(context.Background(), stdioAcceptanceSpec())
	if _, err := prepareTrustedAcceptance(ctx, project); err == nil {
		t.Fatal("repository-controlled runtime was accepted as a trusted interpreter")
	}
}
