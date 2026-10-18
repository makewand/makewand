package proxyconfig

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range append(append([]string(nil), Keys...), "NO_PROXY", "no_proxy", "MAKEWAND_DEFAULT_PROXY", "MAKEWAND_DEFAULT_NO_PROXY", "MAKEWAND_GEMINI_USE_PROXY", "MAKEWAND_GEMINI_BYPASS_PROXY") {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

func TestSharedProxyContract(t *testing.T) {
	data, err := os.ReadFile("../../tests/fixtures/proxy_contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name                     string
		Defaults                 Defaults
		Env, Overrides, Expected map[string]string
		Source                   string
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			clearEnv(t)
			var env []string
			for k, v := range tc.Env {
				env = append(env, k+"="+v)
				t.Setenv(k, v)
			}
			for k, v := range tc.Overrides {
				t.Setenv(k, v)
			}
			if got := values(Apply(env, tc.Defaults)); !reflect.DeepEqual(got, tc.Expected) {
				t.Fatalf("got %#v, want %#v", got, tc.Expected)
			}
			if got := Status(env, tc.Defaults)["source"]; got != tc.Source {
				t.Fatalf("source=%s, want %s", got, tc.Source)
			}
		})
	}
}

func TestProxyTransport(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"proxy":{"default":"http://user:secret@proxy.example:8181","no_proxy":".internal,10.0.0.0/8,port.example:8443,127.0.0.1"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"https://api.example/v1", "http://api.example/v1"} {
		req, requestErr := http.NewRequest("GET", target, nil)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		got, err := Proxy(req)
		if err != nil || got == nil || got.Host != "proxy.example:8181" {
			t.Fatalf("proxy=%v err=%v", got, err)
		}
	}
	for _, target := range []string{"https://host.internal", "http://10.1.2.3", "https://port.example:8443", "http://127.0.0.1"} {
		req, requestErr := http.NewRequest("GET", target, nil)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		got, err := Proxy(req)
		if err != nil || got != nil {
			t.Fatalf("%s proxy=%v err=%v", target, got, err)
		}
	}
	req, _ := http.NewRequest("GET", "https://generativelanguage.googleapis.com", nil)
	if got, _ := Proxy(req); got == nil {
		t.Fatal("Google unexpectedly bypassed")
	}
	t.Setenv("MAKEWAND_GEMINI_BYPASS_PROXY", "1")
	if got, _ := Proxy(req); got != nil {
		t.Fatal("Google did not bypass")
	}
	t.Setenv("MAKEWAND_GEMINI_USE_PROXY", "1")
	if got, _ := Proxy(req); got == nil {
		t.Fatal("legacy USE_PROXY precedence lost")
	}
	// Reporting no URL is deliberate: even malformed userinfo cannot leak.
	if got := Status(nil, Defaults{Default: "http://user:secret@proxy.example"}); len(got) != 1 || got["source"] != "config_default" {
		t.Fatalf("unsafe status: %v", got)
	}
}

func TestLoadRejectsInvalidProxyConfiguration(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", dir)
	for _, data := range []string{`null`, `[]`, `{"proxy":[]}`, `{"proxy":"bad"}`, `{"proxy":{"default":123}}`, `{"proxy":{"no_proxy":false}}`} {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestGeminiBypassMergesCasesWithoutMutatingCaller(t *testing.T) {
	clearEnv(t)
	t.Setenv("MAKEWAND_GEMINI_BYPASS_PROXY", "1")
	original := []string{"NO_PROXY=upper.example", "no_proxy=lower.example"}
	got := values(Gemini(original))
	want := "upper.example,lower.example," + GoogleHosts
	if got["NO_PROXY"] != want || got["no_proxy"] != want {
		t.Fatalf("inconsistent bypass: %v", got)
	}
	if original[0] != "NO_PROXY=upper.example" {
		t.Fatal("caller env mutated")
	}
	if !reflect.DeepEqual(values(Gemini(Gemini(original))), got) {
		t.Fatal("bypass is not idempotent")
	}
}
