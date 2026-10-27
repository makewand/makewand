package execution

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ledgerRegistryName mirrors makewand/ledger_registry.py: admission notes every
// ledger other than the default one so `makewand observe` can show calls the
// default ledger never sees. Recording never affects admission.
const ledgerRegistryName = "ledger-registry.jsonl"

var notedLedgers sync.Map

func resolvedLedgerPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func noteLedger(path string) {
	resolved := resolvedLedgerPath(path)
	if resolved == "" {
		return
	}
	defaultPath, err := DefaultBudgetPath()
	if err != nil || resolved == resolvedLedgerPath(defaultPath) {
		return
	}
	if _, seen := notedLedgers.LoadOrStore(resolved, true); seen {
		return
	}
	line, err := json.Marshal(map[string]any{"path": resolved, "ts": float64(time.Now().UnixNano()) / 1e9})
	if err != nil || len(line) >= 4096 {
		return
	}
	registry := filepath.Join(filepath.Dir(defaultPath), ledgerRegistryName)
	f, err := os.OpenFile(registry, accountingOpenFlags(os.O_WRONLY|os.O_APPEND|os.O_CREATE), 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}
