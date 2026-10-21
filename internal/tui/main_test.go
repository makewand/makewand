package tui

import (
	"fmt"
	"os"
	"testing"

	"github.com/makewand/makewand/internal/testfixture"
)

// TestMain isolates all config-dir-backed state (routing stats, chat sessions,
// and the monthly budget ledger) from the developer's real ~/.config/makewand
// during tests. Without this, any test that drives a cost-recording path would
// write the shared monthly_spend.json. Individual tests may still override
// MAKEWAND_CONFIG_DIR via t.Setenv for finer isolation. Fixture dispatches
// record provider evidence, which likewise stays out of the user cache.
func TestMain(m *testing.M) {
	cleanupEvidence, err := testfixture.IsolateExecutionEvidence()
	if err != nil {
		fmt.Fprintln(os.Stderr, "execution evidence isolation:", err)
		os.Exit(1)
	}
	code := runIsolatedConfig(m)
	cleanupEvidence()
	os.Exit(code)
}

func runIsolatedConfig(m *testing.M) int {
	if os.Getenv("MAKEWAND_CONFIG_DIR") == "" {
		dir, err := os.MkdirTemp("", "makewand-tui-test-")
		if err == nil {
			_ = os.Setenv("MAKEWAND_CONFIG_DIR", dir)
			defer func() { _ = os.RemoveAll(dir) }()
		}
	}
	return m.Run()
}
