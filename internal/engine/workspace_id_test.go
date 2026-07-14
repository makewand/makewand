package engine

import "testing"

func TestStableWorkspaceID_UsesEnvOverride(t *testing.T) {
	t.Setenv("MAKEWAND_WORKSPACE_ID", "shared-workspace")

	got, err := StableWorkspaceID(t.TempDir())
	if err != nil {
		t.Fatalf("StableWorkspaceID: %v", err)
	}
	if got != "shared-workspace" {
		t.Fatalf("StableWorkspaceID = %q, want shared-workspace", got)
	}
}
