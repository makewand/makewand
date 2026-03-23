package main

import (
	"testing"
)

func TestResolveOptionalRemoteAdminTarget(t *testing.T) {
	t.Setenv("MAKEWAND_REMOTE_URL", "http://127.0.0.1:8080")
	t.Setenv("MAKEWAND_REMOTE_TOKEN", "secret")

	urlValue, tokenValue, remoteMode, err := resolveOptionalRemoteAdminTarget("", "")
	if err != nil {
		t.Fatalf("resolveOptionalRemoteAdminTarget: %v", err)
	}
	if remoteMode {
		t.Fatal("remoteMode = true, want false when no flags are supplied")
	}

	urlValue, tokenValue, remoteMode, err = resolveOptionalRemoteAdminTarget("http://127.0.0.1:8080", "")
	if err != nil {
		t.Fatalf("resolveOptionalRemoteAdminTarget(flags): %v", err)
	}
	if !remoteMode {
		t.Fatal("remoteMode = false, want true")
	}
	if urlValue != "http://127.0.0.1:8080" || tokenValue != "secret" {
		t.Fatalf("resolved remote target = (%q, %q), want (%q, %q)", urlValue, tokenValue, "http://127.0.0.1:8080", "secret")
	}
}
