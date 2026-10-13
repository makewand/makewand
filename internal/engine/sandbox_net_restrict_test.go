//go:build linux

package engine

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runFakeUserNetLaunch runs the fake bwrap with a user-network launch the way
// execVerification does, and returns the launch once the run is over.
func runFakeUserNetLaunch(t *testing.T, env fakeUserNetEnv) (*userNetLaunch, *ExecResult, time.Duration) {
	t.Helper()
	project, err := NewProject("usernet-launch", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bwrap, err := verifyLookPath("bwrap")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--unshare-net", "npm", "install"}
	launch, network := prepareUserNetwork(args)
	if launch == nil {
		t.Fatalf("no user-network launch: %+v", network)
	}
	start := time.Now()
	result, err := project.execWithPolicyLaunch(context.Background(), bwrap, insertBeforeCommand(args, 1, launch.bwrapArgs()...), isolatedExecPolicy(), launch)
	launch.release()
	if err != nil {
		t.Fatalf("execWithPolicyLaunch: %v", err)
	}
	return launch, result, time.Since(start)
}

// assertHeldCommandNeverRan fails when the user-network launch released or
// ran its command.
func assertHeldCommandNeverRan(t *testing.T, env fakeUserNetEnv) []string {
	t.Helper()
	events := eventLines(t, env.log)
	pid := userNetLaunchPID(events)
	if pid == "" {
		t.Fatalf("no user-network launch in:\n%s", strings.Join(events, "\n"))
	}
	if eventLogged(events, "bwrap["+pid+"]: released") || eventLogged(events, "bwrap["+pid+"]: ran") {
		t.Fatalf("the held command ran although the network could not be set up:\n%s", strings.Join(events, "\n"))
	}
	return events
}

func TestUserNetBlockedPrefixesCoverNonPublicDestinations_DepsNet(t *testing.T) {
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("192.168.10.12"), Mask: net.CIDRMask(24, 32)}, // covered by 192.168.0.0/16
		&net.IPNet{IP: net.ParseIP("10.0.0.12"), Mask: net.CIDRMask(24, 32)},     // covered by 10.0.0.0/8
		&net.IPNet{IP: net.ParseIP("203.0.114.7"), Mask: net.CIDRMask(24, 32)},   // a public host address
		&net.IPNet{IP: net.ParseIP("198.18.0.1"), Mask: net.CIDRMask(16, 32)},    // a proxy's TUN address
		&net.IPNet{IP: net.ParseIP("203.0.114.7"), Mask: net.CIDRMask(32, 32)},   // listed twice
		&net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(64, 128)},  // IPv6 stays disabled
		&net.IPAddr{IP: net.ParseIP("8.8.8.8")},                                  // not an interface prefix
	}
	blocked := userNetBlockedPrefixes(addrs)
	for _, want := range []string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10",
		"127.0.0.0/8", "0.0.0.0/8", "224.0.0.0/4", "240.0.0.0/4",
		"203.0.114.7/32", "198.18.0.1/32",
	} {
		if !slices.Contains(blocked, netip.MustParsePrefix(want)) {
			t.Errorf("blocked prefixes lack %s: %v", want, blocked)
		}
	}
	if got, want := len(blocked), len(userNetBlockedIPv4)+2; got != want {
		t.Errorf("got %d prefixes, want %d (covered, duplicate and IPv6 addresses add none): %v", got, want, blocked)
	}
	for _, prefix := range blocked {
		if prefix.Contains(netip.MustParseAddr("198.18.5.5")) && prefix.Bits() < 32 {
			t.Errorf("%s blocks the fake-IP range proxies answer DNS from", prefix)
		}
	}
}

func TestUserNetRouteScript_DepsNet(t *testing.T) {
	blocked := userNetBlockedPrefixes([]net.Addr{&net.IPNet{IP: net.ParseIP("203.0.114.7"), Mask: net.CIDRMask(24, 32)}})
	lines := strings.Split(strings.TrimSpace(userNetRouteScript(blocked)), "\n")
	for _, prefix := range blocked {
		if !slices.Contains(lines, "route add unreachable "+prefix.String()) {
			t.Errorf("no unreachable route for %s:\n%s", prefix, strings.Join(lines, "\n"))
		}
	}
	// Inside slirp4netns' own network only the DNS forwarder stays reachable.
	for _, want := range []string{"route add unreachable 10.0.2.0/25", "route add unreachable 10.0.2.128/25", "route add 10.0.2.3/32 dev tap0"} {
		if !slices.Contains(lines, want) {
			t.Errorf("route script lacks %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
	if got, want := len(lines), len(blocked)+3; got != want {
		t.Errorf("route script has %d lines, want %d", got, want)
	}
}

func TestUserNetFilterRuleset_DepsNet(t *testing.T) {
	blocked := userNetBlockedPrefixes(nil)
	ruleset := userNetFilterRuleset(blocked)
	elements := make([]string, len(blocked))
	for i, prefix := range blocked {
		elements[i] = prefix.String()
	}
	for _, want := range []string{
		"type filter hook output priority 0; policy accept;",
		`oifname "lo" accept`,
		"meta nfproto ipv6 drop",
		"ip daddr 10.0.2.3 udp dport 53 accept",
		"ip daddr 10.0.2.3 tcp dport 53 accept",
		"ip daddr { " + strings.Join(elements, ", ") + " } drop",
	} {
		if !strings.Contains(ruleset, want) {
			t.Errorf("ruleset lacks %q:\n%s", want, ruleset)
		}
	}
	// Loopback and DNS are accepted before the drop rules apply.
	if strings.Index(ruleset, `oifname "lo" accept`) > strings.Index(ruleset, "meta nfproto ipv6 drop") ||
		strings.Index(ruleset, "tcp dport 53 accept") > strings.Index(ruleset, " } drop") {
		t.Errorf("accept rules must precede the drop rules:\n%s", ruleset)
	}
}

// slirp4netns is handed the namespace as a descriptor, runs in the user
// namespace owning it, keeps host loopback off its gateway and never enables
// IPv6.
func TestUserNetHelperCommand_DepsNet(t *testing.T) {
	dir := t.TempDir()
	open := func(name string) *os.File {
		writeFile(t, filepath.Join(dir, name), name)
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	launch := &userNetLaunch{helper: "/trusted/slirp4netns", nsenter: "/trusted/nsenter"}
	ready, exit := open("ready"), open("exit")
	for _, tc := range []struct {
		name string
		ns   sandboxNamespaces
		want []string
	}{
		{"own user namespace", sandboxNamespaces{user: open("user"), net: open("net")}, []string{
			"/trusted/nsenter", "--user=/proc/self/fd/3", "--preserve-credentials", "--", "/trusted/slirp4netns",
			"--configure", "--mtu=65520", "--disable-host-loopback", "--enable-sandbox", "--enable-seccomp",
			"--ready-fd=5", "--exit-fd=6", "--netns-type=path", "/proc/self/fd/4", "tap0",
		}},
		{"shared user namespace (root)", sandboxNamespaces{net: open("net-root")}, []string{
			"/trusted/slirp4netns",
			"--configure", "--mtu=65520", "--disable-host-loopback", "--enable-sandbox", "--enable-seccomp",
			"--ready-fd=4", "--exit-fd=5", "--netns-type=path", "/proc/self/fd/3", "tap0",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := launch.helperCommand(tc.ns, ready, exit)
			if !slices.Equal(cmd.Args, tc.want) {
				t.Fatalf("argv:\n%q\nwant:\n%q", cmd.Args, tc.want)
			}
			if slices.Contains(cmd.Args, "--enable-ipv6") || slices.Contains(cmd.Args, "-6") {
				t.Fatalf("IPv6 enabled: %q", cmd.Args)
			}
			wantFiles := []*os.File{tc.ns.net, ready, exit}
			if tc.ns.user != nil {
				wantFiles = append([]*os.File{tc.ns.user}, wantFiles...)
			}
			if !slices.Equal(cmd.ExtraFiles, wantFiles) {
				t.Fatalf("descriptors %v, want %v", cmd.ExtraFiles, wantFiles)
			}
		})
	}
	args, files := sandboxNamespaces{user: open("u2"), net: open("n2")}.enter("/trusted/nsenter")
	if want := []string{"/trusted/nsenter", "--user=/proc/self/fd/3", "--preserve-credentials", "--net=/proc/self/fd/4", "--"}; !slices.Equal(args, want) || len(files) != 2 {
		t.Fatalf("enter = %q (%d files), want %q", args, len(files), want)
	}
	args, files = sandboxNamespaces{net: open("n3")}.enter("/trusted/nsenter")
	if want := []string{"/trusted/nsenter", "--net=/proc/self/fd/3", "--"}; !slices.Equal(args, want) || len(files) != 1 {
		t.Fatalf("enter (root) = %q (%d files), want %q", args, len(files), want)
	}
}

// When the routes cannot be installed the held sandbox is killed: its command
// never runs with an unrestricted network, the helper is stopped, and the
// launch reports the failure promptly.
func TestUserNetRouteFailureKillsHeldSandbox_DepsNet(t *testing.T) {
	env := installFakeUserNet(t, fakeUserNetOptions{ipOutput: "RTNETLINK answers: Operation not permitted", ipExit: 2})
	launch, _, elapsed := runFakeUserNetLaunch(t, env)
	if launch.attached || !strings.Contains(launch.failure, "private and host addresses") || !strings.Contains(launch.failure, "RTNETLINK answers: Operation not permitted") {
		t.Fatalf("attached=%v failure=%q, want the route failure", launch.attached, launch.failure)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("route failure took %s", elapsed)
	}
	events := assertHeldCommandNeverRan(t, env)
	assertEventOrder(t, events, "slirp: ready-sent", "ip: -4 -batch -", "slirp: exit-fd closed")
	if strings.Contains(strings.Join(events, "\n"), "nft: ") {
		t.Fatalf("the filter was installed after the routes failed:\n%s", strings.Join(events, "\n"))
	}
}

// The held sandbox is killed when its namespaces cannot be confirmed as its
// own, before slirp4netns or any configuration tool runs.
func TestUserNetNamespaceChecksFailClosed_DepsNet(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts fakeUserNetOptions
		want string
	}{
		{"root not mapped to the user", fakeUserNetOptions{uidMap: "1000 1000 1"}, "does not map root to uid"},
		{"host network namespace", fakeUserNetOptions{sharedNet: true}, "does not have a network namespace of its own"},
		{"not the bwrap child", fakeUserNetOptions{childPPID: "1"}, "is no longer held by bwrap"},
		{"uid map never written", fakeUserNetOptions{uidMap: "-"}, "user namespace was not set up"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := installFakeUserNet(t, tc.opts)
			if tc.opts.uidMap == "-" {
				old := userNetAttachTimeout
				userNetAttachTimeout = 500 * time.Millisecond
				t.Cleanup(func() { userNetAttachTimeout = old })
			}
			launch, _, _ := runFakeUserNetLaunch(t, env)
			if launch.attached || !strings.Contains(launch.failure, tc.want) {
				t.Fatalf("attached=%v failure=%q, want %q", launch.attached, launch.failure, tc.want)
			}
			events := assertHeldCommandNeverRan(t, env)
			for _, event := range events {
				if strings.HasPrefix(event, "nsenter:") || strings.HasPrefix(event, "slirp:") || strings.HasPrefix(event, "ip:") {
					t.Fatalf("a helper ran for an unconfirmed namespace:\n%s", strings.Join(events, "\n"))
				}
			}
		})
	}
}

// The nft filter only adds coverage for sockets that name their interface:
// the install still runs when nft fails or is missing.
func TestUserNetFilterIsBestEffort_DepsNet(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts fakeUserNetOptions
	}{
		{"nft fails", fakeUserNetOptions{nftExit: 1}},
		{"nft missing", fakeUserNetOptions{missing: []string{"nft"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := installFakeUserNet(t, tc.opts)
			launch, result, _ := runFakeUserNetLaunch(t, env)
			if !launch.attached || launch.failure != "" || result == nil || !strings.Contains(result.Stdout, "sandboxed command ran") {
				t.Fatalf("attached=%v failure=%q result=%+v", launch.attached, launch.failure, result)
			}
			events := eventLines(t, env.log)
			pid := userNetLaunchPID(events)
			assertEventOrder(t, events, "ip: -4 -batch -", "bwrap["+pid+"]: ran")
			if logged := strings.Contains(strings.Join(events, "\n"), "nft: "); logged != (tc.opts.nftExit != 0) {
				t.Fatalf("nft invoked = %v:\n%s", logged, strings.Join(events, "\n"))
			}
		})
	}
}

// Without nsenter or ip the namespace cannot be restricted, so the install
// runs loopback-only (npm retries off) and the guidance names the tool.
func TestDependencyInstallWithoutNetworkConfigToolsRunsOffline_DepsNet(t *testing.T) {
	for tool, pkg := range map[string]string{"nsenter": "util-linux", "ip": "iproute2"} {
		t.Run(tool, func(t *testing.T) {
			env := installFakeUserNet(t, fakeUserNetOptions{
				bwrapTail: "echo \"npm error code EAI_AGAIN\" >&2\nexit 1",
				missing:   []string{tool},
			})
			launch, network := prepareUserNetwork([]string{"npm", "install"})
			if launch != nil {
				launch.release()
				t.Fatalf("user-network launch prepared without %s", tool)
			}
			guidance := network.guidance()
			for _, want := range []string{tool + ", " + userNetToolRoles[tool] + ", was not found", "install " + pkg, "MAKEWAND_" + strings.ToUpper(tool) + "_PATH", "not a candidate failure"} {
				if !strings.Contains(guidance, want) {
					t.Fatalf("guidance %q lacks %q", guidance, want)
				}
			}
			if strings.Contains(guidance, "apt install slirp4netns") {
				t.Fatalf("guidance %q points at slirp4netns", guidance)
			}
			project, err := NewProject("usernet-no-"+tool, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			declareNpmRegistryDependency(t, project.Path)
			if _, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}}); err == nil || !strings.Contains(err.Error(), tool+", "+userNetToolRoles[tool]) {
				t.Fatalf("err = %v, want guidance naming %s", err, tool)
			}
			log := readFixFile(t, env.log)
			if strings.Contains(log, "info-sent") || strings.Contains(log, "slirp:") || !strings.Contains(log, "]: setenv npm_config_fetch_retries=0") {
				t.Fatalf("expected a loopback-only install with npm retries off:\n%s", log)
			}
		})
	}
}

// fakeUserNetConfiguration lets a fake bwrap that speaks only the
// --info-fd/--block-fd protocol attach a fake slirp4netns: the install then
// runs once, with the network.
func TestFakeUserNetConfigurationAttaches_DepsNet(t *testing.T) {
	fakeWorkingBwrap(t)
	swapSeccompVars(t, "linux", false, nil)
	dir := t.TempDir()
	log := filepath.Join(dir, "events.log")
	bwrap := filepath.Join(dir, "bwrap")
	writeFixScript(t, bwrap, `log=`+strconv.Quote(log)+`
info=; block=
while [ $# -gt 0 ]; do
  case "$1" in
    --info-fd) info=$2; shift 2 ;;
    --block-fd) block=$2; shift 2 ;;
    --ro-bind-data) shift 3 ;;
    *) shift ;;
  esac
done
echo "launch info=${info:-none}" >> "$log"
if [ -n "$info" ]; then
  eval "printf '{\"child-pid\": %d}\n' $$ >&$info"
  eval "exec $info>&-"
  eval "read -r unblocked <&$block"
fi
echo "ran" >> "$log"
`)
	verifyLookPath = func(string) (string, error) { return bwrap, nil }
	slirp := filepath.Join(dir, "slirp4netns")
	writeFixScript(t, slirp, fakeSlirp(log))
	resolv := filepath.Join(dir, "resolv.conf")
	writeFile(t, resolv, "nameserver 127.0.0.53\n")
	oldLookPath, oldResolv := userNetLookPath, userNetResolvConf
	userNetLookPath = func(string) (string, error) { return slirp, nil }
	userNetResolvConf = resolv
	t.Cleanup(func() { userNetLookPath, userNetResolvConf = oldLookPath, oldResolv })
	fakeUserNetConfiguration(t)

	project, err := NewProject("usernet-stubbed", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}})
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	events := eventLines(t, log)
	if strings.Count(strings.Join(events, "\n"), "launch ") != 1 {
		t.Fatalf("want a single networked launch:\n%s", strings.Join(events, "\n"))
	}
	assertEventOrder(t, events, "launch info=3", "slirp: ready-sent", "ran", "slirp: exit-fd closed")
}

func TestUserNetProcParsing_DepsNet(t *testing.T) {
	dir := t.TempDir()
	stat := filepath.Join(dir, "stat")
	writeFile(t, stat, "4242 (bw (rap) x) S 4241 4242 4242 0 -1\n")
	if ppid, err := procParentPID(stat); err != nil || ppid != 4241 {
		t.Fatalf("procParentPID = %d, %v; want 4241", ppid, err)
	}
	writeFile(t, stat, "4242 bwrap S")
	if _, err := procParentPID(stat); err == nil {
		t.Fatal("malformed stat accepted")
	}
	for _, tc := range []struct {
		uidMap string
		want   bool
	}{
		{"0 1000 1", true},
		{"         0       1000          1", true}, // as the kernel aligns it
		{"1000 1000 1", false},                     // bwrap's single mapping without root
		{"0 1000 0", false},
		{"0 0 4294967295", false}, // the initial namespace
		{"", false},
	} {
		if got := uidMapMapsRoot(tc.uidMap, 1000); got != tc.want {
			t.Errorf("uidMapMapsRoot(%q, 1000) = %v, want %v", tc.uidMap, got, tc.want)
		}
	}
}
