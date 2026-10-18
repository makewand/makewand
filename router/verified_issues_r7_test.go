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
	"syscall"
	"testing"
)

func TestR7ForeignMissingResolvedRoots(t *testing.T) {
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
		for _, location := range []string{"workspace", "home"} {
			for _, shape := range []string{"dangling", "dangling-child", "relative-link", "link-parent", "relative-parent"} {
				t.Run(variable+"/"+location+"/"+shape, func(t *testing.T) {
					root := t.TempDir()
					home, work, outside := filepath.Join(root, "home"), filepath.Join(root, "work"), filepath.Join(root, "outside")
					for _, path := range []string{home, work, outside} {
						if err := os.MkdirAll(path, 0700); err != nil {
							t.Fatal(err)
						}
					}
					target := work
					if location == "home" {
						target = home
					}
					link := filepath.Join(outside, "link")
					destination := filepath.Join(target, "missing")
					selected := link
					if shape == "link-parent" || shape == "relative-parent" {
						destination = filepath.Join(target, "sub")
						if err := os.Mkdir(destination, 0700); err != nil {
							t.Fatal(err)
						}
						selected += "/../missing"
					}
					if shape == "dangling-child" {
						selected += "/account"
					}
					if shape == "relative-link" {
						var err error
						destination, err = filepath.Rel(outside, destination)
						if err != nil {
							t.Fatal(err)
						}
					}
					if err := os.Symlink(destination, link); err != nil {
						t.Fatal(err)
					}
					if shape == "relative-parent" {
						selected = "../outside/link/../missing"
					}
					cmd := exec.Command("fixture")
					cmd.Dir = work
					cmd.Env = []string{"HOME=" + home, variable + "=" + selected}
					wrapped, err := wrapCLICommandWithSandbox(context.Background(), "grok", cmd)
					if err == nil {
						if wrapped != nil {
							cliAfterWait(wrapped)
						}
						t.Fatalf("foreign root skipped: %s", selected)
					}
					if !strings.Contains(err.Error(), variable) {
						t.Fatalf("unrelated refusal: %v", err)
					}
				})
			}
		}
	}
}

func TestR7ForeignRootENOTDIRAndEACCES(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions and errno")
	}
	root := t.TempDir()
	home, work, outside := filepath.Join(root, "home"), filepath.Join(root, "work"), filepath.Join(root, "outside")
	for _, path := range []string{home, work, outside} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, base := range []string{home, work, outside} {
		for _, kind := range []string{"file", "locked"} {
			t.Run(filepath.Base(base)+"/"+kind, func(t *testing.T) {
				parent := filepath.Join(base, kind)
				wantErr := syscall.ENOTDIR
				if kind == "file" {
					if err := os.WriteFile(parent, []byte("fixture"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					if os.Geteuid() == 0 {
						t.Skip("root bypasses EACCES")
					}
					if err := os.Mkdir(parent, 0000); err != nil {
						t.Fatal(err)
					}
					defer os.Chmod(parent, 0700)
					wantErr = syscall.EACCES
				}
				selected := parent + "/account"
				if _, err := os.Stat(selected); !errors.Is(err, wantErr) {
					t.Fatalf("fixture: %v", err)
				}
				want := base != outside
				if got := cliForeignRootCreatable(selected, home, []string{"--bind", work, work}); got != want {
					t.Fatalf("creatable=%v, want %v", got, want)
				}
			})
		}
	}
}

func TestR7ClaudeMetadataUnavailableAndFallback(t *testing.T) {
	for _, kind := range []string{"oversized", "directory", "loop", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			t.Setenv("CODEX_HOME", "")
			t.Setenv("MAKEWAND_LAYERED_BUDGET", "")
			if err := os.Mkdir(filepath.Join(home, ".claude"), 0700); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(home, ".claude.json")
			switch kind {
			case "oversized":
				if err := os.WriteFile(state, []byte(strings.Repeat("x", (4<<20)+1)), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(state, 0700); err != nil {
					t.Fatal(err)
				}
			case "loop":
				if err := os.Symlink(".claude.json", state); err != nil {
					t.Skip(err)
				}
			case "unreadable":
				if runtime.GOOS == "windows" || os.Geteuid() == 0 {
					t.Skip("requires Unix permission enforcement")
				}
				if err := os.WriteFile(state, []byte(`{"oauthAccount":{"accountUuid":"hidden"}}`), 0000); err != nil {
					t.Fatal(err)
				}
			}
			ctx, _ := budgetTestContext(t, 5)
			bound, err := bindCredentialAdmission(ctx, "claude")
			if err != nil {
				t.Fatal(err)
			}
			if readQuotaIdentity("claude", selectedQuotaRoot("claude")).IdentityKnown {
				t.Fatal("unavailable metadata established identity")
			}
			b := bound.Value(credentialBindingKey{}).(*credentialBinding)
			if f := b.Files[canonicalQuotaRoot(state)]; f.Exists || f.IdentityRef != "" {
				t.Fatalf("metadata fingerprint: %+v", f)
			}
			private, err := newCLIPrivateHome(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer private.cleanup()
			private.binding = b
			if err := private.copyIn(home, private.home, cliPrivateCredFile{rel: ".claude.json"}); err != nil {
				t.Fatal(err)
			}
			if fi, err := os.Stat(state); err == nil {
				args, err := private.addProviderEntry(nil, "claude", home, ".claude.json", state, fi, t.TempDir())
				if err != nil || len(args) != 0 {
					t.Fatalf("unavailable identity was mounted: %v %v", args, err)
				}
			}
			if _, err := os.Stat(filepath.Join(private.home, ".claude.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unavailable metadata was copied: %v", err)
			}
			var primaryCalls, fallbackCalls atomic.Int32
			primary := &dispatchBudgetProvider{name: "claude", calls: &primaryCalls, err: errors.New("fixture transport failure")}
			fallback := &dispatchBudgetProvider{name: "gemini", calls: &fallbackCalls}
			r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{
				"claude": {Provider: primary, Access: AccessSubscription},
				"gemini": {Provider: fallback, Access: AccessSubscription},
			}, DefaultModel: "claude", CodingModel: "claude"})
			if _, _, _, err := r.Chat(ctx, TaskCode, nil, ""); err != nil || primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 {
				t.Fatalf("fallback blocked: %v calls=%d/%d", err, primaryCalls.Load(), fallbackCalls.Load())
			}
		})
	}
}

func TestR7ClaudeIdentityAndBindingFourMiBLimit(t *testing.T) {
	for _, size := range []int{(1 << 20) + 1, 4 << 20} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		prefix, suffix := `{"oauthAccount":{"accountUuid":"same"},"pad":"`, `"}`
		raw := prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		q := readQuotaIdentity("claude", selectedQuotaRoot("claude"))
		if !q.IdentityKnown {
			t.Fatalf("identity unknown at %d bytes", size)
		}
		b, err := readCredentialBinding("claude")
		if err != nil {
			t.Fatal(err)
		}
		if f := b.Files[canonicalQuotaRoot(filepath.Join(home, ".claude.json"))]; f.IdentityRef != q.AccountRef {
			t.Fatal("identity and binding disagree")
		}
	}
}

func TestR7ForeignIsolatedRootsRemainSkippable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux sandbox paths")
	}
	oldLookup, oldProbe := cliBwrapLookup, cliBwrapProbe
	t.Cleanup(func() { cliBwrapLookup, cliBwrapProbe = oldLookup, oldProbe })
	cliBwrapLookup = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	cliBwrapProbe = func(string) error { return nil }
	t.Setenv("MAKEWAND_NO_BWRAP", "")
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	root := t.TempDir()
	home, work, outside := filepath.Join(root, "home"), filepath.Join(root, "work"), filepath.Join(root, "outside")
	for _, path := range []string{home, work, outside} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	link, alias := filepath.Join(root, "dangling"), filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(outside, "missing"), link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	for _, selected := range []string{link, link + "/account", filepath.Join(alias, "missing"), "../outside/missing"} {
		cmd := exec.Command("fixture")
		cmd.Dir = work
		cmd.Env = []string{"HOME=" + home, "CODEX_HOME=" + selected}
		wrapped, err := wrapCLICommandWithSandbox(context.Background(), "grok", cmd)
		if err != nil {
			t.Fatalf("isolated root blocked launch: %s: %v", selected, err)
		}
		cliAfterWait(wrapped)
		if cliEnvValue(wrapped.Env, "CODEX_HOME") != "" || strings.Contains(strings.Join(wrapped.Args, " "), selected) {
			t.Fatalf("stale foreign root was exposed: %s", selected)
		}
	}
	cmd := exec.Command("fixture")
	cmd.Dir = work
	cmd.Env = []string{"HOME=" + home, "CODEX_HOME=missing/account"}
	if wrapped, err := wrapCLICommandWithSandbox(context.Background(), "grok", cmd); err == nil {
		cliAfterWait(wrapped)
		t.Fatal("relative missing root in the child workspace was skipped")
	}
}
