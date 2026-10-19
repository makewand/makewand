package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/testfixture"
	"github.com/makewand/makewand/router"
	"github.com/spf13/cobra"
)

type staticQuotaSource struct{ q router.ProviderQuota }

func (s staticQuotaSource) Provider() string                                   { return s.q.Provider }
func (s staticQuotaSource) Read(context.Context) (router.ProviderQuota, error) { return s.q, nil }

func TestQuotaPresentationIncludesAccountAndThirtyDayWindow(t *testing.T) {
	pct := 70.0
	at := time.Now().UTC()
	q := router.ProviderQuota{Provider: "codex", IdentityKnown: true,
		AccountRef: "codex:account:fake-hash", PoolRef: "codex:account:fake-hash:subscription",
		CredentialRoot: "/fake/selected", SourceAt: at, ThirtyDayPct: &pct, HasData: true,
		Windows: []router.QuotaWindow{{PoolRef: "codex:account:fake-hash:subscription", WindowMinutes: 43200, UsedPercent: pct, SourceAt: at}}}
	snap := router.NewQuotaSnapshotter(time.Hour, staticQuotaSource{q}).Refresh(context.Background())
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := printQuotaJSON(cmd, snap); err != nil {
		t.Fatal(err)
	}
	var raw map[string]map[string]any
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	entry := raw["codex"]
	if entry["identity_known"] != true || entry["account_ref"] != q.AccountRef || entry["pool_ref"] != q.PoolRef || entry["thirty_day_pct"] != pct || entry["credential_root"] != q.CredentialRoot || entry["windows"] == nil {
		t.Fatalf("quota JSON lost identity or windows: %+v", entry)
	}
	out.Reset()
	printQuotaHuman(cmd, snap, router.DefaultQuotaPolicy())
	if !bytes.Contains(out.Bytes(), []byte("30 days")) {
		t.Fatal("30-day usage missing")
	}
	q.IdentityKnown, q.PoolRef, q.AccountRef, q.HasData = false, "", "codex:unknown:fake-root", false
	snap = router.NewQuotaSnapshotter(time.Hour, staticQuotaSource{q}).Refresh(context.Background())
	out.Reset()
	printQuotaHuman(cmd, snap, router.DefaultQuotaPolicy())
	if !bytes.Contains(out.Bytes(), []byte("shared pool unknown")) {
		t.Fatal("unknown identity looks like an independent pool")
	}
}

// TestQuotaUntrustedSkipsLocalCLISource verifies that `makewand quota
// --repo-trust=untrusted` never execs a local-CLI quota source: the fake `agy`
// binary's sentinel is not written. The trusted run proves the fixture is
// otherwise reachable (it is exec'd and writes the sentinel), so the untrusted
// skip is meaningful rather than a false pass.
func TestQuotaUntrustedSkipsLocalCLISource(t *testing.T) {
	binDir := t.TempDir()

	// A native fixture proves the trusted source is reachable on every OS.
	sentinel := filepath.Join(binDir, "agy_models_ran")
	testfixture.WriteCLIAt(t, binDir, "agy", testfixture.Spec{Stdout: "model-a\nmodel-b\n", Sentinel: sentinel})

	run := func(t *testing.T, trust string) {
		t.Helper()
		// Fresh HOME so the shared quota disk cache is empty (a cached snapshot is
		// read-through and would bypass the sources entirely).
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
		t.Setenv("PATH", binDir)
		_ = os.Remove(sentinel)

		cmd := newRootCmd()
		cmd.SetArgs([]string{"quota", "--repo-trust=" + trust})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("quota --repo-trust=%s error: %v", trust, err)
		}
	}

	t.Run("untrusted does not exec local CLI", func(t *testing.T) {
		run(t, "untrusted")
		if _, err := os.Stat(sentinel); err == nil {
			t.Fatalf("local-CLI quota source was executed in untrusted mode (sentinel %q exists)", sentinel)
		}
	})

	t.Run("trusted execs local CLI", func(t *testing.T) {
		run(t, "trusted")
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("trusted quota did not exec the local-CLI source (sentinel %q missing): %v", sentinel, err)
		}
	})
}

func TestQuotaDisabledProviderSkipsCLIAndCachedData(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "agy-executed")
	testfixture.WriteCLIAt(t, dir, "agy", testfixture.Spec{Stdout: "model-a\n", Sentinel: sentinel})
	t.Setenv("PATH", dir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MAKEWAND_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"enabled_providers":{"google":false,"claude":false,"codex":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	command := newRootCmd()
	command.SetArgs([]string{"quota", "--json"})
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("quota probed disabled provider", err)
	}
}

func TestR6QuotaSealResetIsVisible(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour)
	observed := time.Now().Add(6 * 24 * time.Hour)
	pct := 20.0
	snapshotter := router.NewQuotaSnapshotter(time.Hour, staticQuotaSource{router.ProviderQuota{
		Provider: "seal-fixture", HasData: true, WeeklyPct: &pct, WeeklyResetAt: observed,
	}})
	snapshotter.Refresh(context.Background())
	snapshotter.MarkExhausted("seal-fixture", reset)
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	printQuotaHuman(cmd, snapshotter.Snapshot(), router.DefaultQuotaPolicy())
	if !bytes.Contains(out.Bytes(), []byte(resetSuffix(reset))) {
		t.Fatalf("429 seal expiry missing: %s", out.String())
	}
}
