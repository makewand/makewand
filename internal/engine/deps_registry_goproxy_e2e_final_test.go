//go:build linux

package engine

import (
	"context"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Final round, end to end through the real probe (the test binary) and
// local servers: the connectivity check walks GOPROXY as the go command does.

// goProxyTLSServers starts TLS servers the probe trusts (one CA bundle for
// all) with the given handlers and returns their addresses.
func goProxyTLSServers(t *testing.T, handlers ...http.HandlerFunc) []string {
	t.Helper()
	unsetProxyEnv(t)
	var bundle []byte
	var addrs []string
	for _, handler := range handlers {
		srv := httptest.NewTLSServer(handler)
		t.Cleanup(srv.Close)
		bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})...)
		addrs = append(addrs, strings.TrimPrefix(srv.URL, "https://"))
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", ca)
	return addrs
}

// closedAddr returns a local address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// recordingHandler answers path -> status (others 200) and records requests.
func recordingHandler(statuses map[string]int, redirects map[string]string) (http.HandlerFunc, func() []string) {
	var mu sync.Mutex
	var asked []string
	return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			asked = append(asked, r.Method+" "+r.URL.Path)
			mu.Unlock()
			if target, ok := redirects[r.URL.Path]; ok {
				http.Redirect(w, r, target, http.StatusFound)
				return
			}
			if status, ok := statuses[r.URL.Path]; ok {
				w.WriteHeader(status)
			}
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(asked)
		}
}

// The real probe (the test binary) walks GOPROXY='https://<down>|https://<up>'
// like the go command: past the proxy that does not answer, to the one that
// does - for modules and for the checksum database.
func TestRegistryProbeMovesPastPipeEntry_Final(t *testing.T) {
	handler, asked := recordingHandler(nil, nil)
	up := goProxyTLSServers(t, handler)[0]
	down := closedAddr(t)
	fakeGoOnPath(t, hostGoEnv{GOPROXY: "https://" + down + "|https://" + up})
	check := checkRegistryReachable(context.Background(), verifyExecEnvironment{mode: verifyExecUnsafeHost}, "go", t.TempDir())
	if !check.done || len(check.unreachable) != 0 || !slices.Equal(check.reached, []string{"127.0.0.1"}) {
		t.Fatalf("check = %+v, want the proxy behind the '|' entry reached", check)
	}
	got := asked()
	slices.Sort(got)
	if want := []string{"GET /sumdb/sum.golang.org/supported", "HEAD /"}; !slices.Equal(got, want) {
		t.Fatalf("the reachable proxy got %q, want %q", got, want)
	}
}

// The probe follows redirects of a deciding URL as the go command does, and
// refuses one from https to http like the go command.
func TestRegistryProbeFollowsRedirectsLikeGo_Final(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(plain.Close)
	handler, asked := recordingHandler(map[string]int{"/elsewhere/supported": http.StatusOK}, map[string]string{
		"/moved/sumdb/sum.golang.org/supported":     "/elsewhere/supported",
		"/downgrade/sumdb/sum.golang.org/supported": plain.URL + "/supported",
	})
	srv := goProxyTLSServers(t, handler)[0]
	for _, tc := range []struct {
		proxy       string
		unreachable string
	}{
		{"https://" + srv + "/moved", ""},
		{"https://" + srv + "/downgrade", "redirected from secure URL"},
	} {
		fakeGoOnPath(t, hostGoEnv{GOPROXY: tc.proxy + ",direct"})
		check := checkRegistryReachable(context.Background(), verifyExecEnvironment{mode: verifyExecUnsafeHost}, "go", t.TempDir())
		switch {
		case !check.done:
			t.Fatalf("%s: check did not run: %+v", tc.proxy, check)
		case tc.unreachable == "" && len(check.unreachable) != 0:
			t.Fatalf("%s: check = %+v, want the redirected support endpoint followed", tc.proxy, check)
		case tc.unreachable != "" && (len(check.unreachable) != 1 || !strings.Contains(check.unreachable[0], tc.unreachable)):
			t.Fatalf("%s: check = %+v, want the downgrade refused", tc.proxy, check)
		}
	}
	if got := asked(); !slices.Contains(got, "GET /elsewhere/supported") {
		t.Fatalf("server got %q, want the redirect followed", got)
	}
}

// End to end (the verifier's case, with local servers): a failed `go mod
// tidy` whose first proxy, followed by '|', does not answer while the next one
// does is the install's own failure, not an environment error - the go command
// fetched from the next proxy.
func TestGoPipeProxyFailureIsCandidateFailure_Final(t *testing.T) {
	handler, _ := recordingHandler(nil, nil)
	up := goProxyTLSServers(t, handler)[0]
	down := closedAddr(t)
	project := newHostDepsProject(t)
	fakeToolPrinting(t, "go", `go: example.com/x@v1.0.0: Get "https://`+down+`/example.com/x/@v/v1.0.0.info": dial tcp `+down+`: connect: connection refused`, 1)
	oldProbe := sandboxGoEnvProbe
	sandboxGoEnvProbe = func(string, string) (hostGoEnv, error) {
		return hostGoEnv{GOPROXY: "https://" + down + "|https://" + up}, nil
	}
	t.Cleanup(func() { sandboxGoEnvProbe = oldProbe })
	result, err := project.RunRestrictedPlan(context.Background(), ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "tidy"}})
	if errors.Is(err, ErrDependencyNetworkUnavailable) || err != nil {
		t.Fatalf("err = %v, want the install's own failure", err)
	}
	if result == nil || result.ExitCode != 1 || !strings.Contains(result.Note, "makewand's own connectivity check reached 127.0.0.1") {
		t.Fatalf("result = %+v, want the failure with a note that the check reached the next proxy", result)
	}
}

// A proxy that answers its checksum database support endpoint with anything
// but 200, 404 or 410 leaves the database unavailable, as for the go command;
// behind a '|' entry the go command moves on to the next proxy instead.
func TestRegistryProbeTreatsOtherSupportAnswersLikeGo_Final(t *testing.T) {
	broken, brokenAsked := recordingHandler(map[string]int{"/sumdb/sum.golang.org/supported": http.StatusInternalServerError}, nil)
	serving, _ := recordingHandler(nil, nil)
	addrs := goProxyTLSServers(t, broken, serving)
	for _, tc := range []struct {
		goproxy     string
		unreachable []string
	}{
		{"https://" + addrs[0] + ",direct", []string{"127.0.0.1 (answered 500 Internal Server Error)"}},
		{"https://" + addrs[0] + "|https://" + addrs[1], nil},
	} {
		fakeGoOnPath(t, hostGoEnv{GOPROXY: tc.goproxy})
		check := checkRegistryReachable(context.Background(), verifyExecEnvironment{mode: verifyExecUnsafeHost}, "go", t.TempDir())
		if !check.done || !slices.Equal(check.unreachable, tc.unreachable) {
			t.Fatalf("GOPROXY=%s: check = %+v, want unreachable %q", tc.goproxy, check, tc.unreachable)
		}
	}
	if got := brokenAsked(); !slices.Contains(got, "GET /sumdb/sum.golang.org/supported") {
		t.Fatalf("broken proxy got %q", got)
	}
}
