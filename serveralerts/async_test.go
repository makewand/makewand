package serveralerts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/makewand/makewand/serverteam"
	"github.com/makewand/makewand/serverusage"
)

func alertFixture(t *testing.T) (*serverteam.SQLiteStore, *serverusage.SQLiteStore) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	teams, err := serverteam.OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { teams.Close() })
	usage, err := serverusage.OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usage.Close() })
	return teams, usage
}

func TestWebhookRetriesDedupesAndDrains(t *testing.T) {
	teams, usage := alertFixture(t)
	org, err := teams.CreateOrganization(serverteam.Organization{ID: "org", Name: "Organization", MonthlyBudgetUSD: 100})
	if err != nil {
		t.Fatal(err)
	}
	entry := serverusage.Entry{Timestamp: time.Now().UTC(), OrganizationID: org.ID, CostUSD: 85}
	if err := usage.Log(entry); err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Idempotency-Key") == "" {
			t.Error("retry missing idempotency key")
		}
		if attempts.Add(1) == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(202)
		}
	}))
	defer server.Close()
	n, err := OpenWebhookNotifierWithOptions(server.URL, "", usage, teams, NotifierOptions{MaxAttempts: 2, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := n.Log(entry); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := n.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := n.Log(entry); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("delivery count=%d, want one retry without duplicates", attempts.Load())
	}
	// Closing is idempotent and Log cannot send on a closed queue.
	if err := n.Log(entry); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookQueueBoundDoesNotBlockUsage(t *testing.T) {
	teams, usage := alertFixture(t)
	entries := make([]serverusage.Entry, 3)
	for i, id := range []string{"org1", "org2", "org3"} {
		_, err := teams.CreateOrganization(serverteam.Organization{ID: id, Name: id, MonthlyBudgetUSD: 100})
		if err != nil {
			t.Fatal(err)
		}
		entries[i] = serverusage.Entry{Timestamp: time.Now().UTC(), OrganizationID: id, CostUSD: 85}
		if err := usage.Log(entries[i]); err != nil {
			t.Fatal(err)
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		w.WriteHeader(202)
	}))
	defer server.Close()
	failures := make(chan error, 2)
	n, err := OpenWebhookNotifierWithOptions(server.URL, "", usage, teams, NotifierOptions{QueueSize: 1, Timeout: time.Second, ErrorHandler: func(err error) { failures <- err }})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Log(entries[0]); err != nil {
		t.Fatal(err)
	}
	<-entered
	started := time.Now()
	if err := n.Log(entries[1]); err != nil {
		t.Fatal(err)
	}
	if err := n.Log(entries[2]); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Error("slow webhook blocked usage observer")
	}
	select {
	case <-failures:
	default:
		t.Error("queue full was not observed")
	}
	close(release)
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("accepted delivery count=%d", calls.Load())
	}
}
