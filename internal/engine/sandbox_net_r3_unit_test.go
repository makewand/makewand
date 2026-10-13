//go:build linux

package engine

import (
	"errors"
	"strings"
	"testing"
)

// userNetFailure.guidance keeps the one stage-independent rule: a failure
// inside nsenter names nsenter, whatever stage ran it.
func TestUserNetFailureGuidanceStages_R3(t *testing.T) {
	for _, tc := range []struct {
		failure userNetFailure
		want    string
	}{
		{userNetFailure{stage: userNetStageRoutes, cause: "ip: exit status 2: RTNETLINK answers: File exists"}, "routes that keep the sandbox off private and host addresses could not be installed (ip: exit status 2"},
		{userNetFailure{stage: userNetStageRoutes, cause: "ip: exit status 1: nsenter: cannot open /proc/self/fd/4: No such file"}, "nsenter could not enter the sandbox's namespaces"},
		{userNetFailure{stage: userNetStageHelper, cause: "start slirp4netns: permission denied"}, "slirp4netns is installed but could not be attached to the sandbox (start slirp4netns: permission denied)"},
		{userNetFailure{stage: userNetStageSandbox, cause: "sandbox did not report its namespace: EOF"}, "the sandbox's network namespace could not be prepared for slirp4netns (sandbox did not report its namespace: EOF)"},
	} {
		if got := (sandboxNetwork{kind: sandboxNetworkNone, attach: &tc.failure}).guidance(); !strings.Contains(got, tc.want) || !strings.HasSuffix(got, environmentNote) {
			t.Errorf("%+v: %q, want %q", tc.failure, got, tc.want)
		}
	}
	// The launch failure keeps its historical wording; the stage rides along.
	stageErr := &userNetStageError{stage: userNetStageRoutes, err: errors.New("ip: exit status 2")}
	if got := stageErr.Error(); got != "could not keep the sandbox network off private and host addresses: ip: exit status 2" {
		t.Fatalf("routes stage error = %q", got)
	}
}
