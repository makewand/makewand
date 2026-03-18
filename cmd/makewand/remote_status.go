package main

import (
	"fmt"
	"strings"

	"github.com/makewand/makewand/internal/config"
)

func printRemoteBackendStatus() {
	fmt.Println("Remote backend:")

	url := config.RemoteBaseURL()
	token := config.RemoteToken()

	switch {
	case url != "" && token != "":
		fmt.Printf("  [x] Remote backend configured -> %s\n", url)
	case url != "" || token != "":
		missing := missingRemoteBackendEnvVars(url, token)
		fmt.Printf("  [!] Remote backend partially configured (missing %s)\n", strings.Join(missing, ", "))
	default:
		fmt.Println("  [ ] Remote backend: not configured")
	}

	if override := config.WorkspaceIDOverride(); override != "" {
		fmt.Printf("  Workspace ID override: %s\n", override)
	}
	fmt.Println()
}

func missingRemoteBackendEnvVars(url, token string) []string {
	missing := make([]string, 0, 2)
	if strings.TrimSpace(url) == "" {
		missing = append(missing, "MAKEWAND_REMOTE_URL")
	}
	if strings.TrimSpace(token) == "" {
		missing = append(missing, "MAKEWAND_REMOTE_TOKEN")
	}
	return missing
}
