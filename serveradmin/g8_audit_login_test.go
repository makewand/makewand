package serveradmin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
)

type memoryAudit struct{ events []serveraudit.Event }

func (m *memoryAudit) Log(evt serveraudit.Event) { m.events = append(m.events, evt) }

func (m *memoryAudit) last(t *testing.T, kind string) serveraudit.Event {
	t.Helper()
	for i := len(m.events) - 1; i >= 0; i-- {
		if m.events[i].Kind == kind {
			return m.events[i]
		}
	}
	t.Fatalf("no %s audit event in %+v", kind, m.events)
	return serveraudit.Event{}
}

// go-server#11: admin mutations record their targets so a self-grant or role
// change is reconstructible from the audit log alone.
func TestG8_AdminAuditRecordsTargets(t *testing.T) {
	f := newAuthorizationFixture(t)
	audit := &memoryAudit{}
	handler := NewHandler(HandlerOptions{Authorizer: f.tokens, TokenManager: f.tokens, UserStore: f.users, TeamStore: f.teams, AuditLogger: audit})

	rec := adminRequest(handler, f.root, http.MethodPost, "/v1/admin/organization-memberships", `{"organization_id":"org-a","user_id":"`+f.bob.ID+`","role":"owner","is_active":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("membership=%d %s", rec.Code, rec.Body.String())
	}
	evt := audit.last(t, "admin_organization_memberships")
	if evt.Action != "upsert_organization_membership" || evt.TargetUserID != f.bob.ID || evt.TargetOrganizationID != "org-a" || evt.TargetRole != "owner" || evt.TargetActive == nil || !*evt.TargetActive {
		t.Fatalf("membership audit=%+v", evt)
	}

	rec = adminRequest(handler, f.root, http.MethodPost, "/v1/admin/project-memberships", `{"project_id":"project-a","user_id":"`+f.bob.ID+`","role":"viewer","is_active":false}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("project membership=%d %s", rec.Code, rec.Body.String())
	}
	evt = audit.last(t, "admin_project_memberships")
	if evt.TargetUserID != f.bob.ID || evt.TargetProjectID != "project-a" || evt.TargetRole != "viewer" || evt.TargetActive == nil || *evt.TargetActive {
		t.Fatalf("project membership audit=%+v", evt)
	}

	// Rejected attempts are recorded with their targets as well.
	selfService := f.issue(t, serverauth.TokenRule{UserID: f.carol.ID, Scopes: []string{serverauth.ScopeAdminUsersWrite}})
	rec = adminRequest(handler, selfService, http.MethodPost, "/v1/admin/organization-memberships", `{"organization_id":"org-b","user_id":"`+f.carol.ID+`","role":"owner"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self grant=%d", rec.Code)
	}
	evt = audit.last(t, "admin_organization_memberships")
	if evt.Status != http.StatusForbidden || evt.TargetOrganizationID != "org-b" || evt.TargetUserID != f.carol.ID || evt.ActorUserID != f.carol.ID {
		t.Fatalf("rejected self grant audit=%+v", evt)
	}

	rec = adminRequest(handler, f.root, http.MethodPost, "/v1/admin/users/"+f.alice.ID+"/role", `{"role":"admin"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("role=%d %s", rec.Code, rec.Body.String())
	}
	evt = audit.last(t, "admin_users")
	if evt.Action != "role" || evt.TargetUserID != f.alice.ID || evt.TargetRole != "admin" {
		t.Fatalf("role audit=%+v", evt)
	}

	rec = adminRequest(handler, f.root, http.MethodPost, "/v1/admin/tokens", `{"id":"svc-1","user_id":"`+f.bob.ID+`","organization_id":"org-b"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue=%d %s", rec.Code, rec.Body.String())
	}
	evt = audit.last(t, "admin_tokens")
	if evt.Action != "issue_token" || evt.TargetTokenID != "svc-1" || evt.TargetUserID != f.bob.ID || evt.TargetOrganizationID != "org-b" {
		t.Fatalf("issue audit=%+v", evt)
	}
	rec = adminRequest(handler, f.root, http.MethodPost, "/v1/admin/tokens/svc-1/revoke", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke=%d %s", rec.Code, rec.Body.String())
	}
	evt = audit.last(t, "admin_tokens")
	if evt.Action != "revoke_token" || evt.TargetTokenID != "svc-1" {
		t.Fatalf("revoke audit=%+v", evt)
	}

	rec = adminRequest(handler, f.root, http.MethodPost, "/v1/admin/organizations", `{"id":"org-z","name":"Z","monthly_budget_usd":5}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("org create=%d %s", rec.Code, rec.Body.String())
	}
	evt = audit.last(t, "admin_organizations")
	if evt.Action != "create_organization" || evt.TargetOrganizationID != "org-z" {
		t.Fatalf("org create audit=%+v", evt)
	}
}

func medianLoginDuration(t *testing.T, sessions *SessionManager, email string) time.Duration {
	t.Helper()
	var samples []time.Duration
	for i := 0; i < 5; i++ {
		body := `{"email":"` + email + `","password":"wrong-password"}`
		start := time.Now()
		rec := httptest.NewRecorder()
		sessions.HandleSessionLogin(rec, httptest.NewRequest(http.MethodPost, "/v1/admin/session/login", bytes.NewBufferString(body)))
		samples = append(samples, time.Since(start))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("login %s=%d %s", email, rec.Code, rec.Body.String())
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[len(samples)/2]
}

// go-server#11: the admin login must spend the same password-hashing work for
// unknown and inactive accounts as for existing ones, so response time does not
// reveal which e-mail addresses are registered.
func TestG8_AdminLoginTimingDoesNotRevealAccounts(t *testing.T) {
	f := newAuthorizationFixture(t)
	if _, err := f.users.SetUserActive(f.carol.ID, false); err != nil {
		t.Fatal(err)
	}
	// A limiter large enough that repeated failures are not throttled.
	limiter := serverauth.NewLoginRateLimiter(1000, time.Hour, time.Minute)
	sessions, err := NewSessionManager(f.users, []byte("0123456789abcdef0123456789abcdef"), time.Hour, limiter)
	if err != nil {
		t.Fatal(err)
	}
	existing := medianLoginDuration(t, sessions, "admin@example.com")
	missing := medianLoginDuration(t, sessions, "nobody-"+filepath.Base(t.TempDir())+"@example.com")
	inactive := medianLoginDuration(t, sessions, "carol@example.com")
	if missing*3 < existing || inactive*3 < existing {
		t.Fatalf("login timing leaks account existence: existing=%s missing=%s inactive=%s", existing, missing, inactive)
	}
}
