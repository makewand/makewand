package serveradmin

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/router"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverteam"
	"github.com/makewand/makewand/serverusage"
)

type aggregateOnlyUsage struct{ *serverusage.SQLiteStore }

func (aggregateOnlyUsage) Load(serverusage.Filter) ([]serverusage.Entry, error) {
	return nil, fmt.Errorf("summary must use SQL aggregation")
}

func adminUsageFixture(t *testing.T) (HandlerOptions, *serverusage.SQLiteStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	tokens, err := serverauth.OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tokens.Close() })
	teams, err := serverteam.OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { teams.Close() })
	for _, id := range []string{"org-a", "org-b"} {
		if _, err := teams.CreateOrganization(serverteam.Organization{ID: id, Name: id, MonthlyBudgetUSD: 100}); err != nil {
			t.Fatal(err)
		}
	}
	_, secret, err := tokens.Issue(serverauth.TokenRule{ID: "tenant-admin", Scopes: serverauth.AllScopes(), OrganizationID: "org-a"})
	if err != nil {
		t.Fatal(err)
	}
	usage, err := serverusage.OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usage.Close() })
	now := serverusage.MonthStart(time.Now().UTC()).Add(time.Hour)
	for i := 0; i < 150; i++ {
		if err := usage.Log(serverusage.Entry{Timestamp: now.Add(time.Duration(i) * time.Second), RequestID: fmt.Sprintf("visible-%03d", i), OrganizationID: "org-a", CostUSD: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := usage.Log(serverusage.Entry{Timestamp: now.Add(time.Duration(i) * time.Minute), RequestID: "private", OrganizationID: "org-b", CostUSD: 10000}); err != nil {
			t.Fatal(err)
		}
	}
	return HandlerOptions{Authorizer: tokens, TokenManager: tokens, UsageStore: usage, TeamStore: teams}, usage, secret
}

func invokeAdminUsage(h http.Handler, path, secret string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAdminUsagePaginationAndCSVRespectTenantScope(t *testing.T) {
	opts, _, secret := adminUsageFixture(t)
	h := NewHandler(opts)
	for _, test := range []struct {
		query               string
		want, limit, offset int
	}{{"", 100, 100, 0}, {"?limit=9999&offset=125&organization_id=org-b", 25, 1000, 125}} {
		rec := invokeAdminUsage(h, "/v1/admin/usage/events"+test.query, secret)
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data       []serverusage.Entry                          `json:"data"`
			Pagination struct{ Total, Limit, Offset, Returned int } `json:"pagination"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Data) != test.want || body.Pagination.Total != 150 || body.Pagination.Limit != test.limit || body.Pagination.Offset != test.offset || body.Pagination.Returned != test.want {
			t.Fatalf("unexpected pagination: %+v", body.Pagination)
		}
		for _, entry := range body.Data {
			if entry.OrganizationID != "org-a" {
				t.Fatal("foreign tenant event leaked")
			}
		}
	}
	rec := invokeAdminUsage(h, "/v1/admin/usage/events?format=csv&limit=5&offset=2", secret)
	rows, err := csv.NewReader(rec.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || len(rows) != 6 || rec.Header().Get("X-Total-Count") != "150" {
		t.Fatalf("CSV paging status/rows/total: %d/%d/%s", rec.Code, len(rows), rec.Header().Get("X-Total-Count"))
	}
	for _, query := range []string{"?offset=-1", "?limit=-1", "?offset=bad"} {
		if rec := invokeAdminUsage(h, "/v1/admin/usage/events"+query, secret); rec.Code != 400 {
			t.Fatalf("invalid paging accepted: %s", query)
		}
	}
}

func TestAdminUsageSummaryBillingAndDashboardUseSQLAggregates(t *testing.T) {
	opts, usage, secret := adminUsageFixture(t)
	opts.UsageStore = aggregateOnlyUsage{usage}
	h := NewHandler(opts)
	for _, path := range []string{"/v1/admin/usage/summary", "/v1/admin/billing/summary", "/v1/admin/billing/periods", "/v1/admin/billing/alerts", "/v1/admin/dashboard"} {
		rec := invokeAdminUsage(h, path+"?organization_id=org-b", secret)
		if rec.Code != 200 {
			t.Fatalf("%s must use scoped SQL aggregation: %d %s", path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "org-b") || strings.Contains(rec.Body.String(), "10000") {
			t.Fatalf("foreign tenant aggregate leaked: %s", rec.Body.String())
		}
	}
}

func TestAdminPasswordLimitBeforeCredentialRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.db")
	tokens, err := serverauth.OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer tokens.Close()
	_, secret, err := tokens.Issue(serverauth.TokenRule{Scopes: serverauth.AllScopes()})
	if err != nil {
		t.Fatal(err)
	}
	users := router.NewUserStore(t.TempDir())
	user, err := users.CreateUser("member@example.invalid", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(HandlerOptions{Authorizer: tokens, TokenManager: tokens, UserStore: users})
	req := httptest.NewRequest("POST", "/v1/admin/users/"+user.ID+"/password", strings.NewReader(`{"password":"`+strings.Repeat("x", 1025)+`"}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("oversized password status=%d", rec.Code)
	}
	current, err := users.GetUserByID(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.ValidatePassword("test-password") {
		t.Fatal("invalid reset mutated credentials")
	}
}
