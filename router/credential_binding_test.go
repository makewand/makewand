package router

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/makewand/makewand/execution"
)

func TestLoginSwitchAfterAdmissionCannotLaunchOtherAccount(t *testing.T) {
	root := t.TempDir()
	home, login, work := filepath.Join(root, "home"), filepath.Join(root, "login"), filepath.Join(root, "work")
	for _, p := range []string{home, login, work} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", login)
	t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	auth := filepath.Join(login, "auth.json")
	if err := os.WriteFile(auth, []byte(`{"tokens":{"account_id":"A"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := execution.ContextWithConfig(context.Background(), execution.Config{LedgerPath: filepath.Join(root, "calls.json"), TaskID: "switch", MaxCalls: 10})
	b := quotaAccountRef("codex", "B")
	full, err := execution.Reserve(execution.ContextWithConfig(ctx, execution.Config{AccountRef: &b}), execution.Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := full.Complete(execution.Outcome{Status: execution.Unknown}); err != nil {
			t.Fatal(err)
		}
	}()
	ctx, a, err := reserveProvider(ctx, NewCodexCLI("fake-unused"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := a.Complete(execution.Outcome{Status: execution.Unknown}); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.WriteFile(auth, []byte(`{"tokens":{"account_id":"B"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	old := cliBwrapLookup
	t.Cleanup(func() { cliBwrapLookup = old })
	cliBwrapLookup = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	cmd := exec.Command("/usr/bin/true")
	cmd.Dir = work
	cmd.Env = []string{"HOME=" + home, "CODEX_HOME=" + login}
	wrapped, err := wrapCLICommandWithSandbox(ctx, "codex", cmd)
	if err == nil {
		cliAfterWait(wrapped)
		t.Fatal("admitted A but prepared B with B already at capacity")
	}
}

func TestDirectCLILoginSwitchInvalidatesAttribution(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	login := filepath.Join(root, "login")
	work := filepath.Join(root, "work")
	for _, p := range []string{home, login, work} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", login)
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "1")
	ack, err := cliTestWriteHostExecAck()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(ack)
	auth := filepath.Join(login, "auth.json")
	if err := os.WriteFile(auth, []byte(`{"tokens":{"account_id":"A"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, err := bindCredentialAdmission(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	old := cliBwrapLookup
	t.Cleanup(func() { cliBwrapLookup = old })
	cliBwrapLookup = func(string) (string, error) { return "", exec.ErrNotFound }
	cmd := exec.Command("/usr/bin/true")
	cmd.Dir = work
	cmd.Env = []string{"HOME=" + home, "CODEX_HOME=" + login}
	wrapped, err := wrapCLICommandWithSandbox(ctx, "codex", cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer cliAfterWait(wrapped)
	if err := os.WriteFile(auth, []byte(`{"tokens":{"account_id":"B"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	selected := cliEnvValue(wrapped.Env, "CODEX_HOME")
	raw, err := os.ReadFile(filepath.Join(selected, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"tokens":{"account_id":"B"}}` || selected != login {
		t.Fatalf("direct execution lost selected configuration root: %s", raw)
	}
	if err := verifyDirectCLIAccount(wrapped); !errors.Is(err, execution.ErrUnknownOutcome) {
		t.Fatalf("account switch was attributed to admitted login: %v", err)
	}
}

func TestCredentialCopyRejectsLoginChangeDuringPreparation(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			variable := "CODEX_HOME"
			name := "auth.json"
			if provider == "claude" {
				variable = "CLAUDE_CONFIG_DIR"
				name = ".credentials.json"
			}
			t.Setenv(variable, root)
			path := filepath.Join(root, name)
			if err := os.WriteFile(path, []byte(`{"fixture":"A"}`), 0600); err != nil {
				t.Fatal(err)
			}
			binding, err := readCredentialBinding(provider)
			if err != nil {
				t.Fatal(err)
			}
			h, err := newCLIPrivateHome(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer h.cleanup()
			h.binding = binding
			if err := os.WriteFile(path, []byte(`{"fixture":"B"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := h.addAccountHome(provider, root); err == nil {
				t.Fatal("copied changed login after validation")
			}
		})
	}
}

func TestCredentialCopyRejectsDeletedOrInvalidAdmittedFile(t *testing.T) {
	for _, fixture := range []struct{ provider, variable, name string }{
		{"codex", "CODEX_HOME", "auth.json"},
		{"claude", "CLAUDE_CONFIG_DIR", ".credentials.json"},
		{"claude", "CLAUDE_CONFIG_DIR", ".claude.json"},
	} {
		for _, mutation := range []string{"deleted", "directory", "dangling-link"} {
			t.Run(fixture.provider+"/"+fixture.name+"/"+mutation, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv(fixture.variable, root)
				path := filepath.Join(root, fixture.name)
				if err := os.WriteFile(path, []byte(`{"fixture":"A"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				binding, err := readCredentialBinding(fixture.provider)
				if err != nil {
					t.Fatal(err)
				}
				h, err := newCLIPrivateHome(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer h.cleanup()
				h.binding = binding
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "directory":
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				case "dangling-link":
					if err := os.Symlink(filepath.Join(root, "missing"), path); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
				}
				if _, err := h.addAccountHome(fixture.provider, root); err == nil {
					t.Fatal("invalid admitted credential silently skipped")
				}
			})
		}
	}
}

func TestProviderEntryRejectsReplacedRootAndMetadata(t *testing.T) {
	for _, entry := range []string{".claude", ".claude.json"} {
		t.Run(entry, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			root := filepath.Join(home, ".claude")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{filepath.Join(root, ".credentials.json"), filepath.Join(home, ".claude.json")} {
				if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			binding, err := readCredentialBinding("claude")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, entry)
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if entry == ".claude" {
				err = os.WriteFile(path, []byte(`{}`), 0o600)
			} else {
				err = os.Mkdir(path, 0o700)
			}
			if err != nil {
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
			h.binding = binding
			if _, err := h.addProviderEntry(nil, "claude", home, entry, path, fi, t.TempDir()); err == nil {
				t.Fatal("replaced root/metadata bypassed credential copying")
			}
		})
	}
}
