package main

import (
	"encoding/json"
	"fmt"
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

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List token rules from an auth config",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveManagedAuthConfigPath(authConfigPath)
			if err != nil {
				return err
			}
			cfg, err := serverauth.LoadConfigFile(path)
			if err != nil {
				return err
			}
			if jsonOutput {
				data, err := json.MarshalIndent(sanitizedTokenRules(cfg.Tokens), "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}
			if len(cfg.Tokens) == 0 {
				fmt.Printf("No tokens in %s\n", path)
				return nil
			}
			fmt.Printf("Auth config: %s\n", path)
			for _, rule := range cfg.Tokens {
				id := strings.TrimSpace(rule.ID)
				if id == "" {
					id = derivedTokenID(rule.Token)
				}
				fmt.Printf("- %s\n", id)
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
				if !rule.ExpiresAt.IsZero() {
					fmt.Printf("  expires at: %s\n", rule.ExpiresAt.UTC().Format(time.RFC3339))
				}
				if rule.Revoked {
					fmt.Println("  revoked: true")
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&authConfigPath, "auth-config", "", "path to JSON auth config (defaults to MAKEWAND_SERVER_AUTH_CONFIG or ~/.config/makewand/server_auth.json)")
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
		jsonOutput         bool
	)

	cmd := &cobra.Command{
		Use:   "issue",
		Short: "Create and persist a new auth token",
		RunE: func(cmd *cobra.Command, args []string) error {
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
			}
			expiresAt, err := parseOptionalRFC3339(expiresAtFlag)
			if err != nil {
				return err
			}
			rule := serverauth.TokenRule{
				ID:                 strings.TrimSpace(tokenID),
				Token:              tokenValue,
				Description:        strings.TrimSpace(description),
				Scopes:             parseCSVOrDefault(scopesFlag, serverauth.AllScopes()),
				WorkspacePrefixes:  parseCSV(workspacePrefixes),
				AllowedProviders:   parseCSV(allowedProviders),
				AllowedModes:       parseCSV(allowedModes),
				ExpiresAt:          expiresAt,
				MaxRequestsPerHour: maxRequestsPerHour,
				MaxRequestsPerDay:  maxRequestsPerDay,
			}
			finalID, err := issueTokenRule(&cfg, rule)
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
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output issued token details as JSON")
	return cmd
}

func tokenRevokeCmd() *cobra.Command {
	var authConfigPath string

	cmd := &cobra.Command{
		Use:   "revoke <token-id>",
		Short: "Revoke a token by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveManagedAuthConfigPath(authConfigPath)
			if err != nil {
				return err
			}
			cfg, err := serverauth.LoadConfigFile(path)
			if err != nil {
				return err
			}
			if err := revokeTokenRule(&cfg, args[0]); err != nil {
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

func issueTokenRule(cfg *serverauth.Config, rule serverauth.TokenRule) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("auth config is nil")
	}
	if strings.TrimSpace(rule.Token) == "" {
		return "", fmt.Errorf("token value is empty")
	}
	cfg.Tokens = append(cfg.Tokens, rule)
	authz, err := serverauth.NewAuthorizer(*cfg)
	if err != nil {
		cfg.Tokens = cfg.Tokens[:len(cfg.Tokens)-1]
		return "", err
	}
	grant, ok := authz.AuthenticateHeader("Bearer " + rule.Token)
	if !ok {
		cfg.Tokens = cfg.Tokens[:len(cfg.Tokens)-1]
		return "", fmt.Errorf("failed to authenticate newly issued token")
	}
	finalID := grant.TokenID()
	if cfg.Tokens[len(cfg.Tokens)-1].ID == "" {
		cfg.Tokens[len(cfg.Tokens)-1].ID = finalID
	}
	return finalID, nil
}

func revokeTokenRule(cfg *serverauth.Config, tokenID string) error {
	tokenID = strings.TrimSpace(tokenID)
	if cfg == nil || tokenID == "" {
		return fmt.Errorf("token id is required")
	}
	for i := range cfg.Tokens {
		id := strings.TrimSpace(cfg.Tokens[i].ID)
		if id == "" {
			id = derivedTokenID(cfg.Tokens[i].Token)
		}
		if id != tokenID {
			continue
		}
		cfg.Tokens[i].Revoked = true
		if cfg.Tokens[i].ID == "" {
			cfg.Tokens[i].ID = id
		}
		return nil
	}
	return fmt.Errorf("token %q not found", tokenID)
}

func derivedTokenID(token string) string {
	authz := serverauth.NewSingleTokenAuthorizer(token)
	grant, ok := authz.AuthenticateHeader("Bearer " + token)
	if !ok {
		return ""
	}
	return grant.TokenID()
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

type tokenRuleView struct {
	ID                 string    `json:"id,omitempty"`
	Description        string    `json:"description,omitempty"`
	Scopes             []string  `json:"scopes"`
	WorkspacePrefixes  []string  `json:"workspace_prefixes,omitempty"`
	AllowedProviders   []string  `json:"allowed_providers,omitempty"`
	AllowedModes       []string  `json:"allowed_modes,omitempty"`
	ExpiresAt          time.Time `json:"expires_at,omitempty"`
	Revoked            bool      `json:"revoked,omitempty"`
	MaxRequestsPerHour int       `json:"max_requests_per_hour,omitempty"`
	MaxRequestsPerDay  int       `json:"max_requests_per_day,omitempty"`
}

func sanitizedTokenRules(rules []serverauth.TokenRule) []tokenRuleView {
	if len(rules) == 0 {
		return nil
	}
	views := make([]tokenRuleView, 0, len(rules))
	for _, rule := range rules {
		id := strings.TrimSpace(rule.ID)
		if id == "" {
			id = derivedTokenID(rule.Token)
		}
		views = append(views, tokenRuleView{
			ID:                 id,
			Description:        rule.Description,
			Scopes:             append([]string(nil), rule.Scopes...),
			WorkspacePrefixes:  append([]string(nil), rule.WorkspacePrefixes...),
			AllowedProviders:   append([]string(nil), rule.AllowedProviders...),
			AllowedModes:       append([]string(nil), rule.AllowedModes...),
			ExpiresAt:          rule.ExpiresAt,
			Revoked:            rule.Revoked,
			MaxRequestsPerHour: rule.MaxRequestsPerHour,
			MaxRequestsPerDay:  rule.MaxRequestsPerDay,
		})
	}
	return views
}
