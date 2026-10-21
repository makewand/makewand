package router

import (
	"github.com/makewand/makewand/execution"
	"testing"
	"time"
)

func TestAllowlistedCLIQuotaRemainsKnownFailure(t *testing.T) {
	err := formatCLIExecutionError("gemini", "", r4GeminiTerminalQuota, errR4Exit, nil, time.Second)
	outcome := providerOutcome(err, Usage{})
	if ErrorKindOf(err) != ErrorKindRateLimit || outcome.Status != execution.Failed || !outcome.Known {
		t.Fatalf("confirmed CLI quota became unknown: %v %+v", err, outcome)
	}
}
