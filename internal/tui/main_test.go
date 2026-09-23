package tui

import (
	"os"
	"testing"
)

// TestMain isolates all config-dir-backed state (routing stats, chat sessions,
// and the monthly budget ledger) from the developer's real ~/.config/makewand
// during tests. Without this, any test that drives a cost-recording path would
// write the shared monthly_spend.json. Individual tests may still override
// MAKEWAND_CONFIG_DIR via t.Setenv for finer isolation.
func TestMain(m *testing.M) {
	if os.Getenv("MAKEWAND_CONFIG_DIR") == "" {
		dir, err := os.MkdirTemp("", "makewand-tui-test-")
		if err == nil {
			_ = os.Setenv("MAKEWAND_CONFIG_DIR", dir)
			code := m.Run()
			_ = os.RemoveAll(dir)
			os.Exit(code)
		}
	}
	os.Exit(m.Run())
}
