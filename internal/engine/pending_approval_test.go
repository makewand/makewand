package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/execution"
)

func pendingApprovalFixture(t *testing.T) *Project {
	t.Helper()
	state := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", state)
	t.Setenv("MAKEWAND_APPLY_STATE_DIR", filepath.Join(state, "apply"))
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "answer.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := OpenProject(workspace)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func sealPendingApproval(t *testing.T, p *Project) *PendingApproval {
	t.Helper()
	record, err := p.SavePendingApproval(context.Background(), []ExtractedFile{{Path: "answer.txt", Content: "after"},
		{Path: "new/nested.txt", Content: "new", ModeKnown: true, Mode: 0o444}}, "file_write", 1, "Review candidate", "two files", nil)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestPendingApprovalProcessHelper(t *testing.T) {
	mode := os.Getenv("MAKEWAND_PENDING_APPROVAL_HELPER")
	if mode == "" {
		return
	}
	p, err := OpenProject(os.Getenv("MAKEWAND_PENDING_WORKSPACE"))
	if err != nil {
		t.Fatal(err)
	}
	record, err := p.SavePendingApproval(context.Background(), []ExtractedFile{{Path: "answer.txt", Content: "after"}}, "file_write", 2, "Review candidate", "one file", nil)
	if err != nil {
		t.Fatal(err)
	}
	if mode == "applied" {
		if err := p.TransitionPendingApproval(context.Background(), record, "authorized", "human approval"); err != nil {
			t.Fatal(err)
		}
		if err := p.ApplyFilesTransactional(context.Background(), record.Files, func() error {
			return p.ValidatePendingApproval(context.Background(), record, record.Files)
		}); err != nil {
			t.Fatal(err)
		}
	}
	// No TUI completion/save handler or deferred shutdown runs.
	os.Exit(86)
}

func TestPendingApprovalHardProcessExitPreservesPayload(t *testing.T) {
	for _, mode := range []string{"pending", "applied"} {
		t.Run(mode, func(t *testing.T) {
			p := pendingApprovalFixture(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestPendingApprovalProcessHelper$") //nolint:gosec // G204: execute only this fixed test binary and one hard-exit fixture.
			cmd.Env = append(os.Environ(), "MAKEWAND_PENDING_APPROVAL_HELPER="+mode, "MAKEWAND_PENDING_WORKSPACE="+p.Path)
			output, err := cmd.CombinedOutput()
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 86 {
				t.Fatalf("child did not exit at the durable boundary: %v, %s", err, output)
			}
			record, err := p.LoadPendingApproval(context.Background())
			if err != nil || record == nil || len(record.Files) != 1 || record.Files[0].Content != "after" {
				t.Fatalf("recovery: %+v, %v", record, err)
			}
			if record.RecoveredApplied != (mode == "applied") {
				t.Fatalf("recovered applied=%v, mode=%s", record.RecoveredApplied, mode)
			}
			data, err := os.ReadFile(filepath.Join(p.Path, "answer.txt"))
			if err != nil {
				t.Fatal(err)
			}
			want := "before"
			if mode == "applied" {
				want = "after"
			}
			if string(data) != want {
				t.Fatalf("load mutated workspace: %q", data)
			}
			if mode == "applied" {
				second, err := p.LoadPendingApproval(context.Background())
				if err != nil || second != nil {
					t.Fatalf("completed payload remained replayable: %+v, %v", second, err)
				}
			}
		})
	}
}

func TestPendingApprovalDependencyEditRetainsDiagnostic(t *testing.T) {
	p := pendingApprovalFixture(t)
	dependency := filepath.Join(p.Path, "node_modules", "fixture", "index.js")
	if err := os.MkdirAll(filepath.Dir(dependency), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dependency, []byte("baseline dependency"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := sealPendingApproval(t, p)
	if err := os.WriteFile(dependency, []byte("external edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if recovered, err := p.LoadPendingApproval(context.Background()); err == nil || recovered != nil || !strings.Contains(err.Error(), "baseline changed") {
		t.Fatalf("stale baseline recovered: %+v, %v", recovered, err)
	}
	s, err := openPendingApprovalStore(context.Background(), p.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	var state, diagnostic string
	var body []byte
	if err := s.db.QueryRow(`SELECT state,diagnostic,body FROM approvals WHERE id=?`, record.ID).Scan(&state, &diagnostic, &body); err != nil {
		t.Fatal(err)
	}
	if state != "invalidated" || !strings.Contains(diagnostic, "baseline changed") || !strings.Contains(string(body), "after") {
		t.Fatalf("evidence lost: %s %s %s", state, diagnostic, body)
	}
	data, _ := os.ReadFile(dependency)
	if string(data) != "external edit" {
		t.Fatal("recovery overwrote an external edit")
	}
}

func TestPendingApprovalTamperedRecordFailsClosed(t *testing.T) {
	p := pendingApprovalFixture(t)
	record := sealPendingApproval(t, p)
	s, err := openPendingApprovalStore(context.Background(), p.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	record.Files[0].Content = "tampered"
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`UPDATE approvals SET body=? WHERE id=?`, body, record.ID)
	s.close()
	if err != nil {
		t.Fatal(err)
	}
	if recovered, err := p.LoadPendingApproval(context.Background()); err == nil || recovered != nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("tampered payload recovered: %+v, %v", recovered, err)
	}
	if recovered, err := p.LoadPendingApproval(context.Background()); err == nil || recovered != nil || !strings.Contains(err.Error(), "recovery blocked") {
		t.Fatalf("tamper diagnostic was not retained: %+v, %v", recovered, err)
	}
}

func TestPendingApprovalReplacedRootFailsClosed(t *testing.T) {
	p := pendingApprovalFixture(t)
	sealPendingApproval(t, p)
	if err := os.Rename(p.Path, p.Path+"-old"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(p.Path + "-old") })
	if err := os.Mkdir(p.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Path, "answer.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if record, err := p.LoadPendingApproval(context.Background()); err == nil || record != nil {
		t.Fatalf("new directory inherited old approval: %+v, %v", record, err)
	}
}

func TestPendingApprovalDeniedIsDurable(t *testing.T) {
	p := pendingApprovalFixture(t)
	record := sealPendingApproval(t, p)
	if err := p.TransitionPendingApproval(context.Background(), record, "denied", "human refused recovery"); err != nil {
		t.Fatal(err)
	}
	if recovered, err := p.LoadPendingApproval(context.Background()); err != nil || recovered != nil {
		t.Fatalf("denied candidate restored: %+v, %v", recovered, err)
	}
}

func TestPendingApprovalAuthorizedBeforeWriteRequiresReview(t *testing.T) {
	p := pendingApprovalFixture(t)
	record := sealPendingApproval(t, p)
	if err := p.TransitionPendingApproval(context.Background(), record, "authorized", "approval message not yet executed"); err != nil {
		t.Fatal(err)
	}
	recovered, err := p.LoadPendingApproval(context.Background())
	if err != nil || recovered == nil || recovered.RecoveredApplied {
		t.Fatalf("unused decision cannot recover evidence: %+v, %v", recovered, err)
	}
	data, _ := os.ReadFile(filepath.Join(p.Path, "answer.txt"))
	if string(data) != "before" {
		t.Fatal("authorization was automatically replayed")
	}
}

func TestPendingApprovalValidationAndLockAreIndependent(t *testing.T) {
	p := pendingApprovalFixture(t)
	record := sealPendingApproval(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := p.ApplyFilesTransactional(ctx, record.Files, func() error {
		if err := p.TransitionPendingApproval(ctx, record, "authorized", "under independent apply lock"); err != nil {
			return err
		}
		return p.ValidatePendingApproval(ctx, record, record.Files)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(p.Path, "new", "nested.txt"), 0o600) })
	recovered, err := p.LoadPendingApproval(ctx)
	if err != nil || recovered == nil || !recovered.RecoveredApplied {
		t.Fatalf("complete postimage not recognized: %+v, %v", recovered, err)
	}
}

func TestPendingApprovalAttemptsExcludeParentAuthority(t *testing.T) {
	p := pendingApprovalFixture(t)
	selection := CandidateSelection{TaskID: "task-audit", Attempts: []CandidateState{{TaskID: "task-audit", ID: "candidate-1", Provider: "offline",
		Status: execution.Passed, Strength: 2, ArtifactDigest: "sealed-digest", Acceptance: &TrustedAcceptanceRecord{SpecDigest: "parent-authority-must-not-persist"}}}}
	if err := p.RecordCandidateAttempts(context.Background(), selection); err != nil {
		t.Fatal(err)
	}
	s, err := openPendingApprovalStore(context.Background(), p.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	var body []byte
	if err := s.db.QueryRow(`SELECT body FROM candidate_attempts`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "sealed-digest") || strings.Contains(string(body), "parent-authority") || strings.Contains(string(body), "Acceptance") {
		t.Fatalf("attempt audit imported authority or lost digest: %s", body)
	}
}

func TestPendingApprovalRefusesWorkspacePrivateState(t *testing.T) {
	p := pendingApprovalFixture(t)
	t.Setenv("MAKEWAND_CONFIG_DIR", filepath.Join(p.Path, "candidate-owned-config"))
	_, err := p.SavePendingApproval(context.Background(), []ExtractedFile{{Path: "answer.txt", Content: "after"}}, "file_write", 2, "Write?", "one file", nil)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("candidate-owned recovery state accepted: %v", err)
	}
}

func TestPendingApprovalEmptyCommandPayloadRoundTripsAndFreezesArguments(t *testing.T) {
	p := pendingApprovalFixture(t)
	plan := &ExecPlan{Kind: "tests", Command: "offline", Args: []string{"frozen argument"}}
	record, err := p.SavePendingApproval(context.Background(), []ExtractedFile{}, "tests", 2, "Test?", "offline", plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.Args[0] = "mutated caller argument"
	recovered, err := p.LoadPendingApproval(context.Background())
	if err != nil || recovered == nil || recovered.Plan.Args[0] != "frozen argument" || record.Plan.Args[0] != "frozen argument" {
		t.Fatalf("empty command payload failed to roundtrip or was not frozen: %+v, %v", recovered, err)
	}
}

func TestPendingApprovalPriorCommandAuthorizationIsNeverReplayed(t *testing.T) {
	p := pendingApprovalFixture(t)
	record, err := p.SavePendingApproval(context.Background(), nil, "tests", 2, "Test?", "offline", &ExecPlan{Kind: "tests", Command: "offline"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.TransitionPendingApproval(context.Background(), record, "authorized", "command approval before abrupt exit"); err != nil {
		t.Fatal(err)
	}
	recovered, err := p.LoadPendingApproval(context.Background())
	if recovered != nil || err == nil || !strings.Contains(err.Error(), "never replayed") {
		t.Fatalf("prior command decision was reused: %+v, %v", recovered, err)
	}
}
