package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/makewand/makewand/internal/trustedbin"
)

// Dependency installs need a package registry, but they must never share the
// host network namespace: install steps that execute project code (setup.py,
// build.rs, npm lifecycle scripts) could then reach services on host loopback
// and host abstract AF_UNIX sockets. Like every other verification step they
// run in a fresh network namespace (bwrap --unshare-net). When a trusted
// slirp4netns is installed it is attached to that namespace and provides
// outbound-only user-mode networking:
//
//   - abstract AF_UNIX sockets are scoped to a network namespace, so host
//     abstract sockets stay unreachable;
//   - 127.0.0.0/8 inside the sandbox is the sandbox's own loopback, and
//     --disable-host-loopback stops slirp4netns from mapping its gateway
//     (10.0.2.2) onto host loopback;
//   - DNS goes to slirp4netns' built-in forwarder (10.0.2.3), so a host stub
//     resolver on loopback (systemd-resolved) is not needed.
//
// slirp4netns opens its outbound connections from the host, so every address
// the host can reach would be reachable too: the host's own addresses (where
// services bound to 0.0.0.0 listen), the LAN, VPNs, container bridges and
// cloud metadata endpoints. Before the held command runs, unreachable routes
// for private, loopback, link-local, CGNAT, multicast and reserved
// destinations and for the host's own addresses are installed in the
// sandbox's network namespace (userNetBlockedIPv4); inside slirp4netns' own
// 10.0.2.0/24 only the DNS forwarder stays reachable. Routes cannot stop a
// socket that names its outgoing interface (SO_BINDTODEVICE, IP_MULTICAST_IF,
// IP_PKTINFO): unicast then goes out on-link and dies unanswered, because
// slirp4netns answers ARP only for its gateway (which it refuses to connect
// anywhere) and its DNS address (which it forwards for port 53 only), but
// multicast would still leave: slirp4netns resends it from a host socket onto
// the host's LAN segment (clearing IFF_MULTICAST on the tap device does not
// stop that; slirp4netns creates it without the flag). When a trusted tc is
// available, a filter on the tap device's egress hook therefore drops IPv4
// multicast before slirp4netns reads it, and when a trusted nft is available
// an output filter drops the same destinations as the routes (and all IPv6)
// regardless of the interface. Both are best effort: on a host with neither,
// a step can still send UDP multicast datagrams onto the host's LAN segment
// (slirp4netns sets no multicast TTL on its host sockets, so the default of 1
// applies) through a socket that names the tap device. IPv6 is never enabled
// on the user-mode link.
//
// The routes are part of the launch contract: if the namespace cannot be
// identified, slirp4netns cannot attach or the routes cannot be installed,
// the held sandbox is killed before its command runs and the launch reports
// the failure and the stage that failed (attached is false). slirp4netns
// itself runs with its own sandbox and seccomp filter, and only for
// dependency installs.
//
// Without slirp4netns (or nsenter, which enters the sandbox's namespaces, and
// ip, which installs the routes) the install keeps the loopback-only
// namespace. A download failure is then reported as
// ErrDependencyNetworkUnavailable (an environment problem) instead of a
// candidate failure, when the install needed a registry at all (see
// dependencyRegistryError).
//
// What a failed install prints can only raise the suspicion of a registry
// outage: candidate code prints too (npm lifecycle scripts, setup.py and
// build backends, build.rs), and project configuration (.npmrc, a
// requirements file's --index-url, .cargo/config.toml) can point the install
// at a host that does not answer. makewand confirms the outage itself
// (checkRegistryReachable): makewand's own executable, started as a probe
// from an empty directory in a fresh sandbox with the same kind of network
// (or on the host under the acknowledged unsafe opt-in), asks the endpoints
// of the ecosystem's default registry for a response. Only when that check
// gets none is the failure an environment problem; a check that cannot run
// confirms nothing.

// userNetHelperName is the user-mode network helper. It is resolved from
// root-owned system directories only (or MAKEWAND_SLIRP4NETNS_PATH), never
// from $PATH, because it runs on the host outside the sandbox. nsenter, ip,
// tc and nft are resolved the same way.
const userNetHelperName = "slirp4netns"

// userNetDNS is slirp4netns' built-in DNS forwarder address.
const userNetDNS = "10.0.2.3"

// userNetDevice is the sandbox-side interface slirp4netns creates.
const userNetDevice = "tap0"

// userNetBlockedIPv4 are the destinations a dependency install must never
// reach: none of them is a public registry, and several reach the host or
// its networks through slirp4netns. 198.18.0.0/15 stays reachable because
// fake-IP proxies (a common way hosts reach registries) answer DNS from it.
var userNetBlockedIPv4 = []string{
	"0.0.0.0/8",       // "this network"
	"10.0.0.0/8",      // private; slirp4netns' 10.0.2.0/24 is more specific
	"100.64.0.0/10",   // carrier-grade NAT
	"127.0.0.0/8",     // loopback, kept off the user-mode link
	"169.254.0.0/16",  // link-local, cloud metadata services
	"172.16.0.0/12",   // private, container bridges
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"192.168.0.0/16",  // private
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, limited broadcast
}

// userNetToolPackages names the package providing each required tool, for
// guidance when one is missing or fails.
var userNetToolPackages = map[string]string{
	"nsenter": "util-linux",
	"ip":      "iproute2",
}

// userNetToolRoles says what each required tool does, for guidance when one
// is missing.
var userNetToolRoles = map[string]string{
	"nsenter": "which enters the sandbox's namespaces to attach slirp4netns and configure the sandbox network",
	"ip":      "which installs the routes that keep the sandbox network off private and host addresses",
}

// userNetMulticastFilter is the `tc -batch` input that drops IPv4 multicast
// on the tap device's egress hook, whatever interface the sending socket
// named. The sandbox has no capabilities in its network namespace, so it
// cannot remove the filter.
const userNetMulticastFilter = "qdisc add dev " + userNetDevice + " clsact\n" +
	"filter add dev " + userNetDevice + " egress protocol ip prio 1 u32 match ip dst 224.0.0.0/4 action drop\n"

// Test seams.
var (
	userNetLookPath       = trustedbin.LookPath
	userNetAttachTimeout  = 20 * time.Second
	userNetReleaseTimeout = 2 * time.Second
	userNetResolvConf     = "/etc/resolv.conf"
	userNetHostAddrs      = net.InterfaceAddrs
	userNetProcDir        = "/proc"
	userNetOpenNamespaces = openSandboxNamespaces

	registryProbeExecutable = registryProbeSelf
	registryCheckEndpoints  = defaultRegistryEndpoints
	registryCheckTimeout    = 30 * time.Second
	checkRegistry           = checkRegistryReachable
)

// ErrDependencyNetworkUnavailable reports that a dependency install failed
// because a package registry could not be reached from where it ran. It is an
// environment problem, never evidence about the candidate.
var ErrDependencyNetworkUnavailable = errors.New("dependency install could not reach the network")

// sandboxNetworkKind is the network a verification command ran with.
type sandboxNetworkKind int

const (
	// sandboxNetworkNone: a fresh network namespace with loopback only.
	sandboxNetworkNone sandboxNetworkKind = iota
	// sandboxNetworkUserMode: a fresh network namespace with outbound access
	// through slirp4netns.
	sandboxNetworkUserMode
	// sandboxNetworkHost: acknowledged unsafe host execution.
	sandboxNetworkHost
)

// sandboxNetwork describes the network of one command run, for guidance when
// a dependency install cannot reach its registry.
type sandboxNetwork struct {
	kind sandboxNetworkKind
	// detail explains why outbound access was not available (kind None).
	detail string
	// remedy replaces the advice to install slirp4netns (kind None).
	remedy string
	// attach, when set (kind None), is why the installed helper could not be
	// attached; it replaces detail and remedy.
	attach *userNetFailure
}

// environmentNote ends every guidance message.
const environmentNote = "this is an environment problem, not a candidate failure"

// guidance explains why a dependency install without outbound network (kind
// None) could not reach its registry, and how to give it one. An install
// with outbound network gets unreachableGuidance instead, once makewand's own
// connectivity check has confirmed the outage.
func (n sandboxNetwork) guidance() string {
	if n.attach != nil {
		return n.attach.guidance()
	}
	detail := n.detail
	if detail == "" {
		detail = "the step ran without outbound network"
	}
	remedy := n.remedy
	if remedy == "" {
		remedy = "install " + userNetHelperName + " from your distribution (e.g. `sudo apt install slirp4netns` or `sudo dnf install slirp4netns`) or set " +
			trustedbin.OverrideEnv(userNetHelperName) + " to a trusted copy"
	}
	return "dependency installs run without outbound network because " + detail + "; " + remedy +
		", or install the project's dependencies outside makewand; " + environmentNote
}

// checkFailure explains, for makewand's own connectivity check, why its
// sandbox would have had no outbound network (kind None).
func (n sandboxNetwork) checkFailure() string {
	if n.attach != nil {
		return n.attach.checkFailure()
	}
	detail := n.detail
	if detail == "" {
		detail = "no user-mode network"
	}
	return "its sandbox would have had no outbound network: " + detail
}

// unreachableGuidance explains a registry outage that makewand's own
// connectivity check confirmed from this network; unreachable lists each
// endpoint that gave no response, with the reason.
func (n sandboxNetwork) unreachableGuidance(unreachable []string) string {
	checked := "makewand's own connectivity check could not reach " + strings.Join(unreachable, ", ")
	if n.kind == sandboxNetworkHost {
		return checked + " from this host either; check network connectivity, DNS and proxy settings; " + environmentNote
	}
	return checked + " from a fresh verification sandbox with outbound access through " + userNetHelperName +
		" either, so the host may be offline, DNS may be failing, or the registry is only reachable through a proxy or mirror on host loopback or a private network address (unreachable from the sandbox by design); " + environmentNote
}

// userNetStage is the part of a user-mode network launch that failed.
type userNetStage int

const (
	// userNetStageSandbox: the held sandbox, its namespaces or its resolver
	// configuration could not be identified or prepared.
	userNetStageSandbox userNetStage = iota
	// userNetStageHelper: slirp4netns did not configure the namespace.
	userNetStageHelper
	// userNetStageRoutes: the routes that keep the namespace off private and
	// host addresses could not be installed.
	userNetStageRoutes
)

// userNetStageError attributes a launch failure to the stage that failed.
type userNetStageError struct {
	stage userNetStage
	err   error
}

func (e *userNetStageError) Error() string {
	if e.stage == userNetStageRoutes {
		return "could not keep the sandbox network off private and host addresses: " + e.err.Error()
	}
	return e.err.Error()
}

func (e *userNetStageError) Unwrap() error { return e.err }

// userNetFailure is why an installed helper could not be attached.
type userNetFailure struct {
	stage userNetStage
	cause string
}

// guidance names the failed stage and its own remedy.
func (f userNetFailure) guidance() string {
	const ranOffline = "), so the dependency install ran without outbound network; "
	const outside = ", or install the project's dependencies outside makewand; " + environmentNote
	switch {
	case strings.Contains(f.cause, "nsenter: "):
		// nsenter runs slirp4netns and ip in the sandbox's namespaces.
		return "nsenter could not enter the sandbox's namespaces to configure its network (" + f.cause + ranOffline +
			"fix nsenter (" + userNetToolPackages["nsenter"] + ") on this host or set " + trustedbin.OverrideEnv("nsenter") + " to a working copy" + outside
	case f.stage == userNetStageRoutes:
		return userNetHelperName + " was attached, but the routes that keep the sandbox off private and host addresses could not be installed (" + f.cause + ranOffline +
			"fix ip (" + userNetToolPackages["ip"] + ") on this host or set " + trustedbin.OverrideEnv("ip") + " to a working copy" + outside
	case f.stage == userNetStageHelper:
		return userNetHelperName + " is installed but could not be attached to the sandbox (" + f.cause + ranOffline +
			"fix that on this host or set " + trustedbin.OverrideEnv(userNetHelperName) + " to a working copy" + outside
	default:
		return "the sandbox's network namespace could not be prepared for " + userNetHelperName + " (" + f.cause + ranOffline +
			"check that bubblewrap can create user and network namespaces on this host" + outside
	}
}

// checkFailure names the stage that kept makewand's own connectivity check
// from getting the user-mode network. Unlike guidance it says nothing about
// the dependency install, which had its network.
func (f userNetFailure) checkFailure() string {
	switch {
	case strings.Contains(f.cause, "nsenter: "):
		return "nsenter could not enter its sandbox's namespaces (" + f.cause + ")"
	case f.stage == userNetStageRoutes:
		return "the routes that keep its sandbox off private and host addresses could not be installed (" + f.cause + ")"
	case f.stage == userNetStageHelper:
		return userNetHelperName + " could not be attached to its sandbox (" + f.cause + ")"
	default:
		return "its sandbox's network namespace could not be prepared for " + userNetHelperName + " (" + f.cause + ")"
	}
}

// verificationPlanUsesUserNetwork reports whether a plan gets outbound access
// through the user-mode network helper. Only dependency installs do; every
// other step stays loopback-only.
func verificationPlanUsesUserNetwork(plan ExecPlan) bool {
	return plan.Kind == "deps"
}

// offlineDependencySettings returns sandbox environment settings for a
// dependency install that has no outbound network: npm's default retries
// (about 70 seconds per unreachable registry request) are disabled so the
// failure is reported promptly. Installs that need no download are unaffected.
func offlineDependencySettings() []string {
	return []string{"--setenv", "npm_config_fetch_retries", "0"}
}

// registryProbeArg, as its first argument, makes makewand's own executable
// the registry connectivity probe (runRegistryProbe) instead of makewand.
const registryProbeArg = "__makewand-registry-probe"

// registryProbeRequestTimeout bounds each request the probe makes.
const registryProbeRequestTimeout = 10 * time.Second

// registryCheckAttempts is how often makewand tries to run its connectivity
// check before the check counts as unable to run.
const registryCheckAttempts = 2

// When makewand's own executable is started as the registry connectivity
// probe, it probes and exits before anything else initializes. The check
// needs no interpreter or other program on the host or in the sandbox.
func init() {
	if len(os.Args) > 1 && os.Args[1] == registryProbeArg {
		os.Exit(runRegistryProbe(os.Stdout, os.Args[2:]))
	}
}

// The probe's command line names each URL of a chain with markers in front of
// it (registryEndpoint.probeArgument): registryDecidesMarker for a URL whose
// status decides how the go command goes on, registryFallBackMarker for one
// whose failure hands the question to the next URL of the chain.
const (
	registryDecidesMarker  = "?"
	registryFallBackMarker = "|"
)

// runRegistryProbe asks the URL chains on its command line (one argument per
// chain, URLs separated by spaces, each with its markers) for a response and
// prints one line per URL it asks: "reachable <url> <status>" for any HTTP
// response, "unreachable <url> <reason>" otherwise. Chains are asked
// concurrently, the URLs of a chain in turn until one settles the question
// (registryEndpoint): a deciding URL settles it only with 200 and hands it on
// after 404 or 410; any other URL settles it with any response. After any
// other answer (or none) the next URL is asked only behind a fall-back
// marker. Requests use verified TLS and honour the proxy settings of the
// environment (the sandbox clears them, as it does for the install); a
// deciding URL gets the GET the go command sends, following redirects as the
// go command does, every other URL a HEAD that follows none.
func runRegistryProbe(out io.Writer, chains []string) int {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// Shorter than the request, so a name lookup or connection that gets
		// no answer is reported as what it is.
		DialContext:         (&net.Dialer{Timeout: registryProbeRequestTimeout * 4 / 5}).DialContext,
		TLSHandshakeTimeout: registryProbeRequestTimeout,
		ForceAttemptHTTP2:   true,
	}
	plain := &http.Client{
		Timeout:       registryProbeRequestTimeout,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	deciding := &http.Client{Timeout: registryProbeRequestTimeout, Transport: transport, CheckRedirect: goCommandRedirectPolicy}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, chain := range chains {
		tokens := strings.Fields(chain)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, token := range tokens {
				target, decides, fallBack := parseRegistryProbeToken(token)
				client := plain
				if decides {
					client = deciding
				}
				status, reason := probeRegistryURL(client, target, decides)
				mu.Lock()
				if reason != "" {
					_, _ = fmt.Fprintln(out, "unreachable", target, reason)
				} else {
					_, _ = fmt.Fprintln(out, "reachable", target, status)
				}
				mu.Unlock()
				switch {
				case reason == "" && (!decides || status == http.StatusOK):
					return // settled
				case reason == "" && (status == http.StatusNotFound || status == http.StatusGone):
					// not found: the next URL is asked
				case !fallBack:
					return
				}
			}
		}()
	}
	wg.Wait()
	return 0
}

// parseRegistryProbeToken splits a URL of the probe's command line from its
// markers.
func parseRegistryProbeToken(token string) (target string, decides, fallBack bool) {
	for {
		switch {
		case strings.HasPrefix(token, registryDecidesMarker):
			decides, token = true, token[len(registryDecidesMarker):]
		case strings.HasPrefix(token, registryFallBackMarker):
			fallBack, token = true, token[len(registryFallBackMarker):]
		default:
			return token, decides, fallBack
		}
	}
}

// goCommandRedirectPolicy follows redirects as the go command does
// (cmd/go/internal/web): at most ten, and never from https to another scheme.
func goCommandRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("redirected from secure URL %s to insecure URL %s", via[len(via)-1].URL, req.URL)
	}
	return nil
}

// probeRegistryURL requests target and returns the HTTP status, or why there
// was no response: a GET for a URL whose answer decides how the go command
// goes on (the request the go command sends), a HEAD otherwise.
func probeRegistryURL(client *http.Client, target string, decides bool) (int, string) {
	method := http.MethodHead
	if decides {
		method = http.MethodGet
	}
	// The URLs are registry endpoints that makewand and the host configuration
	// name, never the project.
	req, err := http.NewRequest(method, target, nil) // #nosec G704
	if err != nil {
		return 0, registryProbeReason(err)
	}
	req.Header.Set("User-Agent", "makewand-registry-check")
	resp, err := client.Do(req) // #nosec G704
	if err != nil {
		return 0, registryProbeReason(err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return resp.StatusCode, ""
}

// registryProbeReason renders a request that got no response on one line.
func registryProbeReason(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	reason := strings.Join(strings.Fields(err.Error()), " ")
	switch {
	case strings.Contains(reason, "Client.Timeout exceeded"):
		reason = "no response within " + registryProbeRequestTimeout.String()
	case reason == "":
		reason = "no response"
	case len(reason) > 200:
		reason = reason[:200]
	}
	return reason
}

// registryEndpoint is a service a dependency install needs, as the chain of
// URLs its tool walks (for the go command: GOPROXY, see goModuleEndpoint and
// goSumDBEndpoint). A URL settles the question when its answer counts as a
// success: any HTTP response, or for a deciding URL only 200. A deciding URL
// that answers 404 or 410 hands the question on to the fallback. Any other
// answer, or none, is a failure: it hands the question on when fallBackOnError
// is set and otherwise leaves the service unreachable.
type registryEndpoint struct {
	url string
	// decides marks a URL the go command GETs to learn how to go on (a module
	// proxy's <proxy>/sumdb/<name>/supported).
	decides bool
	// fallBackOnError marks a URL whose failure the tool passes over, as the
	// go command passes over a GOPROXY entry followed by '|'.
	fallBackOnError bool
	// afterNotFoundOnly marks a fallback that is used only when no failure
	// was passed over on the way: the go command connects to the checksum
	// database itself after its proxies only when each of them answered 404
	// or 410, unless GOPROXY lists "direct" or "off" after them.
	afterNotFoundOnly bool
	fallback          *registryEndpoint
}

// chain returns the URLs of the endpoint in the order they are asked.
func (e registryEndpoint) chain() []string {
	var urls []string
	for link := &e; link != nil; link = link.fallback {
		urls = append(urls, link.url)
	}
	return urls
}

// probeArgument renders the chain for the probe's command line: its URLs in
// order, each with its markers (runRegistryProbe).
func (e registryEndpoint) probeArgument() string {
	var tokens []string
	for link := &e; link != nil; link = link.fallback {
		token := link.url
		if link.fallBackOnError {
			token = registryFallBackMarker + token
		}
		if link.decides {
			token = registryDecidesMarker + token
		}
		tokens = append(tokens, token)
	}
	return strings.Join(tokens, " ")
}

// registryChain links endpoints into a chain, each the fallback of the one
// before it, and returns its first URL (nil for none).
func registryChain(links []registryEndpoint) *registryEndpoint {
	var first *registryEndpoint
	for i := len(links) - 1; i >= 0; i-- {
		link := links[i]
		link.fallback = first
		first = &link
	}
	return first
}

// registryEndpoints returns plain endpoints for urls.
func registryEndpoints(urls ...string) []registryEndpoint {
	endpoints := make([]registryEndpoint, len(urls))
	for i, u := range urls {
		endpoints[i] = registryEndpoint{url: u}
	}
	return endpoints
}

// registryCheck is the outcome of checkRegistryReachable.
type registryCheck struct {
	// done reports a verdict for every endpoint. Without one the check proves
	// nothing, and failure says why it could not run.
	done    bool
	failure string
	// unreachable lists each endpoint that gave no response, with the reason.
	unreachable []string
	// reached lists the hosts that answered.
	reached []string
}

// registryProbeRun is the output of one run of the probe, or why it did not
// run (failure).
type registryProbeRun struct {
	stdout, stderr string
	failure        string
}

// checkRegistryReachable checks, from the same kind of network a failed
// dependency install had, whether the default registry of command's
// ecosystem answers: from a fresh sandbox with the user-mode network, or from
// the host under the acknowledged unsafe opt-in. makewand's own executable
// asks (runRegistryProbe), started from an empty directory, so neither
// candidate code nor project configuration can influence the result, and no
// interpreter has to be installed. A check that cannot run (its sandbox could
// not get the user-mode network, for example) is tried once more.
func checkRegistryReachable(ctx context.Context, env verifyExecEnvironment, command string) registryCheck {
	endpoints := registryCheckEndpoints(command)
	if len(endpoints) == 0 {
		return registryCheck{failure: "it does not know the registry of " + command}
	}
	var check registryCheck
	attempts := 0
	for attempts < registryCheckAttempts && ctx.Err() == nil {
		attempts++
		if check = runRegistryCheck(ctx, env, endpoints); check.done {
			return check
		}
	}
	if attempts > 1 {
		check.failure += "; tried " + strconv.Itoa(attempts) + " times"
	}
	return check
}

// runRegistryCheck runs the probe once for endpoints.
func runRegistryCheck(ctx context.Context, env verifyExecEnvironment, endpoints []registryEndpoint) registryCheck {
	probe, err := registryProbeExecutable()
	if err != nil {
		return registryCheck{failure: err.Error()}
	}
	dir, err := os.MkdirTemp("", "makewand-registry-check-*")
	if err != nil {
		return registryCheck{failure: "its working directory could not be created: " + err.Error()}
	}
	defer os.RemoveAll(dir)
	checkCtx, cancel := context.WithTimeout(ctx, registryCheckTimeout)
	defer cancel()
	args := []string{registryProbeArg}
	for _, endpoint := range endpoints {
		args = append(args, endpoint.probeArgument())
	}
	var run registryProbeRun
	if env.mode == verifyExecUnsafeHost {
		run = runRegistryProbeOnHost(checkCtx, dir, probe, args)
	} else {
		run = runRegistryProbeInSandbox(checkCtx, env.bwrapPath, dir, probe, args)
	}
	if run.failure != "" || ctx.Err() != nil {
		return registryCheck{failure: run.failure}
	}
	return parseRegistryCheck(run, endpoints, checkCtx.Err() != nil)
}

// registryProbeSelf returns makewand's own executable, which runs the probe.
// It must still be the file this process runs: one replaced on disk since
// could be another version.
func registryProbeSelf() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("makewand's own executable could not be located: %w", err)
	}
	info, err := os.Stat(exe)
	if err != nil {
		return "", fmt.Errorf("makewand's own executable is not available: %w", err)
	}
	if running, err := os.Stat("/proc/self/exe"); err == nil && !os.SameFile(info, running) {
		return "", fmt.Errorf("makewand's own executable %s was replaced after makewand started", exe)
	}
	return exe, nil
}

// runRegistryProbeInSandbox runs the probe in a fresh verification sandbox
// whose workspace is the empty directory dir, with the user-mode network a
// dependency install gets. The executable is bound into the sandbox, which
// hides HOME, /tmp and /mnt, where it may lie.
func runRegistryProbeInSandbox(ctx context.Context, bwrapPath, dir, probe string, args []string) registryProbeRun {
	wrappedCmd, wrappedArgs, err := wrapVerificationCommand(bwrapPath, dir, probe, args, false, nil)
	if err != nil {
		return registryProbeRun{failure: "its sandbox could not be prepared: " + err.Error()}
	}
	wrappedArgs = insertBeforeCommand(wrappedArgs, len(args), "--ro-bind", probe, probe)
	userNet, network := prepareUserNetwork(wrappedArgs)
	if userNet == nil {
		return registryProbeRun{failure: network.checkFailure()}
	}
	defer userNet.release()
	checkProject := &Project{Name: "registry-check", Path: dir}
	result, err := checkProject.execSandboxed(ctx, wrappedCmd, insertBeforeCommand(wrappedArgs, len(args), userNet.bwrapArgs()...), userNet)
	switch {
	case !userNet.attached && userNet.failed.cause != "":
		return registryProbeRun{failure: userNet.failed.checkFailure()}
	case result == nil || (err != nil && ctx.Err() == nil):
		failure := "its sandbox did not run"
		if err != nil {
			failure += ": " + err.Error()
		}
		return registryProbeRun{failure: failure}
	}
	return registryProbeRun{stdout: result.Stdout, stderr: result.Stderr}
}

// runRegistryProbeOnHost runs the probe on the host from the empty directory
// dir, with the environment a host install gets (proxy settings included).
func runRegistryProbeOnHost(ctx context.Context, dir, probe string, args []string) registryProbeRun {
	// #nosec G204 -- makewand's own executable; fixed probe arguments, no shell.
	cmd := exec.CommandContext(ctx, probe, args...)
	cmd.Dir = dir
	cmd.Env = sanitizeExecEnv(os.Environ())
	stdout, stderr := &boundedBuffer{max: 64 << 10}, &boundedBuffer{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = userNetReleaseTimeout
	if err := cmd.Start(); err != nil {
		return registryProbeRun{failure: "it could not be started: " + err.Error()}
	}
	_ = cmd.Wait()
	return registryProbeRun{stdout: stdout.String(), stderr: stderr.String()}
}

// registryAnswer is the probe's answer for one URL: an HTTP status, or why
// there was no response.
type registryAnswer struct {
	status int
	reason string
}

// parseRegistryCheck reads the probe's output. A URL the probe had to ask but
// gave no line for got no response in time when the check ran out of time
// (timedOut); in any other case a missing line means the probe failed.
func parseRegistryCheck(run registryProbeRun, endpoints []registryEndpoint, timedOut bool) registryCheck {
	answers := map[string]registryAnswer{}
	for _, line := range strings.Split(run.stdout, "\n") {
		verdict, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		target, detail, _ := strings.Cut(rest, " ")
		detail = strings.TrimSpace(detail)
		switch verdict {
		case "reachable":
			status, _ := strconv.Atoi(detail)
			answers[target] = registryAnswer{status: status}
		case "unreachable":
			if detail == "" {
				detail = "no response"
			}
			answers[target] = registryAnswer{reason: detail}
		}
	}
	check := registryCheck{done: true}
	for _, endpoint := range endpoints {
		// failures are the failures met on the way, each "host (reason)".
		var failures []string
		reached := ""
	chain:
		for link := &endpoint; link != nil; link = link.fallback {
			if link.afterNotFoundOnly && len(failures) > 0 {
				break // a passed-over failure leaves the service unavailable
			}
			answer, ok := answers[link.url]
			switch {
			case !ok && !timedOut:
				failure := "it gave no answer for " + link.url
				if detail := firstLine(run.stderr); detail != "" {
					failure += " (" + detail + ")"
				}
				return registryCheck{failure: failure}
			case !ok:
				answer.reason = "no response within " + registryCheckTimeout.String()
			}
			switch {
			case answer.reason == "" && (!link.decides || answer.status == http.StatusOK):
				reached = urlHost(link.url)
				break chain
			case answer.reason == "" && link.fallback != nil && (answer.status == http.StatusNotFound || answer.status == http.StatusGone):
				continue
			case answer.reason == "":
				// The go command treats any other answer as the checksum
				// database being unavailable through this proxy.
				answer.reason = "answered " + strconv.Itoa(answer.status) + " " + http.StatusText(answer.status)
			}
			failures = append(failures, urlHost(link.url)+" ("+strings.TrimSpace(answer.reason)+")")
			if !link.fallBackOnError {
				break
			}
		}
		if reached != "" {
			if !slices.Contains(check.reached, reached) {
				check.reached = append(check.reached, reached)
			}
			continue
		}
		for _, failure := range failures {
			if !slices.Contains(check.unreachable, failure) {
				check.unreachable = append(check.unreachable, failure)
			}
		}
	}
	return check
}

// defaultRegistryEndpoints returns the endpoints of the default registry that
// a dependency install of command's ecosystem needs (index and downloads), or
// nil for a command whose outages makewand does not recognize.
func defaultRegistryEndpoints(command string) []registryEndpoint {
	switch command {
	case "npm":
		return registryEndpoints("https://registry.npmjs.org/")
	case "pip", "pip3":
		return registryEndpoints("https://pypi.org/simple/", "https://files.pythonhosted.org/")
	case "go":
		return goRegistryEndpoints()
	case "cargo":
		return registryEndpoints("https://index.crates.io/config.json", "https://static.crates.io/")
	}
	return nil
}

// goRegistryEndpoints returns what the sandboxed go command uses: its module
// proxies (goModuleEndpoint), and its checksum database reached the way the
// go command reaches it (goSumDBEndpoint). Both follow the host's GOPROXY,
// GOSUMDB, GONOSUMDB and GOPRIVATE, which makewand passes into the sandbox (a
// project cannot set them); by default proxy.golang.org and sum.golang.org.
func goRegistryEndpoints() []registryEndpoint {
	var hostEnv hostGoEnv
	if goBin := lookPathIn("go", sandboxPathEnv()); goBin != "" {
		if probed, err := sandboxGoEnvProbe(goBin); err == nil {
			hostEnv = probed
		}
	}
	proxies := sandboxGoProxies(hostEnv)
	var endpoints []registryEndpoint
	if modules := goModuleEndpoint(proxies); modules != nil {
		endpoints = append(endpoints, *modules)
	}
	if sumdb := goSumDBEndpoint(hostEnv, proxies); sumdb != nil {
		endpoints = append(endpoints, *sumdb)
	}
	return endpoints
}

// sandboxGoProxies returns the GOPROXY entries of the sandboxed go command
// (sandboxGoProxy), as the go command reads them.
func sandboxGoProxies(hostEnv hostGoEnv) []goProxySpec {
	value, _ := sandboxGoProxy(hostEnv)
	return parseGoProxyList(value)
}

// isNetworkGoProxy reports whether a GOPROXY entry is a proxy reached over
// the network (not a local directory, "direct" or "off").
func isNetworkGoProxy(proxy string) bool {
	return strings.HasPrefix(proxy, "https://") || strings.HasPrefix(proxy, "http://")
}

// goModuleEndpoint returns the module proxies the go command downloads
// modules from, as the chain it walks, or nil when it uses none (a "direct"
// or "off" entry comes first). A local proxy directory hands every module it
// lacks to the next entry, so it is passed over. A proxy that answers is
// reachable (the go command never asks a proxy's root URL, so any response
// there counts); one that does not stops the go command, unless its entry is
// followed by '|': the go command then moves on to the next entry, as it does
// after any error.
func goModuleEndpoint(proxies []goProxySpec) *registryEndpoint {
	var links []registryEndpoint
	for _, proxy := range proxies {
		if proxy.url == "direct" || proxy.url == "off" {
			break
		}
		if !isNetworkGoProxy(proxy.url) {
			continue
		}
		links = append(links, registryEndpoint{url: proxy.url, fallBackOnError: proxy.fallBackOnError})
		if !proxy.fallBackOnError {
			break // the go command never gets past it without an answer
		}
	}
	return registryChain(links)
}

// goSumDBEndpoint returns how the go command reaches its checksum database,
// or nil when it verifies no module with one (GOSUMDB=off, GONOSUMDB or
// GOPRIVATE covering every module, or a local proxy directory that serves
// it). A URL in GOSUMDB is used directly. Otherwise the go command asks the
// module proxies in turn whether they serve the database
// (<proxy>/sumdb/<name>/supported): it uses the first that answers 200 and
// moves on after 404 or 410; any other answer, or none, leaves the database
// unavailable, except from a proxy whose entry is followed by '|', which the
// go command passes over after any error. It connects to the database itself
// when a "direct" or "off" entry comes, and when no proxy is left - then only
// if every proxy answered 404 or 410. A local proxy directory without that
// file is passed over.
func goSumDBEndpoint(hostEnv hostGoEnv, proxies []goProxySpec) *registryEndpoint {
	name, explicit, ok := goSumDB(hostEnv.GOSUMDB)
	if !ok || goPatternsMatchAll(goNoSumDBPatterns(hostEnv)) {
		return nil
	}
	if explicit != "" {
		return &registryEndpoint{url: explicit}
	}
	var links []registryEndpoint
	listed := false // a "direct" or "off" entry follows the proxies
	for _, proxy := range proxies {
		if proxy.url == "direct" || proxy.url == "off" {
			listed = true
			break
		}
		if dir, ok := strings.CutPrefix(proxy.url, "file://"); ok {
			if _, err := os.Stat(filepath.Join(filepath.FromSlash(dir), "sumdb", name, "supported")); err == nil {
				return nil
			}
			continue
		}
		if isNetworkGoProxy(proxy.url) {
			links = append(links, registryEndpoint{
				url:             strings.TrimSuffix(proxy.url, "/") + "/sumdb/" + name + "/supported",
				decides:         true,
				fallBackOnError: proxy.fallBackOnError,
			})
		}
	}
	links = append(links, registryEndpoint{url: "https://" + name + "/", afterNotFoundOnly: !listed && len(links) > 0})
	return registryChain(links)
}

// goSumDB parses GOSUMDB like the go command: the checksum database name, the
// URL the value names explicitly (which bypasses the module proxies), and
// false when it is off. Like the sandbox it ignores a value that carries
// credentials, so the go command falls back to sum.golang.org.
func goSumDB(value string) (name, explicit string, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "@") {
		value = "sum.golang.org"
	}
	if value == "sum.golang.google.cn" {
		// The go command's alias for sum.golang.org, reachable in mainland China.
		value = "sum.golang.org https://sum.golang.google.cn"
	}
	fields := strings.Fields(value)
	if fields[0] == "off" {
		return "", "", false
	}
	name, _, _ = strings.Cut(fields[0], "+")
	if len(fields) > 1 {
		explicit = fields[1]
	}
	return name, explicit, true
}

// goNoSumDBPatterns returns the module path patterns the go command verifies
// without the checksum database: GONOSUMDB, which defaults to GOPRIVATE. Like
// the sandbox it ignores values that carry credentials.
func goNoSumDBPatterns(hostEnv hostGoEnv) string {
	for _, value := range []string{hostEnv.GONOSUMDB, hostEnv.GOPRIVATE} {
		if value = strings.TrimSpace(value); value != "" && !strings.Contains(value, "@") {
			return value
		}
	}
	return ""
}

// goPatternsMatchAll reports whether a GONOSUMDB-style pattern list matches
// every module path.
func goPatternsMatchAll(patterns string) bool {
	for _, pattern := range strings.Split(patterns, ",") {
		if strings.TrimSpace(pattern) == "*" {
			return true
		}
	}
	return false
}

// insertBeforeCommand inserts bwrap options in front of the wrapped command,
// whose argv is the last argc+1 entries of args.
func insertBeforeCommand(args []string, argc int, extra ...string) []string {
	cut := len(args) - argc - 1
	out := make([]string, 0, len(args)+len(extra))
	out = append(out, args[:cut]...)
	out = append(out, extra...)
	return append(out, args[cut:]...)
}

// userNetLaunch attaches slirp4netns to the network namespace of one bwrap
// launch. bwrap reports the sandbox child PID on --info-fd and holds the
// command on --block-fd until the network is attached and restricted.
// --ro-bind-data points the sandbox resolver at slirp4netns' DNS forwarder;
// bwrap reads that data while its child is still in the first, root-mapped
// user namespace that owns the network namespace (it moves into a nested one
// later), so the data is withheld until that namespace has been opened.
type userNetLaunch struct {
	helper       string
	nsenter      string
	ip           string
	tc           string         // optional multicast filter
	nft          string         // optional egress filter
	blocked      []netip.Prefix // userNetBlockedIPv4 plus the host's own addresses
	fdBase       int            // child fd of the first descriptor (after the seccomp fd)
	resolvTarget string

	infoR, infoW     *os.File
	blockR, blockW   *os.File
	resolvR, resolvW *os.File

	sandbox *exec.Cmd   // the bwrap launch (attachFiles)
	child   *os.Process // the held sandbox child, once reported

	helperCmd *exec.Cmd
	helperOut *boundedBuffer
	exitW     *os.File

	attached bool
	failure  string
	// failed is the stage and cause of the failure, for guidance.
	failed   userNetFailure
	released bool
}

// prepareUserNetwork resolves the helpers and creates the launch descriptors.
// A nil launch means the dependency step runs loopback-only; the returned
// sandboxNetwork explains why.
func prepareUserNetwork(wrappedArgs []string) (*userNetLaunch, sandboxNetwork) {
	helper, err := userNetLookPath(userNetHelperName)
	if err != nil {
		detail := fmt.Sprintf("%s was not found in a trusted system directory (%s)", userNetHelperName, strings.Join(trustedbin.SystemDirs, ", "))
		if !errors.Is(err, trustedbin.ErrNotFound) {
			detail = fmt.Sprintf("%s was rejected (%v)", userNetHelperName, err)
		}
		return nil, sandboxNetwork{kind: sandboxNetworkNone, detail: detail}
	}
	tools := map[string]string{}
	for _, name := range []string{"nsenter", "ip"} {
		path, err := userNetLookPath(name)
		if err != nil {
			detail := fmt.Sprintf("%s, %s, was not found in a trusted system directory (%s)", name, userNetToolRoles[name], strings.Join(trustedbin.SystemDirs, ", "))
			if !errors.Is(err, trustedbin.ErrNotFound) {
				detail = fmt.Sprintf("%s, %s, was rejected (%v)", name, userNetToolRoles[name], err)
			}
			remedy := fmt.Sprintf("install %s (for %s) from your distribution or set %s to a trusted copy", userNetToolPackages[name], name, trustedbin.OverrideEnv(name))
			return nil, sandboxNetwork{kind: sandboxNetworkNone, detail: detail, remedy: remedy}
		}
		tools[name] = path
	}
	resolvTarget := sandboxResolvConfTarget(userNetResolvConf)
	if resolvTarget == "" {
		return nil, sandboxNetwork{
			kind:   sandboxNetworkNone,
			detail: fmt.Sprintf("the sandbox resolver configuration (%s) cannot be pointed at %s's DNS forwarder", userNetResolvConf, userNetHelperName),
			remedy: fmt.Sprintf("make %s a regular file or a link to a file under /run", userNetResolvConf),
		}
	}
	fdBase := 3
	if sandboxSeccompRequested(wrappedArgs) {
		fdBase = sandboxSeccompFD + 1
	}
	launch, err := newUserNetLaunch(helper, fdBase, resolvTarget)
	if err != nil {
		return nil, sandboxNetwork{kind: sandboxNetworkNone, detail: userNetHelperName + " setup failed: " + err.Error()}
	}
	launch.nsenter, launch.ip = tools["nsenter"], tools["ip"]
	if tc, err := userNetLookPath("tc"); err == nil {
		launch.tc = tc
	}
	if nft, err := userNetLookPath("nft"); err == nil {
		launch.nft = nft
	}
	hostAddrs, _ := userNetHostAddrs()
	launch.blocked = userNetBlockedPrefixes(hostAddrs)
	return launch, sandboxNetwork{kind: sandboxNetworkUserMode}
}

func newUserNetLaunch(helper string, fdBase int, resolvTarget string) (*userNetLaunch, error) {
	n := &userNetLaunch{helper: helper, fdBase: fdBase, resolvTarget: resolvTarget}
	var err error
	if n.infoR, n.infoW, err = os.Pipe(); err != nil {
		return nil, err
	}
	if n.blockR, n.blockW, err = os.Pipe(); err != nil {
		n.release()
		return nil, err
	}
	if n.resolvR, n.resolvW, err = os.Pipe(); err != nil {
		n.release()
		return nil, err
	}
	return n, nil
}

// userNetBlockedPrefixes returns userNetBlockedIPv4 plus a /32 for each IPv4
// address of the host that those ranges do not cover (a public or otherwise
// unusual address still reaches the host's own services).
func userNetBlockedPrefixes(hostAddrs []net.Addr) []netip.Prefix {
	blocked := make([]netip.Prefix, 0, len(userNetBlockedIPv4)+len(hostAddrs))
	for _, cidr := range userNetBlockedIPv4 {
		blocked = append(blocked, netip.MustParsePrefix(cidr))
	}
	for _, addr := range hostAddrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipNet.IP.To4())
		if !ok {
			continue
		}
		covered := false
		for _, prefix := range blocked {
			if prefix.Contains(ip) {
				covered = true
				break
			}
		}
		if !covered {
			blocked = append(blocked, netip.PrefixFrom(ip, 32))
		}
	}
	return blocked
}

// userNetRouteScript is the `ip -batch` input that keeps the sandbox off the
// blocked destinations. slirp4netns' own 10.0.2.0/24 link route is more
// specific than 10.0.0.0/8; within it only the DNS forwarder is needed (the
// default route already holds the gateway as its next hop).
func userNetRouteScript(blocked []netip.Prefix) string {
	var b strings.Builder
	for _, prefix := range blocked {
		fmt.Fprintf(&b, "route add unreachable %s\n", prefix)
	}
	b.WriteString("route add unreachable 10.0.2.0/25\n")
	b.WriteString("route add unreachable 10.0.2.128/25\n")
	fmt.Fprintf(&b, "route add %s/32 dev %s\n", userNetDNS, userNetDevice)
	return b.String()
}

// userNetFilterRuleset is the nft ruleset that drops the blocked destinations
// and all IPv6 whatever interface a socket names. Sandbox-local loopback
// traffic and DNS to the forwarder pass.
func userNetFilterRuleset(blocked []netip.Prefix) string {
	elements := make([]string, len(blocked))
	for i, prefix := range blocked {
		elements[i] = prefix.String()
	}
	return fmt.Sprintf(`table inet makewand_sandbox {
	chain egress {
		type filter hook output priority 0; policy accept;
		oifname "lo" accept
		meta nfproto ipv6 drop
		ip daddr %[1]s udp dport 53 accept
		ip daddr %[1]s tcp dport 53 accept
		ip daddr { %[2]s } drop
	}
}
`, userNetDNS, strings.Join(elements, ", "))
}

// bwrapArgs are the bwrap options for this launch.
func (n *userNetLaunch) bwrapArgs() []string {
	return []string{
		"--info-fd", strconv.Itoa(n.fdBase),
		"--block-fd", strconv.Itoa(n.fdBase + 1),
		"--ro-bind-data", strconv.Itoa(n.fdBase + 2), n.resolvTarget,
	}
}

// attachFiles hands the child ends to cmd at the descriptors bwrapArgs named.
func (n *userNetLaunch) attachFiles(cmd *exec.Cmd) error {
	if 3+len(cmd.ExtraFiles) != n.fdBase {
		return fmt.Errorf("user-mode network descriptors must start at child fd %d", n.fdBase)
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, n.infoW, n.blockR, n.resolvR)
	n.sandbox = cmd
	return nil
}

// closeChildEnds closes the parent's copies of the descriptors the child
// inherited. It runs after Start on every path.
func (n *userNetLaunch) closeChildEnds() {
	for _, f := range []**os.File{&n.infoW, &n.blockR, &n.resolvR} {
		if *f != nil {
			_ = (*f).Close()
			*f = nil
		}
	}
}

// connect attaches the helper to the started sandbox, restricts the sandbox
// network and then releases the held command. When any step fails the
// sandbox is killed while it is still held, so the command never runs with an
// unrestricted network; n.failure and n.failed record why and attached stays
// false.
func (n *userNetLaunch) connect(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, userNetAttachTimeout)
	defer cancel()
	if err := n.setUp(ctx); err != nil {
		n.failure = err.Error()
		n.failed = userNetFailure{stage: userNetStageSandbox, cause: err.Error()}
		var stageErr *userNetStageError
		if errors.As(err, &stageErr) {
			n.failed = userNetFailure{stage: stageErr.stage, cause: stageErr.err.Error()}
		}
		n.stopSandbox()
		n.stopHelper()
		return
	}
	n.attached = true
	n.unblock()
}

// network describes the network a step falls back to after this launch could
// not attach: loopback-only, with guidance naming the stage that failed.
func (n *userNetLaunch) network() sandboxNetwork {
	failed := n.failed
	return sandboxNetwork{kind: sandboxNetworkNone, attach: &failed}
}

func (n *userNetLaunch) setUp(ctx context.Context) error {
	sandboxErr := func(err error) error { return &userNetStageError{stage: userNetStageSandbox, err: err} }
	pid, err := readSandboxChildPID(ctx, n.infoR)
	if err != nil {
		return sandboxErr(err)
	}
	// The child is held by descriptors makewand owns, so the PID cannot have
	// been reused yet.
	if n.child, err = os.FindProcess(pid); err != nil {
		return sandboxErr(fmt.Errorf("sandbox child %d: %w", pid, err))
	}
	sandboxPID := 0
	if n.sandbox != nil && n.sandbox.Process != nil {
		sandboxPID = n.sandbox.Process.Pid
	}
	ns, err := userNetOpenNamespaces(ctx, pid, sandboxPID)
	if err != nil {
		return sandboxErr(err)
	}
	defer ns.close()
	if err := n.releaseResolver(); err != nil {
		return sandboxErr(err)
	}
	if err := n.startHelper(ctx, ns); err != nil {
		return &userNetStageError{stage: userNetStageHelper, err: err}
	}
	if err := n.runInSandboxNetwork(ctx, ns, userNetRouteScript(n.blocked), n.ip, "-4", "-batch", "-"); err != nil {
		return &userNetStageError{stage: userNetStageRoutes, err: err}
	}
	// Best effort: the routes above already cover every socket that lets the
	// kernel choose the interface (see the file comment).
	if n.tc != "" {
		_ = n.runInSandboxNetwork(ctx, ns, userNetMulticastFilter, n.tc, "-batch", "-")
	}
	if n.nft != "" {
		_ = n.runInSandboxNetwork(ctx, ns, userNetFilterRuleset(n.blocked), n.nft, "-f", "-")
	}
	return nil
}

// readSandboxChildPID reads the PID bwrap reports on --info-fd. It fails when
// bwrap exits before reporting (a setup failure, surfaced by its exit status).
func readSandboxChildPID(ctx context.Context, r *os.File) (int, error) {
	type info struct {
		pid int
		err error
	}
	done := make(chan info, 1)
	go func() {
		var report struct {
			ChildPID int `json:"child-pid"`
		}
		err := json.NewDecoder(io.LimitReader(r, 64<<10)).Decode(&report)
		done <- info{pid: report.ChildPID, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			return 0, fmt.Errorf("sandbox did not report its namespace: %w", got.err)
		}
		if got.pid <= 0 {
			return 0, errors.New("sandbox reported no child pid")
		}
		return got.pid, nil
	case <-ctx.Done():
		_ = r.Close() // unblocks the decoder
		<-done
		return 0, fmt.Errorf("sandbox did not report its namespace: %w", ctx.Err())
	}
}

// sandboxNamespaces are the network namespace of a held sandbox and the user
// namespace owning it, or a nil user when that is makewand's own (root runs
// bwrap without a user namespace).
type sandboxNamespaces struct {
	user, net *os.File
}

func (ns sandboxNamespaces) close() {
	for _, f := range []*os.File{ns.user, ns.net} {
		if f != nil {
			_ = f.Close()
		}
	}
}

// enter returns the nsenter argv prefix that joins the sandbox network
// namespace, and the descriptors it names (child fds 3 and up). Joining the
// owning user namespace first gives the configuring tool its capabilities:
// root there is the invoking user (--preserve-credentials).
func (ns sandboxNamespaces) enter(nsenter string) ([]string, []*os.File) {
	argv := []string{nsenter}
	var files []*os.File
	if ns.user != nil {
		files = append(files, ns.user)
		argv = append(argv, "--user=/proc/self/fd/"+strconv.Itoa(2+len(files)), "--preserve-credentials")
	}
	files = append(files, ns.net)
	argv = append(argv, "--net=/proc/self/fd/"+strconv.Itoa(2+len(files)), "--")
	return argv, files
}

// openSandboxNamespaces opens the namespaces of the held sandbox child pid and
// checks that they are the sandbox's: pid is still bwrap's child, the network
// namespace is not makewand's, and the user namespace maps root to the
// invoking user (bwrap's first user namespace does, because of --dev).
func openSandboxNamespaces(ctx context.Context, pid, sandboxPID int) (sandboxNamespaces, error) {
	dir := filepath.Join(userNetProcDir, strconv.Itoa(pid))
	self := filepath.Join(userNetProcDir, "self")
	var ns sandboxNamespaces
	var err error
	if ns.user, err = os.Open(filepath.Join(dir, "ns", "user")); err != nil {
		return sandboxNamespaces{}, fmt.Errorf("open the sandbox user namespace: %w", err)
	}
	if ns.net, err = os.Open(filepath.Join(dir, "ns", "net")); err != nil {
		ns.close()
		return sandboxNamespaces{}, fmt.Errorf("open the sandbox network namespace: %w", err)
	}
	fail := func(format string, args ...any) (sandboxNamespaces, error) {
		ns.close()
		return sandboxNamespaces{}, fmt.Errorf(format, args...)
	}
	// Checked after opening: the namespaces belong to pid as long as it is
	// still the child bwrap started.
	if ppid, err := procParentPID(filepath.Join(dir, "stat")); err != nil || ppid != sandboxPID {
		return fail("sandbox child %d is no longer held by bwrap %d (parent %d, %v)", pid, sandboxPID, ppid, err)
	}
	if same, err := sameFileAs(ns.net, filepath.Join(self, "ns", "net")); err != nil || same {
		return fail("the sandbox does not have a network namespace of its own (%v)", err)
	}
	same, err := sameFileAs(ns.user, filepath.Join(self, "ns", "user"))
	if err != nil {
		return fail("compare the sandbox user namespace: %w", err)
	}
	if same {
		if os.Geteuid() != 0 {
			return fail("the sandbox network namespace is not in a user namespace of its own")
		}
		_ = ns.user.Close()
		ns.user = nil
		return ns, nil
	}
	if err := waitForRootMapping(ctx, filepath.Join(dir, "uid_map"), os.Geteuid()); err != nil {
		return fail("%w", err)
	}
	return ns, nil
}

// waitForRootMapping waits until the sandbox child has written the uid_map of
// its first user namespace (it does so after reporting its PID and before
// reading the resolver data) and checks that the map makes uid root there.
func waitForRootMapping(ctx context.Context, path string, uid int) error {
	for {
		data, err := os.ReadFile(path) //nolint:gosec // G304: a /proc path built from a PID
		if err != nil {
			return fmt.Errorf("read the sandbox uid map: %w", err)
		}
		if uidMap := strings.TrimSpace(string(data)); uidMap != "" {
			if !uidMapMapsRoot(uidMap, uid) {
				return fmt.Errorf("the sandbox user namespace does not map root to uid %d (uid_map %q)", uid, uidMap)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the sandbox user namespace was not set up: %w", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

// procParentPID returns the parent PID from a /proc/<pid>/stat file.
func procParentPID(statPath string) (int, error) {
	data, err := os.ReadFile(statPath) //nolint:gosec // G304: a /proc path built from a PID
	if err != nil {
		return 0, err
	}
	stat := string(data)
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, fmt.Errorf("malformed %s", statPath)
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed %s", statPath)
	}
	return strconv.Atoi(fields[1])
}

// uidMapMapsRoot reports whether uid_map content maps uid 0 to uid.
func uidMapMapsRoot(uidMap string, uid int) bool {
	for _, line := range strings.Split(uidMap, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "0" && fields[1] == strconv.Itoa(uid) && fields[2] != "0" {
			return true
		}
	}
	return false
}

func sameFileAs(f *os.File, path string) (bool, error) {
	a, err := f.Stat()
	if err != nil {
		return false, err
	}
	b, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return os.SameFile(a, b), nil
}

// releaseResolver hands bwrap the resolver data, letting the sandbox setup
// continue to --block-fd. A few bytes always fit in the pipe buffer, so this
// cannot block.
func (n *userNetLaunch) releaseResolver() error {
	_, writeErr := io.WriteString(n.resolvW, "nameserver "+userNetDNS+"\n")
	closeErr := n.resolvW.Close()
	n.resolvW = nil
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("hand the resolver configuration to the sandbox: %w", err)
	}
	return nil
}

// startHelper runs slirp4netns against the sandbox network namespace and
// waits until it reports the interface configured.
func (n *userNetLaunch) startHelper(ctx context.Context, ns sandboxNamespaces) error {
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer readyR.Close()
	exitR, exitW, err := os.Pipe()
	if err != nil {
		_ = readyW.Close()
		return err
	}
	cmd := n.helperCommand(ns, readyW, exitR)
	cmd.Env = []string{}
	n.helperOut = &boundedBuffer{max: 4 << 10}
	cmd.Stdout = n.helperOut
	cmd.Stderr = n.helperOut
	setProcessGroup(cmd)
	startErr := cmd.Start()
	_ = readyW.Close()
	_ = exitR.Close()
	if startErr != nil {
		_ = exitW.Close()
		return fmt.Errorf("start %s: %w", userNetHelperName, startErr)
	}
	n.helperCmd, n.exitW = cmd, exitW

	ready := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := io.ReadFull(readyR, buf)
		if err == nil && buf[0] != '1' {
			err = fmt.Errorf("unexpected readiness byte %q", buf[0])
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			n.stopHelper()
			return fmt.Errorf("%s did not configure the sandbox network: %v%s", userNetHelperName, err, n.helperDetail())
		}
		return nil
	case <-ctx.Done():
		_ = readyR.Close()
		<-ready
		n.stopHelper()
		return fmt.Errorf("%s did not configure the sandbox network: %w%s", userNetHelperName, ctx.Err(), n.helperDetail())
	}
}

// helperCommand is the slirp4netns launch. The network namespace is passed as
// a descriptor (--netns-type=path), never as a PID; slirp4netns runs in the
// user namespace owning it, entered through nsenter, because joining the
// sandbox child's own (nested) user namespace would not grant it the
// capabilities over that network namespace.
func (n *userNetLaunch) helperCommand(ns sandboxNamespaces, readyW, exitR *os.File) *exec.Cmd {
	var argv []string
	var files []*os.File
	fd := func(f *os.File) string {
		files = append(files, f)
		return strconv.Itoa(2 + len(files))
	}
	if ns.user != nil {
		argv = append(argv, n.nsenter, "--user=/proc/self/fd/"+fd(ns.user), "--preserve-credentials", "--")
	}
	netFD := fd(ns.net)
	argv = append(argv, n.helper,
		"--configure",
		"--mtu=65520",
		"--disable-host-loopback",
		"--enable-sandbox",
		"--enable-seccomp",
		"--ready-fd="+fd(readyW),
		"--exit-fd="+fd(exitR),
		"--netns-type=path",
		"/proc/self/fd/"+netFD,
		userNetDevice,
	)
	// #nosec G204 -- trusted absolute paths; fixed argv, no shell.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.ExtraFiles = files
	return cmd
}

// runInSandboxNetwork runs a trusted configuration tool inside the sandbox
// network namespace with input on stdin.
func (n *userNetLaunch) runInSandboxNetwork(ctx context.Context, ns sandboxNamespaces, input, tool string, args ...string) error {
	argv, files := ns.enter(n.nsenter)
	argv = append(append(argv, tool), args...)
	// #nosec G204 -- trusted absolute paths; fixed argv, no shell.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{}
	cmd.ExtraFiles = files
	cmd.Stdin = strings.NewReader(input)
	out := &boundedBuffer{max: 4 << 10}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = userNetReleaseTimeout
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(out.String()); detail != "" {
			return fmt.Errorf("%s: %v: %s", filepath.Base(tool), err, firstLine(detail))
		}
		return fmt.Errorf("%s: %w", filepath.Base(tool), err)
	}
	return nil
}

func (n *userNetLaunch) helperDetail() string {
	if n.helperOut == nil {
		return ""
	}
	if out := strings.TrimSpace(n.helperOut.String()); out != "" {
		return ": " + firstLine(out)
	}
	return ""
}

// unblock lets the held sandbox command run. Only a completed setup calls it.
func (n *userNetLaunch) unblock() {
	if n.blockW != nil {
		_, _ = n.blockW.Write([]byte{1})
		_ = n.blockW.Close()
		n.blockW = nil
	}
}

// stopSandbox kills the held sandbox child and bwrap itself. Closing the
// resolver or block descriptor alone would let a held sandbox continue: bwrap
// treats end-of-file like data.
func (n *userNetLaunch) stopSandbox() {
	if n.child != nil {
		_ = n.child.Kill()
	}
	if n.sandbox != nil && n.sandbox.Process != nil {
		_ = n.sandbox.Process.Kill()
	}
}

// stopHelper closes the helper's exit descriptor and reaps it, killing it
// when it does not exit promptly.
func (n *userNetLaunch) stopHelper() {
	if n.exitW != nil {
		_ = n.exitW.Close()
		n.exitW = nil
	}
	if n.helperCmd == nil {
		return
	}
	cmd := n.helperCmd
	n.helperCmd = nil
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(userNetReleaseTimeout):
		killProcessGroup(cmd)
		<-done
	}
}

// release frees every resource of the launch. It is idempotent. A launch that
// never completed its setup kills the sandbox first, so closing the
// descriptors that hold it cannot let its command run.
func (n *userNetLaunch) release() {
	if n == nil || n.released {
		return
	}
	n.released = true
	if !n.attached {
		n.stopSandbox()
	}
	for _, f := range []**os.File{&n.resolvW, &n.blockW, &n.infoR} {
		if *f != nil {
			_ = (*f).Close()
			*f = nil
		}
	}
	n.closeChildEnds()
	n.stopHelper()
	if n.child != nil {
		_ = n.child.Release()
	}
}

// sandboxResolvConfTarget returns where the sandbox resolver configuration
// must be bound: /etc/resolv.conf, or its symlink target when it is a link
// (systemd-resolved links it into /run, which the sandbox replaces with a
// tmpfs). It returns "" when bwrap could not create or cover that path.
func sandboxResolvConfTarget(path string) string {
	chain := symlinkChain(path)
	target := chain[len(chain)-1]
	if !filepath.IsAbs(target) {
		return ""
	}
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
		return target
	}
	for _, tmpfsRoot := range []string{"/run", "/tmp", "/var/tmp"} {
		if pathWithin(tmpfsRoot, target) && target != tmpfsRoot {
			return target
		}
	}
	return ""
}

// boundedBuffer keeps the first max bytes written to it.
type boundedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
