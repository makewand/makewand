package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
)

type captureAudit struct{ events []serveraudit.Event }

func (c *captureAudit) Log(evt serveraudit.Event) { c.events = append(c.events, evt) }

// go-server#3: registration limits are operator-configurable, the global cap
// can be turned off, and hitting it alerts instead of failing silently.
func TestServeRegistrationLimits(t *testing.T) {
	limits, err := serveRegistrationLimits(3, 0, 2*time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	if limits.MaxGlobal != -1 || limits.MaxPerSource != 3 || limits.Window != 2*time.Hour || limits.MaxConcurrent != 4 {
		t.Fatalf("limits=%+v", limits)
	}
	limiter := serverauth.NewRegistrationRateLimiterWithLimits(limits)
	now := time.Now().UTC()
	for i := 0; i < 100; i++ {
		if !limiter.AllowAt(fmt.Sprintf("198.51.%d.%d", i/200, i%200), now) {
			t.Fatalf("global cap should be disabled (attempt %d)", i)
		}
	}
	for _, tc := range []struct {
		perIP, global, slots int
		window               time.Duration
	}{
		{0, 30, 2, time.Hour}, {5, -1, 2, time.Hour}, {5, 30, 0, time.Hour}, {5, 30, 2, 0},
	} {
		if _, err := serveRegistrationLimits(tc.perIP, tc.global, tc.window, tc.slots); err == nil {
			t.Fatalf("invalid limits accepted: %+v", tc)
		}
	}
	cmd := serveCmd()
	for _, name := range []string{"registration-per-ip-limit", "registration-global-limit", "registration-window", "registration-concurrency"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("serve is missing --%s", name)
		}
	}
}

func TestServeRegistrationGlobalLimitAlert(t *testing.T) {
	var stderr bytes.Buffer
	audit := &captureAudit{}
	limiter := serverauth.NewRegistrationRateLimiterWithLimits(serverauth.RegistrationLimits{MaxPerSource: 1, MaxGlobal: 2, Window: time.Hour})
	limiter.SetGlobalLimitAlert(registrationGlobalLimitAlert(&stderr, audit))
	now := time.Now().UTC()
	for _, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"} {
		limiter.AllowAt(ip, now)
	}
	if !strings.Contains(stderr.String(), "self-registration global limit reached (2 per 1h0m0s") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if strings.Count(stderr.String(), "warning:") != 1 {
		t.Fatalf("alert repeated within a window: %q", stderr.String())
	}
	if len(audit.events) != 1 || audit.events[0].Kind != "registration_global_limit" || audit.events[0].Status != 429 {
		t.Fatalf("audit events=%+v", audit.events)
	}
}

// go-server#4: an API-key-only deployment (the documented Docker Compose shape)
// must be told to opt into paid API use instead of "run makewand setup".
func TestServeNoModelsErrorExplainsAPIPolicy(t *testing.T) {
	t.Setenv("MAKEWAND_API_POLICY", "")
	if err := os.Unsetenv("MAKEWAND_API_POLICY"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{OpenAIAPIKey: "sk-test"}
	if cfg.HasAnyModel() {
		t.Fatal("API key alone should not enable a model under the default policy")
	}
	err := serveNoModelsError(cfg)
	if err == nil || !strings.Contains(err.Error(), "MAKEWAND_API_POLICY=allow_paid") {
		t.Fatalf("error=%v, want a MAKEWAND_API_POLICY=allow_paid hint", err)
	}
	t.Setenv("MAKEWAND_API_POLICY", "allow_paid")
	if !cfg.HasAnyModel() {
		t.Fatal("allow_paid should enable the API key provider")
	}
	if err := serveNoModelsError(&config.Config{}); err == nil || !strings.Contains(err.Error(), "makewand setup") {
		t.Fatalf("no-key error=%v", err)
	}
}
