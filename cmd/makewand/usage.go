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
	"github.com/makewand/makewand/serverusage"
	"github.com/spf13/cobra"
)

func usageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Inspect structured server usage logs",
		Long:  "Summarize or list events from a makewand server JSONL usage ledger.",
	}
	cmd.AddCommand(usageSummaryCmd())
	cmd.AddCommand(usageEventsCmd())
	return cmd
}

func usageSummaryCmd() *cobra.Command {
	var (
		pathFlag       string
		tokenIDFlag    string
		providerFlag   string
		statusFlag     int
		sinceFlag      string
		untilFlag      string
		streamOnlyFlag bool
		jsonOutput     bool
		remoteURL      string
		remoteToken    string
	)

	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Summarize usage activity",
		RunE: func(cmd *cobra.Command, args []string) error {
			baseURL, bearer, remoteMode, err := resolveOptionalRemoteAdminTarget(remoteURL, remoteToken)
			if err != nil {
				return err
			}
			query := buildUsageQuery(tokenIDFlag, providerFlag, statusFlag, sinceFlag, untilFlag, 0, streamOnlyFlag)
			if remoteMode {
				var resp remoteUsageSummaryResponse
				if err := adminGetJSON(baseURL, bearer, "/v1/admin/usage/summary", query, &resp); err != nil {
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
				printUsageSummary(resp.Path, resp.Usage)
				return nil
			}

			path, err := resolveUsageLogPath(pathFlag)
			if err != nil {
				return err
			}
			filter, err := buildUsageFilter(tokenIDFlag, providerFlag, statusFlag, sinceFlag, untilFlag, 0, streamOnlyFlag)
			if err != nil {
				return err
			}
			entries, err := loadUsageEntries(path, filter)
			if err != nil {
				return err
			}
			summary := serverusage.SummarizeEntries(entries)
			if jsonOutput {
				data, err := json.MarshalIndent(summary, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}
			printUsageSummary(path, summary)
			return nil
		},
	}

	cmd.Flags().StringVar(&pathFlag, "path", "", "path to usage JSONL (defaults to MAKEWAND_SERVER_USAGE_LOG or ~/.config/makewand/server/usage.jsonl)")
	cmd.Flags().StringVar(&tokenIDFlag, "token-id", "", "filter by token ID")
	cmd.Flags().StringVar(&providerFlag, "provider", "", "filter by actual provider")
	cmd.Flags().IntVar(&statusFlag, "status", 0, "filter by HTTP status code")
	cmd.Flags().StringVar(&sinceFlag, "since", "", "filter entries since RFC3339 time or relative duration like 24h")
	cmd.Flags().StringVar(&untilFlag, "until", "", "filter entries until RFC3339 time or relative duration like 1h")
	cmd.Flags().BoolVar(&streamOnlyFlag, "stream-only", false, "include only streaming chat entries")
	cmd.Flags().StringVar(&remoteURL, "remote-url", "", "remote makewand admin base URL")
	cmd.Flags().StringVar(&remoteToken, "remote-token", "", "remote makewand admin Bearer token")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output summary as JSON")
	return cmd
}

func usageEventsCmd() *cobra.Command {
	var (
		pathFlag       string
		tokenIDFlag    string
		providerFlag   string
		statusFlag     int
		sinceFlag      string
		untilFlag      string
		limitFlag      int
		streamOnlyFlag bool
		jsonOutput     bool
		remoteURL      string
		remoteToken    string
	)

	cmd := &cobra.Command{
		Use:   "events",
		Short: "List matching usage entries",
		RunE: func(cmd *cobra.Command, args []string) error {
			baseURL, bearer, remoteMode, err := resolveOptionalRemoteAdminTarget(remoteURL, remoteToken)
			if err != nil {
				return err
			}
			query := buildUsageQuery(tokenIDFlag, providerFlag, statusFlag, sinceFlag, untilFlag, limitFlag, streamOnlyFlag)
			if remoteMode {
				var resp remoteUsageEventsResponse
				if err := adminGetJSON(baseURL, bearer, "/v1/admin/usage/events", query, &resp); err != nil {
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
				printUsageEvents(resp.Data)
				return nil
			}

			path, err := resolveUsageLogPath(pathFlag)
			if err != nil {
				return err
			}
			filter, err := buildUsageFilter(tokenIDFlag, providerFlag, statusFlag, sinceFlag, untilFlag, limitFlag, streamOnlyFlag)
			if err != nil {
				return err
			}
			entries, err := loadUsageEntries(path, filter)
			if err != nil {
				return err
			}
			if jsonOutput {
				data, err := json.MarshalIndent(entries, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}
			printUsageEvents(entries)
			return nil
		},
	}

	cmd.Flags().StringVar(&pathFlag, "path", "", "path to usage JSONL (defaults to MAKEWAND_SERVER_USAGE_LOG or ~/.config/makewand/server/usage.jsonl)")
	cmd.Flags().StringVar(&tokenIDFlag, "token-id", "", "filter by token ID")
	cmd.Flags().StringVar(&providerFlag, "provider", "", "filter by actual provider")
	cmd.Flags().IntVar(&statusFlag, "status", 0, "filter by HTTP status code")
	cmd.Flags().StringVar(&sinceFlag, "since", "", "filter entries since RFC3339 time or relative duration like 24h")
	cmd.Flags().StringVar(&untilFlag, "until", "", "filter entries until RFC3339 time or relative duration like 1h")
	cmd.Flags().IntVar(&limitFlag, "limit", 50, "maximum number of entries to display (0 = unlimited)")
	cmd.Flags().BoolVar(&streamOnlyFlag, "stream-only", false, "include only streaming chat entries")
	cmd.Flags().StringVar(&remoteURL, "remote-url", "", "remote makewand admin base URL")
	cmd.Flags().StringVar(&remoteToken, "remote-token", "", "remote makewand admin Bearer token")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output entries as JSON")
	return cmd
}

func resolveUsageLogPath(flagValue string) (string, error) {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue != "" {
		return flagValue, nil
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return resolveServeUsagePath("", filepath.Join(dir, "server")), nil
}

func buildUsageFilter(tokenID, provider string, status int, sinceRaw, untilRaw string, limit int, streamOnly bool) (serverusage.Filter, error) {
	now := time.Now().UTC()
	since, err := parseAuditTimeValue(sinceRaw, now)
	if err != nil {
		return serverusage.Filter{}, fmt.Errorf("parse since: %w", err)
	}
	until, err := parseAuditTimeValue(untilRaw, now)
	if err != nil {
		return serverusage.Filter{}, fmt.Errorf("parse until: %w", err)
	}
	return serverusage.Filter{
		TokenID:    strings.TrimSpace(tokenID),
		Provider:   strings.TrimSpace(provider),
		Status:     status,
		Since:      since,
		Until:      until,
		Limit:      limit,
		StreamOnly: streamOnly,
	}, nil
}

func buildUsageQuery(tokenID, provider string, status int, since, until string, limit int, streamOnly bool) url.Values {
	query := url.Values{}
	if strings.TrimSpace(tokenID) != "" {
		query.Set("token_id", strings.TrimSpace(tokenID))
	}
	if strings.TrimSpace(provider) != "" {
		query.Set("provider", strings.TrimSpace(provider))
	}
	if status > 0 {
		query.Set("status", fmt.Sprintf("%d", status))
	}
	if strings.TrimSpace(since) != "" {
		query.Set("since", strings.TrimSpace(since))
	}
	if strings.TrimSpace(until) != "" {
		query.Set("until", strings.TrimSpace(until))
	}
	if limit > 0 {
		query.Set("limit", fmt.Sprintf("%d", limit))
	}
	if streamOnly {
		query.Set("stream_only", "true")
	}
	return query
}

func loadUsageEntries(path string, filter serverusage.Filter) ([]serverusage.Entry, error) {
	entries, err := serverusage.LoadEntries(path, filter)
	if err != nil && os.IsNotExist(err) {
		return nil, nil
	}
	return entries, err
}

func printUsageSummary(path string, summary serverusage.Summary) {
	fmt.Printf("Usage log: %s\n", path)
	fmt.Printf("Total requests: %d\n", summary.TotalRequests)
	if !summary.Earliest.IsZero() {
		fmt.Printf("Window: %s -> %s\n", summary.Earliest.Format(time.RFC3339), summary.Latest.Format(time.RFC3339))
	}
	fmt.Printf("Prompt tokens: %d\n", summary.TotalPromptTokens)
	fmt.Printf("Completion tokens: %d\n", summary.TotalCompletionTokens)
	fmt.Printf("Total cost: $%.4f\n", summary.TotalCostUSD)
	for _, key := range serverusage.SortedStringCounts(summary.ByToken) {
		fmt.Printf("Token %s: %d\n", key, summary.ByToken[key])
	}
	for _, key := range serverusage.SortedStringCounts(summary.ByProvider) {
		fmt.Printf("Provider %s: %d\n", key, summary.ByProvider[key])
	}
	for _, key := range serverusage.SortedStringTotals(summary.CostByToken) {
		fmt.Printf("Cost token %s: $%.4f\n", key, summary.CostByToken[key])
	}
	for _, key := range serverusage.SortedStringTotals(summary.CostByProvider) {
		fmt.Printf("Cost provider %s: $%.4f\n", key, summary.CostByProvider[key])
	}
}

func printUsageEvents(entries []serverusage.Entry) {
	if len(entries) == 0 {
		fmt.Println("No matching usage entries.")
		return
	}
	for _, entry := range entries {
		fmt.Printf("%s status=%d token=%s provider=%s cost=$%.4f prompt=%d completion=%d stream=%t request_id=%s\n",
			entry.Timestamp.Format(time.RFC3339),
			entry.Status,
			emptyFallback(entry.TokenID, "-"),
			emptyFallback(entry.ActualProvider, "-"),
			entry.CostUSD,
			entry.PromptTokens,
			entry.CompletionTokens,
			entry.Stream,
			emptyFallback(entry.RequestID, "-"),
		)
	}
}

func emptyFallback(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
