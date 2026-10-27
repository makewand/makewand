package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReserveRegistersNonDefaultLedgerOnce(t *testing.T) {
	config := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", config)
	notedLedgers.Clear()
	t.Cleanup(notedLedgers.Clear)
	own := filepath.Join(t.TempDir(), "call-budget.json")
	ctx := ContextWithConfig(context.Background(), Config{LedgerPath: own, MaxCalls: 3})
	for range 2 {
		attempt, err := Reserve(ctx, Metadata{Engine: "registry-test"})
		if err != nil {
			t.Fatal(err)
		}
		if err := attempt.Complete(Outcome{Status: Passed, Known: true}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(config, ledgerRegistryName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("want one registration, got %q", raw)
	}
	var row struct {
		Path string  `json:"path"`
		TS   float64 `json:"ts"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil || row.Path != resolvedLedgerPath(own) || row.TS <= 0 {
		t.Fatalf("bad registration %q: %v", lines[0], err)
	}
}

func TestDefaultLedgerIsNotRegistered(t *testing.T) {
	config := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", config)
	notedLedgers.Clear()
	t.Cleanup(notedLedgers.Clear)
	noteLedger(filepath.Join(config, "call-budget.json"))
	if _, err := os.Stat(filepath.Join(config, ledgerRegistryName)); !os.IsNotExist(err) {
		t.Fatalf("default ledger registered: %v", err)
	}
}
