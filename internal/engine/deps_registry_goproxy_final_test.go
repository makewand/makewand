//go:build linux

package engine

import (
	"slices"
	"strings"
	"testing"
)

// Final round: makewand's connectivity check walks GOPROXY exactly as the go
// command does. After an entry followed by '|' the go command moves on after
// any error, after one followed by ',' only after 404 or 410 - for module
// downloads and for <proxy>/sumdb/<name>/supported alike - and it treats any
// other answer to the latter as the checksum database being unavailable.

func TestParseGoProxyList_Final(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  []goProxySpec
	}{
		{"", nil},
		{"https://a.example,https://b.example", []goProxySpec{{"https://a.example", false}, {"https://b.example", false}}},
		{"https://a.example|https://b.example", []goProxySpec{{"https://a.example", true}, {"https://b.example", false}}},
		{"https://a.example|https://b.example,direct", []goProxySpec{{"https://a.example", true}, {"https://b.example", false}, {"direct", false}}},
		{"goproxy.cn|direct", []goProxySpec{{"https://goproxy.cn", true}, {"direct", false}}},
		{"localhost:3000", []goProxySpec{{"https://localhost:3000", false}}},
		{" https://a.example , ,|off,https://never.example", []goProxySpec{{"https://a.example", false}, {"off", false}}},
		{"file:///cache,https://a.example|direct,https://never.example", []goProxySpec{{"file:///cache", false}, {"https://a.example", true}, {"direct", false}}},
		{"/srv/proxy", []goProxySpec{{"/srv/proxy", false}}},
	} {
		if got := parseGoProxyList(tc.value); !slices.Equal(got, tc.want) {
			t.Errorf("parseGoProxyList(%q) = %+v, want %+v", tc.value, got, tc.want)
		}
	}
}

// goProxyChainDescription renders a chain with the flags of each URL:
// "?" deciding, "|" falls back on error, "!" only after not-found answers.
func goProxyChainDescription(endpoint registryEndpoint) string {
	var links []string
	for link := &endpoint; link != nil; link = link.fallback {
		flags := ""
		if link.decides {
			flags += "?"
		}
		if link.fallBackOnError {
			flags += "|"
		}
		if link.afterNotFoundOnly {
			flags += "!"
		}
		links = append(links, flags+link.url)
	}
	return strings.Join(links, " ")
}

// The chains the check walks for each GOPROXY: the module proxies up to the
// first one the go command never gets past without an answer, and the
// checksum database through each proxy's support endpoint, then directly.
func TestGoRegistryChainsFollowGOPROXYSeparators_Final(t *testing.T) {
	const sumdb = "sum.golang.org"
	for _, tc := range []struct {
		name, goproxy string
		want          []string
	}{
		{"default", "", []string{
			"https://proxy.golang.org",
			"?https://proxy.golang.org/sumdb/sum.golang.org/supported https://sum.golang.org/",
		}},
		{"pipe without direct", "https://proxy.golang.org|https://goproxy.cn", []string{
			"|https://proxy.golang.org https://goproxy.cn",
			"?|https://proxy.golang.org/sumdb/sum.golang.org/supported ?https://goproxy.cn/sumdb/sum.golang.org/supported !https://sum.golang.org/",
		}},
		{"pipe with direct", "https://proxy.golang.org|https://goproxy.cn,direct", []string{
			"|https://proxy.golang.org https://goproxy.cn",
			"?|https://proxy.golang.org/sumdb/sum.golang.org/supported ?https://goproxy.cn/sumdb/sum.golang.org/supported https://sum.golang.org/",
		}},
		{"comma stops the module walk", "https://a.example,https://b.example", []string{
			"https://a.example",
			"?https://a.example/sumdb/sum.golang.org/supported ?https://b.example/sumdb/sum.golang.org/supported !https://sum.golang.org/",
		}},
		{"pipe then comma", "https://a.example|https://b.example,https://c.example|direct", []string{
			"|https://a.example https://b.example",
			"?|https://a.example/sumdb/sum.golang.org/supported ?https://b.example/sumdb/sum.golang.org/supported ?|https://c.example/sumdb/sum.golang.org/supported https://sum.golang.org/",
		}},
		{"bare host", "goproxy.cn|direct", []string{
			"|https://goproxy.cn",
			"?|https://goproxy.cn/sumdb/sum.golang.org/supported https://sum.golang.org/",
		}},
		{"direct only", "direct", []string{"https://sum.golang.org/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeGoOnPath(t, hostGoEnv{GOPROXY: tc.goproxy, GOSUMDB: sumdb})
			var got []string
			for _, endpoint := range defaultRegistryEndpoints("go", "") {
				got = append(got, goProxyChainDescription(endpoint))
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("chains:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// How the check reads the probe's answers along a chain, as the go command
// walks GOPROXY.
func TestParseRegistryCheckFollowsGOPROXYSeparators_Final(t *testing.T) {
	modulesPipe := registryChain([]registryEndpoint{{url: "https://a.example", fallBackOnError: true}, {url: "https://b.example"}})
	modulesComma := registryChain([]registryEndpoint{{url: "https://a.example"}})
	sumdbNoDirect := registryChain([]registryEndpoint{
		{url: "https://a.example/sumdb/s/supported", decides: true, fallBackOnError: true},
		{url: "https://b.example/sumdb/s/supported", decides: true},
		{url: "https://s.example/", afterNotFoundOnly: true},
	})
	sumdbDirect := registryChain([]registryEndpoint{
		{url: "https://a.example/sumdb/s/supported", decides: true, fallBackOnError: true},
		{url: "https://b.example/sumdb/s/supported", decides: true},
		{url: "https://s.example/"},
	})
	for _, tc := range []struct {
		name        string
		endpoint    *registryEndpoint
		stdout      string
		unreachable []string
		reached     []string
	}{
		{"modules: '|' moves on after no response", modulesPipe,
			"unreachable https://a.example blocked\nreachable https://b.example 200\n", nil, []string{"b.example"}},
		{"modules: '|' moves on to a proxy that does not answer either", modulesPipe,
			"unreachable https://a.example blocked\nunreachable https://b.example refused\n", []string{"a.example (blocked)", "b.example (refused)"}, nil},
		{"modules: ',' stops at no response", modulesComma,
			"unreachable https://a.example blocked\n", []string{"a.example (blocked)"}, nil},
		{"modules: any answer is a reachable proxy", modulesPipe,
			"reachable https://a.example 404\n", nil, []string{"a.example"}},
		{"database: '|' moves on after any other answer", sumdbNoDirect,
			"reachable https://a.example/sumdb/s/supported 503\nreachable https://b.example/sumdb/s/supported 200\n", nil, []string{"b.example"}},
		{"database: ',' proxy answering anything else leaves it unavailable", sumdbNoDirect,
			"reachable https://a.example/sumdb/s/supported 404\nreachable https://b.example/sumdb/s/supported 500\n", []string{"b.example (answered 500 Internal Server Error)"}, nil},
		{"database: ',' proxy answering 403 leaves it unavailable", sumdbNoDirect,
			"reachable https://a.example/sumdb/s/supported 410\nreachable https://b.example/sumdb/s/supported 403\n", []string{"b.example (answered 403 Forbidden)"}, nil},
		{"database: not found everywhere, then the database itself", sumdbNoDirect,
			"reachable https://a.example/sumdb/s/supported 404\nreachable https://b.example/sumdb/s/supported 410\nreachable https://s.example/ 302\n", nil, []string{"s.example"}},
		{"database: a passed-over failure and no direct entry leave it unavailable", sumdbNoDirect,
			"unreachable https://a.example/sumdb/s/supported blocked\nreachable https://b.example/sumdb/s/supported 404\nreachable https://s.example/ 200\n", []string{"a.example (blocked)"}, nil},
		{"database: a passed-over failure before a direct entry", sumdbDirect,
			"unreachable https://a.example/sumdb/s/supported blocked\nreachable https://b.example/sumdb/s/supported 404\nreachable https://s.example/ 200\n", nil, []string{"s.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := parseRegistryCheck(registryProbeRun{stdout: tc.stdout}, []registryEndpoint{*tc.endpoint}, false)
			if !check.done || !slices.Equal(check.unreachable, tc.unreachable) || !slices.Equal(check.reached, tc.reached) {
				t.Fatalf("check = %+v, want unreachable %q reached %q", check, tc.unreachable, tc.reached)
			}
		})
	}
}
