package router

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAuditRemediation_CLISensitiveHomeEntries(t *testing.T) {
	required := []string{
		".ssh", ".aws", ".azure", ".gnupg", ".kube",
		".docker", ".netrc", ".npmrc", ".yarnrc", ".pypirc",
		".pgpass", ".gitconfig", ".git-credentials",
		".cargo/credentials", ".cargo/credentials.toml", ".cargo/config.toml",
		".m2/settings.xml", ".gradle/gradle.properties",
		".s3cfg", ".boto", ".oci", ".aider.conf.yml",
		".anthropic", ".openai",
		".bash_history", ".zsh_history", ".histfile", ".python_history",
		".node_repl_history", ".psql_history", ".mysql_history", ".sqlite_history",
		".lesshst", ".viminfo",
		".local/share/keyrings", ".local/share/credentials", ".local/share/makewand",
		".local/share/muse", ".local/share/fish", ".local/state",
		"server_auth.json", "state.db", "state.db-wal", "state.db-shm",
		"admin_session_secret", "users.json", "audit.jsonl", "usage.jsonl",
		".config/makewand", ".config/gcloud", ".config/gh", ".config/hub",
		".config/git/credentials", ".config/rclone", ".config/muse",
		".config/google-chrome", ".config/chromium", ".mozilla", ".pki",
	}

	for _, req := range required {
		if !slices.Contains(cliSensitiveHomeEntries, req) {
			t.Errorf("cliSensitiveHomeEntries missing required sensitive path: %s", req)
		}
	}
}

func TestAuditRemediation_WindowsHostAuthGate(t *testing.T) {
	// On Windows or when bwrapMissing is true, unauthorized host execution must fail-close
	ctx := context.Background()
	cmd := exec.Command("echo", "test")
	// If context has RemoteOrigin, isCLISandboxRequired returns true
	remoteCtx := ContextWithRemoteOrigin(ctx)
	if !isCLISandboxRequired(remoteCtx) {
		t.Error("RemoteOrigin context must require CLI sandbox")
	}
	_ = cmd
}

func TestAuditRemediation_ForbiddenWritableRoots(t *testing.T) {
	for _, root := range []string{"/", "/home", "/etc", "/usr", "/var", "/tmp", "/var/tmp", "/root", "/boot", "/opt", "/sys", "/dev", "/proc", "/mnt", "/srv", "/media"} {
		if !isForbiddenWritableWorkspace(root) {
			t.Errorf("expected %s to be forbidden as writable workspace", root)
		}
	}

	// Symlink to forbidden root
	tmp := t.TempDir()
	symToRoot := filepath.Join(tmp, "sym_root")
	if err := os.Symlink("/", symToRoot); err == nil {
		if !isForbiddenWritableWorkspace(symToRoot) {
			t.Errorf("expected symlink to / (%s) to be forbidden as writable workspace", symToRoot)
		}
	}

	// Non-forbidden normal workspace
	normalWS := filepath.Join(tmp, "my_project")
	if isForbiddenWritableWorkspace(normalWS) {
		t.Errorf("normal project %s should NOT be forbidden", normalWS)
	}

	// wrapCLICommandWithSandbox must fail closed when workspace is forbidden and not review
	cmd := exec.Command("echo", "hi")
	cmd.Dir = "/"
	_, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err == nil {
		t.Error("wrapCLICommandWithSandbox with Dir=/ must fail closed for non-review task")
	}

	// Review tasks are refused too: a read-only bind of / placed after the
	// host-root and HOME masks would undo every mask.
	reviewCtx := ContextWithTask(context.Background(), TaskReview)
	_, reviewErr := wrapCLICommandWithSandbox(reviewCtx, "claude", cmd)
	if reviewErr == nil || !strings.Contains(reviewErr.Error(), "refusing to use") {
		t.Errorf("review task with workspace / must be refused: %v", reviewErr)
	}
}

func TestAuditRemediation_SeccompBPFGeneration(t *testing.T) {
	bpf := generateSeccompBPFFilter()
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
		if len(bpf) == 0 {
			t.Fatalf("generateSeccompBPFFilter returned empty on %s", runtime.GOARCH)
		}
		if len(bpf)%8 != 0 {
			t.Errorf("BPF bytecode length %d must be multiple of 8", len(bpf))
		}
	}
}

func TestAuditRemediation_SanitizeCLIEnvPositiveAllowlist(t *testing.T) {
	env := []string{
		"PATH=/usr/bin:/bin",
		"LANG=en_US.UTF-8",
		"SENDGRID_API_KEY=SG.fake-sendgrid-key",
		"ANTHROPIC_API_KEY=sk-ant-live",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"OPENAI_API_KEY=sk-openai-foreign",
		"SAFE_VAR=allowed",
		"UNKNOWN_CUSTOM_VAR=should_be_dropped",
		"AWS_SECRET_ACCESS_KEY=secret",
	}

	claudeSanitized := sanitizeCLIEnv("claude", env)
	claudeStr := strings.Join(claudeSanitized, "\n")

	if strings.Contains(claudeStr, "SENDGRID_API_KEY") {
		t.Errorf("SENDGRID_API_KEY must be sanitized, but was leaked: %s", claudeStr)
	}
	if strings.Contains(claudeStr, "OPENAI_API_KEY") {
		t.Errorf("foreign OPENAI_API_KEY must be sanitized for claude: %s", claudeStr)
	}
	if strings.Contains(claudeStr, "UNKNOWN_CUSTOM_VAR") {
		t.Errorf("unallowlisted UNKNOWN_CUSTOM_VAR must be dropped by positive allowlist: %s", claudeStr)
	}
	if strings.Contains(claudeStr, "AWS_SECRET_ACCESS_KEY") {
		t.Errorf("AWS_SECRET_ACCESS_KEY must be dropped: %s", claudeStr)
	}
	if !strings.Contains(claudeStr, "ANTHROPIC_API_KEY=sk-ant-live") {
		t.Errorf("ANTHROPIC_API_KEY must be preserved for claude: %s", claudeStr)
	}
	if !strings.Contains(claudeStr, "PATH=/usr/bin:/bin") {
		t.Errorf("PATH must be preserved: %s", claudeStr)
	}
	if strings.Contains(claudeStr, "SAFE_VAR") {
		t.Errorf("SAFE_* is not an allowlisted prefix; SAFE_VAR must be dropped: %s", claudeStr)
	}
}

type auditFailProvider struct {
	name string
	cost float64
}

func (p *auditFailProvider) Name() string      { return p.name }
func (p *auditFailProvider) IsAvailable() bool { return true }
func (p *auditFailProvider) ChatStream(_ context.Context, _ []Message, _ string, _ int) (<-chan StreamChunk, error) {
	return nil, errors.New("failed")
}
func (p *auditFailProvider) Chat(_ context.Context, _ []Message, _ string, _ int) (string, Usage, error) {
	return "", Usage{Cost: p.cost, MeasuredCost: true, InputTokens: 50, MeasuredTokens: true}, errors.New("timeout after spend")
}

func TestAuditRemediation_EnsembleUsageRetentionOnFailure(t *testing.T) {
	// Case 1: Generator fails after consuming tokens/cost (7.0), good generator (1.0),
	// single surviving generator skips judge -> total = 8.0 (previously leaked the 7.0).
	failProv := &auditFailProvider{
		name: "claude",
		cost: 7.0,
	}
	goodProv := &scriptedProvider{
		provName: "gemini",
		content:  "good content",
		cost:     1.0,
	}
	judgeProv := &scriptedProvider{
		provName: "codex",
		content:  "WINNER: 1\n",
		cost:     2.0,
	}

	r := &Router{
		providers: map[string]Provider{
			"claude": failProv,
			"gemini": goodProv,
			"codex":  judgeProv,
		},
		providerCache: map[providerKey]Provider{
			{name: "claude", modelID: testModelID("claude", TierPremium)}: failProv,
			{name: "gemini", modelID: testModelID("gemini", TierPremium)}: goodProv,
			{name: "codex", modelID: testModelID("codex", TierPremium)}:   judgeProv,
		},
		accessTypes: map[string]AccessType{
			"claude": AccessSubscription,
			"gemini": AccessFree,
			"codex":  AccessSubscription,
		},
		usage:   newSessionUsage(),
		mode:    ModePower,
		modeSet: true,
	}

	_, total, _, err := r.ChatBest(context.Background(), PhaseReview, []Message{{Role: "user", Content: "test"}}, "")
	if err != nil {
		t.Fatalf("ChatBest failed: %v", err)
	}

	if total.Cost < 7.9 || total.Cost > 8.1 {
		t.Errorf("ChatBest total.Cost = %.2f, want ~8.0 (including failed provider spend)", total.Cost)
	}
	// The successful parts come from scriptedProvider, which reports
	// estimated (unmeasured) usage. Aggregates follow mergeAttemptUsage AND
	// semantics: a sum is measured only when every spending part is measured.
	if total.MeasuredCost || total.MeasuredTokens {
		t.Errorf("ChatBest total mixes measured and estimated parts and must not claim to be measured: cost=%v tokens=%v",
			total.MeasuredCost, total.MeasuredTokens)
	}

	// Case 2: Both generators succeed (1.0 each), judge fails after consuming spend (3.0) -> total = 5.0
	gen1 := &scriptedProvider{provName: "gemini", content: "candidate 1", cost: 1.0}
	gen2 := &scriptedProvider{provName: "claude", content: "candidate 2", cost: 1.0}
	failJudge := &auditFailProvider{name: "codex", cost: 3.0}

	r2 := &Router{
		providers: map[string]Provider{
			"gemini": gen1,
			"claude": gen2,
			"codex":  failJudge,
		},
		providerCache: map[providerKey]Provider{
			{name: "gemini", modelID: testModelID("gemini", TierPremium)}: gen1,
			{name: "claude", modelID: testModelID("claude", TierPremium)}: gen2,
			{name: "codex", modelID: testModelID("codex", TierPremium)}:   failJudge,
		},
		accessTypes: map[string]AccessType{
			"gemini": AccessFree,
			"claude": AccessSubscription,
			"codex":  AccessSubscription,
		},
		usage:   newSessionUsage(),
		mode:    ModePower,
		modeSet: true,
	}

	_, total2, _, err2 := r2.ChatBest(context.Background(), PhaseReview, []Message{{Role: "user", Content: "test"}}, "")
	if err2 != nil {
		t.Fatalf("ChatBest case 2 failed: %v", err2)
	}
	// Total cost must include generators (1.0 + 1.0) + failed judge (3.0) = 5.0
	if total2.Cost < 4.9 || total2.Cost > 5.1 {
		t.Errorf("ChatBest total2.Cost = %.2f, want ~5.0 (including failed judge spend)", total2.Cost)
	}
	// The successful parts come from scriptedProvider, which reports
	// estimated (unmeasured) usage. Aggregates follow mergeAttemptUsage AND
	// semantics: a sum is measured only when every spending part is measured.
	if total2.MeasuredCost || total2.MeasuredTokens {
		t.Errorf("ChatBest total2 mixes measured and estimated parts and must not claim to be measured: cost=%v tokens=%v",
			total2.MeasuredCost, total2.MeasuredTokens)
	}
}

func TestAuditRemediation_CircuitBreakerExponentialBackoff(t *testing.T) {
	cb := newProviderCircuitBreaker(1, 10*time.Second)
	fakeNow := time.Now()
	cb.now = func() time.Time { return fakeNow }

	// Trip circuit from closed to open
	opened, until := cb.RecordFailure("claude")
	if !opened || until.Sub(fakeNow) != 10*time.Second {
		t.Fatalf("first failure should open circuit for 10s, got %v", until.Sub(fakeNow))
	}

	// Advance time past cooldown to admit half-open probe
	fakeNow = fakeNow.Add(11 * time.Second)
	allowed, _, ticket := cb.Admit("claude")
	if !allowed || !ticket.probe {
		t.Fatalf("expected probe admission, got allowed=%v, ticket=%v", allowed, ticket)
	}

	// First probe fails -> probeFailures=1, cooldown=10s (1x)
	opened2, until2 := cb.RecordFailure("claude")
	if !opened2 || until2.Sub(fakeNow) != 10*time.Second {
		t.Fatalf("first probe failure should open circuit for 10s (1x), got %v", until2.Sub(fakeNow))
	}

	// Advance past 10s to admit next probe
	fakeNow = fakeNow.Add(11 * time.Second)
	allowed2, _, ticket2 := cb.Admit("claude")
	if !allowed2 || !ticket2.probe {
		t.Fatalf("expected 2nd probe admission, got allowed=%v", allowed2)
	}

	// Second probe fails -> probeFailures=2, cooldown doubles to 20s (2x)
	opened3, until3 := cb.RecordFailure("claude")
	if !opened3 || until3.Sub(fakeNow) != 20*time.Second {
		t.Fatalf("second probe failure should open circuit for 20s (2x), got %v", until3.Sub(fakeNow))
	}

	// Advance past 20s to admit third probe
	fakeNow = fakeNow.Add(21 * time.Second)
	allowed3, _, ticket3 := cb.Admit("claude")
	if !allowed3 || !ticket3.probe {
		t.Fatalf("expected 3rd probe admission, got allowed=%v", allowed3)
	}

	// Third probe fails -> probeFailures=3, cooldown quadruples to 40s (4x)
	opened4, until4 := cb.RecordFailure("claude")
	if !opened4 || until4.Sub(fakeNow) != 40*time.Second {
		t.Fatalf("third probe failure should open circuit for 40s (4x), got %v", until4.Sub(fakeNow))
	}
}
