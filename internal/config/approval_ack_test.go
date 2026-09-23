package config

import (
	"errors"
	"testing"
	"time"
)

func TestParseApprovalMode(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"manual", ApprovalModeManual, true},
		{"safe", ApprovalModeSafe, true},
		{"autopilot", ApprovalModeAuto, true},
		{"  SAFE  ", ApprovalModeSafe, true},
		{"Autopilot", ApprovalModeAuto, true},
		{"", "", false},
		{"auto", "", false},
		{"autopilto", "", false},
		{"yolo", "", false},
	}
	for _, tt := range tests {
		got, ok := ParseApprovalMode(tt.in)
		if got != tt.want || ok != tt.wantOK {
			t.Fatalf("ParseApprovalMode(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

// TestParseApprovalModeIsStrict pins the difference from NormalizeApprovalMode:
// the CLI parser must reject what Normalize silently migrates to manual.
func TestParseApprovalModeIsStrict(t *testing.T) {
	for _, in := range []string{"bogus", "auto", "MANUALx"} {
		if got := NormalizeApprovalMode(in); got != ApprovalModeManual {
			t.Fatalf("NormalizeApprovalMode(%q) = %q, want manual migration", in, got)
		}
		if _, ok := ParseApprovalMode(in); ok {
			t.Fatalf("ParseApprovalMode(%q) accepted, want rejection", in)
		}
	}
}

func swapAckHostname(t *testing.T, fn func() (string, error)) {
	t.Helper()
	old := ackHostname
	ackHostname = fn
	t.Cleanup(func() { ackHostname = old })
}

func TestUnsafeHostExecAckRecordAndValidate(t *testing.T) {
	swapAckHostname(t, func() (string, error) { return "host-a", nil })

	cfg := DefaultConfig()
	if cfg.UnsafeHostExecAckValid() {
		t.Fatal("fresh config must not carry a valid acknowledgment")
	}

	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	if err := cfg.RecordUnsafeHostExecAck(now); err != nil {
		t.Fatalf("RecordUnsafeHostExecAck: %v", err)
	}
	if cfg.UnsafeHostExecAckVersion != UnsafeHostExecAckCurrentVersion {
		t.Fatalf("version = %d, want %d", cfg.UnsafeHostExecAckVersion, UnsafeHostExecAckCurrentVersion)
	}
	if cfg.UnsafeHostExecAckAt != "2026-07-21T12:00:00Z" {
		t.Fatalf("at = %q, want RFC3339 UTC stamp", cfg.UnsafeHostExecAckAt)
	}
	if cfg.UnsafeHostExecAckHost != "host-a" {
		t.Fatalf("host = %q, want host-a", cfg.UnsafeHostExecAckHost)
	}
	if !cfg.UnsafeHostExecAckValid() {
		t.Fatal("recorded acknowledgment must validate on the same host")
	}
}

func TestUnsafeHostExecAckInvalidCases(t *testing.T) {
	swapAckHostname(t, func() (string, error) { return "host-a", nil })

	base := func() *Config {
		cfg := DefaultConfig()
		if err := cfg.RecordUnsafeHostExecAck(time.Now()); err != nil {
			t.Fatalf("RecordUnsafeHostExecAck: %v", err)
		}
		return cfg
	}

	t.Run("older version requires re-ack", func(t *testing.T) {
		cfg := base()
		cfg.UnsafeHostExecAckVersion = UnsafeHostExecAckCurrentVersion - 1
		if cfg.UnsafeHostExecAckValid() {
			t.Fatal("older ack version must not validate")
		}
	})

	t.Run("copied config from another host requires re-ack", func(t *testing.T) {
		cfg := base()
		cfg.UnsafeHostExecAckHost = "host-b"
		if cfg.UnsafeHostExecAckValid() {
			t.Fatal("ack recorded on another host must not validate")
		}
	})

	t.Run("empty host never validates", func(t *testing.T) {
		cfg := base()
		cfg.UnsafeHostExecAckHost = ""
		if cfg.UnsafeHostExecAckValid() {
			t.Fatal("ack without host binding must not validate")
		}
	})

	t.Run("hostname error fails closed", func(t *testing.T) {
		cfg := base()
		swapAckHostname(t, func() (string, error) { return "", errors.New("no hostname") })
		if cfg.UnsafeHostExecAckValid() {
			t.Fatal("hostname resolution failure must invalidate the ack")
		}
		if err := cfg.RecordUnsafeHostExecAck(time.Now()); err == nil {
			t.Fatal("RecordUnsafeHostExecAck must fail when the hostname cannot be resolved")
		}
	})

	t.Run("nil config", func(t *testing.T) {
		var cfg *Config
		if cfg.UnsafeHostExecAckValid() {
			t.Fatal("nil config must not validate")
		}
	})
}
