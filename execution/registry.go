package execution

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ledgerRegistryName mirrors makewand/ledger_registry.py: admission notes every
// ledger other than the default one, after releasing the ledger lock, so
// `makewand observe` can show calls the default ledger never sees. Recording
// never affects admission. Unlike Python it prints nothing: the TUI owns the
// terminal.
const ledgerRegistryName = "ledger-registry.jsonl"

var notedLedgers sync.Map

// resolvedLedgerPath canonicalizes like Python's Path.resolve(): a leaf that
// does not exist yet still gets its directory's symlinks resolved.
func resolvedLedgerPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(dir, filepath.Base(abs))
	}
	return abs
}

func noteLedger(path string) {
	resolved := resolvedLedgerPath(path)
	if resolved == "" {
		return
	}
	if _, seen := notedLedgers.Load(resolved); seen {
		return
	}
	defaultPath, err := DefaultBudgetPath()
	if err != nil || resolved == resolvedLedgerPath(defaultPath) {
		return
	}
	line, err := json.Marshal(map[string]any{"path": resolved, "ts": float64(time.Now().UnixNano()) / 1e9})
	if err != nil || len(line) >= 4096 {
		return
	}
	dir := filepath.Dir(defaultPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	// accountingOpenFlags: no symlink (Unix) or reparse point (Windows), no FIFO wait.
	f, err := os.OpenFile(filepath.Join(dir, ledgerRegistryName), accountingOpenFlags(os.O_WRONLY|os.O_APPEND|os.O_CREATE), 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() || protectExecutionFile(f) != nil {
		return
	}
	if _, err := f.Write(append(line, '\n')); err == nil {
		notedLedgers.Store(resolved, true) // only once written, so a later admission retries
	}
}
