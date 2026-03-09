package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

func ollamaRemoteAllowed() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("MAKEWAND_OLLAMA_ALLOW_REMOTE")))
	return v == "1" || v == "true" || v == "yes"
}

func ollamaEndpointHost(baseURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("invalid ollama URL: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("ollama URL has empty host")
	}
	return u.Hostname(), nil
}

func isLocalOllamaHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

func ollamaSetupNotice(baseURL string) string {
	host, err := ollamaEndpointHost(baseURL)
	if err != nil {
		return err.Error()
	}
	if isLocalOllamaHost(host) {
		return "Remote Ollama hosts are blocked by default. Set MAKEWAND_OLLAMA_ALLOW_REMOTE=1 to allow a non-localhost endpoint."
	}
	if ollamaRemoteAllowed() {
		return fmt.Sprintf("Remote Ollama host %q is allowed via MAKEWAND_OLLAMA_ALLOW_REMOTE=1.", host)
	}
	return fmt.Sprintf("Remote Ollama host %q is currently blocked. Set MAKEWAND_OLLAMA_ALLOW_REMOTE=1 to allow it.", host)
}

func ollamaDoctorCheck(baseURL string) (doctorCheck, bool) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return doctorCheck{}, false
	}

	host, err := ollamaEndpointHost(baseURL)
	if err != nil {
		return doctorCheck{
			Name:    "ollama endpoint policy",
			Status:  doctorWarn,
			Details: err.Error(),
		}, true
	}

	if isLocalOllamaHost(host) {
		return doctorCheck{
			Name:    "ollama endpoint policy",
			Status:  doctorPass,
			Details: "localhost endpoint configured",
		}, true
	}

	if ollamaRemoteAllowed() {
		return doctorCheck{
			Name:    "ollama endpoint policy",
			Status:  doctorPass,
			Details: fmt.Sprintf("remote host %q allowed via MAKEWAND_OLLAMA_ALLOW_REMOTE=1", host),
		}, true
	}

	return doctorCheck{
		Name:    "ollama endpoint policy",
		Status:  doctorWarn,
		Details: fmt.Sprintf("remote host %q is blocked by default; set MAKEWAND_OLLAMA_ALLOW_REMOTE=1 to allow it", host),
	}, true
}
