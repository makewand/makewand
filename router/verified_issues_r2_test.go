package router

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/testfixture"
)

// Round 7 aligned Go with Python: oversized identity metadata is treated as
// unavailable (identity unknown) instead of refusing the dispatch. What must
// still hold is that no bind is built for it, neither a dangling private copy
// nor the raw host file.
func TestVerifiedOversizedClaudeMetadataIsOmittedWithoutBind(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, make([]byte, cliPrivateCredMaxBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := newCLIPrivateHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.cleanup()
	args, err := h.addProviderEntry(nil, "claude", home, ".claude.json", path, fi, t.TempDir())
	if err != nil {
		t.Fatalf("oversized metadata should be omitted as unknown identity, got error: %v", err)
	}
	for _, arg := range args {
		if strings.Contains(arg, ".claude.json") {
			t.Fatalf("oversized metadata must not be bound (private or host): %q", args)
		}
	}
	if _, err := os.Stat(filepath.Join(h.home, ".claude.json")); !os.IsNotExist(err) {
		t.Fatalf("no private copy may be created for oversized metadata: %v", err)
	}
}

func TestVerifiedRegisterWorkerFailureClearsFence(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "stream"}[stream], func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("USERPROFILE", root)
			t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
			t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "0")
			t.Setenv("MAKEWAND_PROJECT_ROOT", root)
			path := filepath.Join(root, "calls.json")
			ctx := execution.ContextWithConfig(t.Context(), execution.Config{LedgerPath: path, MaxCalls: 10, TaskID: "registration"})
			fixture := testfixture.WriteCLI(t, "worker", testfixture.Spec{Sleep: 30 * time.Second})
			p := NewCommandCLI("verified-lease", fixture, nil, "legacy")
			injected := errors.New("temporary RegisterWorker failure")
			old := cliRegisterJobWorker
			t.Cleanup(func() { cliRegisterJobWorker = old })
			cliRegisterJobWorker = func(*execution.Attempt, int) error { return injected }
			var err error
			if stream {
				_, err = ChatStreamProvider(ctx, p, nil, "fixture", 16)
			} else {
				_, _, err = ChatProvider(ctx, p, nil, "fixture", 16)
			}
			if !errors.Is(err, injected) {
				t.Fatalf("registration failure was not exercised: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var data struct{ Leases map[string]any }
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Fatal(err)
			}
			if len(data.Leases) != 0 {
				t.Fatalf("reaped worker left a launch fence: %s", raw)
			}
		})
	}
}

func TestVerifiedForeignAccountHomesMasked(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap arguments are Linux specific")
	}
	old := cliBwrapLookup
	t.Cleanup(func() { cliBwrapLookup = old })
	cliBwrapLookup = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	for _, provider := range []string{"claude", "codex", "grok", "agy"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			home, work := filepath.Join(root, "home"), filepath.Join(root, "work")
			codex, claude := filepath.Join(root, "codex"), filepath.Join(root, "claude")
			for _, path := range []string{home, work, codex, claude} {
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", home)
			if provider == "agy" {
				gemini := filepath.Join(home, ".gemini")
				if err := os.MkdirAll(gemini, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(gemini, "settings.json"), []byte(`{"useG1Credits":false}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("fixture")
			cmd.Dir = work
			cmd.Env = []string{"HOME=" + home, "CODEX_HOME=" + codex, "CLAUDE_CONFIG_DIR=" + claude}
			wrapped, err := wrapCLICommandWithSandbox(context.Background(), provider, cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer cliAfterWait(wrapped)
			for owner, path := range map[string]string{"codex": codex, "claude": claude} {
				if owner == provider {
					continue
				}
				masked := false
				for i := 0; i+1 < len(wrapped.Args); i++ {
					if wrapped.Args[i] == "--tmpfs" && wrapped.Args[i+1] == path {
						masked = true
					}
				}
				if !masked {
					t.Errorf("%s can read foreign %s home: %s", provider, owner, path)
				}
			}
		})
	}
}

func TestVerifiedInvalidForeignClaudeHomeFailsClosed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap arguments are Linux specific")
	}
	old := cliBwrapLookup
	t.Cleanup(func() { cliBwrapLookup = old })
	cliBwrapLookup = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	home := t.TempDir()
	t.Setenv("HOME", home)
	cmd := exec.Command("fixture")
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + home}
	if wrapped, err := wrapCLICommandWithSandbox(context.Background(), "grok", cmd); err == nil {
		cliAfterWait(wrapped)
		t.Fatal("overbroad foreign account was accepted")
	}
}

func TestVerifiedDirectClaudePreservesUserConfiguration(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit"}[explicit], func(t *testing.T) {
			home, work := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			root := filepath.Join(home, ".claude")
			if err := os.MkdirAll(filepath.Join(root, "hooks"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"settings.json", "hooks/guard.sh"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			cmd := exec.Command("fixture")
			cmd.Dir = work
			cmd.Env = []string{"HOME=" + home}
			if explicit {
				t.Setenv("CLAUDE_CONFIG_DIR", root)
				cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+root)
			}
			ctx, err := bindCredentialAdmission(context.Background(), "claude")
			if err != nil {
				t.Fatal(err)
			}
			if err := freezeDirectCLIAccount(ctx, "claude", cmd); err != nil {
				t.Fatal(err)
			}
			defer cliAfterWait(cmd)
			effective := cliEnvValue(cmd.Env, "CLAUDE_CONFIG_DIR")
			if effective == "" {
				effective = root
			}
			if effective != root {
				t.Fatalf("Claude configuration path changed: %s", effective)
			}
			for _, name := range []string{"settings.json", "hooks/guard.sh"} {
				if raw, err := os.ReadFile(filepath.Join(effective, name)); err != nil || string(raw) != "fixture" {
					t.Fatalf("user configuration unavailable: %s: %v", name, err)
				}
			}
		})
	}
}
