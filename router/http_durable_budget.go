package router

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverusage"
)

func (opt HTTPHandlerOptions) reserveDurableBudget(ctx context.Context, grant *serverauth.Grant, entry *serverusage.Entry) error {
	if entry == nil {
		return fmt.Errorf("durable accounting entry is missing")
	}
	now := entry.Timestamp.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
		entry.Timestamp = now
	}
	month := now.Format("2006-01")
	var scopes []serverusage.BudgetScope
	add := func(kind, id, period string, cap float64) {
		if cap > 0 {
			scopes = append(scopes, serverusage.BudgetScope{Kind: kind, ID: id, Period: period, LimitMicroUSD: serverusage.MicroUSD(cap)})
		}
	}
	if grant != nil {
		add("token_day", grant.TokenID(), now.Format("2006-01-02"), grant.MaxCostUSDPerDay())
		add("token_month", grant.TokenID(), month, grant.MaxCostUSDPerMonth())
		orgID := strings.TrimSpace(grant.OrganizationID())
		if projectID := strings.TrimSpace(grant.ProjectID()); projectID != "" {
			if opt.TeamStore == nil {
				return fmt.Errorf("project budget store unavailable")
			}
			project, err := opt.TeamStore.GetProject(projectID)
			if err != nil {
				return err
			}
			if project == nil {
				return fmt.Errorf("project %q missing", projectID)
			}
			parent := strings.TrimSpace(project.OrganizationID)
			if orgID != "" && parent != "" && orgID != parent {
				return &httpStatusError{Status: http.StatusForbidden, Code: "scope_conflict", Message: "token project and organization disagree"}
			}
			if orgID == "" {
				orgID = parent
				entry.OrganizationID = orgID
			}
			add("project_month", projectID, month, project.MonthlyBudgetUSD)
		}
		if orgID != "" {
			if opt.TeamStore == nil {
				return fmt.Errorf("organization budget store unavailable")
			}
			org, err := opt.TeamStore.GetOrganization(orgID)
			if err != nil {
				return err
			}
			if org == nil {
				return fmt.Errorf("organization %q missing", orgID)
			}
			add("org_month", orgID, month, org.MonthlyBudgetUSD)
		}
	}
	// Correlation IDs can repeat and are client-controlled. Only this trusted,
	// unpredictable identity owns the reservation and exactly-once settlement.
	id := "account_" + rand.Text()
	err := opt.BudgetReserver.ReserveBudget(ctx, serverusage.BudgetReservation{ID: id, EstimateMicroUSD: serverusage.MicroUSD(opt.BudgetReservationUSD), ExpiresAt: now.Add(15 * time.Minute), Scopes: scopes})
	if err == nil {
		entry.ReservationID = id
	}
	return err
}
