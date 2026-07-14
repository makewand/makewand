package config

import (
	"os"
	"strings"
)

// RemoteBaseURL returns the configured remote makewand server URL, if any.
func RemoteBaseURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("MAKEWAND_REMOTE_URL")), "/")
}

// RemoteToken returns the configured Bearer token for the remote makewand server.
func RemoteToken() string {
	return strings.TrimSpace(os.Getenv("MAKEWAND_REMOTE_TOKEN"))
}

// HasRemoteBackend reports whether a remote makewand server is configured.
func HasRemoteBackend() bool {
	return RemoteBaseURL() != "" && RemoteToken() != ""
}

// WorkspaceIDOverride returns an explicit workspace identifier for cross-device
// session continuity when the user wants to override git/path-derived identity.
func WorkspaceIDOverride() string {
	return strings.TrimSpace(os.Getenv("MAKEWAND_WORKSPACE_ID"))
}
