package main

import (
	"encoding/json"
	"fmt"
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
	)

	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Summarize audit activity",
		RunE: func(cmd *cobra.Command, args []string) error {
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
	)

	cmd := &cobra.Command{
		Use:   "events",
		Short: "List matching audit events",
		RunE: func(cmd *cobra.Command, args []string) error {
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
