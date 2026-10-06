package serveradmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"
)

func TestAdminBrowserSessionRemainsSecureBehindTLSProxy(t *testing.T) {
	f := newAuthorizationFixture(t)
	sessions, err := NewSessionManager(f.users, []byte("0123456789abcdef0123456789abcdef"), time.Hour, nil)
	if err != nil {
		t.Fatal(err)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/admin/session/login", sessions.HandleSessionLogin)
	mux.HandleFunc("/v1/admin/session/me", sessions.HandleSessionMe)
	mux.HandleFunc("/v1/admin/session/logout", sessions.HandleSessionLogout)
	origin := httptest.NewServer(mux)
	defer origin.Close()
	upstream, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
		return
	}
	// This TLS proxy forwards HTTP without an X-Forwarded-Proto header. Cookie
	// protection must not depend on the proxy or a client supplying that header.
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(upstream))
	defer proxy.Close()
	client := proxy.Client()
	// Go and browsers treat loopback HTTP as a secure origin. Use an ordinary
	// hostname for the cookie policy while dialing only our local listeners.
	transport := client.Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || host != "admin.example.test" {
			return nil, errors.New("unexpected cookie transport test destination")
		}
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	}
	client.Transport = transport
	defer transport.CloseIdleConnections()
	logicalURL := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
			return ""
		}
		u.Host = net.JoinHostPort("admin.example.test", u.Port())
		return u.String()
	}
	secureEndpoint := logicalURL(proxy.URL)
	client.Jar, err = cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
		return
	}
	resp, err := client.Post(secureEndpoint+"/v1/admin/session/login", "application/json",
		bytes.NewBufferString(`{"email":"admin@example.com","password":"password123"}`))
	if err != nil {
		t.Fatal(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxied login status = %d", resp.StatusCode)
	}
	var login adminSessionLoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	for _, candidate := range resp.Cookies() {
		if candidate.Name == adminSessionCookieName {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatal("proxied login did not issue an admin cookie")
		return
	}
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("admin cookie attributes = Secure:%t HttpOnly:%t SameSite:%v",
			cookie.Secure, cookie.HttpOnly, cookie.SameSite)
	}

	me, err := client.Get(secureEndpoint + "/v1/admin/session/me")
	if err != nil {
		t.Fatal(err)
		return
	}
	defer me.Body.Close()
	var current adminSessionLoginResponse
	if err := json.NewDecoder(me.Body).Decode(&current); err != nil {
		t.Fatal(err)
	}
	if !current.Authenticated || current.User.ID != f.admin.ID {
		t.Fatal("browser cookie jar did not authenticate through HTTPS")
	}

	// Cookies are host-scoped rather than port-scoped. An HTTP listener on the
	// same host must not receive the HTTPS session even through a real client.
	captured := make(chan string, 1)
	plaintext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		captured <- req.Header.Get("Cookie")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer plaintext.Close()
	probe, err := client.Get(logicalURL(plaintext.URL))
	if err != nil {
		t.Fatal(err)
		return
	}
	_ = probe.Body.Close()
	if got := <-captured; got != "" {
		t.Error("browser cookie jar sent the admin session over HTTP")
	}

	logout, err := http.NewRequest(http.MethodPost, secureEndpoint+"/v1/admin/session/logout", nil)
	if err != nil {
		t.Fatal(err)
		return
	}
	logout.Header.Set("X-CSRF-Token", login.CSRFToken)
	signedOut, err := client.Do(logout)
	if err != nil {
		t.Fatal(err)
		return
	}
	_ = signedOut.Body.Close()
	if signedOut.StatusCode != http.StatusOK {
		t.Fatalf("proxied logout status = %d", signedOut.StatusCode)
	}
	cleared := false
	for _, candidate := range signedOut.Cookies() {
		if candidate.Name == adminSessionCookieName {
			cleared = candidate.Secure && candidate.HttpOnly && candidate.MaxAge < 0
		}
	}
	if !cleared {
		t.Fatal("logout did not securely delete the admin cookie")
	}
	endpoint, err := url.Parse(secureEndpoint)
	if err != nil {
		t.Fatal(err)
		return
	}
	if len(client.Jar.Cookies(endpoint)) != 0 {
		t.Fatal("browser cookie jar retained the logged-out session")
	}
	if sessionAuthenticated(sessions, cookie) {
		t.Fatal("server accepted a saved copy of the logged-out session")
	}
}

func TestAdminCookieProtectionIgnoresForwardedScheme(t *testing.T) {
	for _, scheme := range []string{"", "http", "https", "https, http"} {
		t.Run(scheme, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/admin/session/logout", nil)
			req.Header.Set("X-Forwarded-Proto", scheme)
			var sessions SessionManager
			rec := httptest.NewRecorder()
			sessions.ClearCookie(rec, req)
			cookies := rec.Result().Cookies()
			if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].MaxAge >= 0 {
				t.Fatal("forwarded scheme weakened the logout cookie")
			}
		})
	}
}
