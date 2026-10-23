package engine

import (
	"os"
	"testing"
)

// liveNetworkTestsEnv opts in to test steps that reach the internet: the host
// resolver, public endpoints and package registries. Without it the tests
// connect to nothing outside this host.
const liveNetworkTestsEnv = "MAKEWAND_TEST_LIVE_NETWORK"

func liveNetworkTests() bool {
	return os.Getenv(liveNetworkTestsEnv) == "1"
}

// liveNetworkExpectations returns whether the sandbox's DNS forwarder must
// answer and which public endpoint it must reach, asking the host resolver
// and public endpoints only when live network tests are enabled.
func liveNetworkExpectations(resolverAnswers func() bool, reachablePublicAddr func() string) (expectDNS bool, public string) {
	if !liveNetworkTests() {
		return false, ""
	}
	return resolverAnswers(), reachablePublicAddr()
}

func TestLiveNetworkChecksAreOptIn(t *testing.T) {
	contacted := 0
	resolver := func() bool { contacted++; return true }
	public := func() string { contacted++; return "192.0.2.1:443" }
	t.Setenv(liveNetworkTestsEnv, "")
	if dns, addr := liveNetworkExpectations(resolver, public); dns || addr != "" || contacted != 0 {
		t.Fatalf("without %s=1: dns=%v public=%q, %d network probes", liveNetworkTestsEnv, dns, addr, contacted)
	}
	t.Setenv(liveNetworkTestsEnv, "1")
	if dns, addr := liveNetworkExpectations(resolver, public); !dns || addr != "192.0.2.1:443" || contacted != 2 {
		t.Fatalf("with %s=1: dns=%v public=%q, %d network probes", liveNetworkTestsEnv, dns, addr, contacted)
	}
}
