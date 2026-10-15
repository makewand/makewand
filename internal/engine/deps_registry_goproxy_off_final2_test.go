//go:build linux

package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Final round 2 (goproxy-off): makewand's connectivity check reaches the
// checksum database the way the go command does on every GOPROXY. The go
// command's own rules (cmd/go/internal/modfetch proxyList, TryProxies and
// dbClient.initBase) are modelled below and the check is compared with them:
// in TryProxies a "direct" entry outranks the error of a proxy passed over
// behind '|' before it, an "off" entry does not, so after such an error the
// database stays unavailable and the go command never contacts it.

// goCommandProxySpec is an entry of the go command's proxy list.
type goCommandProxySpec struct {
	url             string
	fallBackOnError bool
}

// goCommandProxyList is go1.27 cmd/go/internal/modfetch proxyList, without
// the "noproxy" entry a GONOPROXY setting adds (dbClient.initBase passes it
// over) and without newProxyRepo's URL validation.
func goCommandProxyList(goproxy string) []goCommandProxySpec {
	var list []goCommandProxySpec
	for goproxy != "" {
		var url string
		fallBackOnError := false
		if i := strings.IndexAny(goproxy, ",|"); i >= 0 {
			url = goproxy[:i]
			fallBackOnError = goproxy[i] == '|'
			goproxy = goproxy[i+1:]
		} else {
			url = goproxy
			goproxy = ""
		}
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		if url == "off" || url == "direct" {
			list = append(list, goCommandProxySpec{url: url})
			break
		}
		if strings.ContainsAny(url, ".:/") && !strings.Contains(url, ":/") && !filepath.IsAbs(url) && !path.IsAbs(url) {
			url = "https://" + url
		}
		list = append(list, goCommandProxySpec{url: url, fallBackOnError: fallBackOnError})
	}
	return list
}

// goCommandSumDBBase is dbClient.initBase over TryProxies for the checksum
// database sum.golang.org: base is "proxy:<url>" for the proxy that serves
// it, "direct" when the go command connects to the database itself, and
// "error:<url>" for the proxy whose error leaves the database unavailable.
// supported answers <proxy>/sumdb/sum.golang.org/supported for a proxy:
// "none" (no response) or an HTTP status. contacted lists the network URLs
// the go command requests, in order, the database itself included.
func goCommandSumDBBase(list []goCommandProxySpec, supported func(proxy string) string) (base string, contacted []string) {
	const (
		notExistRank = iota
		proxyRank
		directRank
	)
	type tryErr struct {
		notExist bool
		from     string
	}
	var bestErr *tryErr
	bestRank := notExistRank
	for _, p := range list {
		var err *tryErr
		switch p.url {
		case "direct", "off":
			err = &tryErr{notExist: true, from: p.url} // errProxyOff
		default:
			if isNetworkGoProxy(p.url) {
				contacted = append(contacted, strings.TrimSuffix(p.url, "/")+"/sumdb/sum.golang.org/supported")
			}
			switch supported(p.url) {
			case "200":
				return "proxy:" + p.url, contacted
			case "404", "410":
				err = &tryErr{notExist: true, from: p.url}
			default:
				err = &tryErr{from: p.url}
			}
		}
		switch {
		case p.url == "direct":
			bestErr, bestRank = err, directRank
		case bestRank <= proxyRank && !err.notExist:
			bestErr, bestRank = err, proxyRank
		case bestRank == notExistRank:
			bestErr = err
		}
		if !p.fallBackOnError && !err.notExist {
			break
		}
	}
	if bestErr == nil || bestErr.notExist {
		return "direct", append(contacted, "https://sum.golang.org/")
	}
	return "error:" + bestErr.from, contacted
}

// The sandbox's GOPROXY is parsed exactly as the go command parses it.
func TestGoProxyListMatchesGoCommand_Final2(t *testing.T) {
	tokens := []string{"https://a.example", "https://b.example/", "goproxy.cn", "direct", "off", "", " ", "file:///tmp/x", "http://c.example", " https://d.example ", "localhost:8080", "corp"}
	r := rand.New(rand.NewSource(1)) //nolint:gosec // G404: a seeded generator keeps the generated test inputs reproducible.
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for j, n := 0, 1+r.Intn(5); j < n; j++ {
			if j > 0 || r.Intn(4) == 0 {
				b.WriteByte(",|"[r.Intn(2)])
			}
			b.WriteString(tokens[r.Intn(len(tokens))])
		}
		if r.Intn(4) == 0 {
			b.WriteByte(",|"[r.Intn(2)])
		}
		value := b.String()
		want := goCommandProxyList(value)
		got := parseGoProxyList(value)
		same := len(want) == len(got)
		for k := 0; same && k < len(want); k++ {
			same = want[k].url == got[k].url && want[k].fallBackOnError == got[k].fallBackOnError
		}
		if !same {
			t.Fatalf("GOPROXY=%q: makewand reads %+v, the go command %+v", value, got, want)
		}
	}
}

// goProxyOffFakeAsk answers the probe's requests from the outcomes of one
// case, records the URLs asked and prints the probe's output lines.
type goProxyOffFakeAsk struct {
	answer   map[string]string // proxy URL -> its support endpoint's outcome
	directUp bool
	asked    []string
	out      strings.Builder
}

func (f *goProxyOffFakeAsk) ask(target string, _ bool) (int, string) {
	f.asked = append(f.asked, target)
	outcome := "none"
	if target == "https://sum.golang.org/" {
		if f.directUp {
			outcome = "404" // any response: the database answers
		}
	} else {
		outcome = f.answer[strings.TrimSuffix(target, "/sumdb/sum.golang.org/supported")]
	}
	if outcome == "none" {
		fmt.Fprintf(&f.out, "unreachable %s dial tcp: i/o timeout\n", target)
		return 0, "dial tcp: i/o timeout"
	}
	status, _ := strconv.Atoi(outcome)
	fmt.Fprintf(&f.out, "reachable %s %d\n", target, status)
	return status, ""
}

// On random GOPROXY values, answers and host module caches, the check's
// verdict on the checksum database is the go command's, and its probe asks
// exactly the URLs the go command requests: none it would not contact.
func TestGoSumDBChainMatchesGoCommand_Final2(t *testing.T) {
	hosts := []string{"https://p1.example", "https://p2.example", "https://p3.example", "https://p4.example"}
	outcomes := []string{"none", "200", "404", "410", "500", "403", "302"}
	cacheWithout := t.TempDir() // a host module cache that does not serve the database
	writeFile(t, filepath.Join(cacheWithout, "cache", "download", "golang.org", "x", "text", "@v", "list"), "")
	cacheWith := t.TempDir() // one that does
	writeFile(t, filepath.Join(cacheWith, "cache", "download", "sumdb", "sum.golang.org", "supported"), "")
	r := rand.New(rand.NewSource(20261014)) //nolint:gosec // G404: a seeded generator keeps the generated test inputs reproducible.
	const runs = 40000
	offAfterPipeFailure := 0
	for i := 0; i < runs; i++ {
		var b strings.Builder
		for j, host := range hosts[:1+r.Intn(len(hosts))] {
			if j > 0 {
				b.WriteByte(",|"[r.Intn(2)])
			}
			b.WriteString(host)
		}
		tail := []string{"", "direct", "off"}[r.Intn(3)]
		if tail != "" {
			b.WriteByte(",|"[r.Intn(2)])
			b.WriteString(tail)
			if r.Intn(4) == 0 {
				b.WriteString(",https://never.example") // ignored after "direct" or "off"
			}
		}
		env := hostGoEnv{GOPROXY: b.String()}
		cache := ""
		switch r.Intn(3) {
		case 1:
			env.GOMODCACHE, cache = cacheWithout, "404"
		case 2:
			env.GOMODCACHE, cache = cacheWith, "200"
		}
		fake := &goProxyOffFakeAsk{answer: map[string]string{}, directUp: r.Intn(2) == 0}
		for _, h := range hosts {
			fake.answer[h] = outcomes[r.Intn(len(outcomes))]
		}
		sandboxValue, _ := sandboxGoProxy(env)
		base, contacted := goCommandSumDBBase(goCommandProxyList(sandboxValue), func(proxy string) string {
			if strings.HasPrefix(proxy, "file://") {
				return cache
			}
			return fake.answer[proxy]
		})
		describe := func() string {
			return fmt.Sprintf("GOPROXY=%q (sandbox %q) answers=%v directUp=%t: go command %s contacting %q", env.GOPROXY, sandboxValue, fake.answer, fake.directUp, base, contacted)
		}
		endpoint := goSumDBEndpoint(env, sandboxGoProxies(env))
		if strings.HasPrefix(base, "proxy:file://") {
			if endpoint != nil {
				t.Fatalf("%s; makewand checks %q though the local cache serves the database", describe(), endpoint.chain())
			}
			continue
		}
		if endpoint == nil {
			t.Fatalf("%s; makewand checks no database", describe())
		}
		walkRegistryChain(strings.Fields(endpoint.probeArgument()), fake.ask)
		if !slices.Equal(fake.asked, contacted) {
			t.Fatalf("%s; the probe asked %q", describe(), fake.asked)
		}
		check := parseRegistryCheck(registryProbeRun{stdout: fake.out.String()}, []registryEndpoint{*endpoint}, false)
		var ok bool
		switch {
		case !check.done:
			ok = false
		case strings.HasPrefix(base, "proxy:"):
			ok = slices.Equal(check.reached, []string{urlHost(strings.TrimPrefix(base, "proxy:"))}) && len(check.unreachable) == 0
		case base == "direct" && fake.directUp:
			ok = slices.Equal(check.reached, []string{"sum.golang.org"}) && len(check.unreachable) == 0
		case base == "direct":
			ok = len(check.reached) == 0 && slices.ContainsFunc(check.unreachable, func(f string) bool { return strings.HasPrefix(f, "sum.golang.org (") })
		default: // the go command reports the database unavailable through this proxy
			blamed := urlHost(strings.TrimPrefix(base, "error:")) + " ("
			ok = len(check.reached) == 0 && slices.ContainsFunc(check.unreachable, func(f string) bool { return strings.HasPrefix(f, blamed) })
			if tail == "off" && strings.Contains(sandboxValue, "|") {
				offAfterPipeFailure++
			}
		}
		if !ok {
			t.Fatalf("%s; makewand's check gives %+v", describe(), check)
		}
	}
	if offAfterPipeFailure < 100 {
		t.Fatalf("only %d cases of an 'off' entry after a '|' proxy's failure: the generator no longer covers the case", offAfterPipeFailure)
	}
}

// The verifier's case: a '|' proxy's failure before "off" leaves the database
// unavailable, before "direct" the go command connects to it itself.
func TestGoSumDBOffAfterPipeFailure_Final2(t *testing.T) {
	const a, b = "https://a.example", "https://b.example"
	for _, tc := range []struct {
		goproxy     string
		chain       string
		reached     []string
		unreachable []string
	}{
		{a + "|" + b + "|off", "?|https://a.example/sumdb/sum.golang.org/supported ?|https://b.example/sumdb/sum.golang.org/supported !https://sum.golang.org/",
			nil, []string{"b.example (refused)"}},
		{a + "," + b + "|off", "?https://a.example/sumdb/sum.golang.org/supported ?|https://b.example/sumdb/sum.golang.org/supported !https://sum.golang.org/",
			nil, []string{"b.example (refused)"}},
		{a + "|" + b + "|direct", "?|https://a.example/sumdb/sum.golang.org/supported ?|https://b.example/sumdb/sum.golang.org/supported https://sum.golang.org/",
			[]string{"sum.golang.org"}, nil},
		{"off", "https://sum.golang.org/", []string{"sum.golang.org"}, nil},
	} {
		endpoint := goSumDBEndpoint(hostGoEnv{GOPROXY: tc.goproxy}, sandboxGoProxies(hostGoEnv{GOPROXY: tc.goproxy}))
		if endpoint == nil {
			t.Fatalf("GOPROXY=%s: no database chain", tc.goproxy)
		}
		if got := goProxyChainDescription(*endpoint); got != tc.chain {
			t.Fatalf("GOPROXY=%s: chain %q, want %q", tc.goproxy, got, tc.chain)
		}
		stdout := "reachable https://a.example/sumdb/sum.golang.org/supported 404\n" +
			"unreachable https://b.example/sumdb/sum.golang.org/supported refused\n" +
			"reachable https://sum.golang.org/ 404\n"
		check := parseRegistryCheck(registryProbeRun{stdout: stdout}, []registryEndpoint{*endpoint}, false)
		if !check.done || !slices.Equal(check.reached, tc.reached) || !slices.Equal(check.unreachable, tc.unreachable) {
			t.Fatalf("GOPROXY=%s: check = %+v, want reached %q unreachable %q", tc.goproxy, check, tc.reached, tc.unreachable)
		}
	}
}

// End to end through the real probe (the test binary) and local servers, the
// verifier's GOPROXY: the first proxy answers 404 for the database, the next
// one, behind '|', does not answer, and "off" follows. The check reports the
// database unavailable, as the go command finds it, and the probe never
// contacts the database itself (every request to a host other than the local
// servers goes through a recording proxy that refuses it). A failed install
// is then an environment problem, not the candidate's failure.
func TestGoSumDBOffAfterPipeFailureEndToEnd_Final2(t *testing.T) {
	handler, asked := recordingHandler(map[string]int{"/sumdb/sum.golang.org/supported": http.StatusNotFound}, nil)
	up := goProxyTLSServers(t, handler)[0]
	down := closedAddr(t)
	proxied := refuseNonLocalRequests(t)
	goproxy := "https://" + up + "|https://" + down + "|off"
	fakeGoOnPath(t, hostGoEnv{GOPROXY: goproxy})
	check := checkRegistryReachable(context.Background(), verifyExecEnvironment{mode: verifyExecUnsafeHost}, "go")
	if !check.done || !slices.Equal(check.reached, []string{"127.0.0.1"}) || len(check.unreachable) != 1 ||
		!strings.HasPrefix(check.unreachable[0], "127.0.0.1 (") || !strings.Contains(check.unreachable[0], down) {
		t.Fatalf("check = %+v, want the modules reached and the database unavailable through %s", check, down)
	}
	if got := proxied(); len(got) != 0 {
		t.Fatalf("the probe contacted %q, which the go command never contacts here", got)
	}
	got := asked()
	slices.Sort(got)
	if want := []string{"GET /sumdb/sum.golang.org/supported", "HEAD /"}; !slices.Equal(got, want) {
		t.Fatalf("the answering proxy got %q, want %q", got, want)
	}
}

// The same through RunRestrictedPlan: `go mod tidy` failing on the database
// it cannot reach is an environment problem.
func TestGoSumDBOffAfterPipeFailureIsEnvironmentError_Final2(t *testing.T) {
	handler, _ := recordingHandler(map[string]int{"/sumdb/sum.golang.org/supported": http.StatusNotFound}, nil)
	up := goProxyTLSServers(t, handler)[0]
	down := closedAddr(t)
	proxied := refuseNonLocalRequests(t)
	project := newHostDepsProject(t)
	fakeToolPrinting(t, "go", `go: verifying go.mod: example.com/x@v1.0.0/go.mod: Get "https://`+down+`/sumdb/sum.golang.org/supported": dial tcp `+down+`: connect: connection refused`, 1)
	oldProbe := sandboxGoEnvProbe
	sandboxGoEnvProbe = func(string) (hostGoEnv, error) {
		return hostGoEnv{GOPROXY: "https://" + up + "|https://" + down + "|off"}, nil
	}
	t.Cleanup(func() { sandboxGoEnvProbe = oldProbe })
	_, err := project.RunRestrictedPlan(context.Background(), ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "tidy"}})
	if !errors.Is(err, ErrDependencyNetworkUnavailable) || !strings.Contains(err.Error(), down) {
		t.Fatalf("err = %v, want ErrDependencyNetworkUnavailable naming %s", err, down)
	}
	if got := proxied(); len(got) != 0 {
		t.Fatalf("the check contacted %q, which the go command never contacts here", got)
	}
}

// refuseNonLocalRequests routes every request of the probe to a host other
// than loopback (the local test servers) through a proxy that records and
// refuses it, so no request leaves the machine; it returns the requests.
func refuseNonLocalRequests(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var proxied []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proxied = append(proxied, r.Method+" "+r.Host)
		mu.Unlock()
		http.Error(w, "refused by the test", http.StatusForbidden)
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(proxied)
	}
}
