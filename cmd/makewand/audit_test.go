package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/makewand/makewand/serveraudit"
)

func TestParseAuditTimeValue(t *testing.T) {
	now := time.Date(2026, 3, 23, 12, 0, 0, 0, time.UTC)
	got, err := parseAuditTimeValue("24h", now)
	if err != nil {
		t.Fatalf("parseAuditTimeValue(duration): %v", err)
	}
	if !got.Equal(now.Add(-24 * time.Hour)) {
		t.Fatalf("duration result = %s, want %s", got, now.Add(-24*time.Hour))
	}

	got, err = parseAuditTimeValue("2026-03-23T10:00:00Z", now)
	if err != nil {
		t.Fatalf("parseAuditTimeValue(rfc3339): %v", err)
	}
	if got.Format(time.RFC3339) != "2026-03-23T10:00:00Z" {
		t.Fatalf("RFC3339 result = %s", got.Format(time.RFC3339))
	}
}

func TestResolveAuditLogPath_DefaultsToServerAuditFile(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", cfgDir)
	t.Setenv("MAKEWAND_SERVER_AUDIT_LOG", "")

	got, err := resolveAuditLogPath("")
	if err != nil {
		t.Fatalf("resolveAuditLogPath: %v", err)
	}
	want := filepath.Join(cfgDir, "server", "audit.jsonl")
	if got != want {
		t.Fatalf("resolveAuditLogPath() = %q, want %q", got, want)
	}
}

func TestLoadAuditEvents_MissingFileReturnsEmpty(t *testing.T) {
	events, err := loadAuditEvents(filepath.Join(t.TempDir(), "missing.jsonl"), serveraudit.Filter{})
	if err != nil {
		t.Fatalf("loadAuditEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("len(events) = %d, want 0", len(events))
	}
}
