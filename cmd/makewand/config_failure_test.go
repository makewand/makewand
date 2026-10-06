package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
)

func TestDoctorInvalidPolicyStopsBeforeRoutingAndProbes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"active_providers":[]}X`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, loadErr := config.LoadWithOptions(config.LoadOptions{SkipCLIDetection: true})
	if !config.IsFatalLoadError(loadErr) {
		t.Fatalf("expected fatal policy error, got %v", loadErr)
	}
	report, failures, _ := runDoctor(cfg, loadErr, doctorOptions{modes: []model.UsageMode{model.ModeFast, model.ModeBalanced}, probe: true, probeTimeout: time.Second})
	if failures != 1 || len(report.ModeCoverage) != 0 || len(report.LiveProbe) != 0 || len(report.DetectedProviders) != 0 {
		t.Fatalf("invalid policy attempted coverage or live probes: %+v", report)
	}
	if len(report.Checks) != 1 || report.Checks[0].Name != "config load" || report.Checks[0].Status != doctorFail {
		t.Fatalf("invalid policy was not reported as a failed check: %+v", report.Checks)
	}
}
