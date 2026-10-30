package router

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/testfixture"
)

func rpcQuotaIdentity(t *testing.T) ProviderQuota {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte(`{"tokens":{"account_id":"quota-rpc-fixture"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return readQuotaIdentity("codex", root)
}

func rpcQuotaResponse(used float64, minutes int, reset time.Time) string {
	raw, _ := json.Marshal(map[string]any{"rateLimitsByLimitId": map[string]any{"codex": map[string]any{
		"limitId": "codex", "primary": map[string]any{"usedPercent": used, "windowDurationMins": minutes, "resetsAt": reset.Unix()},
	}}})
	return string(raw)
}

func TestCodexQuotaRPCReadsOnlyFixedMetadataMethods(t *testing.T) {
	q := rpcQuotaIdentity(t)
	logDir := t.TempDir()
	bin := testfixture.WriteCLI(t, "codex", testfixture.Spec{Mode: "codex-quota", LogDir: logDir,
		Stdout: rpcQuotaResponse(33, 10080, time.Now().Add(time.Hour))})
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	result, err := readCodexQuotaRPC(context.Background(), q)
	if err != nil || !result.HasData || result.WeeklyPct == nil || *result.WeeklyPct != 33 || !quotaMatches(q, result) {
		t.Fatalf("read-only quota response lost: %+v, %v", result, err)
	}
	methods, err := os.ReadFile(filepath.Join(logDir, "quota-methods.txt"))
	if err != nil || string(methods) != "initialize\ninitialized\naccount/rateLimits/read\n" {
		t.Fatalf("unexpected protocol methods: %q, %v", methods, err)
	}
}

func TestCodexQuotaRPCRefusesChangedLogin(t *testing.T) {
	q := rpcQuotaIdentity(t)
	bin := testfixture.WriteCLI(t, "codex", testfixture.Spec{Mode: "codex-quota",
		Stdout: rpcQuotaResponse(0, 43200, time.Now().Add(time.Hour)), StateFile: filepath.Join(q.CredentialRoot, "auth.json"),
		FirstError: `{"tokens":{"account_id":"different-rpc-fixture"}}`})
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	result, err := readCodexQuotaRPC(context.Background(), q)
	if err == nil || result.HasData {
		t.Fatalf("changed login authorized another account's quota: %+v, %v", result, err)
	}
}

func TestCodexQuotaRPCRefusesRestoredLoginAfterABASwitch(t *testing.T) {
	q := rpcQuotaIdentity(t)
	path := filepath.Join(q.CredentialRoot, "auth.json")
	auth, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	bin := testfixture.WriteCLI(t, "codex", testfixture.Spec{Mode: "codex-quota-aba",
		Stdout: rpcQuotaResponse(99, 300, time.Now().Add(time.Hour)), StateFile: path, FirstError: string(auth), RestoreMtime: old.UnixNano()})
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	result, err := readCodexQuotaRPC(context.Background(), q)
	if err == nil || result.HasData {
		t.Fatal("restored login bytes accepted another account's possible quota")
	}
}

func TestQuotaAuthVersionRequiresVerifiedGeneration(t *testing.T) {
	q := rpcQuotaIdentity(t)
	path := filepath.Join(q.CredentialRoot, "auth.json")
	stamp, err := readQuotaAuthVersion(path)
	if err != nil {
		t.Fatal(err)
	}
	if !quotaAuthVersionEqual(stamp, stamp) {
		t.Fatal("stable verified generation was rejected")
	}
	unknown := stamp
	unknown.known = false
	if quotaAuthVersionEqual(stamp, unknown) || quotaAuthVersionEqual(unknown, unknown) {
		t.Fatal("unavailable generation was accepted as equal")
	}
}

func TestCodexQuotaRPCDoesNotExecInUntrustedContext(t *testing.T) {
	q := rpcQuotaIdentity(t)
	marker := filepath.Join(t.TempDir(), "must-not-launch")
	bin := testfixture.WriteCLI(t, "codex", testfixture.Spec{Mode: "codex-quota", Sentinel: marker})
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	result, err := readCodexQuotaRPC(context.WithValue(context.Background(), quotaNoCLIContext{}, true), q)
	if err != nil || result.HasData {
		t.Fatalf("untrusted metadata result: %+v, %v", result, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("untrusted quota launched a CLI")
	}
}

func TestCodexQuotaRPCRequiresAdvertisedAppServer(t *testing.T) {
	q := rpcQuotaIdentity(t)
	bin := testfixture.WriteCLI(t, "codex", testfixture.Spec{Stdout: "Usage: old CLI [prompt]\n"})
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	result, err := readCodexQuotaRPC(context.Background(), q)
	if err == nil || result.HasData {
		t.Fatal("old CLI was treated as a safe protocol server")
	}
}

func TestCodexQuotaRPCCancellationStopsHelpChild(t *testing.T) {
	q := rpcQuotaIdentity(t)
	bin := testfixture.WriteCLI(t, "codex", testfixture.Spec{Sleep: 10 * time.Second})
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := readCodexQuotaRPC(ctx, q)
	if err == nil || result.HasData || time.Since(started) > 4*time.Second {
		t.Fatalf("cancelled quota read did not stop: %+v, %v", result, err)
	}
}

func TestCodexQuotaRPCRejectsOversizeProtocolResponse(t *testing.T) {
	q := rpcQuotaIdentity(t)
	bin := testfixture.WriteCLI(t, "codex", testfixture.Spec{Mode: "codex-quota",
		Stdout: `{"extra":"` + strings.Repeat("x", 2<<20) + `"}`})
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	result, err := readCodexQuotaRPC(context.Background(), q)
	if err == nil || result.HasData {
		t.Fatalf("oversize metadata was accepted: %+v, %v", result, err)
	}
}

func TestCodexQuotaRPCRejectsInvalidWindows(t *testing.T) {
	q := rpcQuotaIdentity(t)
	for _, tc := range []struct {
		used    float64
		minutes int
		reset   time.Time
	}{
		{-1, 300, time.Now().Add(time.Hour)}, {101, 300, time.Now().Add(time.Hour)},
		{100, 0, time.Now().Add(time.Hour)}, {100, 525601, time.Now().Add(time.Hour)},
		{100, 300, time.Now().Add(-time.Hour)},
	} {
		result, err := parseCodexQuotaRPC([]byte(rpcQuotaResponse(tc.used, tc.minutes, tc.reset)), q, time.Now())
		if err != nil || result.HasData || len(result.Windows) != 0 {
			t.Fatalf("invalid quota was retained: %+v, %v", result, err)
		}
	}
}

func TestCodexQuotaRPCPreservesActualWindowDuration(t *testing.T) {
	q := rpcQuotaIdentity(t)
	for _, minutes := range []int{15, 300, 10080, 43200} {
		result, err := parseCodexQuotaRPC([]byte(rpcQuotaResponse(33, minutes, time.Now().Add(time.Hour))), q, time.Now())
		if err != nil || len(result.Windows) != 1 || result.Windows[0].WindowMinutes != minutes {
			t.Fatalf("primary slot was given a guessed duration: %+v, %v", result, err)
		}
	}
}

func TestCodexQuotaRPCOpaqueBucketDoesNotBecomeAModelOrSharedLimit(t *testing.T) {
	q := rpcQuotaIdentity(t)
	var payload map[string]any
	_ = json.Unmarshal([]byte(rpcQuotaResponse(10, 10080, time.Now().Add(time.Hour))), &payload)
	buckets := payload["rateLimitsByLimitId"].(map[string]any)
	buckets["codex_other"] = map[string]any{"limitId": "codex_other", "primary": map[string]any{
		"usedPercent": 100, "windowDurationMins": 300, "resetsAt": time.Now().Add(time.Hour).Unix()}}
	raw, _ := json.Marshal(payload)
	result, err := parseCodexQuotaRPC(raw, q, time.Now())
	if err != nil || len(result.Windows) != 2 {
		t.Fatalf("opaque bucket lost: %+v, %v", result, err)
	}
	used, known := result.EffectiveUsedPct()
	if !known || used != 10 || result.ScopedPct != nil {
		t.Fatal("opaque bucket was promoted to a shared/model limit")
	}
	for _, window := range result.Windows {
		if window.LimitID == "codex_other" && (window.Scope != "unknown" || window.Model != "") {
			t.Fatal("opaque bucket got a guessed model")
		}
	}
	if strings.Contains(string(raw), q.AccountRef) {
		t.Fatal("fixture unexpectedly contains account information")
	}
}
