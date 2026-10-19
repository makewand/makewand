package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/testfixture"
)

func evidenceFixture(t *testing.T) (string, string) {
	t.Helper()
	root, store := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", root)
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", store)
	if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte(`{"tokens":{"account_id":"account-a","access_token":"SECRET"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return root, store
}

func evidenceEvents(account string, at time.Time) string {
	return fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"account_id\":%q,\"model\":\"actual-model\"}}\n"+
		"{\"type\":\"event_msg\",\"timestamp\":%q,\"payload\":{\"type\":\"token_count\",\"rate_limits\":{\"primary\":{\"used_percent\":42,\"window_minutes\":300,\"resets_at\":%d}}}}\n"+
		"{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"PRIVATE PROMPT access_token SECRET\"}}\n", account, at.UTC().Format(time.RFC3339Nano), time.Now().Add(time.Hour).Unix())
}

func readOnlyRecord(t *testing.T, store string) execution.ProviderEvidence {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(store, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("records: %v %v", files, err)
	}
	record, err := execution.ReadEvidence(files[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(files[0])
	for _, secret := range []string{"SECRET", "PRIVATE PROMPT", "access_token", "requested-model"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("secret/request leaked: %s", secret)
		}
	}
	return record
}

func TestPrivateExecutionEvidenceSurvivesCleanupAndBindsQuota(t *testing.T) {
	root, store := evidenceFixture(t)
	attempt, err := execution.Reserve(context.Background(), execution.Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := initializeCLIEvidence(attempt, "codex"); err != nil {
		t.Fatal(err)
	}
	h, err := newCLIPrivateHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.cleanup()
	h.attempt = attempt
	priv, err := h.addCodexHome(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(priv, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(priv, "sessions", "sample.jsonl"), []byte(evidenceEvents("account-a", time.Now())), 0600); err != nil {
		t.Fatal(err)
	}
	h.collectEvidence()
	before := readOnlyRecord(t, store)
	if before.OutcomeKnown || before.Status != execution.Unknown || before.Model != "actual-model" {
		t.Fatalf("before cleanup: %+v", before)
	}
	h.cleanup()
	if _, err := os.Stat(priv); !os.IsNotExist(err) {
		t.Fatal("private home survived")
	}
	if err := attempt.Complete(execution.Outcome{Status: execution.Passed, Known: true}); err != nil {
		t.Fatal(err)
	}
	record := readOnlyRecord(t, store)
	if !record.IdentityKnown || !record.OutcomeKnown || len(record.Windows) != 1 || record.Windows[0].UsedPercent != 42 {
		t.Fatalf("record: %+v", record)
	}
	quota, err := NewCodexQuotaSource(filepath.Join(root, "sessions")).Read(context.Background())
	if err != nil || quota.FiveHourPct == nil || *quota.FiveHourPct != 42 {
		t.Fatalf("quota: %+v %v", quota, err)
	}
	if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte(`{"tokens":{"account_id":"other-account"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	quota, err = NewCodexQuotaSource(filepath.Join(root, "sessions")).Read(context.Background())
	if err != nil || quota.HasData {
		t.Fatalf("other account borrowed evidence: %+v %v", quota, err)
	}
}

func TestExecutionEvidenceProtocolRejectsMessagesAndOtherAccounts(t *testing.T) {
	root, _ := evidenceFixture(t)
	identity := readQuotaIdentity("codex", root)
	record := execution.ProviderEvidence{Engine: "codex", AccountRef: identity.AccountRef, IdentityKnown: true, PoolRef: identity.PoolRef}
	collectCLIProtocol(&record, []byte(evidenceEvents("other-account", time.Now())))
	if len(record.Windows) != 0 || record.Model != "" {
		t.Fatal("foreign account accepted")
	}
	raw, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"text": evidenceEvents("account-a", time.Now()), "model": "forged-model", "rate_limits": map[string]any{"primary": map[string]any{"used_percent": 0}}}})
	collectCLIProtocol(&record, raw)
	if len(record.Windows) != 0 || record.Model != "" {
		t.Fatal("message text accepted")
	}
	collectCLIProtocol(&record, []byte(evidenceEvents("account-a", time.Now().Add(-time.Hour))))
	// Old observations can be archived but must not authorize current quota.
	quota := identity
	for _, w := range record.Windows {
		quota.Windows = append(quota.Windows, QuotaWindow{PoolRef: identity.PoolRef, UsedPercent: w.UsedPercent, WindowMinutes: w.Minutes, SourceAt: time.UnixMilli(w.ObservedAt)})
	}
	if currentQuotaWindows(quota).HasData {
		t.Fatal("stale evidence renewed")
	}
}

func TestExecutionEvidenceFakeCLIUnknownAndStream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			_, store := evidenceFixture(t)
			old := cliBwrapLookup
			cliBwrapLookup = func(string) (string, error) { return "", exec.ErrNotFound }
			defer func() { cliBwrapLookup = old }()
			t.Setenv("MAKEWAND_CONFIG_DIR", os.Getenv("MAKEWAND_CONFIG_DIR"))
			t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", os.Getenv("MAKEWAND_UNSAFE_HOST_EXEC"))
			ack, err := cliTestWriteHostExecAck()
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(ack)
			bin := testfixture.WriteCLI(t, "codex", testfixture.Spec{Stdout: evidenceEvents("account-a", time.Now()), ExitCode: 7})
			provider := NewCodexCLI(bin)
			var callErr error
			if stream {
				ch, err := provider.ChatStream(context.Background(), []Message{{Role: "user", Content: "PRIVATE PROMPT"}}, "", 100)
				callErr = err
				if ch != nil {
					for chunk := range ch {
						if chunk.Error != nil {
							callErr = chunk.Error
						}
					}
				}
			} else {
				_, _, callErr = provider.Chat(context.Background(), []Message{{Role: "user", Content: "PRIVATE PROMPT"}}, "", 100)
			}
			if !errors.Is(callErr, execution.ErrUnknownOutcome) {
				t.Fatalf("exit 7: %v", callErr)
			}
			record := readOnlyRecord(t, store)
			if record.OutcomeKnown || record.Status != execution.Unknown {
				t.Fatalf("unknown result lost: %+v", record)
			}
			if !stream && record.Model != "actual-model" {
				t.Fatalf("ephemeral metadata lost: %+v", record)
			}
			if stream && record.Model != "" {
				t.Fatal("plain stream treated as metadata")
			}
		})
	}
}

func TestExecutionEvidenceUnavailableStopsLaunch(t *testing.T) {
	_, store := evidenceFixture(t)
	blocked := filepath.Join(store, "file")
	if err := os.WriteFile(blocked, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", filepath.Join(blocked, "child"))
	attempt, err := execution.Reserve(context.Background(), execution.Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := initializeCLIEvidence(attempt, "codex"); !errors.Is(err, execution.ErrUnknownOutcome) {
		t.Fatalf("evidence failure: %v", err)
	}
}

func TestPrivateExecutionEvidenceAccountSwitchAndSymlink(t *testing.T) {
	root, store := evidenceFixture(t)
	attempt, err := execution.Reserve(context.Background(), execution.Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := initializeCLIEvidence(attempt, "codex"); err != nil {
		t.Fatal(err)
	}
	h, err := newCLIPrivateHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.cleanup()
	h.attempt = attempt
	priv, err := h.addCodexHome(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(priv, "auth.json"), []byte(`{"tokens":{"account_id":"account-b"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	h.collectEvidence()
	record := readOnlyRecord(t, store)
	if record.AccountRef != quotaAccountRef("codex", "account-b") || !record.IdentityKnown {
		t.Fatalf("account switch lost: %+v", record)
	}
	if err := os.Remove(filepath.Join(priv, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "auth.json"), filepath.Join(priv, "auth.json")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	h.collectEvidence()
	record = readOnlyRecord(t, store)
	if record.IdentityKnown || len(record.Windows) != 0 {
		t.Fatalf("symlink used as identity: %+v", record)
	}
}

func TestExecutionEvidenceFakeCLITimeout(t *testing.T) {
	_, store := evidenceFixture(t)
	old := cliBwrapLookup
	cliBwrapLookup = func(string) (string, error) { return "", exec.ErrNotFound }
	defer func() { cliBwrapLookup = old }()
	t.Setenv("MAKEWAND_CONFIG_DIR", os.Getenv("MAKEWAND_CONFIG_DIR"))
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", os.Getenv("MAKEWAND_UNSAFE_HOST_EXEC"))
	ack, err := cliTestWriteHostExecAck()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(ack)
	bin := testfixture.WriteCLI(t, "codex", testfixture.Spec{Sleep: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err = NewCodexCLI(bin).Chat(ctx, []Message{{Role: "user", Content: "fixture"}}, "", 100)
	if !errors.Is(err, execution.ErrUnknownOutcome) {
		t.Fatalf("timeout was replayable: %v", err)
	}
	record := readOnlyRecord(t, store)
	if record.Status != execution.Timeout || record.OutcomeKnown || record.Model != "" {
		t.Fatalf("timeout evidence: %+v", record)
	}
}

func TestExecutionEvidenceMeasuredProtocolUsage(t *testing.T) {
	record := execution.ProviderEvidence{Engine: "codex"}
	collectCLIProtocol(&record, []byte(`{"type":"turn.completed","usage":{"input_tokens":21,"output_tokens":13},"text":"SECRET"}`))
	if record.Tokens == nil || *record.Tokens != 34 {
		t.Fatalf("measured usage lost: %+v", record)
	}
	collectCLIProtocol(&record, []byte(`{"type":"turn.completed","usage":{"input_tokens":9223372036854775807,"output_tokens":1}}`))
	if *record.Tokens != 34 {
		t.Fatal("overflow replaced known usage")
	}
	record = execution.ProviderEvidence{Engine: "claude"}
	collectCLIProtocol(&record, []byte(`{"type":"result","modelUsage":{"claude-observed":{"inputTokens":21}},"usage":{"input_tokens":21,"output_tokens":13},"result":"SECRET"}`))
	if record.Model != "claude-observed" || record.Tokens == nil || *record.Tokens != 34 {
		t.Fatalf("Claude metadata lost: %+v", record)
	}
}

func TestClaudeExplicitDefaultIdentityAndInvalidPrivateEvidence(t *testing.T) {
	for _, mutation := range []string{"host-switch", "private-switch", "deleted", "symlink", "invalid"} {
		t.Run(mutation, func(t *testing.T) {
			home, store := t.TempDir(), t.TempDir()
			root := filepath.Join(home, ".claude")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("CLAUDE_CONFIG_DIR", root)
			t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", store)
			metadata := filepath.Join(home, ".claude.json")
			for path, raw := range map[string]string{metadata: `{"oauthAccount":{"accountUuid":"A"}}`, filepath.Join(root, ".credentials.json"): `{}`} {
				if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			attempt, err := execution.Reserve(t.Context(), execution.Metadata{Engine: "claude"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := attempt.Complete(execution.Outcome{Status: execution.Unknown}); err != nil {
					t.Fatal(err)
				}
			}()
			if err := initializeCLIEvidence(attempt, "claude"); err != nil {
				t.Fatal(err)
			}
			h, err := newCLIPrivateHome(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer h.cleanup()
			h.attempt = attempt
			private, err := h.addAccountHome("claude", root)
			if err != nil {
				t.Fatal(err)
			}
			identity := filepath.Join(private, ".claude.json")
			if raw, err := os.ReadFile(identity); err != nil || string(raw) != `{"oauthAccount":{"accountUuid":"A"}}` {
				t.Fatalf("explicit default metadata not frozen: %s %v", raw, err)
			}
			if mutation != "host-switch" {
				if err := attempt.UpdateEvidence(func(e *execution.ProviderEvidence) {
					e.Windows = []execution.EvidenceWindow{{Key: "|300", Minutes: 300, UsedPercent: 12, ObservedAt: 1}}
				}); err != nil {
					t.Fatal(err)
				}
			}
			expected := ""
			switch mutation {
			case "host-switch", "private-switch":
				path := identity
				expected = quotaAccountRef("claude", "B")
				if mutation == "host-switch" {
					path, expected = metadata, quotaAccountRef("claude", "A")
				}
				if err := os.WriteFile(path, []byte(`{"oauthAccount":{"accountUuid":"B"}}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				if err := os.WriteFile(identity, []byte(`{"oauthAccount":{"accountUuid":123}}`), 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Remove(identity); err != nil {
					t.Fatal(err)
				}
				if mutation == "symlink" {
					if err := os.Symlink(metadata, identity); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
				}
			}
			h.collectEvidence()
			record := readOnlyRecord(t, store)
			if record.AccountRef != expected || record.IdentityKnown != (expected != "") || len(record.Windows) != 0 {
				t.Fatalf("incorrect private identity: %+v", record)
			}
		})
	}
}

func TestPrivateEvidencePathsRemainBoundAfterPreparation(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			root, other, store := t.TempDir(), t.TempDir(), t.TempDir()
			variable, name, raw := "CODEX_HOME", "auth.json", `{"tokens":{"account_id":"A"}}`
			if provider == "claude" {
				variable, name, raw = "CLAUDE_CONFIG_DIR", ".claude.json", `{"oauthAccount":{"accountUuid":"A"}}`
				if err := os.WriteFile(filepath.Join(root, ".credentials.json"), []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv(variable, root)
			t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", store)
			if err := os.WriteFile(filepath.Join(root, name), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			attempt, err := execution.Reserve(t.Context(), execution.Metadata{Engine: provider})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := attempt.Complete(execution.Outcome{Status: execution.Unknown}); err != nil {
					t.Fatal(err)
				}
			}()
			if err := initializeCLIEvidence(attempt, provider); err != nil {
				t.Fatal(err)
			}
			h, err := newCLIPrivateHome(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer h.cleanup()
			h.attempt = attempt
			private, err := h.addAccountHome(provider, root)
			if err != nil {
				t.Fatal(err)
			}
			// Changing the selected host root must not skip invalidation of
			// the copy that this invocation actually used.
			t.Setenv(variable, other)
			if err := os.Remove(filepath.Join(private, name)); err != nil {
				t.Fatal(err)
			}
			// A different metadata path is never a fallback after deletion.
			if provider == "claude" {
				if err := os.WriteFile(filepath.Join(h.home, name), []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			h.collectEvidence()
			record := readOnlyRecord(t, store)
			if record.IdentityKnown || record.AccountRef != "" || record.PoolRef != "" {
				t.Fatalf("stale private identity retained: %+v", record)
			}
		})
	}
}
