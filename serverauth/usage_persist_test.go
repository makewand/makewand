package serverauth

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestTokenCountersSurviveRestart is the core R2 guarantee: a token's consumed
// request quota and accrued spend are restored after the store is closed and
// reopened (simulating a server restart), instead of resetting to zero.
func TestTokenCountersSurviveRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	now := time.Now().UTC()

	store, err := OpenSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	_, secret, err := store.Issue(TokenRule{
		Scopes:             []string{ScopeChatInvoke},
		MaxRequestsPerDay:  2,
		MaxCostUSDPerMonth: 10,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	grant, ok := store.AuthenticateHeader("Bearer " + secret)
	if !ok {
		t.Fatal("authenticate issued token failed")
	}
	// Consume the full daily request quota and record some spend.
	if err := grant.CheckAndConsumeRequestAt(now); err != nil {
		t.Fatalf("first request should pass: %v", err)
	}
	if err := grant.CheckAndConsumeRequestAt(now); err != nil {
		t.Fatalf("second request should pass: %v", err)
	}
	grant.RecordCostAt(now, 4)

	if err := store.PersistUsageCounters(); err != nil {
		t.Fatalf("PersistUsageCounters: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen: counters must be restored, not reset.
	store2, err := OpenSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()

	grant2, ok := store2.AuthenticateHeader("Bearer " + secret)
	if !ok {
		t.Fatal("authenticate after restart failed")
	}
	// The daily request quota was already exhausted before the restart.
	if err := grant2.CheckAndConsumeRequestAt(now); !errors.Is(err, ErrDailyQuotaExceeded) {
		t.Fatalf("after restart the daily quota should still be exhausted, got %v", err)
	}
	// Prior spend must also be preserved (4 of 10 already spent).
	if err := grant2.CheckCostBudgetAt(now); err != nil {
		t.Fatalf("4/10 spent should still be under budget: %v", err)
	}
	grant2.RecordCostAt(now, 6) // now 10/10
	if err := grant2.CheckCostBudgetAt(now); !errors.Is(err, ErrMonthlyCostExceeded) {
		t.Fatalf("restored spend + new spend should exceed monthly budget, got %v", err)
	}
}

// TestLoadUsageCountersFailsClosedOnCorruptTime guards that a corrupt persisted
// window timestamp fails startup rather than silently resetting an exhausted
// counter (which would let a token bypass its limit).
func TestLoadUsageCountersFailsClosedOnCorruptTime(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := OpenSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	_, secret, err := store.Issue(TokenRule{Scopes: []string{ScopeChatInvoke}, MaxRequestsPerDay: 2})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	grant, ok := store.AuthenticateHeader("Bearer " + secret)
	if !ok {
		t.Fatal("authenticate failed")
	}
	if err := grant.CheckAndConsumeRequestAt(time.Now().UTC()); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if err := store.PersistUsageCounters(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if _, err := store.db.Exec("UPDATE token_usage_counters SET quota_day_start = 'not-a-timestamp'"); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := OpenSQLiteStore(dbPath); err == nil {
		t.Fatal("reopen should fail closed on a corrupt persisted window timestamp")
	}
}

func TestParseUsageTimeRejectsWhitespace(t *testing.T) {
	if _, err := parseUsageTime(""); err != nil {
		t.Fatalf("empty string should be a valid zero time, got %v", err)
	}
	if _, err := parseUsageTime("   "); err == nil {
		t.Fatal("whitespace-only timestamp should fail to parse, not become a zero time")
	}
	if _, err := parseUsageTime("not-a-time"); err == nil {
		t.Fatal("malformed timestamp should fail to parse")
	}
}

// TestConcurrentIssueKeepsAllTokens guards the mutation/reload serialization:
// concurrent Issue calls must not drop any token via an out-of-order reload.
func TestConcurrentIssueKeepsAllTokens(t *testing.T) {
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	defer store.Close()

	const n = 12
	secrets := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, sec, e := store.Issue(TokenRule{Scopes: []string{ScopeChatInvoke}})
			secrets[i], errs[i] = sec, e
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("issue %d: %v", i, e)
		}
	}
	for i, sec := range secrets {
		if _, ok := store.AuthenticateHeader("Bearer " + sec); !ok {
			t.Fatalf("token %d was dropped by a concurrent Issue reload race", i)
		}
	}
}

// TestPersistSkipsEmptyCounters keeps the counters table free of rows for tokens
// that have never accrued usage.
func TestPersistSkipsEmptyCounters(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := OpenSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	defer store.Close()
	if _, _, err := store.Issue(TokenRule{Scopes: []string{ScopeChatInvoke}, MaxRequestsPerDay: 5}); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := store.PersistUsageCounters(); err != nil {
		t.Fatalf("PersistUsageCounters: %v", err)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM token_usage_counters").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("token with no usage should not be persisted, got %d rows", count)
	}
}
