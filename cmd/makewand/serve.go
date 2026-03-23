package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
	"github.com/makewand/makewand/internal/remotesession"
	"github.com/makewand/makewand/router"
	"github.com/makewand/makewand/serveradmin"
	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverhttp"
	"github.com/makewand/makewand/servermetrics"
	"github.com/makewand/makewand/serverusage"
	"github.com/spf13/cobra"
)

func serveCmd() *cobra.Command {
	var (
		listenAddr  string
		token       string
		dataDir     string
		authConfig  string
		auditPath   string
		usagePath   string
		enableUsers bool
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

			authz, manager, err := loadServeAuthorizer(token, authConfig)
			if err != nil {
				return err
			}

			if strings.TrimSpace(dataDir) == "" {
				cfgDir, err := config.ConfigDir()
				if err != nil {
					return err
				}
				dataDir = filepath.Join(cfgDir, "server")
			}
			auditPath = resolveServeAuditPath(auditPath, dataDir)
			usagePath = resolveServeUsagePath(usagePath, dataDir)
			var auditLogger *serveraudit.JSONLLogger
			if strings.TrimSpace(auditPath) != "" {
				auditLogger, err = serveraudit.OpenJSONL(auditPath)
				if err != nil {
					return fmt.Errorf("open audit log: %w", err)
				}
				defer auditLogger.Close()
			}
			var usageLogger *serverusage.JSONLLogger
			if strings.TrimSpace(usagePath) != "" {
				usageLogger, err = serverusage.OpenJSONL(usagePath)
				if err != nil {
					return fmt.Errorf("open usage log: %w", err)
				}
				defer usageLogger.Close()
			}

			rtr := serveRouter(cfg)
			statsDir, err := config.ConfigDir()
			if err == nil {
				if loadErr := rtr.LoadStats(statsDir); loadErr != nil {
					fmt.Fprintf(os.Stderr, "warning: could not load routing stats: %v\n", loadErr)
				}
			}
			sessions := remotesession.NewStore(filepath.Join(dataDir, "sessions"))
			var userStore *router.UserStore
			if enableUsers {
				userStore = router.NewUserStore(filepath.Join(dataDir, "users"))
			}

			httpOpts := router.HTTPHandlerOptions{
				Authorizer:  authz,
				StatsDir:    statsDir,
				AuditLogger: auditLogger,
				UsageLogger: usageLogger,
			}
			base := rtr.HTTPHandler(httpOpts)
			if userStore != nil {
				base = rtr.HTTPHandlerWithUsers(userStore, httpOpts)
			}
			mux := http.NewServeMux()
			mux.Handle("/v1/sessions/", remotesession.NewHandlerWithOptions(sessions, remotesession.HandlerOptions{
				Authorizer:  authz,
				AuditLogger: auditLogger,
			}))
			if manager != nil {
				mux.Handle("/v1/admin/", serveradmin.NewHandler(serveradmin.HandlerOptions{
					Authorizer:   authz,
					TokenManager: manager,
					AuditPath:    auditPath,
					AuditLogger:  auditLogger,
					UsagePath:    usagePath,
					UserStore:    userStore,
				}))
			}
			metrics := servermetrics.NewRecorder()
			mux.Handle("/metrics", serveProtectedHandler(authz, serverauth.ScopeAdminMetricsRead, metrics.Handler()))
			mux.Handle("/", base)
			handler := serverhttp.WithRequestID(metrics.Middleware(mux))

			server := &http.Server{
				Addr:    listenAddr,
				Handler: handler,
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			go func() {
				<-ctx.Done()
				_ = server.Close()
			}()

			fmt.Printf("makewand server listening on %s\n", listenAddr)
			fmt.Printf("Sessions directory: %s\n", filepath.Join(dataDir, "sessions"))
			if auditLogger != nil {
				fmt.Printf("Audit log: %s\n", auditPath)
			}
			if usageLogger != nil {
				fmt.Printf("Usage log: %s\n", usagePath)
			}
			if userStore != nil {
				fmt.Printf("Users directory: %s\n", filepath.Join(dataDir, "users"))
			}
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&listenAddr, "listen", "127.0.0.1:8080", "listen address for the personal makewand server")
	cmd.Flags().StringVar(&token, "token", "", "Bearer token required by remote clients")
	cmd.Flags().StringVar(&authConfig, "auth-config", "", "path to JSON auth config with scoped tokens")
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "directory used to persist remote sessions (default: ~/.config/makewand/server)")
	cmd.Flags().StringVar(&auditPath, "audit-log", "", "path to append-only JSONL audit log (default: <data-dir>/audit.jsonl when MAKEWAND_SERVER_AUDIT_LOG is set)")
	cmd.Flags().StringVar(&usagePath, "usage-log", "", "path to append-only JSONL usage ledger (default: <data-dir>/usage.jsonl)")
	cmd.Flags().BoolVar(&enableUsers, "enable-users", false, "enable multi-user registration and admin user management")
	return cmd
}

func loadServeAuthorizer(token, authConfig string) (serverauth.RequestAuthorizer, *serverauth.Manager, error) {
	authConfig = strings.TrimSpace(authConfig)
	if authConfig == "" {
		authConfig = strings.TrimSpace(os.Getenv("MAKEWAND_SERVER_AUTH_CONFIG"))
	}
	if authConfig != "" {
		manager, err := serverauth.LoadManager(authConfig)
		if err != nil {
			return nil, nil, fmt.Errorf("load auth config: %w", err)
		}
		return manager, manager, nil
	}

	token = strings.TrimSpace(token)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("MAKEWAND_SERVER_TOKEN"))
	}
	if token == "" {
		return nil, nil, fmt.Errorf("serve requires --token, MAKEWAND_SERVER_TOKEN, --auth-config, or MAKEWAND_SERVER_AUTH_CONFIG")
	}
	return serverauth.NewSingleTokenAuthorizer(token), nil, nil
}

func serveRouter(cfg *config.Config) *model.Router {
	restore := temporarilyUnsetRemoteBackendEnv()
	defer restore()
	return model.NewRouter(cfg)
}

func resolveServeAuditPath(flagValue, dataDir string) string {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue != "" {
		return flagValue
	}
	envValue := strings.TrimSpace(os.Getenv("MAKEWAND_SERVER_AUDIT_LOG"))
	if envValue == "" {
		return ""
	}
	if strings.EqualFold(envValue, "1") || strings.EqualFold(envValue, "true") {
		return filepath.Join(dataDir, "audit.jsonl")
	}
	return envValue
}

func resolveServeUsagePath(flagValue, dataDir string) string {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue != "" {
		return flagValue
	}
	envValue := strings.TrimSpace(os.Getenv("MAKEWAND_SERVER_USAGE_LOG"))
	switch strings.ToLower(envValue) {
	case "", "1", "true":
		return filepath.Join(dataDir, "usage.jsonl")
	case "0", "false", "off", "disabled":
		return ""
	default:
		return envValue
	}
}

func serveProtectedHandler(authz serverauth.RequestAuthorizer, scope string, next http.Handler) http.Handler {
	if authz == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		grant, ok := authz.AuthenticateRequest(req)
		if !ok {
			writeServeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing Bearer token")
			return
		}
		if err := grant.CheckAndConsumeRequestAt(time.Now()); err != nil {
			writeServeError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
			return
		}
		if !grant.AllowsScope(scope) {
			writeServeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("token does not allow scope %q", scope))
			return
		}
		next.ServeHTTP(w, req)
	})
}

func writeServeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"message":%q,"type":"error","code":%q}}`, message, code)
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
