package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/makewand/makewand/internal/backup"
	"github.com/makewand/makewand/internal/config"
	"github.com/spf13/cobra"
)

func stateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Back up and restore server state",
		Long: "Create and restore consistent backups of the makewand server state:\n" +
			"the SQLite state database (snapshotted with VACUUM INTO so it is safe to\n" +
			"run against a live server), the auth config, and optional JSONL ledgers.",
	}
	cmd.AddCommand(stateBackupCmd())
	cmd.AddCommand(stateRestoreCmd())
	return cmd
}

// resolveStatePaths fills defaults consistent with `serve`: the state DB and
// ledgers live under the data directory (default ~/.config/makewand/server).
func resolveStatePaths(dataDir, stateDB, authConfig string) (rDataDir, rStateDB, rAuthConfig string) {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		if cfgDir, err := config.ConfigDir(); err == nil {
			dataDir = filepath.Join(cfgDir, "server")
		}
	}
	stateDB = strings.TrimSpace(stateDB)
	if stateDB == "" && dataDir != "" {
		stateDB = filepath.Join(dataDir, "state.db")
	}
	return dataDir, stateDB, strings.TrimSpace(authConfig)
}

// existingExtras returns the JSONL ledgers present under dataDir, if any.
func existingExtras(dataDir string) []string {
	if dataDir == "" {
		return nil
	}
	var out []string
	for _, name := range []string{"audit.jsonl", "usage.jsonl", "alert_state.json"} {
		p := filepath.Join(dataDir, name)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func stateBackupCmd() *cobra.Command {
	var dataDir, stateDB, authConfig string

	cmd := &cobra.Command{
		Use:   "backup <archive.tar.gz>",
		Short: "Create a consistent state backup archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir, stateDB, authConfig = resolveStatePaths(dataDir, stateDB, authConfig)

			opts := backup.Options{AuthConfigPath: authConfig}
			if _, err := os.Stat(stateDB); err == nil {
				opts.StateDBPath = stateDB
			} else if stateDB != "" {
				fmt.Fprintf(os.Stderr, "note: state db %q not found; skipping\n", stateDB)
			}
			opts.ExtraFiles = existingExtras(dataDir)

			manifest, err := backup.Create(args[0], opts)
			if err != nil {
				return fmt.Errorf("create backup: %w", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Created backup: %s\n", args[0])
			for _, f := range manifest.Files {
				fmt.Fprintf(out, "  %s (%d bytes)\n", f.Name, f.Size)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "server data directory (default: ~/.config/makewand/server)")
	cmd.Flags().StringVar(&stateDB, "state-db", "", "path to the SQLite state database (default: <data-dir>/state.db)")
	cmd.Flags().StringVar(&authConfig, "auth-config", "", "path to the JSON auth config to include (e.g. /etc/makewand/server_auth.json)")
	return cmd
}

func stateRestoreCmd() *cobra.Command {
	var dataDir, stateDB, authConfig string
	var force bool

	cmd := &cobra.Command{
		Use:   "restore <archive.tar.gz>",
		Short: "Restore state from a backup archive",
		Long: "Restore verifies every file against the archive manifest checksum, then\n" +
			"atomically installs each component. Stop the server first: restoring a live\n" +
			"state database can corrupt it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir, stateDB, authConfig = resolveStatePaths(dataDir, stateDB, authConfig)
			if !force {
				return fmt.Errorf("restore overwrites live state; stop the server, then re-run with --force")
			}
			// Default the auth-config target so an archived server_auth.json is not
			// silently dropped. Pass --auth-config to restore it to its real home
			// (e.g. /etc/makewand/server_auth.json under the systemd layout).
			if authConfig == "" && dataDir != "" {
				authConfig = filepath.Join(dataDir, "server_auth.json")
			}
			opts := backup.Options{StateDBPath: stateDB, AuthConfigPath: authConfig}
			manifest, err := backup.Restore(args[0], opts)
			if err != nil {
				return fmt.Errorf("restore backup: %w", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Restored backup: %s\n", args[0])
			for _, f := range manifest.Files {
				fmt.Fprintf(out, "  %s\n", f.Name)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "server data directory (default: ~/.config/makewand/server)")
	cmd.Flags().StringVar(&stateDB, "state-db", "", "target path for the restored state database (default: <data-dir>/state.db)")
	cmd.Flags().StringVar(&authConfig, "auth-config", "", "target path for the restored auth config")
	cmd.Flags().BoolVar(&force, "force", false, "confirm the server is stopped and overwrite live state")
	return cmd
}
