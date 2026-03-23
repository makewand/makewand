package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/serverauth"
	"github.com/spf13/cobra"
)

func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage remote server auth tokens",
		Long:  "Create, list, and revoke tokens in a makewand server auth config.",
	}
	cmd.AddCommand(tokenListCmd())
	cmd.AddCommand(tokenIssueCmd())
	cmd.AddCommand(tokenRevokeCmd())
	return cmd
}

func tokenListCmd() *cobra.Command {
	var authConfigPath string
	var jsonOutput bool
	var remoteURL string
	var remoteToken string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List token rules from an auth config",
		RunE: func(cmd *cobra.Command, args []string) error {
			baseURL, bearer, remoteMode, err := resolveOptionalRemoteAdminTarget(remoteURL, remoteToken)
			if err != nil {
				return err
			}
			if remoteMode {
				var resp remoteTokenListResponse
				if err := adminGetJSON(baseURL, bearer, "/v1/admin/tokens", nil, &resp); err != nil {
					return err
				}
				if jsonOutput {
					data, err := json.MarshalIndent(resp.Data, "", "  ")
					if err != nil {
						return err
					}
					fmt.Println(string(data))
					return nil
				}
				printTokenRules("Remote admin API: "+baseURL, resp.Data)
				return nil
			}

			path, err := resolveManagedAuthConfigPath(authConfigPath)
			if err != nil {
				return err
			}
			cfg, err := serverauth.LoadConfigFile(path)
			if err != nil {
				return err
			}
			if jsonOutput {
				data, err := json.MarshalIndent(serverauth.SanitizedRules(cfg.Tokens), "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}
			printTokenRules("Auth config: "+path, serverauth.SanitizedRules(cfg.Tokens))
			return nil
		},
	}

	cmd.Flags().StringVar(&authConfigPath, "auth-config", "", "path to JSON auth config (defaults to MAKEWAND_SERVER_AUTH_CONFIG or ~/.config/makewand/server_auth.json)")
	cmd.Flags().StringVar(&remoteURL, "remote-url", "", "remote makewand admin base URL")
	cmd.Flags().StringVar(&remoteToken, "remote-token", "", "remote makewand admin Bearer token")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output token rules as JSON")
	return cmd
}

func tokenIssueCmd() *cobra.Command {
	var (
		authConfigPath     string
		tokenID            string
		description        string
		tokenValue         string
		scopesFlag         string
		workspacePrefixes  string
		allowedProviders   string
		allowedModes       string
		expiresAtFlag      string
		maxRequestsPerHour int
		maxRequestsPerDay  int
		maxCostUSDPerDay   float64
		maxCostUSDPerMonth float64
		jsonOutput         bool
		remoteURL          string
		remoteToken        string
	)

	cmd := &cobra.Command{
		Use:   "issue",
		Short: "Create and persist a new auth token",
		RunE: func(cmd *cobra.Command, args []string) error {
			expiresAt, err := parseOptionalRFC3339(expiresAtFlag)
			if err != nil {
				return err
			}
			rule := serverauth.TokenRule{
				ID:                 strings.TrimSpace(tokenID),
				Token:              tokenValue,
				Description:        strings.TrimSpace(description),
				Scopes:             parseCSVOrDefault(scopesFlag, serverauth.AllClientScopes()),
				WorkspacePrefixes:  parseCSV(workspacePrefixes),
				AllowedProviders:   parseCSV(allowedProviders),
				AllowedModes:       parseCSV(allowedModes),
				ExpiresAt:          expiresAt,
				MaxRequestsPerHour: maxRequestsPerHour,
				MaxRequestsPerDay:  maxRequestsPerDay,
				MaxCostUSDPerDay:   maxCostUSDPerDay,
				MaxCostUSDPerMonth: maxCostUSDPerMonth,
			}
			baseURL, bearer, remoteMode, err := resolveOptionalRemoteAdminTarget(remoteURL, remoteToken)
			if err != nil {
				return err
			}
			if remoteMode {
				var resp remoteTokenIssueResponse
				if err := adminPostJSON(baseURL, bearer, "/v1/admin/tokens", rule, &resp); err != nil {
					return err
				}
				if jsonOutput {
					data, err := json.MarshalIndent(map[string]any{
						"remote_url": baseURL,
						"token_id":   resp.TokenID,
						"token":      resp.Token,
						"rule":       resp.Rule,
					}, "", "  ")
					if err != nil {
						return err
					}
					fmt.Println(string(data))
					return nil
				}
				fmt.Printf("Remote admin API: %s\n", baseURL)
				fmt.Printf("Token ID: %s\n", resp.TokenID)
				fmt.Printf("Token: %s\n", resp.Token)
				return nil
			}

			path, err := resolveManagedAuthConfigPath(authConfigPath)
			if err != nil {
				return err
			}
			cfg, err := loadOrCreateAuthConfig(path)
			if err != nil {
				return err
			}
			if strings.TrimSpace(tokenValue) == "" {
				tokenValue, err = serverauth.GenerateToken()
				if err != nil {
					return err
				}
				rule.Token = tokenValue
			}
			finalID, err := serverauth.IssueTokenRule(&cfg, rule)
			if err != nil {
				return err
			}
			if err := serverauth.SaveConfigFile(path, cfg); err != nil {
				return err
			}

			if jsonOutput {
				payload := map[string]any{
					"auth_config": path,
					"token_id":    finalID,
					"token":       tokenValue,
				}
				data, err := json.MarshalIndent(payload, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}

			fmt.Printf("Auth config: %s\n", path)
			fmt.Printf("Token ID: %s\n", finalID)
			fmt.Printf("Token: %s\n", tokenValue)
			return nil
		},
	}

	cmd.Flags().StringVar(&authConfigPath, "auth-config", "", "path to JSON auth config (defaults to MAKEWAND_SERVER_AUTH_CONFIG or ~/.config/makewand/server_auth.json)")
	cmd.Flags().StringVar(&tokenID, "id", "", "stable token identifier")
	cmd.Flags().StringVar(&description, "description", "", "human-readable token description")
	cmd.Flags().StringVar(&tokenValue, "token", "", "explicit bearer token value (default: generated)")
	cmd.Flags().StringVar(&scopesFlag, "scopes", "", "comma-separated scopes (default: all scopes)")
	cmd.Flags().StringVar(&workspacePrefixes, "workspace-prefixes", "", "comma-separated allowed workspace prefixes")
	cmd.Flags().StringVar(&allowedProviders, "allowed-providers", "", "comma-separated allowed providers")
	cmd.Flags().StringVar(&allowedModes, "allowed-modes", "", "comma-separated allowed modes")
	cmd.Flags().StringVar(&expiresAtFlag, "expires-at", "", "RFC3339 expiry timestamp")
	cmd.Flags().IntVar(&maxRequestsPerHour, "max-requests-per-hour", 0, "maximum requests per hour (0 = unlimited)")
	cmd.Flags().IntVar(&maxRequestsPerDay, "max-requests-per-day", 0, "maximum requests per day (0 = unlimited)")
	cmd.Flags().Float64Var(&maxCostUSDPerDay, "max-cost-usd-per-day", 0, "maximum realized cost per day in USD (0 = unlimited)")
	cmd.Flags().Float64Var(&maxCostUSDPerMonth, "max-cost-usd-per-month", 0, "maximum realized cost per month in USD (0 = unlimited)")
	cmd.Flags().StringVar(&remoteURL, "remote-url", "", "remote makewand admin base URL")
	cmd.Flags().StringVar(&remoteToken, "remote-token", "", "remote makewand admin Bearer token")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output issued token details as JSON")
	return cmd
}

func tokenRevokeCmd() *cobra.Command {
	var authConfigPath string
	var remoteURL string
	var remoteToken string

	cmd := &cobra.Command{
		Use:   "revoke <token-id>",
		Short: "Revoke a token by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			baseURL, bearer, remoteMode, err := resolveOptionalRemoteAdminTarget(remoteURL, remoteToken)
			if err != nil {
				return err
			}
			if remoteMode {
				endpoint := "/v1/admin/tokens/" + url.PathEscape(args[0]) + "/revoke"
				if err := adminPostJSON(baseURL, bearer, endpoint, nil, &struct{}{}); err != nil {
					return err
				}
				fmt.Printf("Revoked token %s via %s\n", args[0], baseURL)
				return nil
			}
			path, err := resolveManagedAuthConfigPath(authConfigPath)
			if err != nil {
				return err
			}
			cfg, err := serverauth.LoadConfigFile(path)
			if err != nil {
				return err
			}
			if err := serverauth.RevokeTokenRule(&cfg, args[0]); err != nil {
				return err
			}
			if err := serverauth.SaveConfigFile(path, cfg); err != nil {
				return err
			}
			fmt.Printf("Revoked token %s in %s\n", args[0], path)
			return nil
		},
	}

	cmd.Flags().StringVar(&authConfigPath, "auth-config", "", "path to JSON auth config (defaults to MAKEWAND_SERVER_AUTH_CONFIG or ~/.config/makewand/server_auth.json)")
	cmd.Flags().StringVar(&remoteURL, "remote-url", "", "remote makewand admin base URL")
	cmd.Flags().StringVar(&remoteToken, "remote-token", "", "remote makewand admin Bearer token")
	return cmd
}

func resolveManagedAuthConfigPath(flagValue string) (string, error) {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue != "" {
		return flagValue, nil
	}
	if env := strings.TrimSpace(os.Getenv("MAKEWAND_SERVER_AUTH_CONFIG")); env != "" {
		return env, nil
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "server_auth.json"), nil
}

func loadOrCreateAuthConfig(path string) (serverauth.Config, error) {
	cfg, err := serverauth.LoadConfigFile(path)
	if err == nil {
		return cfg, nil
	}
	if os.IsNotExist(err) {
		return serverauth.Config{}, nil
	}
	return serverauth.Config{}, err
}

func derivedTokenID(token string) string {
	return serverauth.DerivedTokenID(token)
}

func printTokenRules(header string, rules []serverauth.TokenRuleView) {
	if len(rules) == 0 {
		fmt.Printf("No tokens in %s\n", strings.TrimSpace(header))
		return
	}
	fmt.Println(header)
	for _, rule := range rules {
		fmt.Printf("- %s\n", rule.ID)
		if rule.Description != "" {
			fmt.Printf("  description: %s\n", rule.Description)
		}
		fmt.Printf("  scopes: %s\n", strings.Join(rule.Scopes, ", "))
		if len(rule.WorkspacePrefixes) > 0 {
			fmt.Printf("  workspaces: %s\n", strings.Join(rule.WorkspacePrefixes, ", "))
		}
		if len(rule.AllowedProviders) > 0 {
			fmt.Printf("  providers: %s\n", strings.Join(rule.AllowedProviders, ", "))
		}
		if len(rule.AllowedModes) > 0 {
			fmt.Printf("  modes: %s\n", strings.Join(rule.AllowedModes, ", "))
		}
		if rule.MaxRequestsPerHour > 0 {
			fmt.Printf("  max requests/hour: %d\n", rule.MaxRequestsPerHour)
		}
		if rule.MaxRequestsPerDay > 0 {
			fmt.Printf("  max requests/day: %d\n", rule.MaxRequestsPerDay)
		}
		if rule.MaxCostUSDPerDay > 0 {
			fmt.Printf("  max cost/day: $%.4f\n", rule.MaxCostUSDPerDay)
		}
		if rule.MaxCostUSDPerMonth > 0 {
			fmt.Printf("  max cost/month: $%.4f\n", rule.MaxCostUSDPerMonth)
		}
		if !rule.ExpiresAt.IsZero() {
			fmt.Printf("  expires at: %s\n", rule.ExpiresAt.UTC().Format(time.RFC3339))
		}
		if rule.Revoked {
			fmt.Println("  revoked: true")
		}
	}
}

func parseCSV(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseCSVOrDefault(raw string, fallback []string) []string {
	values := parseCSV(raw)
	if len(values) > 0 {
		return values
	}
	out := make([]string, len(fallback))
	copy(out, fallback)
	return out
}

func parseOptionalRFC3339(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse expires-at: %w", err)
	}
	return value.UTC(), nil
}
