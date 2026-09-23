package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/remotesession"
	"github.com/makewand/makewand/router"
	"github.com/makewand/makewand/serveradmin"
	"github.com/makewand/makewand/serverauth"
	"github.com/spf13/cobra"
)

type reviewReevalProvider struct{}

func (reviewReevalProvider) Name() string        { return "claude" }
func (reviewReevalProvider) IsAvailable() bool  { return true }
func (reviewReevalProvider) Chat(context.Context, []router.Message, string, int) (string, router.Usage, error) {
	return "review", router.Usage{}, nil
}
func (reviewReevalProvider) ChatStream(context.Context, []router.Message, string, int) (<-chan router.StreamChunk, error) {
	panic("not used")
}

func reviewReevalReq(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestReviewLifecycleAPIAndCLI(t *testing.T) {
	for _, via := range []string{"api", "cli"} {
		t.Run(via, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "review.db")
			users, err := router.OpenSQLiteUserStore(db)
			if err != nil {
				t.Fatal(err)
			}
			defer users.Close()

			tokens, err := serverauth.OpenSQLiteStore(db)
			if err != nil {
				t.Fatal(err)
			}
			defer tokens.Close()

			user, err := users.CreateUserWithRoleActive("review@example.test", "review-initial-password", router.UserRoleAdmin, true)
			if err != nil {
				t.Fatal(err)
			}
			_, operator, err := tokens.Issue(serverauth.TokenRule{Scopes: serverauth.AllScopes()})
			if err != nil {
				t.Fatal(err)
			}

			r, err := router.NewRouterFromConfig(router.RouterConfig{
				Providers: map[string]router.ProviderEntry{
					"claude": {Provider: reviewReevalProvider{}, Access: router.AccessSubscription},
				},
				DefaultModel: "claude",
				CodingModel:  "claude",
			})
			if err != nil {
				t.Fatal(err)
			}

			login := r.HandleUserLogin(users, tokens, nil, nil)
			w := reviewReevalReq(login, "POST", "/v1/users/login", "", `{"email":"review@example.test","password":"review-initial-password"}`)
			if w.Code != http.StatusOK {
				t.Fatal(w.Body.String())
			}
			var lr router.UserLoginResponse
			if err = json.Unmarshal(w.Body.Bytes(), &lr); err != nil {
				t.Fatal(err)
			}

			admin := serveradmin.NewHandler(serveradmin.HandlerOptions{Authorizer: tokens, TokenManager: tokens, UserStore: users})
			api := r.HTTPHandler(router.HTTPHandlerOptions{Authorizer: tokens})
			sessions := remotesession.NewHandlerWithAuthorizer(remotesession.NewStore(t.TempDir()), tokens)

			mutate := func(action, body string, cmd *cobra.Command, args []string) {
				if via == "api" {
					w := reviewReevalReq(admin, "POST", "/v1/admin/users/"+user.ID+"/"+action, operator, body)
					if w.Code != http.StatusOK {
						t.Fatal(w.Code, w.Body.String())
					}
				} else {
					cmd.SetArgs(append(args, "--state-db", db))
					if err := cmd.Execute(); err != nil {
						t.Fatal(err)
					}
				}
			}

			mutate("role", `{"role":"member"}`, userRoleCmd(), []string{user.ID, "member"})
			afterRole := reviewReevalReq(admin, "GET", "/v1/admin/tokens", lr.Token, "").Code
			selfRestore := reviewReevalReq(admin, "POST", "/v1/admin/users/"+user.ID+"/role", lr.Token, `{"role":"admin"}`).Code
			if afterRole == http.StatusOK {
				t.Fatalf("afterRole should not be 200, got %d", afterRole)
			}
			if selfRestore == http.StatusOK {
				t.Fatalf("selfRestore should not be 200, got %d", selfRestore)
			}

			mutate("password", `{"password":"review-new-password"}`, userPasswordCmd(), []string{user.ID, "--password", "review-new-password"})
			afterPassword := reviewReevalReq(admin, "GET", "/v1/admin/tokens", lr.Token, "").Code
			if afterPassword == http.StatusOK {
				t.Fatalf("afterPassword should not be 200, got %d", afterPassword)
			}

			mutate("deactivate", `{}`, userDeactivateCmd(), []string{user.ID})
			afterDeactivate := reviewReevalReq(admin, "GET", "/v1/admin/tokens", lr.Token, "").Code
			if afterDeactivate != http.StatusUnauthorized {
				t.Fatalf("afterDeactivate should be 401, got %d", afterDeactivate)
			}

			chat := reviewReevalReq(api, "POST", "/v1/chat/completions", lr.Token, `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`).Code
			responses := reviewReevalReq(api, "POST", "/v1/responses", lr.Token, `{"model":"claude","input":"hi"}`).Code
			model := reviewReevalReq(api, "GET", "/v1/models", lr.Token, "").Code
			session := reviewReevalReq(sessions, "PUT", "/v1/sessions/test", lr.Token, `{"review":true}`).Code

			if chat != http.StatusUnauthorized {
				t.Fatalf("deactivated chat should be 401, got %d", chat)
			}
			if responses != http.StatusUnauthorized {
				t.Fatalf("deactivated responses should be 401, got %d", responses)
			}
			if model != http.StatusUnauthorized {
				t.Fatalf("deactivated model should be 401, got %d", model)
			}
			if session != http.StatusUnauthorized {
				t.Fatalf("deactivated session should be 401, got %d", session)
			}

			t.Logf("via=%s after_role_admin=%d self_restore_role=%d after_password_admin=%d after_deactivate_admin=%d deactivated_chat=%d responses=%d models=%d session_put=%d",
				via, afterRole, selfRestore, afterPassword, afterDeactivate, chat, responses, model, session)
		})
	}
}
