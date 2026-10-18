package router

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestR8AmbiguousAccountPathsRefusedBeforeSkip(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux sandbox paths")
	}
	oldLookup, oldProbe := cliBwrapLookup, cliBwrapProbe
	t.Cleanup(func() { cliBwrapLookup, cliBwrapProbe = oldLookup, oldProbe })
	cliBwrapLookup = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	cliBwrapProbe = func(string) error { return nil }
	t.Setenv("MAKEWAND_NO_BWRAP", "")
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		for _, owner := range []bool{false, true} {
			for _, shape := range []string{"missing", "relative", "file", "locked", "outside-link", "work-link", "home-link"} {
				provider := "grok"
				if owner {
					provider = map[string]string{"CODEX_HOME": "codex", "CLAUDE_CONFIG_DIR": "claude"}[variable]
				}
				t.Run(variable+"/"+provider+"/"+shape, func(t *testing.T) {
					base := t.TempDir()
					home, work, outside := filepath.Join(base, "home"), filepath.Join(base, "work"), filepath.Join(base, "outside")
					for _, path := range []string{home, work, filepath.Join(outside, "a", "b"), filepath.Join(work, "sub"), filepath.Join(home, "locked", "s")} {
						if err := os.MkdirAll(path, 0700); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.WriteFile(filepath.Join(work, "file"), []byte("fixture"), 0600); err != nil {
						t.Fatal(err)
					}
					for link, target := range map[string]string{
						filepath.Join(outside, "link"): filepath.Join(work, "sub"),
						filepath.Join(work, "link"):    filepath.Join(outside, "a", "b"),
						filepath.Join(home, "link"):    filepath.Join(outside, "a", "b"),
					} {
						if err := os.Symlink(target, link); err != nil {
							t.Fatal(err)
						}
					}
					locked := filepath.Join(home, "locked")
					if err := os.Chmod(locked, 0000); err != nil {
						t.Fatal(err)
					}
					defer os.Chmod(locked, 0700)
					// Keep the configured spelling: filepath.Join would erase '..'.
					selected := map[string]string{
						"missing":      work + "/missing/../../outside/x",
						"relative":     "missing/../../outside/x",
						"file":         work + "/file/../../outside/x",
						"locked":       home + "/locked/s/../../../outside/x",
						"outside-link": outside + "/link/../x",
						"work-link":    work + "/link/../../x",
						"home-link":    home + "/link/../../x",
					}[shape]
					cmd := exec.Command("fixture")
					cmd.Dir = work
					cmd.Env = []string{"HOME=" + home, variable + "=" + selected}
					wrapped, err := wrapCLICommandWithSandbox(context.Background(), provider, cmd)
					if wrapped != nil && err == nil {
						cliAfterWait(wrapped)
					}
					if err == nil || !strings.Contains(err.Error(), variable+" has ambiguous path traversal") || ErrorKindOf(err) != ErrorKindConfig {
						t.Fatalf("ambiguous path was skipped or failed for another reason: %s: %v", selected, err)
					}
				})
			}
		}
	}
}

func TestR8LeadingParentRemainsUnambiguous(t *testing.T) {
	for _, path := range []string{"../outside/missing", "../../outside/missing", "./../outside/missing", "user..name/account"} {
		if !cliUnambiguousAccountPath(path) {
			t.Fatalf("unambiguous path rejected: %s", path)
		}
	}
	for _, path := range []string{"missing/../../outside/x", "../outside/link/../x"} {
		if cliUnambiguousAccountPath(path) {
			t.Fatalf("ambiguous path accepted: %s", path)
		}
	}
	// R7's TestR7ForeignIsolatedRootsRemainSkippable also exercises the full
	// sandbox wrapper with '../outside/missing'.
}

func TestR8UnavailableTokensOmittedAndFallbackAllowed(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		for _, kind := range []string{"unreadable", "oversized", "directory"} {
			t.Run(provider+"/"+kind, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("USERPROFILE", home)
				t.Setenv("CODEX_HOME", "")
				t.Setenv("CLAUDE_CONFIG_DIR", "")
				t.Setenv("MAKEWAND_LAYERED_BUDGET", "")
				t.Setenv("MAKEWAND_CALL_BUDGET_FILE", "")
				t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "")
				root := filepath.Join(home, "."+provider)
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				name := "auth.json"
				if provider == "claude" {
					name = ".credentials.json"
				}
				state := filepath.Join(root, name)
				switch kind {
				case "directory":
					if err := os.Mkdir(state, 0700); err != nil {
						t.Fatal(err)
					}
				case "oversized":
					if err := os.WriteFile(state, []byte(strings.Repeat("x", cliPrivateCredMaxBytes+1)), 0600); err != nil {
						t.Fatal(err)
					}
				case "unreadable":
					if runtime.GOOS == "windows" || os.Geteuid() == 0 {
						t.Skip("requires Unix permission enforcement")
					}
					if err := os.WriteFile(state, []byte(`{}`), 0000); err != nil {
						t.Fatal(err)
					}
				}
				ctx, ledger := budgetTestContext(t, 1)
				bound, err := bindCredentialAdmission(ctx, provider)
				if err != nil {
					t.Fatalf("unavailable token blocked admission: %v", err)
				}
				if readQuotaIdentity(provider, root).IdentityKnown {
					t.Fatal("unavailable token established identity")
				}
				binding := bound.Value(credentialBindingKey{}).(*credentialBinding)
				if binding.Files[canonicalQuotaRoot(state)].Exists {
					t.Fatal("unavailable token fingerprinted as readable")
				}
				for _, explicit := range []bool{false, true} {
					h, err := newCLIPrivateHome(t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					defer h.cleanup()
					h.binding = binding
					var private string
					if explicit {
						private, err = h.addAccountHome(provider, root)
					} else {
						fi, statErr := os.Stat(root)
						if statErr != nil {
							t.Fatal(statErr)
						}
						var args []string
						args, err = h.addProviderEntry(nil, provider, home, "."+provider, root, fi, t.TempDir())
						private = filepath.Join(h.home, "."+provider)
						for i := 0; i+2 < len(args); i++ {
							if (args[i] == "--bind" || args[i] == "--ro-bind") && (args[i+1] == root || args[i+1] == state) {
								t.Fatalf("raw host credential exposed: %v", args)
							}
						}
					}
					if err != nil {
						t.Fatalf("unavailable token blocked private copy: %v", err)
					}
					if _, err := os.Lstat(filepath.Join(private, name)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("unavailable token copied: %v", err)
					}
				}
				// Simulate a CLI refusing before invocation; release its pending
				// reservation, then let the next provider use the only call.
				_, pending, err := reserveProvider(ctx, &CLIProvider{provider: provider})
				if err != nil {
					t.Fatalf("absent token blocked pending admission: %v", err)
				}
				if err := pending.ReleaseUnstarted(); err != nil {
					t.Fatal(err)
				}
				var calls atomic.Int32
				fallback := &dispatchBudgetProvider{name: "gemini", calls: &calls}
				if _, _, err := ChatProvider(ctx, fallback, nil, "", 16); err != nil || calls.Load() != 1 {
					t.Fatalf("fallback blocked: %v calls=%d", err, calls.Load())
				}
				attempts := budgetTestAttempts(t, ledger)
				if len(attempts) != 2 || attempts[0]["status"] != "released" {
					t.Fatalf("pre-launch refusal spent a call: %v", attempts)
				}
				if kind == "directory" {
					if err := os.Remove(state); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Chmod(state, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(state, []byte(`{"fixture":"readable"}`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := verifyCredentialBinding(bound, provider); err == nil {
					t.Fatal("unavailable -> readable credential change was ignored")
				}
			})
		}
	}
}

func TestR8ReadableTokenChangeStillRefused(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("CODEX_HOME", "")
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			root := filepath.Join(home, "."+provider)
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			name := "auth.json"
			if provider == "claude" {
				name = ".credentials.json"
			}
			path := filepath.Join(root, name)
			if err := os.WriteFile(path, []byte(`{"fixture":"before"}`), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, err := bindCredentialAdmission(context.Background(), provider)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"fixture":"after"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := verifyCredentialBinding(ctx, provider); err == nil {
				t.Fatal("readable token replacement ignored")
			}
		})
	}
}
