package main

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/makewand/makewand/internal/quota"
)

func printQuotaJSON(w io.Writer, snap *quota.QuotaSnapshot) error {
	type dim struct {
		FiveHourPct    *float64            `json:"five_hour_pct,omitempty"`
		WeeklyPct      *float64            `json:"weekly_pct,omitempty"`
		ThirtyDayPct   *float64            `json:"thirty_day_pct,omitempty"`
		CredentialRoot string              `json:"credential_root,omitempty"`
		AccountRef     string              `json:"account_ref,omitempty"`
		IdentityKnown  bool                `json:"identity_known"`
		PoolRef        string              `json:"pool_ref,omitempty"`
		SourceAt       time.Time           `json:"source_at"`
		Windows        []quota.QuotaWindow `json:"windows,omitempty"`
		ScopedPct      *float64            `json:"scoped_pct,omitempty"`
		Authed         bool                `json:"authed"`
		HasData        bool                `json:"has_data"`
		Band           string              `json:"band"`
		ResetAt        string              `json:"reset_at,omitempty"`
	}
	pol := quota.DefaultQuotaPolicy()
	out := map[string]dim{}
	for _, q := range snap.All() {
		reset := ""
		if !q.ResetAt.IsZero() {
			reset = q.ResetAt.Format(time.RFC3339)
		}
		out[q.Provider] = dim{
			FiveHourPct:    q.FiveHourPct,
			WeeklyPct:      q.WeeklyPct,
			ThirtyDayPct:   q.ThirtyDayPct,
			CredentialRoot: q.CredentialRoot, AccountRef: q.AccountRef,
			IdentityKnown: q.IdentityKnown, PoolRef: q.PoolRef,
			SourceAt: q.SourceAt, Windows: q.Windows,
			ScopedPct: q.ScopedPct,
			Authed:    q.Authed,
			HasData:   q.HasData,
			Band:      bandLabel(pol.Band(q)),
			ResetAt:   reset,
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printQuotaHuman(w io.Writer, snap *quota.QuotaSnapshot, pol quota.QuotaPolicy) {
	all := snap.All()
	if len(all) == 0 {
		fmt.Fprintln(w, "No providers detected. Install claude / codex / agy and sign in.")
		return
	}
	for _, q := range all {
		fmt.Fprintf(w, "── %s\n", q.Provider)
		if q.AccountRef != "" {
			if q.IdentityKnown {
				fmt.Fprintf(w, "   account     %s\n", q.AccountRef)
			} else {
				fmt.Fprintln(w, "   account     unknown (credential directory isolated; shared pool unknown)")
			}
		}
		if !q.HasData {
			fmt.Fprintf(w, "   no quota data (source unavailable)\n")
			continue
		}
		if q.FiveHourPct == nil && q.WeeklyPct == nil && q.ThirtyDayPct == nil && len(q.Windows) == 0 {
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
			fmt.Fprintf(w, "   weekly      %s%s\n", pctStr(*q.WeeklyPct), resetSuffix(q.WeeklyResetAt))
		}
		if q.ThirtyDayPct != nil {
			fmt.Fprintf(w, "   30 days     %s%s\n", pctStr(*q.ThirtyDayPct), resetSuffix(q.ThirtyDayResetAt))
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

func bandLabel(b quota.QuotaBand) string {
	switch b {
	case quota.QuotaBandWarn:
		return "warn"
	case quota.QuotaBandCritical:
		return "critical"
	default:
		return "ok"
	}
}

func bandAdvice(b quota.QuotaBand) string {
	switch b {
	case quota.QuotaBandWarn:
		return "getting low — routing will deprioritize this pool"
	case quota.QuotaBandCritical:
		return "near/at cap — routing will avoid this pool"
	default:
		return "healthy headroom"
	}
}
