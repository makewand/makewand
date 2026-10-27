package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/makewand/makewand/internal/testfixture"
)

// TestMain keeps execution evidence, inherited accounting paths and the
// ledger registry of these tests out of the user's cache and configuration.
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
