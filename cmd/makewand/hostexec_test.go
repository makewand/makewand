package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
)

func swapUnsafeHostExecRequested(t *testing.T, requested bool) {
	t.Helper()
	old := unsafeHostExecRequested
	unsafeHostExecRequested = func() bool { return requested }
	t.Cleanup(func() { unsafeHostExecRequested = old })
}

func TestResolveUnsafeHostExecAuthNotRequested(t *testing.T) {
	swapUnsafeHostExecRequested(t, false)

	auth := resolveUnsafeHostExecAuth(config.DefaultConfig())
	if auth.Acknowledged {
		t.Fatal("auth.Acknowledged = true, want false when the env opt-in is absent")
	}
}

func TestResolveUnsafeHostExecAuthWithValidAck(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	swapUnsafeHostExecRequested(t, true)

	cfg := config.DefaultConfig()
	if err := cfg.RecordUnsafeHostExecAck(time.Now()); err != nil {
		t.Fatalf("RecordUnsafeHostExecAck: %v", err)
	}

	auth := resolveUnsafeHostExecAuth(cfg)
	if !auth.Acknowledged {
		t.Fatal("auth.Acknowledged = false, want true with a valid recorded acknowledgment")
	}
	if auth.Source != "config-ack" {
		t.Fatalf("auth.Source = %q, want config-ack", auth.Source)
	}
	if auth.Audit == nil {
		t.Fatal("auth.Audit = nil, want the audit sink wired in")
	}
}

// TestResolveUnsafeHostExecAuthNonInteractiveRefuses pins the fail-closed
// contract: the env opt-in without a recorded acknowledgment must NOT be
// authorized in a non-interactive process (`go test` has no TTY stdin).
func TestResolveUnsafeHostExecAuthNonInteractiveRefuses(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	swapUnsafeHostExecRequested(t, true)

	auth := resolveUnsafeHostExecAuth(config.DefaultConfig())
	if auth.Acknowledged {
		t.Fatal("auth.Acknowledged = true, want refusal without acknowledgment in non-interactive mode")
	}
}

func swapUnsafeHostExecInteractive(t *testing.T, interactive bool, line string, readErr error) {
	t.Helper()
	oldInteractive := unsafeHostExecInteractive
	oldReadLine := unsafeHostExecReadLine
	unsafeHostExecInteractive = func() bool { return interactive }
	unsafeHostExecReadLine = func() (string, error) { return line, readErr }
	t.Cleanup(func() {
		unsafeHostExecInteractive = oldInteractive
		unsafeHostExecReadLine = oldReadLine
	})
}

func swapPersistUnsafeHostExecAck(t *testing.T, err error) *int {
	t.Helper()
	old := persistUnsafeHostExecAckFn
	calls := 0
	persistUnsafeHostExecAckFn = func(*config.Config) error {
		calls++
		return err
	}
	t.Cleanup(func() { persistUnsafeHostExecAckFn = old })
	return &calls
}

func TestResolveUnsafeHostExecAuthInteractiveAccept(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	swapUnsafeHostExecRequested(t, true)
	swapUnsafeHostExecInteractive(t, true, "yes\n", nil)
	calls := swapPersistUnsafeHostExecAck(t, nil)

	auth := resolveUnsafeHostExecAuth(config.DefaultConfig())
	if !auth.Acknowledged || auth.Source != "interactive-ack" {
		t.Fatalf("auth = %+v, want acknowledged interactive-ack", auth)
	}
	if *calls != 1 {
		t.Fatalf("persist calls = %d, want 1", *calls)
	}
}

// TestResolveUnsafeHostExecAuthPersistFailureFailsClosed pins the must-fix
// contract: an acknowledgment that cannot be recorded (hostname unavailable,
// corrupt config, save failure) authorizes NOTHING, even though the user
// typed yes.
func TestResolveUnsafeHostExecAuthPersistFailureFailsClosed(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	swapUnsafeHostExecRequested(t, true)
	swapUnsafeHostExecInteractive(t, true, "yes\n", nil)
	swapPersistUnsafeHostExecAck(t, errors.New("no hostname"))

	auth := resolveUnsafeHostExecAuth(config.DefaultConfig())
	if auth.Acknowledged {
		t.Fatal("auth.Acknowledged = true, want fail-closed refusal when the ack cannot be recorded")
	}
}

// TestPersistUnsafeHostExecAckFailureLeavesNoResidue exercises the REAL
// persist path (no seam) against a corrupt config file: persistence must fail,
// and — critically — the caller's in-memory cfg must carry no ack fields
// afterwards, because setup ignores the resolver result, displays cfg's ack
// state, and re-saves cfg at the end. Residue would resurrect an
// acknowledgment the user was just told failed.
func TestPersistUnsafeHostExecAckFailureLeavesNoResidue(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", cfgDir)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatalf("WriteFile(corrupt config): %v", err)
	}

	cfg := config.DefaultConfig()
	if err := persistUnsafeHostExecAck(cfg); err == nil {
		t.Fatal("persistUnsafeHostExecAck should fail on a corrupt config")
	}
	if cfg.UnsafeHostExecAckVersion != 0 || cfg.UnsafeHostExecAckAt != "" || cfg.UnsafeHostExecAckHost != "" {
		t.Fatalf("cfg carries ack residue after failed persist: version=%d at=%q host=%q",
			cfg.UnsafeHostExecAckVersion, cfg.UnsafeHostExecAckAt, cfg.UnsafeHostExecAckHost)
	}
	if cfg.UnsafeHostExecAckValid() {
		t.Fatal("cfg.UnsafeHostExecAckValid() = true after failed persist, want false")
	}
}

// TestSetupDoesNotResurrectFailedAck runs the real setup command with the env
// opt-in, an interactive "yes", and a corrupt config that makes the REAL
// persist fail. Setup's own trailing config.Save (which rewrites the corrupt
// file with defaults) must NOT smuggle the failed acknowledgment to disk.
func TestSetupDoesNotResurrectFailedAck(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", cfgDir)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("MAKEWAND_REMOTE_URL", "")
	t.Setenv("MAKEWAND_REMOTE_TOKEN", "")
	t.Cleanup(func() { resolvedApprovalOverride = "" })
	swapUnsafeHostExecRequested(t, true)
	swapUnsafeHostExecInteractive(t, true, "yes\n", nil)
	// NO persist seam: the real persist must fail on the corrupt config below.

	path := filepath.Join(cfgDir, "config.json")
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatalf("WriteFile(corrupt config): %v", err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"setup"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("setup error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(config): %v", err)
	}
	var saved struct {
		AckVersion int    `json:"unsafe_host_exec_ack_version"`
		AckAt      string `json:"unsafe_host_exec_ack_at"`
		AckHost    string `json:"unsafe_host_exec_ack_host"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("Unmarshal(saved config): %v (raw: %s)", err, data)
	}
	if saved.AckVersion != 0 || saved.AckAt != "" || saved.AckHost != "" {
		t.Fatalf("setup persisted the failed acknowledgment: %+v\nraw: %s", saved, data)
	}
}

func TestResolveUnsafeHostExecAuthInteractiveDecline(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	swapUnsafeHostExecRequested(t, true)
	swapUnsafeHostExecInteractive(t, true, "no\n", nil)
	calls := swapPersistUnsafeHostExecAck(t, nil)

	auth := resolveUnsafeHostExecAuth(config.DefaultConfig())
	if auth.Acknowledged {
		t.Fatal("auth.Acknowledged = true, want refusal on decline")
	}
	if *calls != 0 {
		t.Fatalf("persist calls = %d, want 0 (nothing recorded on decline)", *calls)
	}
}

// TestAuditUnsafeHostExecTightensLoosePermissions verifies a pre-existing
// wider-permission audit file is chmodded back to 0600 on the next write.
func TestAuditUnsafeHostExecTightensLoosePermissions(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", cfgDir)

	path := filepath.Join(cfgDir, unsafeHostExecAuditFile)
	//nolint:gosec // G306: deliberately loose permissions — the test verifies they get tightened to 0600.
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pre-existing audit): %v", err)
	}

	auditUnsafeHostExec(engine.UnsafeHostExecEvent{Context: "verification", Command: "go", Dir: "/tmp/p", Source: "test"})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(audit): %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("audit perms = %o, want 0600 after tightening", info.Mode().Perm())
	}
}

func TestAuditUnsafeHostExecWritesJSONL(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", cfgDir)

	auditUnsafeHostExec(engine.UnsafeHostExecEvent{
		Context: "verification",
		Command: "go",
		Args:    []string{"test", "./..."},
		Dir:     "/tmp/proj",
		Source:  "config-ack",
	})
	auditUnsafeHostExec(engine.UnsafeHostExecEvent{
		Context: "preview",
		Command: "npm",
		Args:    []string{"run", "dev"},
		Dir:     "/tmp/proj",
		Source:  "interactive-ack",
	})

	data, err := os.ReadFile(filepath.Join(cfgDir, unsafeHostExecAuditFile))
	if err != nil {
		t.Fatalf("ReadFile(audit log): %v", err)
	}
	lines := splitNonEmptyLines(string(data))
	if len(lines) != 2 {
		t.Fatalf("audit log has %d lines, want 2 (one per execution)", len(lines))
	}

	var first struct {
		Time    string   `json:"time"`
		Context string   `json:"context"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Dir     string   `json:"dir"`
		Source  string   `json:"source"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("Unmarshal(first audit line): %v", err)
	}
	if first.Context != "verification" || first.Command != "go" || first.Source != "config-ack" || first.Dir != "/tmp/proj" {
		t.Fatalf("first audit line = %+v, want the verification event", first)
	}
	if first.Time == "" {
		t.Fatal("audit line missing timestamp")
	}
	if _, err := time.Parse(time.RFC3339, first.Time); err != nil {
		t.Fatalf("audit timestamp %q is not RFC3339: %v", first.Time, err)
	}
}

func splitNonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
