package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/router"
	"github.com/spf13/cobra"
)

func userCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage registered server users",
		Long:  "List users and update activation state or role in a local or remote makewand server.",
	}
	cmd.AddCommand(userListCmd())
	cmd.AddCommand(userActivateCmd())
	cmd.AddCommand(userDeactivateCmd())
	cmd.AddCommand(userRoleCmd())
	cmd.AddCommand(userPasswordCmd())
	cmd.AddCommand(userLoginCmd())
	return cmd
}

func userListCmd() *cobra.Command {
	var (
		usersDir    string
		stateDBPath string
		jsonOutput  bool
		remoteURL   string
		remoteToken string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered users",
		RunE: func(cmd *cobra.Command, args []string) error {
			baseURL, bearer, remoteMode, err := resolveOptionalRemoteAdminTarget(remoteURL, remoteToken)
			if err != nil {
				return err
			}
			if remoteMode {
				var resp remoteUserListResponse
				if err := adminGetJSON(baseURL, bearer, "/v1/admin/users", nil, &resp); err != nil {
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
				printUsers("Remote admin API: "+baseURL, resp.Data)
				return nil
			}

			store, closeFn, header, err := openUserStore(usersDir, stateDBPath)
			if err != nil {
				return err
			}
			defer closeFn()
			users, err := store.ListUsers()
			if err != nil {
				return err
			}
			if jsonOutput {
				data, err := json.MarshalIndent(users, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}
			printUsers(header, users)
			return nil
		},
	}

	cmd.Flags().StringVar(&usersDir, "users-dir", "", "path to server users directory (defaults to ~/.config/makewand/server/users)")
	cmd.Flags().StringVar(&stateDBPath, "state-db", "", "path to SQLite state DB for user operations")
	cmd.Flags().StringVar(&remoteURL, "remote-url", "", "remote makewand admin base URL")
	cmd.Flags().StringVar(&remoteToken, "remote-token", "", "remote makewand admin Bearer token")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output users as JSON")
	return cmd
}

func userActivateCmd() *cobra.Command {
	return userActionCmd("activate")
}

func userDeactivateCmd() *cobra.Command {
	return userActionCmd("deactivate")
}

func userActionCmd(action string) *cobra.Command {
	var (
		usersDir    string
		stateDBPath string
		remoteURL   string
		remoteToken string
	)
	cmd := &cobra.Command{
		Use:   action + " <user-id>",
		Short: titleWord(action) + " a user account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			userID := strings.TrimSpace(args[0])
			baseURL, bearer, remoteMode, err := resolveOptionalRemoteAdminTarget(remoteURL, remoteToken)
			if err != nil {
				return err
			}
			if remoteMode {
				endpoint := "/v1/admin/users/" + url.PathEscape(userID) + "/" + action
				if err := adminPostJSON(baseURL, bearer, endpoint, nil, &struct{}{}); err != nil {
					return err
				}
				fmt.Printf("%s user %s via %s\n", titleWord(action), userID, baseURL)
				return nil
			}

			store, closeFn, header, err := openUserStore(usersDir, stateDBPath)
			if err != nil {
				return err
			}
			defer closeFn()
			active := action == "activate"
			if _, err := store.SetUserActive(userID, active); err != nil {
				return err
			}
			fmt.Printf("%s user %s in %s\n", pastTenseAction(action), userID, header)
			return nil
		},
	}
	cmd.Flags().StringVar(&usersDir, "users-dir", "", "path to server users directory (defaults to ~/.config/makewand/server/users)")
	cmd.Flags().StringVar(&stateDBPath, "state-db", "", "path to SQLite state DB for user operations")
	cmd.Flags().StringVar(&remoteURL, "remote-url", "", "remote makewand admin base URL")
	cmd.Flags().StringVar(&remoteToken, "remote-token", "", "remote makewand admin Bearer token")
	return cmd
}

func userRoleCmd() *cobra.Command {
	var (
		usersDir    string
		stateDBPath string
		remoteURL   string
		remoteToken string
	)
	cmd := &cobra.Command{
		Use:   "role <user-id> <role>",
		Short: "Change a user's role (member, admin)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			userID := strings.TrimSpace(args[0])
			role := strings.TrimSpace(args[1])
			baseURL, bearer, remoteMode, err := resolveOptionalRemoteAdminTarget(remoteURL, remoteToken)
			if err != nil {
				return err
			}
			if remoteMode {
				endpoint := "/v1/admin/users/" + url.PathEscape(userID) + "/role"
				if err := adminPostJSON(baseURL, bearer, endpoint, map[string]string{"role": role}, &struct{}{}); err != nil {
					return err
				}
				fmt.Printf("Updated role for %s via %s\n", userID, baseURL)
				return nil
			}

			store, closeFn, header, err := openUserStore(usersDir, stateDBPath)
			if err != nil {
				return err
			}
			defer closeFn()
			if _, err := store.SetUserRole(userID, role); err != nil {
				return err
			}
			fmt.Printf("Updated role for %s in %s\n", userID, header)
			return nil
		},
	}

	cmd.Flags().StringVar(&usersDir, "users-dir", "", "path to server users directory (defaults to ~/.config/makewand/server/users)")
	cmd.Flags().StringVar(&stateDBPath, "state-db", "", "path to SQLite state DB for user operations")
	cmd.Flags().StringVar(&remoteURL, "remote-url", "", "remote makewand admin base URL")
	cmd.Flags().StringVar(&remoteToken, "remote-token", "", "remote makewand admin Bearer token")
	return cmd
}

func userPasswordCmd() *cobra.Command {
	var (
		usersDir    string
		stateDBPath string
		remoteURL   string
		remoteToken string
		password    string
	)
	cmd := &cobra.Command{
		Use:   "password <user-id>",
		Short: "Reset a user's password",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			userID := strings.TrimSpace(args[0])
			if strings.TrimSpace(password) == "" {
				return fmt.Errorf("--password is required")
			}
			baseURL, bearer, remoteMode, err := resolveOptionalRemoteAdminTarget(remoteURL, remoteToken)
			if err != nil {
				return err
			}
			if remoteMode {
				endpoint := "/v1/admin/users/" + url.PathEscape(userID) + "/password"
				if err := adminPostJSON(baseURL, bearer, endpoint, map[string]string{"password": password}, &struct{}{}); err != nil {
					return err
				}
				fmt.Printf("Updated password for %s via %s\n", userID, baseURL)
				return nil
			}

			store, closeFn, header, err := openUserStore(usersDir, stateDBPath)
			if err != nil {
				return err
			}
			defer closeFn()
			if _, err := store.SetUserPassword(userID, password); err != nil {
				return err
			}
			fmt.Printf("Updated password for %s in %s\n", userID, header)
			return nil
		},
	}
	cmd.Flags().StringVar(&usersDir, "users-dir", "", "path to server users directory (defaults to ~/.config/makewand/server/users)")
	cmd.Flags().StringVar(&stateDBPath, "state-db", "", "path to SQLite state DB for user operations")
	cmd.Flags().StringVar(&remoteURL, "remote-url", "", "remote makewand admin base URL")
	cmd.Flags().StringVar(&remoteToken, "remote-token", "", "remote makewand admin Bearer token")
	cmd.Flags().StringVar(&password, "password", "", "new password")
	return cmd
}

func userLoginCmd() *cobra.Command {
	var (
		remoteURL      string
		email          string
		password       string
		organizationID string
		projectID      string
		jsonOutput     bool
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Login and issue a user bearer token from a remote server",
		RunE: func(cmd *cobra.Command, args []string) error {
			baseURL := strings.TrimRight(strings.TrimSpace(remoteURL), "/")
			if baseURL == "" {
				return fmt.Errorf("user login requires --remote-url")
			}
			payload := map[string]string{
				"email":    strings.TrimSpace(email),
				"password": password,
			}
			if strings.TrimSpace(organizationID) != "" {
				payload["organization_id"] = strings.TrimSpace(organizationID)
			}
			if strings.TrimSpace(projectID) != "" {
				payload["project_id"] = strings.TrimSpace(projectID)
			}
			data, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/users/login", bytes.NewReader(data))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := newAdminHTTPClient().Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			if resp.StatusCode >= 400 {
				return fmt.Errorf("user login failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
			}
			var loginResp router.UserLoginResponse
			if err := json.Unmarshal(body, &loginResp); err != nil {
				return err
			}
			if jsonOutput {
				rendered, err := json.MarshalIndent(loginResp, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(rendered))
				return nil
			}
			fmt.Printf("Token ID: %s\n", loginResp.TokenID)
			fmt.Printf("Token: %s\n", loginResp.Token)
			if !loginResp.ExpiresAt.IsZero() {
				fmt.Printf("Expires at: %s\n", loginResp.ExpiresAt.UTC().Format(time.RFC3339))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&remoteURL, "remote-url", "", "remote makewand base URL")
	cmd.Flags().StringVar(&email, "email", "", "user email")
	cmd.Flags().StringVar(&password, "password", "", "user password")
	cmd.Flags().StringVar(&organizationID, "organization-id", "", "organization to scope the issued login token to")
	cmd.Flags().StringVar(&projectID, "project-id", "", "project to scope the issued login token to")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output login response as JSON")
	_ = cmd.MarkFlagRequired("email")
	_ = cmd.MarkFlagRequired("password")
	return cmd
}

func openUserStore(usersDir, stateDBPath string) (router.UserManager, func(), string, error) {
	if stateDB := resolveManagedStateDBPath(stateDBPath); stateDB != "" {
		store, err := router.OpenSQLiteUserStore(stateDB)
		if err != nil {
			return nil, nil, "", err
		}
		return store, func() { _ = store.Close() }, "sqlite:" + stateDB, nil
	}
	path := resolveUserStorePath(usersDir)
	return router.NewUserStore(path), func() {}, path, nil
}

func resolveUserStorePath(flagValue string) string {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue != "" {
		return flagValue
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "makewand", "server", "users")
	}
	return filepath.Join(dir, "server", "users")
}

func printUsers(header string, users []router.UserView) {
	if len(users) == 0 {
		fmt.Printf("No users in %s\n", strings.TrimSpace(header))
		return
	}
	fmt.Println(header)
	for _, user := range users {
		fmt.Printf("- %s\n", user.ID)
		fmt.Printf("  email: %s\n", user.Email)
		fmt.Printf("  role: %s\n", user.Role)
		fmt.Printf("  active: %t\n", user.IsActive)
		fmt.Printf("  created: %s\n", user.CreatedAt.UTC().Format(time.RFC3339))
		fmt.Printf("  updated: %s\n", user.UpdatedAt.UTC().Format(time.RFC3339))
	}
}

func titleWord(value string) string {
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func pastTenseAction(action string) string {
	switch strings.TrimSpace(action) {
	case "activate":
		return "Activated"
	case "deactivate":
		return "Deactivated"
	default:
		return titleWord(action)
	}
}
