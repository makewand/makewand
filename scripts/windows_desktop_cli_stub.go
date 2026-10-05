//go:build ignore

// An offline frontend fixture. It never runs a model or records prompt arguments.
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	args, category, status := os.Args[1:], "blocked_invocation", 97
	if len(args) == 1 && args[0] == "--version" {
		category, status = "version", 0
		fmt.Println("codex-cli 0.0.0-offline-frontend-fixture")
	} else if len(args) == 2 && args[0] == "auth" && args[1] == "status" {
		category, status = "synthetic_auth_status", 0
		fmt.Println("Logged in using ChatGPT (synthetic offline frontend fixture)")
	}
	path := os.Getenv("MAKEWAND_DESKTOP_STUB_LOG")
	if path == "" {
		os.Exit(98)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(98)
	}
	if err := json.NewEncoder(file).Encode(map[string]any{"category": category, "exit": status}); err != nil {
		file.Close()
		os.Exit(98)
	}
	if err := file.Close(); err != nil {
		os.Exit(98)
	}
	os.Exit(status)
}
