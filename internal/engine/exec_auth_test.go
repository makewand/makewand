package engine

import (
	"context"
	"os"
	"testing"
)

// TestRunVerificationPlanUnsafeHostAuditsEveryExecution verifies that when the
// acknowledged unsafe opt-in routes a restricted plan to the host, EVERY
// execution is reported to the authorization's audit hook with the source
// stamped in.
func TestRunVerificationPlanUnsafeHostAuditsEveryExecution(t *testing.T) {
	swapVerifyIsolationVars(t)
	verifyUnsafe = func() bool { return true }

	project, err := NewProject("audit-project", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	var audited []UnsafeHostExecEvent
	project.SetUnsafeHostExecAuthorization(UnsafeHostExecAuthorization{
		Acknowledged: true,
		Source:       "test-ack",
		Audit:        func(ev UnsafeHostExecEvent) { audited = append(audited, ev) },
	})

	plan := ExecPlan{Kind: "tests", Command: "go", Args: []string{"version"}}
	for i := 0; i < 2; i++ {
		result, err := project.RunVerificationPlan(context.Background(), plan)
		if err != nil {
			t.Fatalf("RunVerificationPlan #%d: %v", i, err)
		}
		if result == nil || result.ExitCode != 0 {
			t.Fatalf("RunVerificationPlan #%d result = %+v, want exit 0", i, result)
		}
	}

	if len(audited) != 2 {
		t.Fatalf("audited %d events, want 2 (every host execution, not just the first)", len(audited))
	}
	for _, ev := range audited {
		if ev.Context != "verification" || ev.Command != "go" || ev.Source != "test-ack" {
			t.Fatalf("audit event = %+v, want verification/go/test-ack", ev)
		}
		if ev.Dir != project.Path {
			t.Fatalf("audit dir = %q, want project path %q", ev.Dir, project.Path)
		}
	}
}

// TestRunVerificationPlanUnsafeRequestedWithoutAckFailsClosed pins that a
// project without the acknowledged authorization refuses host execution even
// when the environment opt-in is present (and no sandbox is available).
func TestRunVerificationPlanUnsafeRequestedWithoutAckFailsClosed(t *testing.T) {
	swapVerifyIsolationVars(t)
	verifyGOOS = "darwin" // no sandbox path
	verifyUnsafe = func() bool { return true }

	project, err := NewProject("noack-project", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	plan := ExecPlan{Kind: "tests", Command: "go", Args: []string{"version"}}
	result, err := project.RunVerificationPlan(context.Background(), plan)
	if err == nil {
		t.Fatal("RunVerificationPlan should fail closed without acknowledgment")
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil (no host execution)", result)
	}
}

// TestCloneToTempInheritsUnsafeHostAuth verifies verification clones carry the
// parent project's authorization, so candidate verification in temp workspaces
// behaves exactly like the parent (including auditing).
func TestCloneToTempInheritsUnsafeHostAuth(t *testing.T) {
	project, err := NewProject("clone-auth", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	project.SetUnsafeHostExecAuthorization(UnsafeHostExecAuthorization{Acknowledged: true, Source: "parent"})

	clone, err := project.CloneToTemp()
	if err != nil {
		t.Fatalf("CloneToTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(clone.Path) })

	got := clone.UnsafeHostExecAuth()
	if !got.Acknowledged || got.Source != "parent" {
		t.Fatalf("clone auth = %+v, want inherited acknowledged auth from parent", got)
	}
}
