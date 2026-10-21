package tui

import (
	"context"
	"testing"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/testfixture"
)

// GEC-1021-4: TUI tests used to write fixture records (for example a gemini
// cli_json PASSED record) into the user's ~/.cache/makewand/execution-evidence.
// One that records evidence without choosing a directory now writes into this
// binary's private directory.
func TestTestMainIsolatesExecutionEvidence(t *testing.T) {
	testfixture.GuardExecutionEvidence(t, func() error {
		attempt, err := execution.Reserve(context.Background(), execution.Metadata{Engine: "isolation-guard"})
		if err != nil {
			return err
		}
		if err := attempt.UpdateEvidence(func(*execution.ProviderEvidence) {}); err != nil {
			return err
		}
		return attempt.Complete(execution.Outcome{Status: execution.Passed, Known: true})
	})
}
