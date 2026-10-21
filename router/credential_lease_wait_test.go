package router

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/makewand/makewand/execution"
)

// GR-2: the credential admission snapshot is taken once the attempt holds its
// account lease, and a login that names its account is bound by that account,
// not by its token bytes. A same-account refresh by the lease holder is no
// account switch; a real switch during the wait is still refused.

type leaseWaitLogin struct {
	provider string
	root     string // the selected credential root
	known    bool   // the login names its account
}

// write stores a login of account with token: codex auth.json, or Claude
// .credentials.json beside its .claude.json identity.
func (l leaseWaitLogin) write(t *testing.T, account, token string) {
	t.Helper()
	files := map[string]any{}
	switch l.provider {
	case "codex":
		tokens := map[string]string{"access_token": token}
		if l.known {
			tokens["account_id"] = account
		}
		files["auth.json"] = map[string]any{"tokens": tokens}
	case "claude":
		files[".credentials.json"] = map[string]any{"claudeAiOauth": map[string]string{"accessToken": token}}
		files[".claude.json"] = map[string]any{"oauthAccount": map[string]string{"accountUuid": account}}
	}
	for name, value := range files {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(l.root, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// reserveAfterLeaseWait admits provider's CLI while another attempt holds its
// account lease. During the wait the holder runs during(), then finishes, so
// the admission continues with the lease the holder released.
func reserveAfterLeaseWait(t *testing.T, login leaseWaitLogin, during func()) (context.Context, *execution.Attempt, string, error) {
	t.Helper()
	t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "30")
	ledger := filepath.Join(t.TempDir(), "calls.json")
	ctx := execution.ContextWithConfig(context.Background(), execution.Config{LedgerPath: ledger, TaskID: "lease-wait", MaxCalls: 10})
	holderCtx := ctx // a login without a known account waits on the unknown pool
	if login.known {
		ref := readQuotaIdentity(login.provider, login.root).AccountRef
		holderCtx = execution.ContextWithConfig(ctx, execution.Config{AccountRef: &ref})
	}
	holder, err := execution.Reserve(holderCtx, execution.Metadata{Engine: login.provider})
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	finish := func() {
		once.Do(func() {
			during()
			if err := holder.Complete(execution.Outcome{Status: execution.Passed, Known: true}); err != nil {
				t.Error(err)
			}
		})
	}
	defer finish()
	// The capacity probe runs while the attempt waits for the busy lease.
	ctx = execution.ContextWithCapacityFallback(ctx, func() bool { finish(); return false })
	var p Provider = NewCodexCLI("fake-unused")
	if login.provider == "claude" {
		p = NewClaudeCLI("fake-unused")
	}
	admitted, attempt, err := reserveProvider(ctx, p)
	return admitted, attempt, ledger, err
}

func leaseWaitSetup(t *testing.T, provider string, known bool) leaseWaitLogin {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	root := t.TempDir()
	variable := "CODEX_HOME"
	if provider == "claude" {
		variable = "CLAUDE_CONFIG_DIR"
	}
	t.Setenv(variable, root)
	return leaseWaitLogin{provider: provider, root: root, known: known}
}

func TestLeaseWaitSameAccountRefreshIsNoAccountSwitch(t *testing.T) {
	for _, tc := range []struct {
		name, provider string
		known          bool
	}{{"codex", "codex", true}, {"claude", "claude", true}, {"codex unknown account", "codex", false}} {
		t.Run(tc.name, func(t *testing.T) {
			login := leaseWaitSetup(t, tc.provider, tc.known)
			login.write(t, "A", "token-before")
			admitted, attempt, _, err := reserveAfterLeaseWait(t, login, func() { login.write(t, "A", "token-refreshed") })
			if err != nil {
				t.Fatalf("admission after the lease wait: %v", err)
			}
			defer func() {
				if err := attempt.Complete(execution.Outcome{Status: execution.Unknown}); err != nil {
					t.Fatal(err)
				}
			}()
			if err := verifyCredentialBinding(admitted, tc.provider); err != nil {
				t.Fatalf("same-account refresh during the lease wait refused: %v", err)
			}
			// The launch copies the refreshed login into the private home.
			h, err := newCLIPrivateHome(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer h.cleanup()
			h.binding, _ = admitted.Value(credentialBindingKey{}).(*credentialBinding)
			if _, err := h.addAccountHome(tc.provider, login.root); err != nil {
				t.Fatalf("refreshed login refused at launch: %v", err)
			}
		})
	}
}

func TestLeaseWaitAccountSwitchIsRefused(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			login := leaseWaitSetup(t, provider, true)
			login.write(t, "A", "token-a")
			_, attempt, ledger, err := reserveAfterLeaseWait(t, login, func() { login.write(t, "B", "token-b") })
			if err == nil {
				_ = attempt.Complete(execution.Outcome{Status: execution.Unknown})
				t.Fatal("login switched to another account during the lease wait was admitted")
			}
			// Refused before launch: the reservation is released, not charged.
			for _, entry := range budgetTestAttempts(t, ledger) {
				if entry["engine"] == provider && entry["status"] != "released" && entry["result_status"] != "PASSED" {
					t.Fatalf("refused switch charged a call: %+v", entry)
				}
			}
		})
	}
}

// After admission the bound account still guards the launch: a switch then
// (TestLoginSwitchAfterAdmissionCannotLaunchOtherAccount for codex) is refused
// by the sandbox preparation for Claude too.
func TestClaudeSwitchAfterLeasedAdmissionIsRefusedAtLaunch(t *testing.T) {
	login := leaseWaitSetup(t, "claude", true)
	login.write(t, "A", "token-a")
	admitted, attempt, _, err := reserveAfterLeaseWait(t, login, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := attempt.Complete(execution.Outcome{Status: execution.Unknown}); err != nil {
			t.Fatal(err)
		}
	}()
	login.write(t, "B", "token-b")
	old := cliBwrapLookup
	t.Cleanup(func() { cliBwrapLookup = old })
	cliBwrapLookup = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	cmd := exec.Command("/usr/bin/true")
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "CLAUDE_CONFIG_DIR=" + login.root}
	if wrapped, err := wrapCLICommandWithSandbox(admitted, "claude", cmd); err == nil {
		cliAfterWait(wrapped)
		t.Fatal("login switched after admission prepared a launch")
	}
}
