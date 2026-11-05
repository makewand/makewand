package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/quota"
)

func TestParseArgsAcceptsTheFormsItsCallersUse(t *testing.T) {
	for _, args := range [][]string{
		{"quota"},
		{"quota", "--json"},
		{"--repo-trust", "untrusted", "quota"},
		{"quota", "--repo-trust=trusted", "--timeout", "5s"},
	} {
		opts, err := parseArgs(args)
		if err != nil || !opts.quota {
			t.Errorf("parseArgs(%v) = %+v, %v", args, opts, err)
		}
	}
	opts, err := parseArgs([]string{"--repo-trust=untrusted", "quota", "--json", "--timeout=2m"})
	if err != nil || opts.repoTrust != quota.RepoTrustUntrusted || !opts.jsonOutput || opts.timeout != 2*time.Minute {
		t.Errorf("parsed %+v, %v", opts, err)
	}
}

func TestParseArgsRefusesAnythingElse(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"serve"},
		{"chat"},
		{"quota", "--timeout", "0s"},
		{"quota", "--timeout"},
		{"quota", "--repo-trust", "maybe"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%v) should fail", args)
		}
	}
}

func TestVersionMatchesWhatTheInstallerChecks(t *testing.T) {
	var out, errs bytes.Buffer
	if code := run(context.Background(), []string{"--version"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if !strings.HasPrefix(out.String(), "makewand version ") {
		t.Errorf("version output %q", out.String())
	}
}

func TestProviderEnabledHonorsDisable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", dir)
	if !providerEnabled("claude") {
		t.Error("a missing config must leave providers on")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"enabled_providers":{"claude":false,"agy":false,"codex":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"claude": false, "gemini": false, "codex": true, "grok": true} {
		if providerEnabled(name) != want {
			t.Errorf("providerEnabled(%q) != %v", name, want)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !providerEnabled("claude") {
		t.Error("a damaged config must leave providers on")
	}
}

func TestQuotaJSONKeepsTheFieldsPythonReads(t *testing.T) {
	used := 37.0
	snap := quota.NewQuotaSnapshotter(time.Hour, staticSource{quota.ProviderQuota{
		Provider: "codex", WeeklyPct: &used, Authed: true, HasData: true, AccountRef: "a", IdentityKnown: true, PoolRef: "p",
	}}).Refresh(context.Background())
	var out bytes.Buffer
	if err := printQuotaJSON(&out, snap); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	entry := decoded["codex"]
	if entry["weekly_pct"] != 37.0 || entry["account_ref"] != "a" || entry["has_data"] != true || entry["band"] == nil {
		t.Errorf("entry %v", entry)
	}
}

type staticSource struct{ q quota.ProviderQuota }

func (s staticSource) Provider() string { return s.q.Provider }
func (s staticSource) Read(context.Context) (quota.ProviderQuota, error) {
	return s.q, nil
}
