package serverusage

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

// Ping checks availability without exposing the store's database handle.
func (s *SQLiteStore) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("usage sqlite store is unavailable")
	}
	return s.db.PingContext(ctx)
}

func (s *SQLiteStore) DBStats() sql.DBStats {
	if s == nil || s.db == nil {
		return sql.DBStats{}
	}
	return s.db.Stats()
}

func (s *SQLiteStore) Count(filter Filter) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("usage sqlite store is unavailable")
	}
	filter.Limit, filter.Offset = 0, 0
	query, args := aggregateSelection(filter)
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM (`+query+`)`, args...).Scan(&count)
	return count, err
}

func (s *SQLiteStore) MonthlyPeriods(filter Filter) ([]PeriodSummary, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("usage sqlite store is unavailable")
	}
	query, args := aggregateSelection(filter)
	// #nosec G202 -- aggregateSelection contains only fixed SQL clauses; every filter value is a bound argument.
	rows, err := s.db.Query(`SELECT substr(timestamp,1,7),COUNT(*),COALESCE(SUM(prompt_tokens),0),COALESCE(SUM(completion_tokens),0),COALESCE(SUM(cost_usd),0) FROM (`+query+`) GROUP BY substr(timestamp,1,7) ORDER BY substr(timestamp,1,7)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var periods []PeriodSummary
	for rows.Next() {
		var p PeriodSummary
		if err := rows.Scan(&p.Period, &p.TotalRequests, &p.TotalPromptTokens, &p.TotalCompletionTokens, &p.TotalCostUSD); err != nil {
			return nil, err
		}
		periods = append(periods, p)
	}
	return periods, rows.Err()
}

func (s *SQLiteStore) Spend(filter Filter) (float64, int, error) {
	if s == nil || s.db == nil {
		return 0, 0, fmt.Errorf("usage sqlite store is unavailable")
	}
	query, args := aggregateSelection(filter)
	args = append([]any{math.MaxFloat64}, args...)
	var cost float64
	var count int
	err := s.db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN cost_usd > 0 AND cost_usd <= ? THEN cost_usd ELSE 0 END),0), COUNT(*) FROM (`+query+`)`, args...).Scan(&cost, &count)
	if math.IsInf(cost, 1) {
		cost = math.MaxFloat64
	}
	return cost, count, err
}

func (s *SQLiteStore) Summarize(filter Filter) (Summary, error) {
	if s == nil || s.db == nil {
		return Summary{}, fmt.Errorf("usage sqlite store is unavailable")
	}
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Summary{}, err
	}
	defer func() { _ = tx.Rollback() }()
	query, args := aggregateSelection(filter)
	selection := ` FROM (` + query + `)`
	// Canonicalize UTC fractions inside SQL, including legacy integer-second
	// rows, so MIN/MAX keep their chronological meaning at second boundaries.
	timestamp := `substr(timestamp,1,19)||'.'||substr((CASE WHEN substr(timestamp,20,1)='.' THEN substr(timestamp,21,length(timestamp)-21) ELSE '' END)||'000000000',1,9)||'Z'`
	summary := SummarizeEntries(nil)
	var earliest, latest sql.NullString
	err = tx.QueryRow(`SELECT COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cost_usd),0), MIN(`+timestamp+`), MAX(`+timestamp+`)`+selection, args...).Scan(&summary.TotalRequests, &summary.TotalPromptTokens, &summary.TotalCompletionTokens, &summary.TotalCostUSD, &earliest, &latest)
	if err != nil {
		return Summary{}, err
	}
	if earliest.Valid {
		summary.Earliest, err = time.Parse(time.RFC3339Nano, earliest.String)
		if err != nil {
			return Summary{}, err
		}
	}
	if latest.Valid {
		summary.Latest, err = time.Parse(time.RFC3339Nano, latest.String)
		if err != nil {
			return Summary{}, err
		}
	}
	groups := []struct {
		column string
		counts map[string]int
		costs  map[string]float64
	}{
		{"token_id", summary.ByToken, summary.CostByToken},
		{"user_id", summary.ByUser, summary.CostByUser},
		{"organization_id", summary.ByOrganization, summary.CostByOrganization},
		{"project_id", summary.ByProject, summary.CostByProject},
		{"actual_provider", summary.ByProvider, summary.CostByProvider},
	}
	for _, group := range groups {
		// #nosec G202 -- group.column comes from the five literal columns above; selection uses fixed SQL with bound filter values.
		rows, err := tx.Query(`SELECT `+group.column+`, COUNT(*), SUM(CASE WHEN cost_usd > 0 THEN cost_usd ELSE 0 END)`+selection+` WHERE `+group.column+` != '' GROUP BY `+group.column, args...)
		if err != nil {
			return Summary{}, err
		}
		for rows.Next() {
			var key string
			var count int
			var cost float64
			if err := rows.Scan(&key, &count, &cost); err != nil {
				rows.Close()
				return Summary{}, err
			}
			group.counts[key] = count
			if cost > 0 {
				group.costs[key] = cost
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return Summary{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Summary{}, err
	}
	return summary, nil
}

func aggregateSelection(filter Filter) (string, []any) {
	query, args := usageSelection(filter)
	if filter.Limit <= 0 && filter.Offset <= 0 {
		query = strings.TrimSuffix(query, " ORDER BY timestamp DESC, id DESC")
	}
	return query, args
}
