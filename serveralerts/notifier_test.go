package serveralerts

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/makewand/makewand/serverteam"
	"github.com/makewand/makewand/serverusage"
)

func TestWebhookNotifier_DedupesBySeverityPerMonth(t *testing.T) {
	stateDB := filepath.Join(t.TempDir(), "state.db")
	teamStore, err := serverteam.OpenSQLiteStore(stateDB)
	if err != nil {
		t.Fatalf("OpenSQLiteStore(team): %v", err)
	}
	defer teamStore.Close()
	usageStore, err := serverusage.OpenSQLiteStore(stateDB)
	if err != nil {
		t.Fatalf("OpenSQLiteStore(usage): %v", err)
	}
	defer usageStore.Close()

	org, err := teamStore.CreateOrganization(serverteam.Organization{
		ID:               "org_1",
		Name:             "Platform",
		MonthlyBudgetUSD: 100,
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	project, err := teamStore.CreateProject(serverteam.Project{
		ID:               "prj_1",
		OrganizationID:   org.ID,
		Name:             "Checkout",
		MonthlyBudgetUSD: 100,
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	var notifications []Notification
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer req.Body.Close()
		var payload Notification
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatalf("Decode webhook payload: %v", err)
		}
		notifications = append(notifications, payload)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	notifier, err := OpenWebhookNotifier(server.URL, filepath.Join(t.TempDir(), "alert_state.json"), usageStore, teamStore)
	if err != nil {
		t.Fatalf("OpenWebhookNotifier: %v", err)
	}

	now := serverusage.MonthStart(time.Now().UTC()).Add(2 * time.Hour)
	for i, cost := range []float64{50, 35, 1, 10, 10} {
		entry := serverusage.Entry{
			Timestamp:      now.Add(time.Duration(i) * time.Minute),
			RequestID:      "req_" + string(rune('a'+i)),
			OrganizationID: org.ID,
			ProjectID:      project.ID,
			CostUSD:        cost,
		}
		usageStore.Log(entry)
		notifier.Log(entry)
	}

	if len(notifications) != 6 {
		t.Fatalf("len(notifications) = %d, want 6 (warning/high/critical for project and org)", len(notifications))
	}
	if notifications[0].Alert.Severity != "warning" || notifications[len(notifications)-1].Alert.Severity != "critical" {
		t.Fatalf("unexpected severities: %+v", notifications)
	}
}
