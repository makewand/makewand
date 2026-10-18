package router

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/testfixture"
)

func TestR3RouterFallsBackAfterBusyAdmission(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "stream"}[stream], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			t.Setenv("CODEX_HOME", "")
			t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
			t.Setenv("MAKEWAND_CALL_BUDGET_FILE", "")
			t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "")
			t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
			t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "0")
			t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
			t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
			t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "100")
			t.Setenv("MAKEWAND_PROJECT_DAILY_CALLS", "100")
			t.Setenv("MAKEWAND_PROJECT_ROOT", t.TempDir())
			ctx, path := budgetTestContext(t, 5)
			holder, err := execution.Reserve(ctx, execution.Metadata{Engine: "claude"})
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Complete(execution.Outcome{Status: execution.Passed, Known: true})
			var primaryCalls, fallbackCalls atomic.Int32
			primary := &dispatchBudgetProvider{name: "claude", calls: &primaryCalls}
			fallback := &dispatchBudgetProvider{name: "gemini", calls: &fallbackCalls}
			r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{
				"claude": {Provider: primary, Access: AccessSubscription},
				"gemini": {Provider: fallback, Access: AccessSubscription},
			}, DefaultModel: "claude", CodingModel: "claude"})
			if stream {
				var chunks <-chan StreamChunk
				chunks, _, err = r.ChatStream(ctx, TaskCode, nil, "")
				if err == nil {
					for chunk := range chunks {
						if chunk.Error != nil {
							err = chunk.Error
						}
					}
				}
			} else {
				_, _, _, err = r.Chat(ctx, TaskCode, nil, "")
			}
			if err != nil || primaryCalls.Load() != 0 || fallbackCalls.Load() != 1 {
				t.Fatalf("busy fallback: %v, calls=%d/%d", err, primaryCalls.Load(), fallbackCalls.Load())
			}
			if len(budgetTestAttempts(t, path)) != 2 {
				t.Fatal("busy admission spent a call")
			}
		})
	}
}

// Exercise the actual Chat/ChatStream wait paths: checking only freeze/verify
// directly would miss a removed call or an entry deleted by cliAfterWait.
func TestR3DirectClaudePostRunAccountCheck(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, change := range []string{"account", "root", "refresh"} {
			t.Run(map[bool]string{false: "chat/", true: "stream/"}[stream]+change, func(t *testing.T) {
				root := t.TempDir()
				home := filepath.Join(root, "home")
				if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("HOME", home)
				t.Setenv("USERPROFILE", home)
				t.Setenv("CLAUDE_CONFIG_DIR", "")
				t.Setenv("CODEX_HOME", "")
				t.Setenv("MAKEWAND_CONFIG_DIR", filepath.Join(root, "config"))
				t.Setenv("MAKEWAND_CALL_BUDGET_FILE", "")
				t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "")
				t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "100")
				t.Setenv("MAKEWAND_PROJECT_DAILY_CALLS", "100")
				t.Setenv("MAKEWAND_PROJECT_ROOT", root)
				t.Setenv("MAKEWAND_NO_BWRAP", "1")
				t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "1")
				t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
				t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "0")
				ack, err := cliTestWriteHostExecAck()
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(ack)
				metadata := filepath.Join(home, ".claude.json")
				if err := os.WriteFile(metadata, []byte(`{"oauthAccount":{"accountUuid":"A"}}`), 0600); err != nil {
					t.Fatal(err)
				}
				alternate := filepath.Join(root, "alternate")
				if err := os.MkdirAll(alternate, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(alternate, ".claude.json"), []byte(`{"oauthAccount":{"accountUuid":"A"}}`), 0600); err != nil {
					t.Fatal(err)
				}
				old := cliRegisterJobWorker
				t.Cleanup(func() { cliRegisterJobWorker = old })
				cliRegisterJobWorker = func(a *execution.Attempt, pid int) error {
					if err := old(a, pid); err != nil {
						return err
					}
					switch change {
					case "root":
						return os.Setenv("CLAUDE_CONFIG_DIR", alternate)
					case "account":
						return os.WriteFile(metadata, []byte(`{"oauthAccount":{"accountUuid":"B"}}`), 0600)
					default:
						return os.WriteFile(metadata, []byte(`{"oauthAccount":{"accountUuid":"A"},"refreshed":true}`), 0600)
					}
				}
				fixture := testfixture.WriteCLI(t, "worker", testfixture.Spec{Stdout: "fixture answer\n"})
				p := NewCommandCLI("claude", fixture, nil, "legacy")
				ctx := execution.ContextWithConfig(t.Context(), execution.Config{LedgerPath: filepath.Join(root, "calls.json"), TaskID: "post-run", MaxCalls: 10})
				if stream {
					var chunks <-chan StreamChunk
					chunks, err = ChatStreamProvider(ctx, p, nil, "fixture", 16)
					if err == nil {
						for chunk := range chunks {
							if chunk.Error != nil {
								err = chunk.Error
							}
						}
					}
				} else {
					_, _, err = ChatProvider(ctx, p, nil, "fixture", 16)
				}
				if change == "refresh" {
					if err != nil {
						t.Fatalf("same-account refresh refused: %v", err)
					}
				} else if !errors.Is(err, execution.ErrUnknownOutcome) {
					t.Fatalf("post-run login switch must be unknown: %v", err)
				}
			})
		}
	}
}
