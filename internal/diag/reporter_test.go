package diag

import (
	"errors"
	"strings"
	"testing"
)

func TestReporterWarnErrFormatsHumanOutput(t *testing.T) {
	var buf strings.Builder
	reporter := New(&buf)

	reporter.WarnErr("could not load config", errors.New("boom"))

	got := buf.String()
	want := "Warning: could not load config: boom\n"
	if got != want {
		t.Fatalf("WarnErr output = %q, want %q", got, want)
	}
}

func TestReporterErrorErrFormatsHumanOutput(t *testing.T) {
	var buf strings.Builder
	reporter := New(&buf)

	reporter.ErrorErr("config load failed", errors.New("boom"))

	got := buf.String()
	want := "Error: config load failed: boom\n"
	if got != want {
		t.Fatalf("ErrorErr output = %q, want %q", got, want)
	}
}

func TestReporterInfoPathFormatsHumanOutput(t *testing.T) {
	var buf strings.Builder
	reporter := New(&buf)

	reporter.InfoPath("Debug trace enabled", "/tmp/trace.jsonl")

	got := buf.String()
	want := "Debug trace enabled: /tmp/trace.jsonl\n"
	if got != want {
		t.Fatalf("InfoPath output = %q, want %q", got, want)
	}
}
