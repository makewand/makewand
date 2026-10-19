package router

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAllCLIProvidersReceiveDefaultProxy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell CLI is Unix only")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, key := range cliAgyConfigDirEnv {
		t.Setenv(key, "")
	}
	for _, rel := range cliAgySettingsRels {
		path := filepath.Join(dir, ".gemini", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"useG1Credits":false}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MAKEWAND_CONFIG_DIR", dir)
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "NO_PROXY", "no_proxy", "MAKEWAND_DEFAULT_PROXY", "MAKEWAND_DEFAULT_NO_PROXY", "MAKEWAND_GEMINI_USE_PROXY", "MAKEWAND_GEMINI_BYPASS_PROXY"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"proxy":{"default":"http://proxy.example:8181","no_proxy":"internal"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake-cli")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n/usr/bin/env\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fake, 0700); err != nil {
		t.Fatal(err)
	}
	old := cliBwrapLookup
	cliBwrapLookup = func(string) (string, error) { return "", exec.ErrNotFound }
	defer func() { cliBwrapLookup = old }()
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "1")
	ctx := ContextWithUnsafeHostExecAuth(context.Background(), UnsafeHostExecAuth{Acknowledged: true, Source: "fixture", Audit: func(string, []string, string) {}})
	for _, provider := range []string{"claude", "codex", "agy", "gemini", "grok", "muse", "local"} {
		t.Run(provider, func(t *testing.T) {
			cmd := exec.Command(fake)
			cmd.Dir = dir
			wrapped, err := wrapCLICommandWithSandbox(ctx, provider, cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer cliAfterWait(wrapped)
			raw, err := wrapped.Output()
			if err != nil {
				t.Fatal(err)
			}
			got := string(raw)
			for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
				if !strings.Contains(got, key+"=http://proxy.example:8181\n") {
					t.Fatalf("missing %s: %s", key, got)
				}
			}
			if !strings.Contains(got, "NO_PROXY=internal\n") || !strings.Contains(got, "no_proxy=internal\n") {
				t.Fatalf("wrong NO_PROXY: %s", got)
			}
		})
	}
}

func TestAPIClientsHaveProxyPolicy(t *testing.T) {
	for _, client := range []*http.Client{newAPIClient(), newStreamClient()} {
		if client.Transport.(*http.Transport).Proxy == nil {
			t.Fatal("API transport lacks proxy policy")
		}
	}
}

func TestSandboxDefaultProxyIsPassedThrough(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("MAKEWAND_CONFIG_DIR", dir)
	t.Setenv("MAKEWAND_DEFAULT_PROXY", "http://proxy.example:8181")
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	oldLookup, oldProbe := cliBwrapLookup, cliBwrapProbe
	cliBwrapLookup = func(string) (string, error) { return "/fixture/bwrap", nil }
	cliBwrapProbe = nil
	defer func() { cliBwrapLookup, cliBwrapProbe = oldLookup, oldProbe }()
	cmd := exec.Command("fixture-cli")
	cmd.Dir = dir + "/workspace"
	if err := os.Mkdir(cmd.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "grok", cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer cliAfterWait(wrapped)
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if cliEnvValue(wrapped.Env, key) != "http://proxy.example:8181" {
			t.Fatalf("sandbox lost %s", key)
		}
	}
}
