// Package proxyconfig resolves proxy defaults without changing the parent environment
// or reading provider credentials.
package proxyconfig

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Defaults struct {
	Default string `json:"default,omitempty"`
	NoProxy string `json:"no_proxy,omitempty"`
}

var Keys = []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"}

func Load() (Defaults, error) {
	dir := strings.TrimSpace(os.Getenv("MAKEWAND_CONFIG_DIR"))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Defaults{}, err
		}
		dir = filepath.Join(home, ".config", "makewand")
	}
	//nolint:gosec // G703: operator-selected local config directory, never a request-supplied path.
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if os.IsNotExist(err) {
		return Defaults{}, nil
	}
	if err != nil {
		return Defaults{}, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return Defaults{}, fmt.Errorf("proxy configuration must contain a JSON object")
	}
	var doc struct {
		Proxy Defaults `json:"proxy"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return Defaults{}, fmt.Errorf("parse proxy configuration: %w", err)
	}
	return doc.Proxy, nil
}

func values(env []string) map[string]string {
	result := map[string]string{}
	for _, entry := range env {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			result[k] = v
		}
	}
	return result
}

// Apply preserves even explicitly empty proxy variables. NO_PROXY defaults are
// filled only when a default proxy is injected, and never replace caller values.
func Apply(env []string, defaults Defaults) []string {
	current := values(env)
	for _, key := range Keys {
		if _, ok := current[key]; ok {
			return env
		}
	}
	proxy := defaults.Default
	if value, ok := os.LookupEnv("MAKEWAND_DEFAULT_PROXY"); ok {
		proxy = value
	}
	if proxy == "" {
		return env
	}
	result := append([]string(nil), env...)
	for _, key := range Keys {
		result = append(result, key+"="+proxy)
	}
	noProxy := defaults.NoProxy
	if value, ok := os.LookupEnv("MAKEWAND_DEFAULT_NO_PROXY"); ok {
		noProxy = value
	}
	if _, upper := current["NO_PROXY"]; !upper {
		if _, lower := current["no_proxy"]; !lower && noProxy != "" {
			result = append(result, "NO_PROXY="+noProxy, "no_proxy="+noProxy)
		}
	}
	return result
}

const GoogleHosts = "googleapis.com,google.com,cloudcode-pa.googleapis.com"

// Gemini keeps the legacy USE_PROXY switch's precedence when both are set.
func Gemini(env []string) []string {
	if flag("MAKEWAND_GEMINI_USE_PROXY") || !flag("MAKEWAND_GEMINI_BYPASS_PROXY") {
		return env
	}
	result := append([]string(nil), env...)
	current := values(env)
	hosts := []string{}
	for _, list := range []string{current["NO_PROXY"], current["no_proxy"], GoogleHosts} {
		for _, host := range strings.Split(list, ",") {
			host = strings.TrimSpace(host)
			if host == "" {
				continue
			}
			found := false
			for _, existing := range hosts {
				if existing == host {
					found = true
					break
				}
			}
			if !found {
				hosts = append(hosts, host)
			}
		}
	}
	value := strings.Join(hosts, ",")
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		replaced := false
		for i, entry := range result {
			if strings.HasPrefix(entry, key+"=") {
				result[i] = key + "=" + value
				replaced = true
			}
		}
		if !replaced {
			result = append(result, key+"="+value)
		}
	}

	return result
}

func flag(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Status never returns a raw URL (including malformed URLs with credentials).
func Status(env []string, defaults Defaults) map[string]string {
	current := values(env)
	for _, key := range Keys {
		if _, ok := current[key]; ok {
			return map[string]string{"source": "environment"}
		}
	}
	effective := values(Apply(env, defaults))
	if effective["HTTP_PROXY"] == "" {
		return map[string]string{"source": "none"}
	}
	source := "config_default"
	if _, ok := os.LookupEnv("MAKEWAND_DEFAULT_PROXY"); ok {
		source = "environment"
	}
	return map[string]string{"source": source}
}

// Proxy uses a fresh environment snapshot, avoiding net/http's process-wide
// ProxyFromEnvironment cache. ALL_PROXY also applies to HTTP API transports.
func Proxy(req *http.Request) (*url.URL, error) {
	defaults, err := Load()
	if err != nil {
		return nil, err
	}
	env := Apply(os.Environ(), defaults)
	if req.URL.Hostname() == "google.com" || strings.HasSuffix(req.URL.Hostname(), ".google.com") || req.URL.Hostname() == "googleapis.com" || strings.HasSuffix(req.URL.Hostname(), ".googleapis.com") {
		env = Gemini(env)
	}
	current := values(env)
	get := func(upper, lower string) string {
		if value, ok := current[lower]; ok {
			return value
		}
		return current[upper]
	}
	if bypass(req.URL, get("NO_PROXY", "no_proxy")) {
		return nil, nil
	}
	proxy := get("HTTP_PROXY", "http_proxy")
	if req.URL.Scheme == "https" {
		proxy = get("HTTPS_PROXY", "https_proxy")
	}
	if proxy == "" {
		proxy = get("ALL_PROXY", "all_proxy")
	}
	if proxy == "" {
		return nil, nil
	}
	if !strings.Contains(proxy, "://") {
		proxy = "http://" + proxy
	}
	parsed, err := url.Parse(proxy)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid proxy URL")
	}
	return parsed, nil
}

func bypass(target *url.URL, noProxy string) bool {
	host := strings.ToLower(target.Hostname())
	ip := net.ParseIP(host)
	port := target.Port()
	if port == "" {
		if target.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	for _, item := range strings.Split(noProxy, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" {
			continue
		}
		if item == "*" {
			return true
		}
		if _, network, err := net.ParseCIDR(item); err == nil && network.Contains(ip) {
			return true
		}
		name, itemPort, err := net.SplitHostPort(item)
		if err == nil {
			if itemPort != port {
				continue
			}
			item = name
		}
		item = strings.TrimPrefix(item, "*.")
		domain := strings.TrimPrefix(item, ".")
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}
