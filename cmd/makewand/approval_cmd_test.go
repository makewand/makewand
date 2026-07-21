package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/config"
)

func TestResolveApprovalOverride(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"   ", "", false},
		{"manual", config.ApprovalModeManual, false},
		{"SAFE", config.ApprovalModeSafe, false},
		{"autopilot", config.ApprovalModeAuto, false},
		{"auto", "", true},
		{"autopilto", "", true},
		{"bogus", "", true},
	}
	for _, tt := range tests {
		got, err := resolveApprovalOverride(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Fatalf("resolveApprovalOverride(%q) = (%q, %v), want (%q, wantErr=%v)", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

// TestPersistentPreRunRejectsInvalidApproval mirrors the --repo-trust contract:
// an invalid --approval is rejected once, globally, before any subcommand RunE,
// instead of being silently normalized to manual.
func TestPersistentPreRunRejectsInvalidApproval(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	t.Cleanup(func() { resolvedApprovalOverride = "" })

	const wantMsg = `invalid --approval "bogus": must be manual, safe, or autopilot`

	cases := []struct {
		name string
		args []string
	}{
		{name: "root", args: []string{"--approval=bogus"}},
		{name: "serve", args: []string{"serve", "--approval=bogus"}},
		{name: "setup", args: []string{"setup", "--approval=bogus"}},
		{name: "chat", args: []string{"chat", "--approval=bogus"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)

			err := cmd.Execute()
			if err == nil {
				t.Fatalf("Execute(%v) error = nil, want rejection", tc.args)
			}
			if !strings.Contains(err.Error(), wantMsg) {
				t.Fatalf("Execute(%v) error = %q, want it to contain %q", tc.args, err.Error(), wantMsg)
			}
		})
	}
}

// TestSetupPersistsApprovalMode verifies the documented persistence split:
// `makewand setup --approval safe` writes the mode to config.json (setup is the
// only command that saves), and the saved value is the canonical form.
func TestSetupPersistsApprovalMode(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", cfgDir)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("MAKEWAND_REMOTE_URL", "")
	t.Setenv("MAKEWAND_REMOTE_TOKEN", "")
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "")
	t.Cleanup(func() { resolvedApprovalOverride = "" })

	path := filepath.Join(cfgDir, "config.json")
	if err := os.WriteFile(path, []byte(`{"approval_mode":"manual"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"setup", "--approval", "SAFE"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("setup --approval error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(config): %v", err)
	}
	var saved struct {
		ApprovalMode string `json:"approval_mode"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("Unmarshal(config): %v", err)
	}
	if saved.ApprovalMode != config.ApprovalModeSafe {
		t.Fatalf("persisted approval_mode = %q, want %q", saved.ApprovalMode, config.ApprovalModeSafe)
	}
}

// TestApprovalOverrideIsRuntimeOnly verifies the flag overrides the loaded
// config in memory without touching the file: only setup persists it.
func TestApprovalOverrideIsRuntimeOnly(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", cfgDir)

	path := filepath.Join(cfgDir, "config.json")
	original := []byte(`{"approval_mode":"manual"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}

	resolvedApprovalOverride = config.ApprovalModeAuto
	t.Cleanup(func() { resolvedApprovalOverride = "" })

	cfg := loadConfigWithWarning()
	if cfg.ApprovalMode != config.ApprovalModeAuto {
		t.Fatalf("cfg.ApprovalMode = %q, want runtime override %q", cfg.ApprovalMode, config.ApprovalModeAuto)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(config): %v", err)
	}
	if string(after) != string(original) {
		t.Fatalf("config file changed by runtime override:\nbefore: %s\nafter: %s", original, after)
	}
}
