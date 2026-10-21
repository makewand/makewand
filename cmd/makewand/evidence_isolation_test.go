package main

import (
	"context"
	"testing"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/testfixture"
)

// GEC-1021-4: a test that records evidence without choosing a directory
// writes into this binary's private directory, never the user's cache.
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
