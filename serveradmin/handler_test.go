package serveradmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/router"
	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverteam"
	"github.com/makewand/makewand/serverusage"
)

func TestHandler_TokenLifecycleAndAuditQueries(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "server_auth.json")
	if err := serverauth.SaveConfigFile(authPath, serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:     "admin",
				Token:  "admin-secret",
				Scopes: serverauth.AllScopes(),
			},
		},
	}); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}
	manager, err := serverauth.LoadManager(authPath)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}

	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	logger, err := serveraudit.OpenJSONL(auditPath)
	if err != nil {
		t.Fatalf("OpenJSONL: %v", err)
	}
	logger.Log(serveraudit.Event{
		Timestamp:        time.Date(2026, 3, 23, 0, 0, 0, 0, time.UTC),
		Kind:             "chat",
		TokenID:          "runner",
		Status:           200,
		ActualProvider:   "codex",
		PromptTokens:     10,
		CompletionTokens: 5,
		CostUSD:          0.25,
	})
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	usagePath := filepath.Join(t.TempDir(), "usage.jsonl")
	usageLogger, err := serverusage.OpenJSONL(usagePath)
	if err != nil {
		t.Fatalf("OpenJSONL(usage): %v", err)
	}
	usageLogger.Log(serverusage.Entry{
		Timestamp:        time.Date(2026, 3, 23, 1, 0, 0, 0, time.UTC),
		TokenID:          "runner",
		ActualProvider:   "codex",
		Status:           200,
		PromptTokens:     12,
		CompletionTokens: 8,
		CostUSD:          0.5,
	})
	if err := usageLogger.Close(); err != nil {
		t.Fatalf("Close(usage): %v", err)
	}
	userStore := router.NewUserStore(filepath.Join(t.TempDir(), "users"))
	user, err := userStore.CreateUser("person@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	handler := NewHandler(HandlerOptions{
		Authorizer:   manager,
		TokenManager: manager,
		AuditPath:    auditPath,
		UsagePath:    usagePath,
		UserStore:    userStore,
	})

	createBody := []byte(`{"id":"runner","allowed_providers":["codex"],"allowed_modes":["balanced"],"workspace_prefixes":["repo-"]}`)
	createReq := httptest.NewRequest(http.MethodPost, "/v1/admin/tokens", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer admin-secret")
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body: %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		TokenID string                   `json:"token_id"`
		Token   string                   `json:"token"`
		Rule    serverauth.TokenRuleView `json:"rule"`
	}
	if err := json.NewDecoder(createRec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.TokenID != "runner" {
		t.Fatalf("created.TokenID = %q, want runner", created.TokenID)
	}
	if created.Token == "" {
		t.Fatal("created.Token = empty")
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v1/admin/tokens", nil)
	listReq.Header.Set("Authorization", "Bearer admin-secret")
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body: %s", listRec.Code, listRec.Body.String())
	}
	if bytes.Contains(listRec.Body.Bytes(), []byte(created.Token)) {
		t.Fatalf("list response leaked raw token: %s", listRec.Body.String())
	}

	summaryReq := httptest.NewRequest(http.MethodGet, "/v1/admin/audit/summary?token_id=runner", nil)
	summaryReq.Header.Set("Authorization", "Bearer admin-secret")
	summaryRec := httptest.NewRecorder()
	handler.ServeHTTP(summaryRec, summaryReq)
	if summaryRec.Code != http.StatusOK {
		t.Fatalf("summary status = %d, want 200; body: %s", summaryRec.Code, summaryRec.Body.String())
	}
	var summaryResp struct {
		Path    string              `json:"path"`
		Summary serveraudit.Summary `json:"summary"`
	}
	if err := json.NewDecoder(summaryRec.Body).Decode(&summaryResp); err != nil {
		t.Fatalf("decode summary response: %v", err)
	}
	if summaryResp.Summary.Total != 1 {
		t.Fatalf("summary total = %d, want 1", summaryResp.Summary.Total)
	}
	if summaryResp.Summary.TotalCostUSD != 0.25 {
		t.Fatalf("summary total cost = %.2f, want 0.25", summaryResp.Summary.TotalCostUSD)
	}

	eventsCSVReq := httptest.NewRequest(http.MethodGet, "/v1/admin/audit/events?format=csv", nil)
	eventsCSVReq.Header.Set("Authorization", "Bearer admin-secret")
	eventsCSVRec := httptest.NewRecorder()
	handler.ServeHTTP(eventsCSVRec, eventsCSVReq)
	if eventsCSVRec.Code != http.StatusOK {
		t.Fatalf("audit csv status = %d, want 200; body: %s", eventsCSVRec.Code, eventsCSVRec.Body.String())
	}
	if got := eventsCSVRec.Header().Get("Content-Type"); !strings.Contains(got, "text/csv") {
		t.Fatalf("audit csv content-type = %q, want text/csv", got)
	}
	if !strings.Contains(eventsCSVRec.Body.String(), "request_id") {
		t.Fatalf("audit csv body missing header: %s", eventsCSVRec.Body.String())
	}

	usageReq := httptest.NewRequest(http.MethodGet, "/v1/admin/usage/summary?token_id=runner", nil)
	usageReq.Header.Set("Authorization", "Bearer admin-secret")
	usageRec := httptest.NewRecorder()
	handler.ServeHTTP(usageRec, usageReq)
	if usageRec.Code != http.StatusOK {
		t.Fatalf("usage status = %d, want 200; body: %s", usageRec.Code, usageRec.Body.String())
	}
	var usageResp struct {
		Path  string              `json:"path"`
		Usage serverusage.Summary `json:"usage"`
	}
	if err := json.NewDecoder(usageRec.Body).Decode(&usageResp); err != nil {
		t.Fatalf("decode usage response: %v", err)
	}
	if usageResp.Path != usagePath {
		t.Fatalf("usage path = %q, want %q", usageResp.Path, usagePath)
	}
	if usageResp.Usage.TotalCostUSD != 0.5 {
		t.Fatalf("usage total cost = %#v, want 0.5", usageResp.Usage.TotalCostUSD)
	}

	usersReq := httptest.NewRequest(http.MethodGet, "/v1/admin/users", nil)
	usersReq.Header.Set("Authorization", "Bearer admin-secret")
	usersRec := httptest.NewRecorder()
	handler.ServeHTTP(usersRec, usersReq)
	if usersRec.Code != http.StatusOK {
		t.Fatalf("users status = %d, want 200; body: %s", usersRec.Code, usersRec.Body.String())
	}
	var usersResp struct {
		Data []router.UserView `json:"data"`
	}
	if err := json.NewDecoder(usersRec.Body).Decode(&usersResp); err != nil {
		t.Fatalf("decode users response: %v", err)
	}
	if len(usersResp.Data) != 1 || usersResp.Data[0].ID != user.ID {
		t.Fatalf("users response = %+v, want created user %s", usersResp.Data, user.ID)
	}

	roleReq := httptest.NewRequest(http.MethodPost, "/v1/admin/users/"+user.ID+"/role", bytes.NewBufferString(`{"role":"admin"}`))
	roleReq.Header.Set("Authorization", "Bearer admin-secret")
	roleReq.Header.Set("Content-Type", "application/json")
	roleRec := httptest.NewRecorder()
	handler.ServeHTTP(roleRec, roleReq)
	if roleRec.Code != http.StatusOK {
		t.Fatalf("role status = %d, want 200; body: %s", roleRec.Code, roleRec.Body.String())
	}

	deactivateReq := httptest.NewRequest(http.MethodPost, "/v1/admin/users/"+user.ID+"/deactivate", nil)
	deactivateReq.Header.Set("Authorization", "Bearer admin-secret")
	deactivateRec := httptest.NewRecorder()
	handler.ServeHTTP(deactivateRec, deactivateReq)
	if deactivateRec.Code != http.StatusOK {
		t.Fatalf("deactivate status = %d, want 200; body: %s", deactivateRec.Code, deactivateRec.Body.String())
	}

	updatedUser, err := userStore.GetUserByID(user.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if updatedUser.Role != router.UserRoleAdmin {
		t.Fatalf("updated role = %q, want %q", updatedUser.Role, router.UserRoleAdmin)
	}
	if updatedUser.IsActive {
		t.Fatal("updated user still active, want deactivated")
	}

	revokeReq := httptest.NewRequest(http.MethodPost, "/v1/admin/tokens/runner/revoke", nil)
	revokeReq.Header.Set("Authorization", "Bearer admin-secret")
	revokeRec := httptest.NewRecorder()
	handler.ServeHTTP(revokeRec, revokeReq)
	if revokeRec.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200; body: %s", revokeRec.Code, revokeRec.Body.String())
	}
}

func TestHandler_RequiresAdminScopes(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "server_auth.json")
	if err := serverauth.SaveConfigFile(authPath, serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:     "viewer",
				Token:  "viewer-secret",
				Scopes: serverauth.AllClientScopes(),
			},
		},
	}); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}
	manager, err := serverauth.LoadManager(authPath)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}
	handler := NewHandler(HandlerOptions{
		Authorizer:   manager,
		TokenManager: manager,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/tokens", nil)
	req.Header.Set("Authorization", "Bearer viewer-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_OrganizationsProjectsBillingAndDashboard(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "server_auth.json")
	if err := serverauth.SaveConfigFile(authPath, serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:     "admin",
				Token:  "admin-secret",
				Scopes: serverauth.AllScopes(),
			},
		},
	}); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}
	manager, err := serverauth.LoadManager(authPath)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}

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

	handler := NewHandler(HandlerOptions{
		Authorizer:   manager,
		TokenManager: manager,
		UsagePath:    "sqlite:" + stateDB,
		UsageStore:   usageStore,
		TeamStore:    teamStore,
	})

	createOrgReq := httptest.NewRequest(http.MethodPost, "/v1/admin/organizations", bytes.NewBufferString(`{
		"name":"Platform Team",
		"slug":"platform-team",
		"description":"Shared platform services",
		"monthly_budget_usd":100
	}`))
	createOrgReq.Header.Set("Authorization", "Bearer admin-secret")
	createOrgReq.Header.Set("Content-Type", "application/json")
	createOrgRec := httptest.NewRecorder()
	handler.ServeHTTP(createOrgRec, createOrgReq)
	if createOrgRec.Code != http.StatusCreated {
		t.Fatalf("create org status = %d, want 201; body: %s", createOrgRec.Code, createOrgRec.Body.String())
	}
	var createOrgResp struct {
		Organization serverteam.Organization `json:"organization"`
	}
	if err := json.NewDecoder(createOrgRec.Body).Decode(&createOrgResp); err != nil {
		t.Fatalf("decode create org response: %v", err)
	}
	if createOrgResp.Organization.ID == "" {
		t.Fatal("organization id = empty")
	}

	createProjectReq := httptest.NewRequest(http.MethodPost, "/v1/admin/projects", bytes.NewBufferString(`{
		"organization_id":"`+createOrgResp.Organization.ID+`",
		"name":"Checkout API",
		"slug":"checkout-api",
		"description":"Critical payment path",
		"monthly_budget_usd":25
	}`))
	createProjectReq.Header.Set("Authorization", "Bearer admin-secret")
	createProjectReq.Header.Set("Content-Type", "application/json")
	createProjectRec := httptest.NewRecorder()
	handler.ServeHTTP(createProjectRec, createProjectReq)
	if createProjectRec.Code != http.StatusCreated {
		t.Fatalf("create project status = %d, want 201; body: %s", createProjectRec.Code, createProjectRec.Body.String())
	}
	var createProjectResp struct {
		Project serverteam.Project `json:"project"`
	}
	if err := json.NewDecoder(createProjectRec.Body).Decode(&createProjectResp); err != nil {
		t.Fatalf("decode create project response: %v", err)
	}
	if createProjectResp.Project.ID == "" {
		t.Fatal("project id = empty")
	}

	now := time.Now().UTC()
	usageStore.Log(serverusage.Entry{
		Timestamp:        serverusage.MonthStart(now).AddDate(0, -1, 0).Add(2 * time.Hour),
		RequestID:        "req_team_old",
		TokenID:          "runner",
		UserID:           "usr_123",
		OrganizationID:   createOrgResp.Organization.ID,
		ProjectID:        createProjectResp.Project.ID,
		ActualProvider:   "codex",
		Status:           200,
		PromptTokens:     10,
		CompletionTokens: 5,
		CostUSD:          50,
	})
	usageStore.Log(serverusage.Entry{
		Timestamp:        serverusage.MonthStart(now).Add(2 * time.Hour),
		RequestID:        "req_team_1",
		TokenID:          "runner",
		UserID:           "usr_123",
		OrganizationID:   createOrgResp.Organization.ID,
		ProjectID:        createProjectResp.Project.ID,
		ActualProvider:   "codex",
		Status:           200,
		PromptTokens:     20,
		CompletionTokens: 10,
		CostUSD:          3.5,
	})

	projectsReq := httptest.NewRequest(http.MethodGet, "/v1/admin/projects?organization_id="+createOrgResp.Organization.ID, nil)
	projectsReq.Header.Set("Authorization", "Bearer admin-secret")
	projectsRec := httptest.NewRecorder()
	handler.ServeHTTP(projectsRec, projectsReq)
	if projectsRec.Code != http.StatusOK {
		t.Fatalf("projects status = %d, want 200; body: %s", projectsRec.Code, projectsRec.Body.String())
	}

	billingReq := httptest.NewRequest(http.MethodGet, "/v1/admin/billing/summary?organization_id="+createOrgResp.Organization.ID, nil)
	billingReq.Header.Set("Authorization", "Bearer admin-secret")
	billingRec := httptest.NewRecorder()
	handler.ServeHTTP(billingRec, billingReq)
	if billingRec.Code != http.StatusOK {
		t.Fatalf("billing status = %d, want 200; body: %s", billingRec.Code, billingRec.Body.String())
	}
	var billingResp struct {
		Usage   serverusage.Summary       `json:"usage"`
		Billing serverteam.BillingSummary `json:"billing"`
	}
	if err := json.NewDecoder(billingRec.Body).Decode(&billingResp); err != nil {
		t.Fatalf("decode billing response: %v", err)
	}
	if billingResp.Usage.TotalCostUSD != 3.5 {
		t.Fatalf("billing total cost = %.2f, want 3.5", billingResp.Usage.TotalCostUSD)
	}
	if billingResp.Usage.ByOrganization[createOrgResp.Organization.ID] != 1 {
		t.Fatalf("billing org counts = %+v, want request count for organization", billingResp.Usage.ByOrganization)
	}
	if len(billingResp.Billing.Organizations) != 1 {
		t.Fatalf("billing org buckets = %+v, want 1", billingResp.Billing.Organizations)
	}
	if billingResp.Billing.Organizations[0].MonthlyBudgetUSD != 100 {
		t.Fatalf("org budget = %.2f, want 100", billingResp.Billing.Organizations[0].MonthlyBudgetUSD)
	}
	if billingResp.Billing.Organizations[0].SpendUSD != 3.5 {
		t.Fatalf("org spend = %.2f, want 3.5", billingResp.Billing.Organizations[0].SpendUSD)
	}
	if billingResp.Billing.Organizations[0].RemainingBudgetUSD != 96.5 {
		t.Fatalf("org remaining = %.2f, want 96.5", billingResp.Billing.Organizations[0].RemainingBudgetUSD)
	}
	if billingResp.Billing.Organizations[0].OverBudget {
		t.Fatal("org over_budget = true, want false")
	}
	if len(billingResp.Billing.Projects) != 1 {
		t.Fatalf("billing project buckets = %+v, want 1", billingResp.Billing.Projects)
	}
	if billingResp.Billing.Projects[0].MonthlyBudgetUSD != 25 {
		t.Fatalf("project budget = %.2f, want 25", billingResp.Billing.Projects[0].MonthlyBudgetUSD)
	}
	if billingResp.Billing.Projects[0].RemainingBudgetUSD != 21.5 {
		t.Fatalf("project remaining = %.2f, want 21.5", billingResp.Billing.Projects[0].RemainingBudgetUSD)
	}

	periodsReq := httptest.NewRequest(http.MethodGet, "/v1/admin/billing/periods", nil)
	periodsReq.Header.Set("Authorization", "Bearer admin-secret")
	periodsRec := httptest.NewRecorder()
	handler.ServeHTTP(periodsRec, periodsReq)
	if periodsRec.Code != http.StatusOK {
		t.Fatalf("billing periods status = %d, want 200; body: %s", periodsRec.Code, periodsRec.Body.String())
	}
	var periodsResp struct {
		Periods []serverusage.PeriodSummary `json:"periods"`
	}
	if err := json.NewDecoder(periodsRec.Body).Decode(&periodsResp); err != nil {
		t.Fatalf("decode periods response: %v", err)
	}
	if len(periodsResp.Periods) != 2 {
		t.Fatalf("periods = %+v, want two month buckets", periodsResp.Periods)
	}
	if periodsResp.Periods[len(periodsResp.Periods)-1].Period != serverusage.MonthStart(now).Format("2006-01") {
		t.Fatalf("latest period = %+v, want current month %s", periodsResp.Periods, serverusage.MonthStart(now).Format("2006-01"))
	}

	alertsReq := httptest.NewRequest(http.MethodGet, "/v1/admin/billing/alerts", nil)
	alertsReq.Header.Set("Authorization", "Bearer admin-secret")
	alertsRec := httptest.NewRecorder()
	handler.ServeHTTP(alertsRec, alertsReq)
	if alertsRec.Code != http.StatusOK {
		t.Fatalf("billing alerts status = %d, want 200; body: %s", alertsRec.Code, alertsRec.Body.String())
	}
	var alertsResp struct {
		Alerts []serverteam.BudgetAlert `json:"alerts"`
	}
	if err := json.NewDecoder(alertsRec.Body).Decode(&alertsResp); err != nil {
		t.Fatalf("decode alerts response: %v", err)
	}
	if len(alertsResp.Alerts) != 0 {
		t.Fatalf("alerts = %+v, want none under threshold", alertsResp.Alerts)
	}

	billingCSVReq := httptest.NewRequest(http.MethodGet, "/v1/admin/billing/summary?organization_id="+createOrgResp.Organization.ID+"&format=csv", nil)
	billingCSVReq.Header.Set("Authorization", "Bearer admin-secret")
	billingCSVRec := httptest.NewRecorder()
	handler.ServeHTTP(billingCSVRec, billingCSVReq)
	if billingCSVRec.Code != http.StatusOK {
		t.Fatalf("billing csv status = %d, want 200; body: %s", billingCSVRec.Code, billingCSVRec.Body.String())
	}
	if got := billingCSVRec.Header().Get("Content-Type"); !strings.Contains(got, "text/csv") {
		t.Fatalf("billing csv content-type = %q, want text/csv", got)
	}
	if !strings.Contains(billingCSVRec.Body.String(), "scope_type") || !strings.Contains(billingCSVRec.Body.String(), createProjectResp.Project.ID) {
		t.Fatalf("billing csv body missing expected values: %s", billingCSVRec.Body.String())
	}

	usageCSVReq := httptest.NewRequest(http.MethodGet, "/v1/admin/usage/events?format=csv", nil)
	usageCSVReq.Header.Set("Authorization", "Bearer admin-secret")
	usageCSVRec := httptest.NewRecorder()
	handler.ServeHTTP(usageCSVRec, usageCSVReq)
	if usageCSVRec.Code != http.StatusOK {
		t.Fatalf("usage csv status = %d, want 200; body: %s", usageCSVRec.Code, usageCSVRec.Body.String())
	}
	if got := usageCSVRec.Header().Get("Content-Type"); !strings.Contains(got, "text/csv") {
		t.Fatalf("usage csv content-type = %q, want text/csv", got)
	}
	if !strings.Contains(usageCSVRec.Body.String(), "request_id") || !strings.Contains(usageCSVRec.Body.String(), "req_team_1") {
		t.Fatalf("usage csv body missing expected values: %s", usageCSVRec.Body.String())
	}

	dashboardReq := httptest.NewRequest(http.MethodGet, "/v1/admin/dashboard", nil)
	dashboardReq.Header.Set("Authorization", "Bearer admin-secret")
	dashboardRec := httptest.NewRecorder()
	handler.ServeHTTP(dashboardRec, dashboardReq)
	if dashboardRec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200; body: %s", dashboardRec.Code, dashboardRec.Body.String())
	}
	var dashboardResp struct {
		Organizations struct {
			Count int `json:"count"`
		} `json:"organizations"`
		Projects struct {
			Count int `json:"count"`
		} `json:"projects"`
		Usage struct {
			Summary serverusage.Summary `json:"summary"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(dashboardRec.Body).Decode(&dashboardResp); err != nil {
		t.Fatalf("decode dashboard response: %v", err)
	}
	if dashboardResp.Organizations.Count != 1 || dashboardResp.Projects.Count != 1 {
		t.Fatalf("dashboard counts = %+v, want one org and one project", dashboardResp)
	}
	if dashboardResp.Usage.Summary.ByProject[createProjectResp.Project.ID] != 1 {
		t.Fatalf("dashboard usage by project = %+v, want project count", dashboardResp.Usage.Summary.ByProject)
	}
}

func TestHandler_AdminBrowserSessionAndCSRF(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "server_auth.json")
	if err := serverauth.SaveConfigFile(authPath, serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:     "admin",
				Token:  "admin-secret",
				Scopes: serverauth.AllScopes(),
			},
		},
	}); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}
	manager, err := serverauth.LoadManager(authPath)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}

	userStore := router.NewUserStore(filepath.Join(t.TempDir(), "users"))
	adminUser, err := userStore.CreateUserWithRole("admin@example.com", "secret123", router.UserRoleAdmin)
	if err != nil {
		t.Fatalf("CreateUserWithRole: %v", err)
	}
	sessionMgr, err := NewSessionManager(userStore, []byte("0123456789abcdef0123456789abcdef"), time.Hour, serverauth.NewLoginRateLimiter(5, time.Minute, time.Minute))
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	teamStore, err := serverteam.OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore(team): %v", err)
	}
	defer teamStore.Close()

	handler := NewHandler(HandlerOptions{
		Authorizer:   manager,
		TokenManager: manager,
		UserStore:    userStore,
		TeamStore:    teamStore,
		SessionMgr:   sessionMgr,
	})

	loginReq := httptest.NewRequest(http.MethodPost, "/v1/admin/session/login", bytes.NewBufferString(`{"email":"admin@example.com","password":"secret123"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	handler.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200; body: %s", loginRec.Code, loginRec.Body.String())
	}
	var loginResp struct {
		CSRFToken string          `json:"csrf_token"`
		User      router.UserView `json:"user"`
	}
	if err := json.NewDecoder(loginRec.Body).Decode(&loginResp); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if loginResp.CSRFToken == "" {
		t.Fatal("csrf_token = empty")
	}
	if loginResp.User.ID != adminUser.ID {
		t.Fatalf("logged in user = %q, want %q", loginResp.User.ID, adminUser.ID)
	}
	cookies := loginRec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("session login returned no cookie")
	}

	meReq := httptest.NewRequest(http.MethodGet, "/v1/admin/session/me", nil)
	meReq.AddCookie(cookies[0])
	meRec := httptest.NewRecorder()
	handler.ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusOK {
		t.Fatalf("me status = %d, want 200; body: %s", meRec.Code, meRec.Body.String())
	}
	var meResp struct {
		Authenticated bool            `json:"authenticated"`
		User          router.UserView `json:"user"`
	}
	if err := json.NewDecoder(meRec.Body).Decode(&meResp); err != nil {
		t.Fatalf("decode me response: %v", err)
	}
	if !meResp.Authenticated || meResp.User.ID != adminUser.ID {
		t.Fatalf("me response = %+v, want authenticated admin user", meResp)
	}

	noSessionReq := httptest.NewRequest(http.MethodGet, "/v1/admin/session/me", nil)
	noSessionRec := httptest.NewRecorder()
	handler.ServeHTTP(noSessionRec, noSessionReq)
	if noSessionRec.Code != http.StatusOK {
		t.Fatalf("anonymous me status = %d, want 200; body: %s", noSessionRec.Code, noSessionRec.Body.String())
	}
	var noSessionResp struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.NewDecoder(noSessionRec.Body).Decode(&noSessionResp); err != nil {
		t.Fatalf("decode anonymous me response: %v", err)
	}
	if noSessionResp.Authenticated {
		t.Fatalf("anonymous me response = %+v, want authenticated=false", noSessionResp)
	}

	orgReqNoCSRF := httptest.NewRequest(http.MethodPost, "/v1/admin/organizations", bytes.NewBufferString(`{"name":"Platform Team"}`))
	orgReqNoCSRF.AddCookie(cookies[0])
	orgReqNoCSRF.Header.Set("Content-Type", "application/json")
	orgRecNoCSRF := httptest.NewRecorder()
	handler.ServeHTTP(orgRecNoCSRF, orgReqNoCSRF)
	if orgRecNoCSRF.Code != http.StatusForbidden {
		t.Fatalf("no-CSRF status = %d, want 403; body: %s", orgRecNoCSRF.Code, orgRecNoCSRF.Body.String())
	}

	orgReq := httptest.NewRequest(http.MethodPost, "/v1/admin/organizations", bytes.NewBufferString(`{"name":"Platform Team"}`))
	orgReq.AddCookie(cookies[0])
	orgReq.Header.Set("Content-Type", "application/json")
	orgReq.Header.Set("X-CSRF-Token", loginResp.CSRFToken)
	orgRec := httptest.NewRecorder()
	handler.ServeHTTP(orgRec, orgReq)
	if orgRec.Code != http.StatusCreated {
		t.Fatalf("create org status = %d, want 201; body: %s", orgRec.Code, orgRec.Body.String())
	}
}

func TestHandler_ScopedAdminFiltersTokensAndRejectsCrossScopeIssue(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "server_auth.json")
	if err := serverauth.SaveConfigFile(authPath, serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:             "scoped-admin",
				Token:          "scoped-secret",
				OrganizationID: "org_a",
				Scopes: []string{
					serverauth.ScopeAdminTokensRead,
					serverauth.ScopeAdminTokensWrite,
					serverauth.ScopeAdminUsersRead,
				},
			},
			{
				ID:             "tok-a",
				Token:          "token-a",
				OrganizationID: "org_a",
				Scopes:         serverauth.AllClientScopes(),
			},
			{
				ID:             "tok-b",
				Token:          "token-b",
				OrganizationID: "org_b",
				Scopes:         serverauth.AllClientScopes(),
			},
		},
	}); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}
	manager, err := serverauth.LoadManager(authPath)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}
	teamStore, err := serverteam.OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore(team): %v", err)
	}
	defer teamStore.Close()
	if _, err := teamStore.CreateOrganization(serverteam.Organization{ID: "org_a", Name: "Org A"}); err != nil {
		t.Fatalf("CreateOrganization(org_a): %v", err)
	}
	if _, err := teamStore.CreateOrganization(serverteam.Organization{ID: "org_b", Name: "Org B"}); err != nil {
		t.Fatalf("CreateOrganization(org_b): %v", err)
	}

	handler := NewHandler(HandlerOptions{
		Authorizer:   manager,
		TokenManager: manager,
		TeamStore:    teamStore,
	})

	listReq := httptest.NewRequest(http.MethodGet, "/v1/admin/tokens", nil)
	listReq.Header.Set("Authorization", "Bearer scoped-secret")
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body: %s", listRec.Code, listRec.Body.String())
	}
	var listResp struct {
		Data []serverauth.TokenRuleView `json:"data"`
	}
	if err := json.NewDecoder(listRec.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listResp.Data) != 2 {
		t.Fatalf("scoped token list = %+v, want two org_a tokens", listResp.Data)
	}
	for _, item := range listResp.Data {
		if item.OrganizationID != "org_a" {
			t.Fatalf("token %+v leaked outside org scope", item)
		}
	}

	orgsReq := httptest.NewRequest(http.MethodGet, "/v1/admin/organizations", nil)
	orgsReq.Header.Set("Authorization", "Bearer scoped-secret")
	orgsRec := httptest.NewRecorder()
	handler.ServeHTTP(orgsRec, orgsReq)
	if orgsRec.Code != http.StatusOK {
		t.Fatalf("organizations status = %d, want 200; body: %s", orgsRec.Code, orgsRec.Body.String())
	}
	var orgsResp struct {
		Data []serverteam.Organization `json:"data"`
	}
	if err := json.NewDecoder(orgsRec.Body).Decode(&orgsResp); err != nil {
		t.Fatalf("decode organizations response: %v", err)
	}
	if len(orgsResp.Data) != 1 || orgsResp.Data[0].ID != "org_a" {
		t.Fatalf("organizations = %+v, want only org_a", orgsResp.Data)
	}

	issueReq := httptest.NewRequest(http.MethodPost, "/v1/admin/tokens", bytes.NewBufferString(`{"id":"cross-org","organization_id":"org_b"}`))
	issueReq.Header.Set("Authorization", "Bearer scoped-secret")
	issueReq.Header.Set("Content-Type", "application/json")
	issueRec := httptest.NewRecorder()
	handler.ServeHTTP(issueRec, issueReq)
	if issueRec.Code != http.StatusForbidden {
		t.Fatalf("cross-scope issue status = %d, want 403; body: %s", issueRec.Code, issueRec.Body.String())
	}
}

func TestHandler_UserEndpointsRequireUserScopes(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "server_auth.json")
	if err := serverauth.SaveConfigFile(authPath, serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:     "viewer",
				Token:  "viewer-secret",
				Scopes: []string{serverauth.ScopeAdminAuditRead},
			},
		},
	}); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}
	manager, err := serverauth.LoadManager(authPath)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}
	handler := NewHandler(HandlerOptions{
		Authorizer:   manager,
		TokenManager: manager,
		UserStore:    router.NewUserStore(filepath.Join(t.TempDir(), "users")),
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/users", nil)
	req.Header.Set("Authorization", "Bearer viewer-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_UserPasswordResetAndMembershipEndpoints(t *testing.T) {
	stateDB := filepath.Join(t.TempDir(), "state.db")
	userStore, err := router.OpenSQLiteUserStore(stateDB)
	if err != nil {
		t.Fatalf("OpenSQLiteUserStore: %v", err)
	}
	defer userStore.Close()
	user, err := userStore.CreateUser("member@example.com", "password123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	authPath := filepath.Join(t.TempDir(), "server_auth.json")
	if err := serverauth.SaveConfigFile(authPath, serverauth.Config{
		Tokens: []serverauth.TokenRule{{
			ID:     "admin",
			Token:  "admin-secret",
			Scopes: serverauth.AllScopes(),
		}},
	}); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}
	manager, err := serverauth.LoadManager(authPath)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}
	teamStore, err := serverteam.OpenSQLiteStore(stateDB)
	if err != nil {
		t.Fatalf("OpenSQLiteStore(team): %v", err)
	}
	defer teamStore.Close()
	org, err := teamStore.CreateOrganization(serverteam.Organization{Name: "Platform Team"})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	project, err := teamStore.CreateProject(serverteam.Project{OrganizationID: org.ID, Name: "Checkout API"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	handler := NewHandler(HandlerOptions{
		Authorizer:   manager,
		TokenManager: manager,
		UserStore:    userStore,
		TeamStore:    teamStore,
	})

	passwordReq := httptest.NewRequest(http.MethodPost, "/v1/admin/users/"+user.ID+"/password", bytes.NewBufferString(`{"password":"newsecret123"}`))
	passwordReq.Header.Set("Authorization", "Bearer admin-secret")
	passwordReq.Header.Set("Content-Type", "application/json")
	passwordRec := httptest.NewRecorder()
	handler.ServeHTTP(passwordRec, passwordReq)
	if passwordRec.Code != http.StatusOK {
		t.Fatalf("password reset status = %d, want 200; body: %s", passwordRec.Code, passwordRec.Body.String())
	}
	updated, err := userStore.GetUserByID(user.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if !updated.ValidatePassword("newsecret123") {
		t.Fatal("password reset did not persist")
	}

	orgMembershipReq := httptest.NewRequest(http.MethodPost, "/v1/admin/organization-memberships", bytes.NewBufferString(`{"organization_id":"`+org.ID+`","user_id":"`+user.ID+`","role":"manager"}`))
	orgMembershipReq.Header.Set("Authorization", "Bearer admin-secret")
	orgMembershipReq.Header.Set("Content-Type", "application/json")
	orgMembershipRec := httptest.NewRecorder()
	handler.ServeHTTP(orgMembershipRec, orgMembershipReq)
	if orgMembershipRec.Code != http.StatusCreated {
		t.Fatalf("org membership status = %d, want 201; body: %s", orgMembershipRec.Code, orgMembershipRec.Body.String())
	}

	projectMembershipReq := httptest.NewRequest(http.MethodPost, "/v1/admin/project-memberships", bytes.NewBufferString(`{"project_id":"`+project.ID+`","user_id":"`+user.ID+`","role":"member"}`))
	projectMembershipReq.Header.Set("Authorization", "Bearer admin-secret")
	projectMembershipReq.Header.Set("Content-Type", "application/json")
	projectMembershipRec := httptest.NewRecorder()
	handler.ServeHTTP(projectMembershipRec, projectMembershipReq)
	if projectMembershipRec.Code != http.StatusCreated {
		t.Fatalf("project membership status = %d, want 201; body: %s", projectMembershipRec.Code, projectMembershipRec.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v1/admin/project-memberships?project_id="+project.ID, nil)
	listReq.Header.Set("Authorization", "Bearer admin-secret")
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("project membership list status = %d, want 200; body: %s", listRec.Code, listRec.Body.String())
	}
}
