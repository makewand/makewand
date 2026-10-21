package main

import (
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/remotesession"
)

// go-server-1021-3: serve stores remote sessions with per-account and
// free-space limits by default, configurable by flag or environment.
func TestServeSessionQuotaDefaultsFlagsAndEnvironment(t *testing.T) {
	for _, key := range []string{"MAKEWAND_SERVER_SESSIONS_PER_OWNER", "MAKEWAND_SERVER_SESSION_MIB_PER_OWNER", "MAKEWAND_SERVER_SESSION_MIN_FREE_MIB"} {
		t.Setenv(key, "")
	}
	cmd := serveCmd()
	resolve := func() (remotesession.Quota, error) {
		t.Helper()
		flags := cmd.Flags()
		perOwner, err := flags.GetInt("sessions-per-owner")
		if err != nil {
			t.Fatal(err)
		}
		mib, err := flags.GetInt64("session-mib-per-owner")
		if err != nil {
			t.Fatal(err)
		}
		minFree, err := flags.GetInt64("session-min-free-mib")
		if err != nil {
			t.Fatal(err)
		}
		return serveSessionQuota(flags.Changed, perOwner, mib, minFree)
	}
	quota, err := resolve()
	if err != nil || quota != remotesession.DefaultQuota() {
		t.Fatalf("default quota = %+v, %v; want %+v", quota, err, remotesession.DefaultQuota())
	}
	if got := describeSessionQuota(quota); got != "1000 sessions and 256 MiB per account; saves keep 1024 MiB free on the disk" {
		t.Fatalf("startup line = %q", got)
	}

	t.Setenv("MAKEWAND_SERVER_SESSIONS_PER_OWNER", "40")
	t.Setenv("MAKEWAND_SERVER_SESSION_MIB_PER_OWNER", "0")
	t.Setenv("MAKEWAND_SERVER_SESSION_MIN_FREE_MIB", "512")
	quota, err = resolve()
	want := remotesession.Quota{MaxSessionsPerOwner: 40, MinFreeBytes: 512 << 20}
	if err != nil || quota != want {
		t.Fatalf("environment quota = %+v, %v; want %+v", quota, err, want)
	}
	if got := describeSessionQuota(quota); !strings.Contains(got, "unlimited size") {
		t.Fatalf("startup line = %q", got)
	}

	// A flag given on the command line wins over the environment.
	if err := cmd.Flags().Set("sessions-per-owner", "7"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("session-min-free-mib", "0"); err != nil {
		t.Fatal(err)
	}
	quota, err = resolve()
	want = remotesession.Quota{MaxSessionsPerOwner: 7}
	if err != nil || quota != want {
		t.Fatalf("flag quota = %+v, %v; want %+v", quota, err, want)
	}

	for _, tc := range []struct{ env, value string }{
		{"MAKEWAND_SERVER_SESSION_MIB_PER_OWNER", "-1"},
		{"MAKEWAND_SERVER_SESSION_MIB_PER_OWNER", "lots"},
		{"MAKEWAND_SERVER_SESSION_MIB_PER_OWNER", "9223372036854775807"},
	} {
		t.Setenv(tc.env, tc.value)
		if _, err := resolve(); err == nil {
			t.Fatalf("%s=%s accepted", tc.env, tc.value)
		}
	}
	if _, err := serveSessionQuota(func(string) bool { return true }, -1, 0, 0); err == nil {
		t.Fatal("negative --sessions-per-owner accepted")
	}
}
