//go:build linux

package engine

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/trustedbin"
)

// parseRegistryCheck: every endpoint needs a line; one that is missing is a
// failed check, unless the check ran out of time.
func TestParseRegistryCheck_R3(t *testing.T) {
	endpoints := registryEndpoints("https://a.example/", "https://b.example/x")
	parse := func(stdout string, timedOut bool) registryCheck {
		return parseRegistryCheck(registryProbeRun{stdout: stdout, stderr: "panic: probe crashed\n"}, endpoints, timedOut)
	}
	check := parse("reachable https://a.example/ 200\nreachable https://b.example/x 404\n", false)
	if !check.done || len(check.unreachable) != 0 || !slices.Equal(check.reached, []string{"a.example", "b.example"}) {
		t.Fatalf("all answered: %+v", check)
	}
	check = parse("reachable https://a.example/ 200\nunreachable https://b.example/x [Errno -3] Temporary failure in name resolution\n", false)
	if !check.done || !slices.Equal(check.unreachable, []string{"b.example ([Errno -3] Temporary failure in name resolution)"}) {
		t.Fatalf("one unreachable: %+v", check)
	}
	if check = parse("reachable https://a.example/ 200\n", false); check.done || check.failure != "it gave no answer for https://b.example/x (panic: probe crashed)" {
		t.Fatalf("a missing line without a timeout must prove nothing: %+v", check)
	}
	check = parse("reachable https://a.example/ 200\n", true)
	if !check.done || len(check.unreachable) != 1 || !strings.HasPrefix(check.unreachable[0], "b.example (no response within ") {
		t.Fatalf("timed out: %+v", check)
	}
	if check = parse("Traceback (most recent call last):\n", false); check.done {
		t.Fatalf("a crashed check must prove nothing: %+v", check)
	}
}

// The real probe (makewand's own executable, here the test binary), run on
// the host as under the unsafe opt-in, against local servers: any HTTP
// response over verified TLS is an answer; a closed port and a certificate
// the system does not trust are not.
func TestRegistryCheckScriptAgainstLocalServers_R3(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		t.Setenv(key, "")
	}
	var served atomic.Int32 // the probe asks concurrently
	trusted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		if r.Method != http.MethodHead || r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer trusted.Close()
	// A server with a certificate of its own, which nothing trusts.
	untrusted := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	untrusted.TLS = &tls.Config{Certificates: []tls.Certificate{selfSignedCertificate(t)}, MinVersion: tls.VersionTLS12}
	untrusted.Config.ErrorLog = log.New(io.Discard, "", 0) // the check aborts the handshake
	untrusted.StartTLS()
	defer untrusted.Close()
	closed, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedURL := "https://" + closed.Addr().String() + "/"
	_ = closed.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: trusted.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", ca)
	urls := []string{trusted.URL + "/", trusted.URL + "/missing", closedURL, untrusted.URL + "/"}
	old := registryCheckEndpoints
	registryCheckEndpoints = func(string) []registryEndpoint { return registryEndpoints(urls...) }
	t.Cleanup(func() { registryCheckEndpoints = old })

	check := checkRegistryReachable(context.Background(), verifyExecEnvironment{mode: verifyExecUnsafeHost}, "npm")
	if !check.done || len(check.unreachable) != 2 {
		t.Fatalf("check = %+v, want the closed port and the untrusted server unreachable", check)
	}
	if !strings.HasPrefix(check.unreachable[0], "127.0.0.1 (") || !strings.Contains(check.unreachable[1], "certificate") {
		t.Fatalf("unreachable = %q", check.unreachable)
	}
	if got := served.Load(); got != 2 {
		t.Fatalf("trusted server answered %d requests, want 2", got)
	}
}

// selfSignedCertificate returns a certificate for 127.0.0.1 that no
// certificate store trusts.
func selfSignedCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "makewand-untrusted"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// Live: the check runs in a real sandbox with the user-mode network, so a
// registry the sandbox cannot reach (here a private address its routes
// block) is unreachable for the check too, and promptly.
func TestRegistryCheckFromRealSandboxLive_R3(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_SANDBOX_MEMORY_MAX", "1G")
	env, err := resolveVerifyExecEnvironment(UnsafeHostExecAuthorization{})
	if err != nil || env.mode != verifyExecIsolated {
		t.Skipf("bubblewrap isolation unavailable: %v", err)
	}
	for _, tool := range []string{userNetHelperName, "nsenter", "ip"} {
		if _, err := trustedbin.LookPath(tool); err != nil {
			t.Skipf("%s unavailable: %v", tool, err)
		}
	}
	old := registryCheckEndpoints
	registryCheckEndpoints = func(string) []registryEndpoint { return registryEndpoints("https://10.255.255.1/") }
	t.Cleanup(func() { registryCheckEndpoints = old })
	start := time.Now()
	check := checkRegistryReachable(context.Background(), env, "npm")
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("check took %s", elapsed)
	}
	if !check.done || len(check.unreachable) != 1 || !strings.HasPrefix(check.unreachable[0], "10.255.255.1 (") {
		t.Fatalf("check = %+v, want the blocked destination unreachable from the sandbox", check)
	}
}
