package router

import (
	"context"
	"testing"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/testfixture"
)

// GR-6: router tests used to leave fixture records in the user's
// ~/.cache/makewand/execution-evidence. One that records evidence without
// choosing a directory now writes into this binary's private directory.
func TestTestMainIsolatesExecutionEvidence(t *testing.T) {
	testfixture.GuardExecutionEvidence(t, writeGuardEvidence)
}

func writeGuardEvidence() error {
	attempt, err := execution.Reserve(context.Background(), execution.Metadata{Engine: "isolation-guard"})
	if err != nil {
		return err
	}
	if err := initializeCLIEvidence(attempt, "isolation-guard"); err != nil {
		return err
	}
	return attempt.Complete(execution.Outcome{Status: execution.Passed, Known: true})
}
