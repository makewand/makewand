package serverusage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// ErrBudgetExceeded means settled and outstanding costs leave no room.
var ErrBudgetExceeded = errors.New("durable budget exceeded")
var ErrReservationExists = errors.New("accounting identity already reserved")

// MicroUSD conservatively rounds a finite positive dollar cost up to an integer
// micro-dollar. Invalid and overflowing amounts are rejected by ReserveBudget.
func MicroUSD(usd float64) int64 {
	if usd <= 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0
	}
	v := usd * 1_000_000
	if v >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	// Avoid turning a binary representation of an exact cent into 10001 micros.
	return int64(math.Ceil(math.Nextafter(v, math.Inf(-1))))
}

// BudgetScope names one token/day, token/month, organization/month, or project/month cap.
type BudgetScope struct {
	Kind          string
	ID            string
	Period        string
	LimitMicroUSD int64
}

// BudgetReservation is submitted before the provider runs. ID must be generated
// by the trusted server, not taken directly from a client's tracing header.
type BudgetReservation struct {
	ID               string
	EstimateMicroUSD int64
	ExpiresAt        time.Time
	Scopes           []BudgetScope
}

// BudgetReserver reserves in a durable, atomic ledger. Usage.Log on the same
// SQLiteStore settles/refunds the reservation in the usage transaction.
type BudgetReserver interface {
	ReserveBudget(context.Context, BudgetReservation) error
}

func (s *SQLiteStore) checkSchemaVersion() error {
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_versions'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	var version int
	err := s.db.QueryRow(`SELECT version FROM schema_versions WHERE component='usage'`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if version < 0 || version > 2 {
		return fmt.Errorf("unsupported usage schema version %d", version)
	}
	return nil
}

func (s *SQLiteStore) ensureBudgetSchema() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS schema_versions(component TEXT PRIMARY KEY, version INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS budget_reservations(
 request_id TEXT PRIMARY KEY, estimate_micro_usd INTEGER NOT NULL CHECK(estimate_micro_usd>=0),
 expires_at TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('pending','expired','settled')),
 actual_micro_usd INTEGER NOT NULL DEFAULT 0 CHECK(actual_micro_usd>=0));
CREATE TABLE IF NOT EXISTS budget_reservation_scopes(
 request_id TEXT NOT NULL REFERENCES budget_reservations(request_id),
 kind TEXT NOT NULL, scope_id TEXT NOT NULL, period TEXT NOT NULL,
 PRIMARY KEY(request_id,kind,scope_id,period));
CREATE INDEX IF NOT EXISTS budget_scope_lookup ON budget_reservation_scopes(kind,scope_id,period);
CREATE INDEX IF NOT EXISTS budget_expiry_lookup ON budget_reservations(state,expires_at);
CREATE TABLE IF NOT EXISTS budget_accounts(
 kind TEXT NOT NULL, scope_id TEXT NOT NULL, period TEXT NOT NULL,
 settled_micro_usd INTEGER NOT NULL CHECK(settled_micro_usd>=0),
 reserved_micro_usd INTEGER NOT NULL CHECK(reserved_micro_usd>=0),
 PRIMARY KEY(kind,scope_id,period));
UPDATE usage_entries SET cost_micro_usd=CAST(ceil(cost_usd*1000000) AS INTEGER)
 WHERE cost_micro_usd=0 AND cost_usd>0 AND cost_usd<9223372036854;
`)
	if err != nil {
		return fmt.Errorf("budget schema: %w", err)
	}
	var version int
	err = s.db.QueryRow(`SELECT version FROM schema_versions WHERE component='usage'`).Scan(&version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if version > 2 {
		return fmt.Errorf("unsupported usage schema version %d", version)
	}
	_, err = s.db.Exec(`INSERT INTO schema_versions(component,version) VALUES('usage',2) ON CONFLICT(component) DO UPDATE SET version=2`)
	return err
}

func scopeColumn(scope BudgetScope) (string, error) {
	switch scope.Kind {
	case "token_day", "token_month":
		return "token_id", nil
	case "org_month":
		return "organization_id", nil
	case "project_month":
		return "project_id", nil
	default:
		return "", fmt.Errorf("unknown budget scope %q", scope.Kind)
	}
}

func scopeWindow(scope BudgetScope) (time.Time, time.Time, error) {
	layout := "2006-01"
	if scope.Kind == "token_day" {
		layout = "2006-01-02"
	}
	begin, err := time.Parse(layout, scope.Period)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end := begin.AddDate(0, 1, 0)
	if scope.Kind == "token_day" {
		end = begin.AddDate(0, 0, 1)
	}
	return begin, end, nil
}

// ReserveBudget serializes across independent processes with BEGIN IMMEDIATE.
// Expired reservations remain conservatively charged at their estimate: a
// crashed process may have already spent it. A late settlement replaces this
// charge atomically; expiry never silently invents a refund.
func (s *SQLiteStore) ReserveBudget(ctx context.Context, request BudgetReservation) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("usage sqlite store is unavailable")
	}
	if strings.TrimSpace(request.ID) == "" || request.EstimateMicroUSD <= 0 || request.ExpiresAt.IsZero() {
		return fmt.Errorf("invalid durable reservation")
	}
	for _, scope := range request.Scopes {
		if scope.ID == "" || scope.LimitMicroUSD <= 0 {
			return fmt.Errorf("invalid budget scope")
		}
		if _, err := scopeColumn(scope); err != nil {
			return err
		}
		if _, _, err := scopeWindow(scope); err != nil {
			return err
		}
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("reserve transaction: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	var exists int
	if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM budget_reservations WHERE request_id=?`, request.ID).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return ErrReservationExists
	}
	if _, err = conn.ExecContext(ctx, `UPDATE budget_reservations SET state='expired' WHERE state='pending' AND expires_at<=?`, time.Now().UTC().Format(sinceBoundaryLayout)); err != nil {
		return err
	}
	for _, scope := range request.Scopes {
		settled, reserved, err := seedBudgetAccount(ctx, conn, scope)
		if err != nil {
			return err
		}

		if settled >= scope.LimitMicroUSD || reserved > scope.LimitMicroUSD-settled || request.EstimateMicroUSD > scope.LimitMicroUSD-settled-reserved {
			return fmt.Errorf("%w: %s %s", ErrBudgetExceeded, scope.Kind, scope.ID)
		}
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO budget_reservations(request_id,estimate_micro_usd,expires_at,state) VALUES(?,?,?,'pending')`, request.ID, request.EstimateMicroUSD, request.ExpiresAt.UTC().Format(sinceBoundaryLayout)); err != nil {
		return err
	}
	for _, scope := range request.Scopes {
		if _, err = conn.ExecContext(ctx, `INSERT INTO budget_reservation_scopes(request_id,kind,scope_id,period) VALUES(?,?,?,?)`, request.ID, scope.Kind, scope.ID, scope.Period); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, `UPDATE budget_accounts SET reserved_micro_usd=reserved_micro_usd+? WHERE kind=? AND scope_id=? AND period=?`, request.EstimateMicroUSD, scope.Kind, scope.ID, scope.Period); err != nil {
			return err
		}
	}

	_, err = conn.ExecContext(ctx, `COMMIT`)
	return err
}

// seedBudgetAccount scans the ledger only on the first admission to a scope.
// Thereafter materialized integer totals are maintained in usage transactions,
// avoiding an O(number of historical requests) scan on every request.
func seedBudgetAccount(ctx context.Context, conn *sql.Conn, scope BudgetScope) (int64, int64, error) {
	var settled, reserved int64
	err := conn.QueryRowContext(ctx, `SELECT settled_micro_usd,reserved_micro_usd FROM budget_accounts WHERE kind=? AND scope_id=? AND period=?`, scope.Kind, scope.ID, scope.Period).Scan(&settled, &reserved)
	if err == nil {
		return settled, reserved, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, err
	}
	column, _ := scopeColumn(scope)
	begin, end, _ := scopeWindow(scope)
	query := `SELECT COALESCE(SUM(cost_micro_usd),0) FROM usage_entries WHERE ` + column + `=? AND timestamp>=? AND timestamp<?`
	if err = conn.QueryRowContext(ctx, query, scope.ID, begin.Format(sinceBoundaryLayout), end.Format(sinceBoundaryLayout)).Scan(&settled); err != nil {
		return 0, 0, err
	}
	if err = conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(r.estimate_micro_usd),0) FROM budget_reservations r JOIN budget_reservation_scopes s ON s.request_id=r.request_id WHERE s.kind=? AND s.scope_id=? AND s.period=? AND r.state IN ('pending','expired')`, scope.Kind, scope.ID, scope.Period).Scan(&reserved); err != nil {
		return 0, 0, err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO budget_accounts(kind,scope_id,period,settled_micro_usd,reserved_micro_usd) VALUES(?,?,?,?,?)`, scope.Kind, scope.ID, scope.Period, settled, reserved)
	return settled, reserved, err
}

func entryScopes(entry Entry) []BudgetScope {
	month := entry.Timestamp.UTC().Format("2006-01")
	return []BudgetScope{{Kind: "token_day", ID: entry.TokenID, Period: entry.Timestamp.UTC().Format("2006-01-02")}, {Kind: "token_month", ID: entry.TokenID, Period: month}, {Kind: "org_month", ID: entry.OrganizationID, Period: month}, {Kind: "project_month", ID: entry.ProjectID, Period: month}}
}

// logAndSettle records usage and releases a durable reservation atomically.
// Repeating the callback cannot duplicate either the usage or the charge.
func (s *SQLiteStore) logAndSettle(entry Entry) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var estimate int64
	if entry.ReservationID != "" {
		if math.IsNaN(entry.CostUSD) || math.IsInf(entry.CostUSD, 0) || entry.CostUSD < 0 {
			return fmt.Errorf("invalid settlement cost")
		}
		if _, err = tx.Exec(`UPDATE budget_reservations SET state=state WHERE request_id=?`, entry.ReservationID); err != nil {
			return err
		}
		var state string
		if err = tx.QueryRow(`SELECT state,estimate_micro_usd FROM budget_reservations WHERE request_id=?`, entry.ReservationID).Scan(&state, &estimate); err != nil {
			return fmt.Errorf("settle reservation: %w", err)
		}
		if state == "settled" {
			return nil
		}
		rows, err := tx.Query(`SELECT kind,scope_id,period FROM budget_reservation_scopes WHERE request_id=?`, entry.ReservationID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var kind, id, period string
			if err = rows.Scan(&kind, &id, &period); err != nil {
				rows.Close()
				return err
			}
			matched := false
			for _, scope := range entryScopes(entry) {
				if scope.Kind == kind && scope.ID == id && scope.Period == period {
					matched = true
				}
			}
			if !matched {
				rows.Close()
				return fmt.Errorf("settlement identity/period differs from reservation")
			}
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	if entry.UncertainCost {
		entry.EstimatedCost = true
	}
	if entry.UncertainCost && estimate > MicroUSD(entry.CostUSD) {
		entry.CostUSD = float64(estimate) / 1_000_000
		entry.EstimatedCost = true
	}
	if err = insertUsage(tx, entry); err != nil {
		return err
	}
	actual := MicroUSD(entry.CostUSD)
	if actual > 0 {
		for _, scope := range entryScopes(entry) {
			if scope.ID == "" {
				continue
			}
			if _, err = tx.Exec(`UPDATE budget_accounts SET settled_micro_usd=settled_micro_usd+? WHERE kind=? AND scope_id=? AND period=?`, actual, scope.Kind, scope.ID, scope.Period); err != nil {
				return err
			}
		}
	}
	if entry.ReservationID != "" {
		if _, err = tx.Exec(`UPDATE budget_accounts SET reserved_micro_usd=reserved_micro_usd-? WHERE (kind,scope_id,period) IN (SELECT kind,scope_id,period FROM budget_reservation_scopes WHERE request_id=?)`, estimate, entry.ReservationID); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE budget_reservations SET state='settled',actual_micro_usd=? WHERE request_id=?`, actual, entry.ReservationID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
