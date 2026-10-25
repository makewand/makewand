//go:build linux

package engine

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/trustedbin"
)

// hostIPAddrs returns this host's own addresses of one family, loopback
// excluded.
func hostIPAddrs(v4 bool) []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || (ipNet.IP.To4() != nil) != v4 {
			continue
		}
		if !v4 && !ipNet.IP.IsGlobalUnicast() {
			continue
		}
		out = append(out, ipNet.IP.String())
	}
	return out
}

// hostResolverAnswers reports whether the first resolv.conf nameserver
// answers a DNS query from the host itself (slirp4netns forwards 10.0.2.3 to
// it).
func hostResolverAnswers() bool {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return false
	}
	server := ""
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "nameserver" {
			server = fields[1]
			break
		}
	}
	if server == "" {
		return false
	}
	conn, err := net.DialTimeout("udp", net.JoinHostPort(server, "53"), 3*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(dnsProbeQuery); err != nil {
		return false
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	return err == nil && n >= 2 && buf[0] == dnsProbeQuery[0] && buf[1] == dnsProbeQuery[1]
}

// dnsProbeQuery asks for the A record of example.com.
var dnsProbeQuery = []byte("\x4d\x57\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x07example\x03com\x00\x00\x01\x00\x01")

// hostReachablePublicAddr returns a public TCP endpoint the host itself can
// reach, or "" when the host has no internet access.
func hostReachablePublicAddr() string {
	for _, addr := range []string{"1.1.1.1:443", "223.5.5.5:443", "8.8.8.8:443"} {
		if conn, err := net.DialTimeout("tcp4", addr, 2*time.Second); err == nil {
			_ = conn.Close()
			return addr
		}
	}
	return ""
}

// Live check of the dependency-install network: from inside a deps step, this
// host's own addresses (with a service listening on 0.0.0.0), other private,
// CGNAT, link-local, multicast and reserved destinations, the slirp4netns
// gateway and loopback are unreachable, even for a socket bound to the tap
// device. With MAKEWAND_TEST_LIVE_NETWORK=1 it also checks that DNS through
// 10.0.2.3 still answers and public egress works, which contacts the host
// resolver and public addresses.
func TestDependencyInstallNetworkBlocksNonPublicDestinations_DepsNet(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_SANDBOX_MEMORY_MAX", "1G")
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}
	for _, tool := range []string{userNetHelperName, "nsenter", "ip"} {
		if _, err := trustedbin.LookPath(tool); err != nil {
			t.Skipf("%s unavailable: %v", tool, err)
		}
	}
	_, nftErr := trustedbin.LookPath("nft")

	serve := func(network, addr string) string {
		l, err := net.Listen(network, addr)
		if err != nil {
			return ""
		}
		t.Cleanup(func() { _ = l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				_, _ = c.Write([]byte("HOST-SERVICE\n"))
				_ = c.Close()
			}
		}()
		_, port, _ := net.SplitHostPort(l.Addr().String())
		return port
	}
	port := serve("tcp4", "0.0.0.0:0")
	if port == "" {
		t.Fatal("cannot listen on 0.0.0.0")
	}
	port6 := ""
	hostV6 := hostIPAddrs(false)
	if len(hostV6) > 0 {
		if port6 = serve("tcp6", "[::]:0"); port6 == "" {
			hostV6 = nil
		}
	}
	expectDNS, public := liveNetworkExpectations(hostResolverAnswers, hostReachablePublicAddr)
	if !liveNetworkTests() {
		t.Logf("DNS and public egress checks skipped: they reach the internet; set %s=1 to run them", liveNetworkTestsEnv)
	}

	project, err := NewProject("usernet-isolation-live", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	script := `import errno, socket, struct, sys, time
port, port6, host4, host6, public, expect_dns, expect_filter = sys.argv[1:8]
port = int(port)
problems = []
ROUTE_ERRORS = (errno.EHOSTUNREACH, errno.ENETUNREACH, errno.EACCES, errno.EPERM)

def attempt(host, p, family=socket.AF_INET, device=None, timeout=4):
    s = socket.socket(family, socket.SOCK_STREAM)
    s.settimeout(timeout)
    start = time.monotonic()
    try:
        if device:
            s.setsockopt(socket.SOL_SOCKET, socket.SO_BINDTODEVICE, device.encode())
        s.connect((host, p))
        try:
            data = s.recv(64)
        except OSError:
            data = b""
        return "connected", data, time.monotonic() - start
    except socket.timeout:
        return "timeout", None, time.monotonic() - start
    except OSError as e:
        return e.errno, None, time.monotonic() - start
    finally:
        s.close()

def must_block(label, host, p, **kw):
    result, data, _ = attempt(host, p, **kw)
    if result == "connected":
        problems.append("%s %s:%d reachable (%r)" % (label, host, p, data))

def must_reject(label, host, p=9):
    result, data, took = attempt(host, p)
    if result not in ROUTE_ERRORS or took > 1.5:
        problems.append("%s %s:%d not rejected by the sandbox routes: %r after %.1fs" % (label, host, p, result, took))

with open("/proc/net/route") as f:
    if not any(line.split()[1] == "00000000" for line in list(f)[1:]):
        problems.append("no default route: user-mode network not attached")
with open("/etc/resolv.conf") as f:
    if "nameserver 10.0.2.3" not in f.read():
        problems.append("resolver not pointed at the slirp4netns forwarder")

for i, addr in enumerate(filter(None, host4.split(","))):
    must_block("host address", addr, port)
    must_reject("host address", addr, port)
    if i == 0:
        # A socket bound to the device skips the route lookup's verdict and
        # sends on-link; slirp4netns never answers ARP for such addresses.
        must_block("host address via tap0", addr, port, device="tap0", timeout=2)
for addr in filter(None, host6.split(",")):
    must_block("host IPv6 address", addr, int(port6), family=socket.AF_INET6)
result, data, _ = attempt("127.0.0.1", port)
if data == b"HOST-SERVICE\n":
    problems.append("host loopback service reachable via 127.0.0.1")
must_block("slirp4netns gateway", "10.0.2.2", port)
must_block("slirp4netns gateway via tap0", "10.0.2.2", port, device="tap0", timeout=2)
must_block("DNS forwarder on a non-DNS port", "10.0.2.3", 443, device="tap0", timeout=2)
for label, addr in (("rfc1918 10/8", "10.255.255.254"), ("rfc1918 172.16/12", "172.31.255.254"),
                    ("rfc1918 192.168/16", "192.168.255.254"), ("slirp4netns network", "10.0.2.50"),
                    ("cgnat", "100.64.0.1"), ("link-local metadata", "169.254.169.254"),
                    ("this network", "0.1.2.3"), ("reserved", "240.0.0.1")):
    must_reject(label, addr)

m = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
m.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_TTL, 4)
try:
    m.sendto(b"probe", ("239.255.77.77", 9))
    problems.append("multicast sent")
except OSError as e:
    if e.errno not in ROUTE_ERRORS:
        problems.append("multicast: unexpected %r" % (e,))
if expect_filter == "1":
    ifindex = socket.if_nametoindex("tap0")
    m.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_IF, struct.pack("4s4si", b"\0" * 4, b"\0" * 4, ifindex))
    try:
        m.sendto(b"probe", ("239.255.77.77", 9))
        problems.append("multicast bound to tap0 sent")
    except OSError as e:
        if e.errno not in ROUTE_ERRORS:
            problems.append("multicast bound to tap0: unexpected %r" % (e,))

if expect_dns == "1":
    q = b"\x4d\x57\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x07example\x03com\x00\x00\x01\x00\x01"
    u = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    u.settimeout(5)
    try:
        u.sendto(q, ("10.0.2.3", 53))
        if u.recvfrom(512)[0][:2] != q[:2]:
            problems.append("DNS answer did not match the query")
    except OSError as e:
        problems.append("DNS through 10.0.2.3 failed: %r" % (e,))
if public:
    host, p = public.rsplit(":", 1)
    result, _, _ = attempt(host, int(p), timeout=8)
    if result != "connected":
        problems.append("public egress to %s failed: %r" % (public, result))

if problems:
    print("\n".join(problems))
    sys.exit(1)
print("ok")
`
	expectFilter := "0"
	if nftErr == nil {
		expectFilter = "1"
	}
	args := []string{"-c", script, port, port6, strings.Join(hostIPAddrs(true), ","), strings.Join(hostV6, ","), public, map[bool]string{false: "0", true: "1"}[expectDNS], expectFilter}
	start := time.Now()
	result, err := runFetchNetworkProbeForTest(t, project, "python3", args)
	if err != nil || result == nil || result.ExitCode != 0 {
		out := ""
		if result != nil {
			out = strings.TrimSpace(result.Stdout + "\n" + result.Stderr)
		}
		t.Fatalf("dependency sandbox network (after %s, dns=%v public=%q filter=%s): err=%v exit=%v\n%s", time.Since(start).Round(time.Millisecond), expectDNS, public, expectFilter, err, exitCodeOf(result), out)
	}
}

func exitCodeOf(result *ExecResult) string {
	if result == nil {
		return "<nil>"
	}
	return strconv.Itoa(result.ExitCode)
}
