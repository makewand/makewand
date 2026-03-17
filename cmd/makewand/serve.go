package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
	"github.com/makewand/makewand/internal/remotesession"
	"github.com/makewand/makewand/router"
	"github.com/spf13/cobra"
)

func serveCmd() *cobra.Command {
	var (
		listenAddr string
		token      string
		dataDir    string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start a personal makewand server for remote clients",
		Long:  "Expose your local makewand routing backend and centralized chat sessions so you can resume work from other computers.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfigWithWarning()
			if !cfg.HasAnyModel() {
				return fmt.Errorf("no local AI models configured on this host; run 'makewand setup' first")
			}

			if strings.TrimSpace(token) == "" {
				token = strings.TrimSpace(os.Getenv("MAKEWAND_SERVER_TOKEN"))
			}
			if strings.TrimSpace(token) == "" {
				return fmt.Errorf("serve requires --token or MAKEWAND_SERVER_TOKEN")
			}

			if strings.TrimSpace(dataDir) == "" {
				cfgDir, err := config.ConfigDir()
				if err != nil {
					return err
				}
				dataDir = filepath.Join(cfgDir, "server")
			}

			rtr := serveRouter(cfg)
			sessions := remotesession.NewStore(filepath.Join(dataDir, "sessions"))

			base := rtr.HTTPHandler(router.HTTPHandlerOptions{BearerToken: token})
			mux := http.NewServeMux()
			mux.Handle("/v1/sessions/", remotesession.NewHandler(sessions, token))
			mux.Handle("/", base)

			server := &http.Server{
				Addr:    listenAddr,
				Handler: mux,
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			go func() {
				<-ctx.Done()
				_ = server.Close()
			}()

			fmt.Printf("makewand server listening on %s\n", listenAddr)
			fmt.Printf("Sessions directory: %s\n", filepath.Join(dataDir, "sessions"))
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&listenAddr, "listen", "127.0.0.1:8080", "listen address for the personal makewand server")
	cmd.Flags().StringVar(&token, "token", "", "Bearer token required by remote clients")
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "directory used to persist remote sessions (default: ~/.config/makewand/server)")
	return cmd
}

func serveRouter(cfg *config.Config) *model.Router {
	restore := temporarilyUnsetRemoteBackendEnv()
	defer restore()
	return model.NewRouter(cfg)
}

func temporarilyUnsetRemoteBackendEnv() func() {
	urlValue, hadURL := os.LookupEnv("MAKEWAND_REMOTE_URL")
	tokenValue, hadToken := os.LookupEnv("MAKEWAND_REMOTE_TOKEN")
	_ = os.Unsetenv("MAKEWAND_REMOTE_URL")
	_ = os.Unsetenv("MAKEWAND_REMOTE_TOKEN")
	return func() {
		if hadURL {
			_ = os.Setenv("MAKEWAND_REMOTE_URL", urlValue)
		}
		if hadToken {
			_ = os.Setenv("MAKEWAND_REMOTE_TOKEN", tokenValue)
		}
	}
}
