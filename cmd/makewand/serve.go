package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
	"github.com/makewand/makewand/internal/remotesession"
	"github.com/makewand/makewand/router"
	"github.com/makewand/makewand/serveradmin"
	"github.com/makewand/makewand/serveralerts"
	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverhttp"
	"github.com/makewand/makewand/servermetrics"
	"github.com/makewand/makewand/serverteam"
	"github.com/makewand/makewand/serverui"
	"github.com/makewand/makewand/serverusage"
	"github.com/spf13/cobra"
)

func serveCmd() *cobra.Command {
	var (
		listenAddr         string
		token              string
		dataDir            string
		authConfig         string
		auditPath          string
		usagePath          string
		alertWebhook       string
		alertState         string
		stateDBPath        string
		enableUsers        bool
		enableRegistration bool
		trustedProxies     []string
		unsafeNoTLS        bool
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start a personal makewand server for remote clients",
		Long:  "Expose your local makewand routing backend and centralized chat sessions so you can resume work from other computers.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Security: reject plaintext listening on non-loopback addresses
			if !unsafeNoTLS && !isLoopbackAddr(listenAddr) {
				return fmt.Errorf("refusing to listen on %q without TLS termination; use --unsafe-no-tls if absolutely necessary (not recommended)", listenAddr)
			}

			// Opening public registration implies the multi-user subsystem.
			if enableRegistration {
				enableUsers = true
			}

			trustedProxySet, err := serverauth.ParseTrustedProxies(trustedProxies)
			if err != nil {
				return err
			}

			cfg := loadConfigWithWarning()
			if !cfg.HasAnyModel() {
				return fmt.Errorf("no local AI models configured on this host; run 'makewand setup' first")
			}

			authz, bootstrapManager, err := loadServeAuthorizer(token, authConfig)
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
			stateDBPath = resolveServeStateDBPath(stateDBPath, dataDir)
			usagePath = resolveServeUsagePath(usagePath, dataDir, stateDBPath != "")
			alertWebhook = resolveServeAlertWebhook(alertWebhook)
			alertState = resolveServeAlertStatePath(alertState, dataDir)
			var auditLogger *serveraudit.JSONLLogger
			if strings.TrimSpace(auditPath) != "" {
				auditLogger, err = serveraudit.OpenJSONL(auditPath)
				if err != nil {
					return fmt.Errorf("open audit log: %w", err)
				}
				defer auditLogger.Close()
			}
			var usageLogger serverusage.Logger
			var usageJSONL *serverusage.JSONLLogger
			if strings.TrimSpace(usagePath) != "" {
				usageJSONL, err = serverusage.OpenJSONL(usagePath)
				if err != nil {
					return fmt.Errorf("open usage log: %w", err)
				}
				defer usageJSONL.Close()
				usageLogger = usageJSONL
			}

			// Honor --repo-trust end-to-end: the HTTP facade routes through the
			// Router's gated Chat/ChatWith pipeline, so constructing with the
			// resolved trust makes untrusted mode fail closed for served requests
			// (and known before the async quota refresh starts). resolvedRepoTrust
			// is parsed and validated once by the root PersistentPreRunE.
			rtr, err := serveRouter(cfg, resolvedRepoTrust)
			if err != nil {
				return fmt.Errorf("initialize model router: %w", err)
			}
			statsDir, err := config.ConfigDir()
			if err == nil {
				if loadErr := rtr.LoadStats(statsDir); loadErr != nil {
					fmt.Fprintf(os.Stderr, "warning: could not load routing stats: %v\n", loadErr)
				}
			}
			sessions := remotesession.NewStore(filepath.Join(dataDir, "sessions"))
			loginLimiter := serverauth.NewLoginRateLimiter(5, 15*time.Minute, 15*time.Minute)
			loginLimiter.SetTrustedProxies(trustedProxySet)
			registrationLimiter := serverauth.NewRegistrationRateLimiter(0, 0, 0, 0)
			registrationLimiter.SetTrustedProxies(trustedProxySet)
			var (
				tokenManager serverauth.TokenManager = bootstrapManager
				userStore    router.UserManager
				usageStore   serverusage.Reader
				teamStore    serverteam.Store
				sessionMgr   *serveradmin.SessionManager
			)
			if strings.TrimSpace(stateDBPath) != "" {
				sqliteTokens, err := serverauth.OpenSQLiteStore(stateDBPath)
				if err != nil {
					return fmt.Errorf("open sqlite token store: %w", err)
				}
				defer sqliteTokens.Close()
				tokenManager = sqliteTokens
				if authz != nil {
					authz = serverauth.NewMultiAuthorizer(authz, sqliteTokens)
				} else {
					authz = sqliteTokens
				}

				sqliteUsage, err := serverusage.OpenSQLiteStore(stateDBPath)
				if err != nil {
					return fmt.Errorf("open sqlite usage store: %w", err)
				}
				defer sqliteUsage.Close()
				usageStore = sqliteUsage
				usageLogger = combineUsageLoggers(usageLogger, sqliteUsage)

				sqliteTeams, err := serverteam.OpenSQLiteStore(stateDBPath)
				if err != nil {
					return fmt.Errorf("open sqlite team store: %w", err)
				}
				defer sqliteTeams.Close()
				teamStore = sqliteTeams

				if enableUsers {
					sqliteUsers, err := router.OpenSQLiteUserStore(stateDBPath)
					if err != nil {
						return fmt.Errorf("open sqlite user store: %w", err)
					}
					defer sqliteUsers.Close()
					userStore = sqliteUsers
				}
			}
			if usageStore == nil && strings.TrimSpace(usagePath) != "" {
				usageStore = serverusage.NewJSONLReader(usagePath)
			}
			if alertWebhook != "" {
				alertNotifier, err := serveralerts.OpenWebhookNotifier(alertWebhook, alertState, usageStore, teamStore)
				if err != nil {
					return fmt.Errorf("open alert notifier: %w", err)
				}
				usageLogger = combineUsageLoggers(usageLogger, alertNotifier)
			}
			if enableUsers && userStore == nil {
				userStore = router.NewUserStore(filepath.Join(dataDir, "users"))
			}
			if userStore != nil {
				secret, err := loadOrCreateAdminSessionSecret(filepath.Join(dataDir, "admin_session_secret"))
				if err != nil {
					return fmt.Errorf("load admin session secret: %w", err)
				}
				sessionMgr, err = serveradmin.NewSessionManager(userStore, secret, 12*time.Hour, loginLimiter)
				if err != nil {
					return fmt.Errorf("create admin session manager: %w", err)
				}
			}
			if authz == nil {
				return fmt.Errorf("serve requires --token/--auth-config or an enabled state DB token store")
			}
			usageDisplayPath := usagePath
			if usageDisplayPath == "" && strings.TrimSpace(stateDBPath) != "" && usageStore != nil {
				usageDisplayPath = "sqlite:" + stateDBPath
			}

			httpOpts := router.HTTPHandlerOptions{
				Authorizer:       authz,
				StatsDir:         statsDir,
				AuditLogger:      auditLogger,
				UsageLogger:      usageLogger,
				UsageReader:      usageStore,
				TeamStore:        teamStore,
				UserTokenManager: tokenManager,
				UserLoginLimiter: loginLimiter,
			}
			base := rtr.HTTPHandler(httpOpts)
			if userStore != nil {
				base = rtr.HTTPHandlerWithUsers(userStore, router.UserEndpointOptions{
					EnableRegistration:  enableRegistration,
					RegistrationLimiter: registrationLimiter,
				}, httpOpts)
			}
			mux := http.NewServeMux()
			mux.Handle("/v1/sessions/", remotesession.NewHandlerWithOptions(sessions, remotesession.HandlerOptions{
				Authorizer:  authz,
				AuditLogger: auditLogger,
			}))
			if tokenManager != nil {
				mux.Handle("/v1/admin/", serveradmin.NewHandler(serveradmin.HandlerOptions{
					Authorizer:   authz,
					TokenManager: tokenManager,
					AuditPath:    auditPath,
					AuditLogger:  auditLogger,
					UsagePath:    usageDisplayPath,
					UsageStore:   usageStore,
					UserStore:    userStore,
					TeamStore:    teamStore,
					SessionMgr:   sessionMgr,
				}))
			}
			metrics := servermetrics.NewRecorder()
			mux.Handle("/metrics", serveProtectedHandler(authz, serverauth.ScopeAdminMetricsRead, metrics.Handler()))
			mux.Handle("/admin", serverui.Handler())
			mux.Handle("/admin/", serverui.Handler())
			mux.Handle("/", base)
			handler := serverhttp.WithRequestID(metrics.Middleware(mux))

			server := &http.Server{
				Addr:              listenAddr,
				Handler:           handler,
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      10 * time.Minute,
				IdleTimeout:       2 * time.Minute,
			}

			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

			go func() {
				sig := <-sigChan
				fmt.Printf("\nReceived signal: %v\n", sig)

				shutdownCtx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
				defer cancel()

				fmt.Println("Shutting down server...")
				if err := server.Shutdown(shutdownCtx); err != nil && err != context.DeadlineExceeded {
					fmt.Fprintf(os.Stderr, "shutdown error: %v\n", err)
				}
			}()

			fmt.Printf("makewand server listening on %s\n", listenAddr)
			fmt.Printf("Sessions directory: %s\n", filepath.Join(dataDir, "sessions"))
			if auditLogger != nil {
				fmt.Printf("Audit log: %s\n", auditPath)
			}
			if usageDisplayPath != "" {
				fmt.Printf("Usage store: %s\n", usageDisplayPath)
			}
			if alertWebhook != "" {
				fmt.Printf("Alert webhook: configured\n")
			}
			if stateDBPath != "" {
				fmt.Printf("State DB: %s\n", stateDBPath)
			}
			if userStore != nil {
				if stateDBPath != "" {
					fmt.Printf("Users store: sqlite:%s\n", stateDBPath)
				} else {
					fmt.Printf("Users directory: %s\n", filepath.Join(dataDir, "users"))
				}
			}
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			fmt.Println("Server stopped gracefully")
			return nil
		},
	}

	cmd.Flags().StringVar(&listenAddr, "listen", "127.0.0.1:8080", "listen address for the personal makewand server")
	cmd.Flags().StringVar(&token, "token", "", "Bearer token required by remote clients")
	cmd.Flags().StringVar(&authConfig, "auth-config", "", "path to JSON auth config with scoped tokens")
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "directory used to persist remote sessions (default: ~/.config/makewand/server)")
	cmd.Flags().StringVar(&auditPath, "audit-log", "", "path to append-only JSONL audit log (default: <data-dir>/audit.jsonl when MAKEWAND_SERVER_AUDIT_LOG is set)")
	cmd.Flags().StringVar(&usagePath, "usage-log", "", "path to append-only JSONL usage ledger (disabled by default when SQLite state DB is enabled)")
	cmd.Flags().StringVar(&alertWebhook, "alert-webhook", "", "HTTP endpoint that receives budget alert webhooks")
	cmd.Flags().StringVar(&alertState, "alert-state", "", "path to persisted alert delivery state (default: <data-dir>/alert_state.json)")
	cmd.Flags().StringVar(&stateDBPath, "state-db", "", "path to SQLite state database for users, tokens, and usage (default: <data-dir>/state.db)")
	cmd.Flags().BoolVar(&enableUsers, "enable-users", false, "enable multi-user auth, login, and admin user management (does not open public registration)")
	cmd.Flags().BoolVar(&enableRegistration, "enable-registration", false, "allow public self-service account registration (implies --enable-users; new accounts require admin activation)")
	cmd.Flags().StringSliceVar(&trustedProxies, "trusted-proxy", nil, "CIDR or IP of reverse proxies whose X-Forwarded-For/X-Real-IP headers are trusted for rate limiting (repeatable)")
	cmd.Flags().BoolVar(&unsafeNoTLS, "unsafe-no-tls", false, "DANGER: allow plaintext listening on non-loopback addresses (only for testing behind a reverse proxy)")
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
		return nil, nil, nil
	}
	return serverauth.NewSingleTokenAuthorizer(token), nil, nil
}

func serveRouter(cfg *config.Config, trust model.RepoTrust) (*model.Router, error) {
	restore := temporarilyUnsetRemoteBackendEnv()
	defer restore()
	return model.NewRouterWithTrust(cfg, trust)
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

func resolveServeUsagePath(flagValue, dataDir string, hasStateDB bool) string {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue != "" {
		return flagValue
	}
	envValue := strings.TrimSpace(os.Getenv("MAKEWAND_SERVER_USAGE_LOG"))
	switch strings.ToLower(envValue) {
	case "":
		if hasStateDB {
			return ""
		}
		return filepath.Join(dataDir, "usage.jsonl")
	case "1", "true":
		return filepath.Join(dataDir, "usage.jsonl")
	case "0", "false", "off", "disabled":
		return ""
	default:
		return envValue
	}
}

func resolveServeStateDBPath(flagValue, dataDir string) string {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue != "" {
		return flagValue
	}
	if envValue := strings.TrimSpace(os.Getenv("MAKEWAND_SERVER_STATE_DB")); envValue != "" {
		if strings.EqualFold(envValue, "0") || strings.EqualFold(envValue, "false") || strings.EqualFold(envValue, "off") {
			return ""
		}
		return envValue
	}
	return filepath.Join(dataDir, "state.db")
}

func resolveServeAlertWebhook(flagValue string) string {
	if value := strings.TrimSpace(flagValue); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv("MAKEWAND_SERVER_ALERT_WEBHOOK"))
}

func resolveServeAlertStatePath(flagValue, dataDir string) string {
	if value := strings.TrimSpace(flagValue); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("MAKEWAND_SERVER_ALERT_STATE")); value != "" {
		return value
	}
	return filepath.Join(dataDir, "alert_state.json")
}

func loadOrCreateAdminSessionSecret(path string) ([]byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("admin session secret path is empty")
	}
	if data, err := os.ReadFile(path); err == nil {
		data = []byte(strings.TrimSpace(string(data)))
		decoded := make([]byte, base64.RawURLEncoding.DecodedLen(len(data)))
		n, decodeErr := base64.RawURLEncoding.Decode(decoded, data)
		if decodeErr == nil && n >= 32 {
			return decoded[:n], nil
		}
		if len(data) >= 32 {
			return data, nil
		}
		return nil, fmt.Errorf("existing admin session secret is too short")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	encoded := base64.RawURLEncoding.EncodeToString(secret)
	if err := os.WriteFile(path, []byte(encoded+"\n"), 0o600); err != nil {
		return nil, err
	}
	return secret, nil
}

type usageMultiLogger struct {
	items []serverusage.Logger
}

func combineUsageLoggers(items ...serverusage.Logger) serverusage.Logger {
	filtered := make([]serverusage.Logger, 0, len(items))
	for _, item := range items {
		if item != nil {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	if len(filtered) == 1 {
		return filtered[0]
	}
	return &usageMultiLogger{items: filtered}
}

func (m *usageMultiLogger) Log(entry serverusage.Entry) {
	if m == nil {
		return
	}
	for _, item := range m.items {
		item.Log(entry)
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
	//nolint:gosec // G705: JSON API error response (Content-Type application/json), not an HTML sink; values are %q-escaped.
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

// isLoopbackAddr reports whether addr binds only to a loopback interface.
// It parses host:port forms (including IPv6 "[::1]:8080" and the bare ":8080")
// with net.SplitHostPort and classifies the host via net.ParseIP.IsLoopback.
// An empty host (bind-all, e.g. ":8080" or "0.0.0.0:8080") is NOT loopback,
// keeping the fail-closed intent so plaintext never binds publicly by accident.
func isLoopbackAddr(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// No port present; treat the whole value as the host.
		host = addr
	}
	host = strings.TrimSpace(host)
	if host == "" {
		// Bind-all (":8080") — reachable off-host, so not loopback.
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
