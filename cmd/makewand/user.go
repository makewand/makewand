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
	return cmd
}

func userListCmd() *cobra.Command {
	var (
		usersDir    string
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

			store, err := resolveUserStore(usersDir)
			if err != nil {
				return err
			}
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
			printUsers("Users store: "+resolveUserStorePath(usersDir), users)
			return nil
		},
	}

	cmd.Flags().StringVar(&usersDir, "users-dir", "", "path to server users directory (defaults to ~/.config/makewand/server/users)")
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

			store, err := resolveUserStore(usersDir)
			if err != nil {
				return err
			}
			active := action == "activate"
			if _, err := store.SetUserActive(userID, active); err != nil {
				return err
			}
			fmt.Printf("%s user %s in %s\n", pastTenseAction(action), userID, resolveUserStorePath(usersDir))
			return nil
		},
	}
	cmd.Flags().StringVar(&usersDir, "users-dir", "", "path to server users directory (defaults to ~/.config/makewand/server/users)")
	cmd.Flags().StringVar(&remoteURL, "remote-url", "", "remote makewand admin base URL")
	cmd.Flags().StringVar(&remoteToken, "remote-token", "", "remote makewand admin Bearer token")
	return cmd
}

func userRoleCmd() *cobra.Command {
	var (
		usersDir    string
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

			store, err := resolveUserStore(usersDir)
			if err != nil {
				return err
			}
			if _, err := store.SetUserRole(userID, role); err != nil {
				return err
			}
			fmt.Printf("Updated role for %s in %s\n", userID, resolveUserStorePath(usersDir))
			return nil
		},
	}

	cmd.Flags().StringVar(&usersDir, "users-dir", "", "path to server users directory (defaults to ~/.config/makewand/server/users)")
	cmd.Flags().StringVar(&remoteURL, "remote-url", "", "remote makewand admin base URL")
	cmd.Flags().StringVar(&remoteToken, "remote-token", "", "remote makewand admin Bearer token")
	return cmd
}

func resolveUserStore(usersDir string) (*router.UserStore, error) {
	return router.NewUserStore(resolveUserStorePath(usersDir)), nil
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
