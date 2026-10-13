//go:build linux

package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/trustedbin"
)

// installFakeTC adds a trusted tc to the fake user-mode network tools.
func installFakeTC(t *testing.T, env fakeUserNetEnv, status int) {
	t.Helper()
	env.tools["tc"] = filepath.Join(env.dir, "bin", "tc")
	writeFixScript(t, env.tools["tc"], fakeNetConfigTool(env.log, "tc", "", status))
	old := userNetLookPath
	userNetLookPath = func(name string) (string, error) {
		if name == "tc" {
			return env.tools["tc"], nil
		}
		return old(name)
	}
	t.Cleanup(func() { userNetLookPath = old })
}

// Each part of the user-mode network setup that can fail gets its own
// explanation and remedy; none of them blames slirp4netns for another tool's
// failure.
func TestUserNetSetupFailureGuidanceNamesTheStage_R3(t *testing.T) {
	const npmOffline = "echo \"npm error code ENOTFOUND\" >&2\nexit 1"
	cases := []struct {
		name    string
		opts    fakeUserNetOptions
		nsenter string // replaces the fake nsenter
		want    []string
		notWant []string
	}{
		{
			name: "route installation fails",
			opts: fakeUserNetOptions{bwrapTail: npmOffline, ipOutput: "RTNETLINK answers: Operation not permitted", ipExit: 2},
			want: []string{
				"slirp4netns was attached, but the routes that keep the sandbox off private and host addresses could not be installed (ip: exit status 2: RTNETLINK answers: Operation not permitted), so the dependency install ran without outbound network",
				"fix ip (iproute2) on this host or set " + trustedbin.OverrideEnv("ip") + " to a working copy",
			},
			notWant: []string{"slirp4netns is installed but could not be attached", trustedbin.OverrideEnv(userNetHelperName)},
		},
		{
			name: "slirp4netns fails",
			opts: fakeUserNetOptions{bwrapTail: npmOffline, slirpBody: "echo 'slirp4netns: open(\"/dev/net/tun\"): Operation not permitted' >&2\nexit 1\n"},
			want: []string{
				"slirp4netns is installed but could not be attached to the sandbox (slirp4netns did not configure the sandbox network: EOF: slirp4netns: open(\"/dev/net/tun\"): Operation not permitted)",
				"set " + trustedbin.OverrideEnv(userNetHelperName) + " to a working copy",
			},
			notWant: []string{trustedbin.OverrideEnv("ip"), trustedbin.OverrideEnv("nsenter")},
		},
		{
			name:    "nsenter cannot enter the namespaces",
			opts:    fakeUserNetOptions{bwrapTail: npmOffline},
			nsenter: "echo \"nsenter: reassociate to namespace 'ns/user' failed: Operation not permitted\" >&2\nexit 1\n",
			want: []string{
				"nsenter could not enter the sandbox's namespaces to configure its network (",
				"nsenter: reassociate to namespace 'ns/user' failed: Operation not permitted",
				"fix nsenter (util-linux) on this host or set " + trustedbin.OverrideEnv("nsenter") + " to a working copy",
			},
			notWant: []string{trustedbin.OverrideEnv(userNetHelperName), trustedbin.OverrideEnv("ip")},
		},
		{
			name: "the sandbox namespace cannot be confirmed",
			opts: fakeUserNetOptions{bwrapTail: npmOffline, uidMap: "1000 1000 1"},
			want: []string{
				"the sandbox's network namespace could not be prepared for slirp4netns (the sandbox user namespace does not map root to uid",
				"check that bubblewrap can create user and network namespaces on this host",
			},
			notWant: []string{trustedbin.OverrideEnv(userNetHelperName), trustedbin.OverrideEnv("ip")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := installFakeUserNet(t, tc.opts)
			if tc.nsenter != "" {
				writeFixScript(t, env.tools["nsenter"], tc.nsenter)
			}
			project, err := NewProject("r3-guidance", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			declareNpmRegistryDependency(t, project.Path)
			_, err = project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}})
			if !errors.Is(err, ErrDependencyNetworkUnavailable) {
				t.Fatalf("err = %v, want ErrDependencyNetworkUnavailable", err)
			}
			for _, want := range append(tc.want, "or install the project's dependencies outside makewand; this is an environment problem, not a candidate failure") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("guidance lacks %q:\n%v", want, err)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("guidance mentions %q:\n%v", bad, err)
				}
			}
		})
	}
}

// With a trusted tc, IPv4 multicast is dropped on the tap device's egress
// before the held command runs, whatever interface a socket names; nft is
// not needed for it.
func TestUserNetDropsMulticastOnTheTapDevice_R3(t *testing.T) {
	env := installFakeUserNet(t, fakeUserNetOptions{bwrapTail: "exit 0", missing: []string{"nft"}})
	installFakeTC(t, env, 0)
	project, err := NewProject("r3-tc", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}})
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	events := eventLines(t, env.log)
	pid := userNetLaunchPID(events)
	child := filepath.Join(env.proc, pid, "ns")
	assertEventOrder(t, events, "ip: -4 -batch -",
		"nsenter: --user="+filepath.Join(child, "user")+" --preserve-credentials --net="+filepath.Join(child, "net")+" -- "+env.tools["tc"]+" -batch -",
		"tc: -batch -", "bwrap["+pid+"]: released", "bwrap["+pid+"]: ran")
	if got := readFixFile(t, env.log+".tc"); got != userNetMulticastFilter {
		t.Fatalf("tc batch:\n%s\nwant:\n%s", got, userNetMulticastFilter)
	}
	for _, want := range []string{"qdisc add dev tap0 clsact", "filter add dev tap0 egress protocol ip prio 1 u32 match ip dst 224.0.0.0/4 action drop"} {
		if !strings.Contains(userNetMulticastFilter, want) {
			t.Fatalf("filter lacks %q", want)
		}
	}
}

// The multicast filter is best effort, like the nft filter: the install runs
// when tc fails or is missing.
func TestUserNetMulticastFilterIsBestEffort_R3(t *testing.T) {
	env := installFakeUserNet(t, fakeUserNetOptions{})
	installFakeTC(t, env, 1)
	launch, result, _ := runFakeUserNetLaunch(t, env)
	if !launch.attached || launch.failure != "" || result == nil || !strings.Contains(result.Stdout, "sandboxed command ran") {
		t.Fatalf("attached=%v failure=%q result=%+v", launch.attached, launch.failure, result)
	}
	events := eventLines(t, env.log)
	assertEventOrder(t, events, "tc: -batch -", "nft: -f -", "bwrap["+userNetLaunchPID(events)+"]: ran")
}

// Live: without nft, multicast a deps step sends through a socket that names
// tap0 never reaches the tap device (where slirp4netns would resend it onto
// the host's LAN segment). The device's transmit counter, read inside the
// sandbox's network namespace, shows it; datagrams to the slirp4netns gateway
// through tap0 (which slirp4netns refuses to forward) are the control.
func TestDependencyInstallMulticastNeverReachesTheTapDevice_R3(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_SANDBOX_MEMORY_MAX", "1G")
	t.Setenv(trustedbin.OverrideEnv("nft"), "/nonexistent/nft")
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}
	for _, tool := range []string{userNetHelperName, "nsenter", "ip", "tc"} {
		if _, err := trustedbin.LookPath(tool); err != nil {
			t.Skipf("%s unavailable: %v", tool, err)
		}
	}
	project, err := NewProject("r3-mcast-live", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sends = 20
	script := `import socket, struct, sys, time
sends = int(sys.argv[1])
def tx():
    for line in open("/proc/net/dev"):
        name, _, counters = line.partition(":")
        if name.strip() == "tap0":
            return int(counters.split()[9])
    raise SystemExit("no tap0: user-mode network not attached")
def burst(setup, address):
    before = tx()
    for _ in range(sends):
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        try:
            setup(s)
            s.sendto(b"makewand-multicast-probe", address)
        except OSError:
            pass
        finally:
            s.close()
    time.sleep(0.2)
    return tx() - before
idx = socket.if_nametoindex("tap0")
bind_tap = lambda s: s.setsockopt(socket.SOL_SOCKET, socket.SO_BINDTODEVICE, b"tap0")
gateway = burst(bind_tap, ("10.0.2.2", 9))
mcast_if = burst(lambda s: s.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_IF, struct.pack("4s4si", b"\0" * 4, b"\0" * 4, idx)), ("239.255.77.77", 9))
bound = burst(bind_tap, ("239.255.77.77", 9))
print("gateway=%d mcast_if=%d bound=%d" % (gateway, mcast_if, bound))
`
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "python3", Args: []string{"-c", script, strconv.Itoa(sends)}})
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("err=%v result=%+v", err, result)
	}
	var gateway, mcastIf, bound int
	if _, err := fmt.Sscanf(strings.TrimSpace(result.Stdout), "gateway=%d mcast_if=%d bound=%d", &gateway, &mcastIf, &bound); err != nil {
		t.Fatalf("output %q: %v", result.Stdout, err)
	}
	if gateway < sends {
		t.Fatalf("control: %d of %d datagrams to the gateway counted on tap0; the counter does not work", gateway, sends)
	}
	if mcastIf >= sends/2 || bound >= sends/2 {
		t.Fatalf("multicast reached tap0 (IP_MULTICAST_IF: %d, SO_BINDTODEVICE: %d of %d packets)", mcastIf, bound, sends)
	}
}

// A plan's own environment settings reach the sandboxed command.
func TestVerificationPlanEnvironmentReachesTheSandbox_R3(t *testing.T) {
	env := installFakeUserNet(t, fakeUserNetOptions{bwrapTail: "exit 0"})
	project, err := NewProject("r3-plan-env", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plan := ExecPlan{Kind: "tests", Command: "pytest", env: []string{"DIST_EXTRA_CONFIG=/tmp/makewand-egg-base-x/distutils.cfg"}}
	if _, err := project.RunVerificationPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if log := readFixFile(t, env.log); !strings.Contains(log, "]: setenv DIST_EXTRA_CONFIG=/tmp/makewand-egg-base-x/distutils.cfg\n") {
		t.Fatalf("plan environment missing from the sandbox:\n%s", log)
	}
}
