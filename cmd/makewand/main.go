// Command makewand-server reads subscription quota for the Python engine and
// mw-hub. The TUI, server and other commands it once carried are archived at
// the tag archive/pre-trim-20261105.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/buildinfo"
	"github.com/makewand/makewand/internal/quota"
)

const usage = `usage: makewand-server [--repo-trust trusted|untrusted] quota [--json] [--timeout 30s]
       makewand-server --version`

type options struct {
	version    bool
	quota      bool
	jsonOutput bool
	timeout    time.Duration
	repoTrust  quota.RepoTrust
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "makewand-server: %v\n%s\n", err, usage)
		return 2
	}
	if opts.version {
		fmt.Fprintf(stdout, "makewand version %s\n", buildinfo.FormatVersion())
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	// In untrusted mode the snapshotter skips sources that exec a local CLI, so
	// this command never runs `agy models` inside an untrusted repository.
	snap := quota.NewDefaultQuotaSnapshotter(0).
		WithProviderFilter(providerEnabled).
		SetRepoTrust(opts.repoTrust).
		Refresh(ctx)
	if opts.jsonOutput {
		if err := printQuotaJSON(stdout, snap); err != nil {
			fmt.Fprintf(stderr, "makewand-server: %v\n", err)
			return 1
		}
		return 0
	}
	if opts.repoTrust == quota.RepoTrustUntrusted {
		fmt.Fprintln(stdout, "(untrusted-repo mode: local-CLI quota sources skipped; some pools show no data)")
	}
	printQuotaHuman(stdout, snap, quota.DefaultQuotaPolicy())
	return 0
}

func parseArgs(args []string) (options, error) {
	opts := options{timeout: 30 * time.Second}
	for index := 0; index < len(args); index++ {
		name, value, hasValue := strings.Cut(args[index], "=")
		takeValue := func() (string, error) {
			if hasValue {
				return value, nil
			}
			index++
			if index >= len(args) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			return args[index], nil
		}
		switch name {
		case "--version", "-v":
			opts.version = true
		case "quota":
			opts.quota = true
		case "--json":
			opts.jsonOutput = true
		case "--timeout":
			text, err := takeValue()
			if err != nil {
				return opts, err
			}
			parsed, err := time.ParseDuration(text)
			if err != nil || parsed <= 0 {
				return opts, fmt.Errorf("invalid --timeout %q", text)
			}
			opts.timeout = parsed
		case "--repo-trust":
			text, err := takeValue()
			if err != nil {
				return opts, err
			}
			switch strings.ToLower(strings.TrimSpace(text)) {
			case "trusted":
				opts.repoTrust = quota.RepoTrustTrusted
			case "untrusted":
				opts.repoTrust = quota.RepoTrustUntrusted
			default:
				return opts, fmt.Errorf("invalid --repo-trust %q", text)
			}
		default:
			return opts, fmt.Errorf("unknown argument %q", args[index])
		}
	}
	if !opts.version && !opts.quota {
		return opts, errors.New("nothing to do")
	}
	return opts, nil
}

// providerEnabled honors `makewand disable`. A missing or damaged config.json
// leaves every provider on; the quota read itself changes nothing.
func providerEnabled(name string) bool {
	dir := strings.TrimSpace(os.Getenv("MAKEWAND_CONFIG_DIR"))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return true
		}
		dir = filepath.Join(home, ".config", "makewand")
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json")) //nolint:gosec // G304: the user's own config directory
	if err != nil {
		return true
	}
	var config struct {
		EnabledProviders map[string]bool `json:"enabled_providers"`
	}
	if json.Unmarshal(data, &config) != nil {
		return true
	}
	// The quota source for agy reports itself as gemini.
	for _, candidate := range []string{name, map[string]string{"gemini": "agy"}[name]} {
		if enabled, set := config.EnabledProviders[candidate]; candidate != "" && set && !enabled {
			return false
		}
	}
	return true
}
