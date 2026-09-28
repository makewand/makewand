package router

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestRouterReadmeDocumentsHTTPSecurityContract is the doc regression for
// go-router#7/#8/#9: the README's HTTP example bound an unauthenticated
// facade to all interfaces, and it said nothing about HTTPHandlerOptions auth,
// RepoTrust, where api_policy is enforced, or that remote messages drive local
// CLIs on the serving host.
func TestRouterReadmeDocumentsHTTPSecurityContract(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readme := string(raw)
	// Prose is line-wrapped; compare phrases on whitespace-normalized text.
	flat := strings.Join(strings.Fields(readme), " ")

	allInterfaces := regexp.MustCompile(`ListenAndServe\(\s*":\d+"`)
	if loc := allInterfaces.FindStringIndex(readme); loc != nil {
		t.Fatalf("README example listens on all interfaces: %q", readme[loc[0]:loc[1]])
	}
	if strings.Contains(readme, "r.HTTPHandler())") {
		t.Fatal("README example serves HTTPHandler() without any auth option")
	}
	for _, want := range []string{
		`ListenAndServe("127.0.0.1:`,
		"HTTPHandlerOptions",
		"BearerToken",
		"Authorizer",
		"without authentication",
		"chat:invoke",
		"RepoTrustUntrusted",
		"SetRepoTrust",
		"UntrustedRepoCapable",
		"ErrNoUntrustedSafeProvider",
		"api_policy",
		"Not Enforced by the Library",
		"ContextWithRemoteOrigin",
		"review --uncommitted",
		"server's OS user",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("README.md is missing %q", want)
		}
	}
}
