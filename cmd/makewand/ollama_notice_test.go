package main

import (
	"strings"
	"testing"
)

func TestOllamaSetupNotice_Localhost(t *testing.T) {
	t.Setenv("MAKEWAND_OLLAMA_ALLOW_REMOTE", "")
	got := ollamaSetupNotice("http://localhost:11434")
	if got == "" {
		t.Fatal("ollamaSetupNotice(localhost) = empty, want guidance")
	}
}

func TestOllamaSetupNotice_RemoteBlocked(t *testing.T) {
	t.Setenv("MAKEWAND_OLLAMA_ALLOW_REMOTE", "")
	got := ollamaSetupNotice("http://10.0.0.2:11434")
	if got == "" || !containsAllStrings(got, "10.0.0.2", "blocked", "MAKEWAND_OLLAMA_ALLOW_REMOTE=1") {
		t.Fatalf("ollamaSetupNotice(remote blocked) = %q", got)
	}
}

func TestOllamaDoctorCheck_RemoteBlocked(t *testing.T) {
	t.Setenv("MAKEWAND_OLLAMA_ALLOW_REMOTE", "")
	check, ok := ollamaDoctorCheck("http://10.0.0.2:11434")
	if !ok {
		t.Fatal("ollamaDoctorCheck() ok = false, want true")
	}
	if check.Status != doctorWarn {
		t.Fatalf("status = %q, want %q", check.Status, doctorWarn)
	}
	if !containsAllStrings(check.Details, "10.0.0.2", "MAKEWAND_OLLAMA_ALLOW_REMOTE=1") {
		t.Fatalf("details = %q", check.Details)
	}
}

func TestOllamaDoctorCheck_RemoteAllowed(t *testing.T) {
	t.Setenv("MAKEWAND_OLLAMA_ALLOW_REMOTE", "1")
	check, ok := ollamaDoctorCheck("http://10.0.0.2:11434")
	if !ok {
		t.Fatal("ollamaDoctorCheck() ok = false, want true")
	}
	if check.Status != doctorPass {
		t.Fatalf("status = %q, want %q", check.Status, doctorPass)
	}
}

func containsAllStrings(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
