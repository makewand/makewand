package main

import (
	"os"
	"testing"

	"github.com/spf13/cobra"
)

// TestMain drops a parent provider-worker marker. The guard itself is covered
// by TestRefuseProviderWorkerDispatch; leaving MAKEWAND_PROVIDER_WORKER=1 set
// makes every in-process command refuse to start.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("MAKEWAND_PROVIDER_WORKER")
	os.Exit(m.Run())
}

func TestRefuseProviderWorkerDispatch(t *testing.T) {
	root := newRootCmd()
	find := func(args ...string) *cobra.Command {
		t.Helper()
		cmd, _, err := root.Find(args)
		if err != nil {
			t.Fatalf("find %v: %v", args, err)
		}
		return cmd
	}

	t.Setenv("MAKEWAND_PROVIDER_WORKER", "")
	if err := refuseProviderWorkerDispatch(find("chat")); err != nil {
		t.Fatalf("guard active without the marker: %v", err)
	}

	t.Setenv("MAKEWAND_PROVIDER_WORKER", "1")
	for _, refused := range [][]string{{}, {"chat"}, {"new"}, {"serve"}, {"preview"}, {"setup"}} {
		if err := refuseProviderWorkerDispatch(find(refused...)); err == nil {
			t.Errorf("%v allowed inside a provider worker", refused)
		}
	}
	for _, allowed := range [][]string{{"quota"}, {"state"}, {"usage"}, {"token", "list"}, {"state", "backup"}} {
		if err := refuseProviderWorkerDispatch(find(allowed...)); err != nil {
			t.Errorf("%v refused inside a provider worker: %v", allowed, err)
		}
	}
	doctor := find("doctor")
	if err := refuseProviderWorkerDispatch(doctor); err == nil {
		t.Error("doctor allowed inside a provider worker")
	}
	if err := doctor.Flags().Set("probe", "true"); err != nil {
		t.Fatal(err)
	}
	if err := refuseProviderWorkerDispatch(doctor); err == nil {
		t.Error("doctor --probe allowed inside a provider worker")
	}
}
