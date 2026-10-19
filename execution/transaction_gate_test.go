package execution

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestTransactionGateCancellationAndIndependentPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.json")
	leave, err := enterTransaction(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer leave()
	// A busy ledger must not block a transaction for another ledger.
	if err := withLockedFile(t.Context(), path+"-other", nil, func(*accountingPath) error { return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- withLockedFile(ctx, path, nil, func(*accountingPath) error {
			t.Error("canceled waiter entered the transaction")
			return nil
		})
	}()
	cancel()
	// The owner still holds the gate: a waiter must honor cancellation
	// without waiting for the owner or opening the ledger's sidecar.
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued transaction: %v", err)
	}
}
