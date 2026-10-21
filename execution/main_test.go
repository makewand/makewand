package execution

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/makewand/makewand/internal/testfixture"
)

// TestMain keeps the evidence these tests record out of the user cache and
// drops accounting paths inherited from the shell. Tests that inspect records
// still choose their own directory.
func TestMain(m *testing.M) {
	cleanup, err := testfixture.IsolateExecutionEvidence()
	if err != nil {
		fmt.Fprintln(os.Stderr, "execution evidence isolation:", err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// GR-6/GEC-1021-4: a test that records evidence without choosing a directory
// writes into this binary's private directory, never the user's cache.
func TestTestMainIsolatesExecutionEvidence(t *testing.T) {
	testfixture.GuardExecutionEvidence(t, func() error {
		attempt, err := Reserve(context.Background(), Metadata{Engine: "isolation-guard"})
		if err != nil {
			return err
		}
		if err := attempt.UpdateEvidence(func(*ProviderEvidence) {}); err != nil {
			return err
		}
		return attempt.Complete(Outcome{Status: Passed, Known: true})
	})
}
