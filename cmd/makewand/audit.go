package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/serveraudit"
	"github.com/spf13/cobra"
)

func auditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Inspect server audit logs",
		Long:  "Summarize or list events from a makewand server JSONL audit log.",
	}
	cmd.AddCommand(auditSummaryCmd())
	cmd.AddCommand(auditEventsCmd())
	return cmd
}

func auditSummaryCmd() *cobra.Command {
	var (
		pathFlag      string
		tokenIDFlag   string
		kindFlag      string
		workspaceFlag string
		statusFlag    int
		sinceFlag     string
		untilFlag     string
		jsonOutput    bool
		remoteURL     string
		remoteToken   string
	)

	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Summarize audit activity",
		RunE: func(cmd *cobra.Command, args []string) error {
			baseURL, bearer, remoteMode, err := resolveOptionalRemoteAdminTarget(remoteURL, remoteToken)
			if err != nil {
				return err
			}
			if remoteMode {
				var resp remoteAuditSummaryResponse
				if err := adminGetJSON(baseURL, bearer, "/v1/admin/audit/summary", buildAuditQuery(tokenIDFlag, kindFlag, workspaceFlag, statusFlag, sinceFlag, untilFlag, 0), &resp); err != nil {
					return err
				}
				if jsonOutput {
					data, err := json.MarshalIndent(resp, "", "  ")
					if err != nil {
						return err
					}
					fmt.Println(string(data))
					return nil
				}
				printAuditSummary(resp.Path, resp.Summary)
				return nil
			}

			path, err := resolveAuditLogPath(pathFlag)
			if err != nil {
				return err
			}
			filter, err := buildAuditFilter(tokenIDFlag, kindFlag, workspaceFlag, statusFlag, sinceFlag, untilFlag, 0)
			if err != nil {
				return err
			}
			events, err := loadAuditEvents(path, filter)
			if err != nil {
				return err
			}
			summary := serveraudit.SummarizeEvents(events)
			if jsonOutput {
				data, err := json.MarshalIndent(summary, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}
			printAuditSummary(path, summary)
			return nil
		},
	}

	cmd.Flags().StringVar(&pathFlag, "path", "", "path to audit JSONL (defaults to MAKEWAND_SERVER_AUDIT_LOG or ~/.config/makewand/server/audit.jsonl)")
	cmd.Flags().StringVar(&tokenIDFlag, "token-id", "", "filter by token ID")
	cmd.Flags().StringVar(&kindFlag, "kind", "", "filter by event kind (chat, models, session)")
	cmd.Flags().StringVar(&workspaceFlag, "workspace", "", "filter by workspace ID")
	cmd.Flags().IntVar(&statusFlag, "status", 0, "filter by HTTP status code")
	cmd.Flags().StringVar(&sinceFlag, "since", "", "filter events since RFC3339 time or relative duration like 24h")
	cmd.Flags().StringVar(&untilFlag, "until", "", "filter events until RFC3339 time or relative duration like 1h")
	cmd.Flags().StringVar(&remoteURL, "remote-url", "", "remote makewand admin base URL")
	cmd.Flags().StringVar(&remoteToken, "remote-token", "", "remote makewand admin Bearer token")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output summary as JSON")
	return cmd
}

func auditEventsCmd() *cobra.Command {
	var (
		pathFlag      string
		tokenIDFlag   string
		kindFlag      string
		workspaceFlag string
		statusFlag    int
		sinceFlag     string
		untilFlag     string
		limitFlag     int
		jsonOutput    bool
		remoteURL     string
		remoteToken   string
	)

	cmd := &cobra.Command{
		Use:   "events",
		Short: "List matching audit events",
		RunE: func(cmd *cobra.Command, args []string) error {
			baseURL, bearer, remoteMode, err := resolveOptionalRemoteAdminTarget(remoteURL, remoteToken)
			if err != nil {
				return err
			}
			if remoteMode {
				var resp remoteAuditEventsResponse
				if err := adminGetJSON(baseURL, bearer, "/v1/admin/audit/events", buildAuditQuery(tokenIDFlag, kindFlag, workspaceFlag, statusFlag, sinceFlag, untilFlag, limitFlag), &resp); err != nil {
					return err
				}
				if jsonOutput {
					data, err := json.MarshalIndent(resp, "", "  ")
					if err != nil {
						return err
					}
					fmt.Println(string(data))
					return nil
				}
				printAuditEvents(resp.Data)
				return nil
			}

			path, err := resolveAuditLogPath(pathFlag)
			if err != nil {
				return err
			}
			filter, err := buildAuditFilter(tokenIDFlag, kindFlag, workspaceFlag, statusFlag, sinceFlag, untilFlag, limitFlag)
			if err != nil {
				return err
			}
			events, err := loadAuditEvents(path, filter)
			if err != nil {
				return err
			}
			if jsonOutput {
				data, err := json.MarshalIndent(events, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}
			printAuditEvents(events)
			return nil
		},
	}

	cmd.Flags().StringVar(&pathFlag, "path", "", "path to audit JSONL (defaults to MAKEWAND_SERVER_AUDIT_LOG or ~/.config/makewand/server/audit.jsonl)")
	cmd.Flags().StringVar(&tokenIDFlag, "token-id", "", "filter by token ID")
	cmd.Flags().StringVar(&kindFlag, "kind", "", "filter by event kind (chat, models, session)")
	cmd.Flags().StringVar(&workspaceFlag, "workspace", "", "filter by workspace ID")
	cmd.Flags().IntVar(&statusFlag, "status", 0, "filter by HTTP status code")
	cmd.Flags().StringVar(&sinceFlag, "since", "", "filter events since RFC3339 time or relative duration like 24h")
	cmd.Flags().StringVar(&untilFlag, "until", "", "filter events until RFC3339 time or relative duration like 1h")
	cmd.Flags().IntVar(&limitFlag, "limit", 50, "maximum number of events to display (0 = unlimited)")
	cmd.Flags().StringVar(&remoteURL, "remote-url", "", "remote makewand admin base URL")
	cmd.Flags().StringVar(&remoteToken, "remote-token", "", "remote makewand admin Bearer token")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output events as JSON")
	return cmd
}

func resolveAuditLogPath(flagValue string) (string, error) {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue != "" {
		return flagValue, nil
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	dataDir := filepath.Join(dir, "server")
	path := resolveServeAuditPath("", dataDir)
	if path != "" {
		return path, nil
	}
	return filepath.Join(dataDir, "audit.jsonl"), nil
}

func buildAuditFilter(tokenID, kind, workspace string, status int, sinceRaw, untilRaw string, limit int) (serveraudit.Filter, error) {
	now := time.Now().UTC()
	since, err := parseAuditTimeValue(sinceRaw, now)
	if err != nil {
		return serveraudit.Filter{}, fmt.Errorf("parse since: %w", err)
	}
	until, err := parseAuditTimeValue(untilRaw, now)
	if err != nil {
		return serveraudit.Filter{}, fmt.Errorf("parse until: %w", err)
	}
	return serveraudit.Filter{
		TokenID:     strings.TrimSpace(tokenID),
		Kind:        strings.TrimSpace(kind),
		WorkspaceID: strings.TrimSpace(workspace),
		Since:       since,
		Until:       until,
		Status:      status,
		Limit:       limit,
	}, nil
}

func parseAuditTimeValue(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if duration, err := time.ParseDuration(raw); err == nil {
		return now.Add(-duration), nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return value.UTC(), nil
}

func printAuditSummary(path string, summary serveraudit.Summary) {
	fmt.Printf("Audit log: %s\n", path)
	fmt.Printf("Total events: %d\n", summary.Total)
	if !summary.Earliest.IsZero() {
		fmt.Printf("Window: %s -> %s\n", summary.Earliest.Format(time.RFC3339), summary.Latest.Format(time.RFC3339))
	}
	fmt.Printf("Prompt tokens: %d\n", summary.TotalPromptTokens)
	fmt.Printf("Completion tokens: %d\n", summary.TotalCompletionTokens)
	fmt.Printf("Total cost: $%.4f\n", summary.TotalCostUSD)
	for _, key := range serveraudit.SortedStringCounts(summary.ByKind) {
		fmt.Printf("Kind %s: %d\n", key, summary.ByKind[key])
	}
	for _, key := range serveraudit.SortedStatusCounts(summary.ByStatus) {
		fmt.Printf("Status %d: %d\n", key, summary.ByStatus[key])
	}
	for _, key := range serveraudit.SortedStringCounts(summary.ByToken) {
		fmt.Printf("Token %s: %d\n", key, summary.ByToken[key])
	}
	for _, key := range serveraudit.SortedStringCounts(summary.ByProvider) {
		fmt.Printf("Provider %s: %d\n", key, summary.ByProvider[key])
	}
	for _, key := range serveraudit.SortedStringTotals(summary.CostByToken) {
		fmt.Printf("Token cost %s: $%.4f\n", key, summary.CostByToken[key])
	}
	for _, key := range serveraudit.SortedStringTotals(summary.CostByProvider) {
		fmt.Printf("Provider cost %s: $%.4f\n", key, summary.CostByProvider[key])
	}
}

func printAuditEvents(events []serveraudit.Event) {
	if len(events) == 0 {
		fmt.Println("No audit events matched.")
		return
	}
	for _, evt := range events {
		line := []string{
			evt.Timestamp.Format(time.RFC3339),
			evt.Kind,
			strconv.Itoa(evt.Status),
		}
		if evt.TokenID != "" {
			line = append(line, "token="+evt.TokenID)
		}
		if evt.ActualProvider != "" {
			line = append(line, "provider="+evt.ActualProvider)
		}
		if evt.WorkspaceID != "" {
			line = append(line, "workspace="+evt.WorkspaceID)
		}
		if evt.Path != "" {
			line = append(line, evt.Path)
		}
		if evt.CostUSD > 0 {
			line = append(line, fmt.Sprintf("cost=$%.4f", evt.CostUSD))
		}
		if evt.Error != "" {
			line = append(line, "error="+evt.Error)
		}
		fmt.Println(strings.Join(line, " "))
	}
}

func loadAuditEvents(path string, filter serveraudit.Filter) ([]serveraudit.Event, error) {
	events, err := serveraudit.LoadEvents(path, filter)
	if err != nil && os.IsNotExist(err) {
		return nil, nil
	}
	return events, err
}

func buildAuditQuery(tokenID, kind, workspace string, status int, since, until string, limit int) url.Values {
	query := url.Values{}
	if strings.TrimSpace(tokenID) != "" {
		query.Set("token_id", strings.TrimSpace(tokenID))
	}
	if strings.TrimSpace(kind) != "" {
		query.Set("kind", strings.TrimSpace(kind))
	}
	if strings.TrimSpace(workspace) != "" {
		query.Set("workspace", strings.TrimSpace(workspace))
	}
	if status != 0 {
		query.Set("status", strconv.Itoa(status))
	}
	if strings.TrimSpace(since) != "" {
		query.Set("since", strings.TrimSpace(since))
	}
	if strings.TrimSpace(until) != "" {
		query.Set("until", strings.TrimSpace(until))
	}
	if limit != 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	return query
}
