package main

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "--child") {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestServerRecoveryDrillShort(t *testing.T) {
	if testing.Short() {
		t.Skip("real child processes and HTTP recovery drill")
	}
	if runtime.GOOS == "windows" {
		t.Skip("server recovery drill requires POSIX process signals")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r := report{}
	err := runDrill(ctx, options{Requests: 40, Concurrency: 4, Tenants: 2, RouterCalls: 1000}, &r)
	if err != nil {
		t.Fatalf("drill failed: %v; checks=%+v", err, r.Checks)
	}
	if r.Recovery.RPOAcknowledgedLost != 0 || r.HTTP.Unexpected != 0 {
		t.Fatalf("lost acknowledged state or unexpected HTTP errors: %+v", r)
	}
}
