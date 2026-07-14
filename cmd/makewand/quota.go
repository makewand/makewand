package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/makewand/makewand/router"
	"github.com/spf13/cobra"
)

func quotaCmd() *cobra.Command {
	var (
		jsonOutput bool
		timeout    time.Duration
	)
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Show remaining subscription quota across providers",
		Long: "Read each configured subscription's remaining usage (5-hour session\n" +
			"window and weekly cap) so you can see which pool to spend and which to\n" +
			"rest. Data is read locally or via your own stored credentials; nothing\n" +
			"is uploaded. Providers without a readable quota source are shown as such.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if timeout <= 0 {
				timeout = 30 * time.Second
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			snap := router.NewDefaultQuotaSnapshotter(0).Refresh(ctx)
			pol := router.DefaultQuotaPolicy()

			if jsonOutput {
				return printQuotaJSON(cmd, snap)
			}
			printQuotaHuman(cmd, snap, pol)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output machine-readable JSON")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "max time to gather quota")
	return cmd
}

func printQuotaJSON(cmd *cobra.Command, snap *router.QuotaSnapshot) error {
	type dim struct {
		FiveHourPct *float64 `json:"five_hour_pct,omitempty"`
		WeeklyPct   *float64 `json:"weekly_pct,omitempty"`
		ScopedPct   *float64 `json:"scoped_pct,omitempty"`
		Authed      bool     `json:"authed"`
		HasData     bool     `json:"has_data"`
		Band        string   `json:"band"`
		ResetAt     string   `json:"reset_at,omitempty"`
	}
	pol := router.DefaultQuotaPolicy()
	out := map[string]dim{}
	for _, q := range snap.All() {
		reset := ""
		if !q.ResetAt.IsZero() {
			reset = q.ResetAt.Format(time.RFC3339)
		}
		out[q.Provider] = dim{
			FiveHourPct: q.FiveHourPct,
			WeeklyPct:   q.WeeklyPct,
			ScopedPct:   q.ScopedPct,
			Authed:      q.Authed,
			HasData:     q.HasData,
			Band:        bandLabel(pol.Band(q)),
			ResetAt:     reset,
		}
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printQuotaHuman(cmd *cobra.Command, snap *router.QuotaSnapshot, pol router.QuotaPolicy) {
	w := cmd.OutOrStdout()
	all := snap.All()
	if len(all) == 0 {
		fmt.Fprintln(w, "No providers detected. Install claude / codex / agy and sign in.")
		return
	}
	for _, q := range all {
		fmt.Fprintf(w, "── %s\n", q.Provider)
		if !q.HasData {
			fmt.Fprintf(w, "   no quota data (source unavailable)\n")
			continue
		}
		if q.FiveHourPct == nil && q.WeeklyPct == nil {
			// agy: login-state only.
			state := "signed in"
			if !q.Authed {
				state = "NOT signed in — run `agy` to authenticate"
			}
			fmt.Fprintf(w, "   subscription: %s (no usage percentage available)\n", state)
			continue
		}
		if q.FiveHourPct != nil {
			fmt.Fprintf(w, "   5h window   %s\n", pctStr(*q.FiveHourPct))
		}
		if q.WeeklyPct != nil {
			fmt.Fprintf(w, "   weekly      %s%s\n", pctStr(*q.WeeklyPct), resetSuffix(q.ResetAt))
		}
		if q.ScopedPct != nil {
			fmt.Fprintf(w, "   top model   %s (per-model weekly cap, informational)\n", pctStr(*q.ScopedPct))
		}
		fmt.Fprintf(w, "   → %s\n", bandAdvice(pol.Band(q)))
	}
}

func pctStr(p float64) string { return fmt.Sprintf("%.0f%% used", p) }

func resetSuffix(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return "  (resets " + t.Local().Format("01-02 15:04") + ")"
}

func bandLabel(b router.QuotaBand) string {
	switch b {
	case router.QuotaBandWarn:
		return "warn"
	case router.QuotaBandCritical:
		return "critical"
	default:
		return "ok"
	}
}

func bandAdvice(b router.QuotaBand) string {
	switch b {
	case router.QuotaBandWarn:
		return "getting low — routing will deprioritize this pool"
	case router.QuotaBandCritical:
		return "near/at cap — routing will avoid this pool"
	default:
		return "healthy headroom"
	}
}
