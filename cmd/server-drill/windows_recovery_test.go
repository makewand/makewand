//go:build windows

package main

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/serverdb"
)

// TestWindowsServerCrashRecoveryWALBudgets exercises abrupt process loss rather
// than POSIX graceful shutdown. The only provider is the in-process local
// fixture; all HTTP and SQLite state belongs to this test's private directory.
func TestWindowsServerCrashRecoveryWALBudgets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	directory := t.TempDir()
	profile := options{Tenants: 2, BudgetCalls: 2}
	child, err := startChild(ctx, profile, directory)
	if err != nil {
		t.Fatalf("start original child: %v", err)
	}
	defer child.kill()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	acknowledged := make(map[string]int)
	for tenant := 0; tenant < profile.Tenants; tenant++ {
		for call := 0; call < profile.BudgetCalls; call++ {
			id := "windows-ack-" + strconv.Itoa(tenant) + "-" + strconv.Itoa(call)
			status, body, err := post(ctx, client, child.url, "drill-token-"+strconv.Itoa(tenant), id, false, id)
			if err != nil || status != http.StatusOK || !strings.Contains(body, id) {
				t.Fatalf("acknowledgment %s: status=%d body=%q err=%v", id, status, body, err)
			}
			acknowledged[id] = tenant
		}
	}

	path := filepath.Join(directory, "state.db")
	db, err := serverdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func(opened *sql.DB) { _ = opened.Close() }(db)
	var mode string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || !strings.EqualFold(mode, "wal") {
		_ = db.Close()
		t.Fatalf("expected actual SQLite WAL: mode=%q err=%v", mode, err)
	}
	assertWindowsDrillAcknowledgments(t, ctx, db, acknowledged)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	wal, err := os.Stat(path + "-wal")
	if err != nil || wal.Size() == 0 {
		t.Fatalf("real child must retain nonempty WAL before crash: info=%v err=%v", wal, err)
	}

	// Admit a real still-running request and observe its durable reservation
	// before terminating the child. A killed request must retain conservative
	// accounting; it must not become a completed/refunded operation on restart.
	const inflightID = "windows-crash-inflight"
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		_, _, _ = post(ctx, client, child.url, "drill-control", "slow:crash", false, inflightID)
	}()
	if err = waitActive(ctx, client, child.url); err != nil {
		t.Fatalf("admitted provider request: %v", err)
	}
	reservationID := assertWindowsDrillPendingReservation(t, ctx, path, "")
	if reservationID == inflightID {
		t.Fatal("durable reservation identity must be independent of the client request ID")
	}
	t.Logf("real loopback acknowledgments=%d WAL bytes=%d; active provider and durable pending reservation observed before hard kill", len(acknowledged), wal.Size())
	child.kill()
	if child.cmd.ProcessState == nil || child.cmd.ProcessState.Success() {
		t.Fatal("owned child did not end through actual abrupt process termination")
	}
	select {
	case <-requestDone:
	case <-time.After(10 * time.Second):
		t.Fatal("killed child left the loopback request blocked")
	}

	restarted, err := startChild(ctx, profile, directory)
	if err != nil {
		t.Fatalf("restart same private state: %v", err)
	}
	defer restarted.kill()
	db, err = serverdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func(opened *sql.DB) { _ = opened.Close() }(db)
	assertWindowsDrillAcknowledgments(t, ctx, db, acknowledged)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	assertWindowsDrillPendingReservation(t, ctx, path, reservationID)
	for tenant := 0; tenant < profile.Tenants; tenant++ {
		status, body, err := post(ctx, client, restarted.url, "drill-token-"+strconv.Itoa(tenant), "after-crash", false)
		if err != nil || status != http.StatusTooManyRequests {
			t.Fatalf("tenant %d lost settled hard budget on restart: status=%d body=%q err=%v", tenant, status, body, err)
		}
	}
	status, body, err := post(ctx, client, restarted.url, "drill-control", "independent-control-after-crash", false)
	if err != nil || status != http.StatusOK || !strings.Contains(body, "independent-control-after-crash") {
		t.Fatalf("exhausted tenant budgets leaked to independent control token: status=%d body=%q err=%v", status, body, err)
	}
	status, body, err = post(ctx, client, restarted.url, "drill-not-issued", "unauthorized", false)
	if err != nil || status != http.StatusUnauthorized {
		t.Fatalf("restart lost token authorization boundary: status=%d body=%q err=%v", status, body, err)
	}
	t.Log("acknowledged rows retained exactly; tenant attribution, exhausted budgets, uncertain reservation, and independent authorization survived real process crash")
}

func assertWindowsDrillAcknowledgments(t *testing.T, ctx context.Context, db *sql.DB, acknowledged map[string]int) {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT request_id,token_id,organization_id,project_id,status,cost_micro_usd FROM usage_entries WHERE request_id LIKE 'windows-ack-%' ORDER BY request_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := make(map[string]int)
	for rows.Next() {
		var id, token, org, project string
		var status int
		var cost int64
		if err = rows.Scan(&id, &token, &org, &project, &status, &cost); err != nil {
			t.Fatal(err)
		}
		tenant, ok := acknowledged[id]
		if !ok || token != "token-"+strconv.Itoa(tenant) || org != "org-"+strconv.Itoa(tenant) || project != "project-"+strconv.Itoa(tenant) || status != http.StatusOK || cost != 10000 {
			t.Fatalf("acknowledged row changed identity or accounting: id=%s token=%s org=%s project=%s status=%d cost=%d", id, token, org, project, status, cost)
		}
		found[id]++
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(found) != len(acknowledged) {
		t.Fatalf("acknowledged rows lost at crash barrier: found=%d acknowledged=%d", len(found), len(acknowledged))
	}
	for id := range acknowledged {
		if found[id] != 1 {
			t.Fatalf("acknowledged request %s retained %d times, want exactly once", id, found[id])
		}
	}
}

func assertWindowsDrillPendingReservation(t *testing.T, ctx context.Context, path, expectedID string) string {
	t.Helper()
	db, err := serverdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var count int
	var id string
	err = db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(r.request_id),'') FROM budget_reservations r JOIN budget_reservation_scopes s ON s.request_id=r.request_id WHERE r.state='pending' AND r.estimate_micro_usd=10000 AND s.kind='token_month' AND s.scope_id='control'`).Scan(&count, &id)
	if err != nil || count != 1 || id == "" || expectedID != "" && id != expectedID {
		t.Fatalf("real uncertain request lost its exact conservative durable reservation: count=%d same_id=%t err=%v", count, expectedID == "" || id == expectedID, err)
	}
	return id
}
