package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/servermetrics"
	"github.com/makewand/makewand/serverusage"
)

func TestServeReadinessAndDatabaseMetrics(t *testing.T) {
	usage, err := serverusage.OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	metrics := servermetrics.NewRecorder()
	check := serveReadinessCheck(usage, metrics)
	if check == nil {
		t.Fatal("SQLite readiness check was not wired")
	}
	if err := check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metrics.RenderPrometheus(), "makewand_db_connections") {
		t.Fatal("database pressure metrics were not wired")
	}
	if err := usage.Close(); err != nil {
		t.Fatal(err)
	}
	if err := check(context.Background()); err == nil {
		t.Fatal("closed ledger still ready")
	}
	if !strings.Contains(metrics.RenderPrometheus(), `makewand_errors_total{kind="db",provider=""} 1`) {
		t.Fatal("database failure was not observed")
	}
}
