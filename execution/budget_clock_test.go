package execution

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A ledger written while the clock was ahead must not stop dispatch or lease
// cleanup, and Go and Python must agree on the daily bucket. The Python side is
// makewand/layered_budget.py.

func editTestLedger(t *testing.T, path string, edit func(map[string]any)) {
	t.Helper()
	err := withLockedFile(t.Context(), path, nil, func(p *accountingPath) error {
		data, err := readLedger(p, 10)
		if err != nil {
			return err
		}
		edit(data)
		return saveLedger(p, data)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func seconds(t time.Time) json.Number {
	return json.Number(strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', -1, 64))
}

// fixLedgerNow pins the budget wall clock; only for tests without heartbeat goroutines.
func fixLedgerNow(t *testing.T, now *time.Time) {
	t.Helper()
	previous := ledgerNow
	ledgerNow = func() time.Time { return *now }
	t.Cleanup(func() { ledgerNow = previous })
}

func recordSkewReports(t *testing.T) (*[]float64, *[]string) {
	t.Helper()
	previousClock, previousDay := reportClockSkew, reportDaySkew
	var leads []float64
	var days []string
	reportClockSkew = func(lead float64) { leads = append(leads, lead) }
	reportDaySkew = func(today, latest string) { days = append(days, today+" < "+latest) }
	t.Cleanup(func() { reportClockSkew, reportDaySkew = previousClock, previousDay })
	return &leads, &days
}

func TestBudgetBackwardDayKeepsCountingIntoTheLatestBucket(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv(budgetTimezoneEnv, "UTC")
	t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "3")
	_, days := recordSkewReports(t)
	reserve := func(task string) error {
		ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 10, TaskID: task})
		a, err := Reserve(ctx, Metadata{Engine: "fake"})
		if err == nil {
			err = a.Complete(Outcome{Status: Passed, Known: true})
		}
		return err
	}
	if err := reserve("before"); err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	tomorrow := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
	// The ledger was last written while the clock read tomorrow.
	editTestLedger(t, path, func(data map[string]any) { data["scopes"].(map[string]any)["day"] = tomorrow })
	for _, task := range []string{"after-1", "after-2"} {
		if err := reserve(task); err != nil {
			t.Fatalf("backward day refused dispatch: %v", err)
		}
	}
	scopes := readTestLedger(t, path)["scopes"].(map[string]any)
	machine := scopes["machine"].(map[string]any)
	if scopes["day"] != tomorrow || machine[tomorrow].(map[string]any)["used"] != float64(2) || machine[today].(map[string]any)["used"] != float64(1) {
		t.Fatalf("calls were not counted into the latest bucket: %v", scopes)
	}
	if len(*days) == 0 {
		t.Fatal("backward day was not reported")
	}
	// The finding's sequence: calls charged into tomorrow's bucket were made today,
	// so 1 + 2 reach today's cap of 3 (a fourth call used to be admitted).
	if err := reserve("after-3"); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("daily cap exceeded after a backward day: %v", err)
	}
}

// admitAt runs the layered admission check of Reserve against data at now and
// charges the call; tests use it to move the clock without heartbeat goroutines.
func admitAt(data map[string]any, task string, now time.Time) error {
	keys, err := scopeKeys(data, Config{TaskID: task, MaxCalls: 100}, now)
	if err != nil {
		return err
	}
	available, err := scopeAvailable(data, keys, nil, nil, now)
	if err != nil {
		return err
	}
	if available <= 0 {
		return ErrBudgetExhausted
	}
	scopeSpend(data, keys, 1)
	return nil
}

func TestBudgetBackwardDayCapCountsEveryBucketFromTodayToTheLatest(t *testing.T) {
	layeredFixture(t)
	t.Setenv(budgetTimezoneEnv, "UTC")
	_, _ = recordSkewReports(t)
	noon := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, scope := range []string{"MAKEWAND_MACHINE_DAILY_CALLS", "MAKEWAND_PROJECT_DAILY_CALLS"} {
		t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "100")
		t.Setenv("MAKEWAND_PROJECT_DAILY_CALLS", "100")
		t.Setenv(scope, "3")
		data := map[string]any{"attempts": []any{}}
		if err := initializeScopes(data); err != nil {
			t.Fatal(err)
		}
		// One call today; the clock briefly reads tomorrow (a capacity check writes the
		// latest day then), and real time again.
		if err := admitAt(data, "before", noon); err != nil {
			t.Fatal(err)
		}
		if _, err := scopeKeys(data, Config{TaskID: "check", MaxCalls: 100}, noon.AddDate(0, 0, 1)); err != nil {
			t.Fatal(err)
		}
		for _, task := range []string{"after-1", "after-2"} {
			if err := admitAt(data, task, noon.Add(time.Minute)); err != nil {
				t.Fatalf("%s: backward day refused dispatch: %v", scope, err)
			}
		}
		if err := admitAt(data, "after-3", noon.Add(time.Minute)); !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("%s: a fourth call was admitted today: %v", scope, err)
		}
		// On the real next day those two calls count again (conservative): one call is left.
		next := noon.AddDate(0, 0, 1).Add(time.Minute)
		if err := admitAt(data, "next-day", next); err != nil {
			t.Fatalf("%s: %v", scope, err)
		}
		if err := admitAt(data, "next-day-2", next); !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("%s: next day's cap exceeded: %v", scope, err)
		}
	}
}

func testCounter(maximum, used int) map[string]any {
	return map[string]any{"maximum": json.Number(strconv.Itoa(maximum)), "used": json.Number(strconv.Itoa(used))}
}

func TestBudgetBackwardDayCapCountsHoldsOnEveryBucketFromToday(t *testing.T) {
	layeredFixture(t)
	t.Setenv(budgetTimezoneEnv, "UTC")
	noon := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	today, tomorrow := "2026-10-15", "2026-10-16"
	data := map[string]any{"attempts": []any{}, "scopes": map[string]any{
		"tasks":    map[string]any{"t": testCounter(100, 0)},
		"projects": map[string]any{"p:" + today: testCounter(100, 1), "p:" + tomorrow: testCounter(100, 0)},
		"machine":  map[string]any{today: testCounter(3, 1), tomorrow: testCounter(3, 0)},
		"day":      tomorrow, "clock": json.Number("0"),
	}}
	keys := []any{[]any{"tasks", "t"}, []any{"projects", "p:" + tomorrow}, []any{"machine", tomorrow}}
	// A hold made today, before the clock jumped, still occupies today's capacity.
	hold := map[string]any{"remaining": json.Number("1"), "scopes": []any{[]any{"tasks", "t"}, []any{"projects", "p:" + today}, []any{"machine", today}}}
	for _, c := range []struct {
		now   time.Time
		claim map[string]any
		want  int
	}{{noon.Add(time.Minute), nil, 1}, {noon.Add(time.Minute), hold, 2}, {noon.AddDate(0, 0, 1), nil, 3}} {
		got, err := scopeAvailable(data, keys, map[string]any{"h": hold}, c.claim, c.now)
		if err != nil || got != c.want {
			t.Fatalf("at %v (claim %v): %d %v, want %d", c.now, c.claim != nil, got, err, c.want)
		}
	}
}

func TestBudgetForwardSkewDeadLeaseBecomesReclaimable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux boot identity fixture")
	}
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	leads, _ := recordSkewReports(t)
	start := time.Now()
	now := start
	fixLedgerNow(t, &now)
	data := map[string]any{"scopes": map[string]any{"clock": seconds(start.Add(2 * time.Hour))}}
	lease, err := admitLease(data, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	limit := float64(start.UnixNano())/1e9 + clockSkewSlack + leaseTTL.Seconds()
	if expires := lease["expires_at"].(float64); expires > limit+1e-6 {
		t.Fatalf("lease admitted under a skewed clock expires %.0fs after real time", expires-float64(start.UnixNano())/1e9)
	}
	if len(*leads) == 0 || (*leads)[0] < 7000 {
		t.Fatalf("clock skew not reported: %v", *leads)
	}
	// Its owner dies (a previous boot) without releasing the lease.
	owner := leaseProcessRef(os.Getpid())
	owner["boot"] = "previous-boot"
	lease["owner"] = owner
	lease["expires_at"] = json.Number(strconv.FormatFloat(lease["expires_at"].(float64), 'f', -1, 64))
	data["leases"] = map[string]any{"dead": lease}
	if _, err := admitLease(data, "codex", nil); !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatalf("an unexpired lease was reclaimed: %v", err)
	}
	// Once real time passes its expiry plus the slack, the dead lease is reclaimed.
	now = start.Add(time.Duration(clockSkewSlack)*time.Second + leaseTTL + time.Second)
	if _, err := admitLease(data, "codex", nil); err != nil {
		t.Fatalf("dead lease from a skewed clock still blocks: %v", err)
	}
	if _, exists := data["leases"].(map[string]any)["dead"]; exists {
		t.Fatal("dead lease retained")
	}
}

func TestBudgetLeaseWrittenWhileTheClockWasAheadExpiresWithoutRepair(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux boot identity fixture")
	}
	layeredFixture(t)
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	_, _ = recordSkewReports(t)
	start := time.Now()
	now := start
	fixLedgerNow(t, &now)
	data := map[string]any{}
	if err := initializeScopes(data); err != nil {
		t.Fatal(err)
	}
	// A writer whose own clock read two hours ahead left a dead owner's lease.
	data["scopes"].(map[string]any)["clock"] = seconds(start.Add(2 * time.Hour))
	owner := leaseProcessRef(os.Getpid())
	owner["boot"] = "previous-boot"
	data["leases"] = map[string]any{"dead": map[string]any{
		"keys": leaseKeys("codex", nil), "owner": owner, "workers": []any{}, "launch_pending": false,
		"expires_at": seconds(start.Add(2*time.Hour + leaseTTL)),
	}}
	cfg := Config{TaskID: "skewed", MaxCalls: 4}
	if _, err := scopeKeys(data, cfg, now); err != nil {
		t.Fatal(err)
	}
	if _, err := admitLease(data, "codex", nil); !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatalf("an unexpired lease was reclaimed: %v", err)
	}
	// Admission dated it like a fresh heartbeat, at most the slack plus one TTL ahead.
	expires := ledgerNumber(data["leases"].(map[string]any)["dead"].(map[string]any)["expires_at"])
	if wall := float64(start.UnixNano()) / 1e9; expires > wall+clockSkewSlack+leaseTTL.Seconds()+1e-6 {
		t.Fatalf("skewed lease still expires %.0fs after real time", expires-wall)
	}
	now = time.Unix(0, int64((expires+1)*1e9))
	if _, err := scopeKeys(data, cfg, now); err != nil {
		t.Fatal(err)
	}
	if _, err := admitLease(data, "codex", nil); err != nil {
		t.Fatalf("dead lease from a skewed writer still blocks: %v", err)
	}
	if _, exists := data["leases"].(map[string]any)["dead"]; exists {
		t.Fatal("dead lease retained")
	}
}

func TestBudgetLeadingClockDoesNotExpireAFreshHold(t *testing.T) {
	_, _ = recordSkewReports(t)
	now := time.Now()
	data := map[string]any{
		"attempts": []any{},
		"scopes":   map[string]any{"tasks": map[string]any{}, "projects": map[string]any{}, "machine": map[string]any{}, "day": "", "clock": seconds(now.Add(2 * time.Hour))},
		"holds":    map[string]any{"review": map[string]any{"remaining": json.Number("1"), "expires_at": seconds(now.Add(10 * time.Minute))}},
	}
	compactLedger(data)
	if _, kept := data["holds"].(map[string]any)["review"]; !kept {
		t.Fatal("a hold with ten minutes left was expired by the skewed clock")
	}
}

func TestBudgetRedateHoldKeepsItsDuration(t *testing.T) {
	clock := 1792065600.0
	for _, c := range []struct {
		name                     string
		dated                    bool
		created, expires         float64
		wantCreated, wantExpires float64
	}{
		{"dated ahead", true, clock + 7200, clock + 7800, clock, clock + 600},
		{"dated at the clock", true, clock, clock + 600, clock, clock + 600},
		{"dated before", true, clock - 60, clock + 540, clock - 60, clock + 540},
		// Written before holds carried created_at: as long as the longest hold.
		{"undated within a day", false, 0, clock + 7200, 0, clock + 7200},
		{"undated beyond a day", false, 0, clock + 3*86400, 0, clock + 86400},
	} {
		hold := map[string]any{"remaining": json.Number("1"), "expires_at": json.Number(strconv.FormatFloat(c.expires, 'f', -1, 64))}
		if c.dated {
			hold["created_at"] = json.Number(strconv.FormatFloat(c.created, 'f', -1, 64))
		}
		expires := redateHold(hold, c.expires, clock)
		if expires != c.wantExpires || ledgerNumber(hold["expires_at"]) != c.wantExpires || ledgerNumber(hold["created_at"]) != c.wantCreated {
			t.Fatalf("%s: %v %v", c.name, expires, hold)
		}
		if _, dated := hold["created_at"]; dated != c.dated {
			t.Fatalf("%s: created_at added or removed: %v", c.name, hold)
		}
	}
}

func TestBudgetHoldWrittenAheadStopsBlockingAfterItsDurationAndTheSlack(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv(budgetTimezoneEnv, "UTC")
	t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "1")
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	_, _ = recordSkewReports(t)
	start := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	now := start
	fixLedgerNow(t, &now)
	// A one-call hold made by a process whose clock read two hours ahead, and a live
	// lease on the account: a call the hold no longer blocks is refused as busy, so
	// no admission here starts a heartbeat.
	editTestLedger(t, path, func(data map[string]any) {
		keys, err := scopeKeys(data, Config{TaskID: "holder", MaxCalls: 10}, start.Add(2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		data["holds"] = map[string]any{"review": map[string]any{"remaining": json.Number("1"), "scopes": keys,
			"created_at": seconds(start.Add(2 * time.Hour)), "expires_at": seconds(start.Add(2*time.Hour + 10*time.Minute))}}
		data["leases"] = map[string]any{"live": map[string]any{"keys": leaseKeys("codex", nil), "owner": leaseProcessRef(os.Getpid()),
			"workers": []any{}, "launch_pending": false, "expires_at": seconds(start.Add(leaseTTL))}}
	})
	reserve := func() error {
		ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 10, TaskID: "other"})
		_, err := Reserve(ctx, Metadata{Engine: "codex"})
		return err
	}
	if err := reserve(); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("the hold did not block: %v", err)
	}
	// Re-dated like a lease: created at the effective clock, at most the slack ahead.
	hold := readTestLedger(t, path)["holds"].(map[string]any)["review"].(map[string]any)
	wall := float64(start.UnixNano()) / 1e9
	if hold["created_at"] != wall+clockSkewSlack || hold["expires_at"] != wall+clockSkewSlack+600 {
		t.Fatalf("hold written two hours ahead was not re-dated keeping its duration: %v", hold)
	}
	now = start.Add(10 * time.Minute)
	if err := reserve(); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("re-dated hold expired early: %v", err)
	}
	now = start.Add(time.Duration(clockSkewSlack)*time.Second + 10*time.Minute + time.Second)
	if err := reserve(); !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatalf("hold written two hours ahead still blocks after its ten minutes and the slack: %v", err)
	}
}

// Go and Python accept and refuse the same hold creation times. A missing one is a
// hold written before holds were dated; one after the expiry is merely expired.
func TestBudgetHoldCreationTimeValidationMatchesPython(t *testing.T) {
	cases := []struct {
		field string
		valid bool
	}{
		{"", true}, {`, "created_at": 1792065600.5`, true}, {`, "created_at": 1792069200`, true},
		{`, "created_at": -1`, false}, {`, "created_at": "1792065600"`, false},
		{`, "created_at": null`, false}, {`, "created_at": true`, false},
	}
	var ledgers []string
	for _, c := range cases {
		ledgers = append(ledgers, `{"schema": 1, "maximum": 3, "attempts": [], "holds": {"x": {"remaining": 1, "expires_at": 1792066200`+c.field+`}}}`)
	}
	encoded, err := json.Marshal(ledgers)
	if err != nil {
		t.Fatal(err)
	}
	output, err := pythonCommand(t, `import json
from makewand import call_budget
def valid(text):
    try:
        call_budget._validate(json.loads(text))
    except call_budget.BudgetError:
        return False
    return True
print("VALID " + json.dumps([valid(text) for text in json.loads(sys.argv[2])]))`, string(encoded)).CombinedOutput()
	if err != nil {
		t.Fatalf("python: %v %s", err, output)
	}
	text, found := strings.CutPrefix(strings.TrimSpace(string(output)), "VALID ")
	var python []bool
	if !found || json.Unmarshal([]byte(text), &python) != nil || len(python) != len(cases) {
		t.Fatalf("no validity line in %s", output)
	}
	for i, c := range cases {
		decoder := json.NewDecoder(strings.NewReader(ledgers[i]))
		decoder.UseNumber()
		var data map[string]any
		if err := decoder.Decode(&data); err != nil {
			t.Fatal(err)
		}
		if goValid := validateRetention(data) == nil; goValid != c.valid || python[i] != c.valid {
			t.Fatalf("created_at%q: Go valid %v, Python valid %v, want %v", c.field, goValid, python[i], c.valid)
		}
	}
}

// pythonBudgetLedger runs Python budget code against the ledger at path and
// returns its last output line.
func pythonBudgetLedger(t *testing.T, path, code string) string {
	t.Helper()
	output, err := pythonCommand(t, code, path).CombinedOutput()
	if err != nil {
		t.Fatalf("python: %v %s", err, output)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	return lines[len(lines)-1]
}

func TestBudgetRepairedFutureDayHoldStillHoldsForGo(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv(budgetTimezoneEnv, "UTC")
	t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "1")
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", path)
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "100") // Python refuses an empty value
	t.Setenv("MAKEWAND_EXECUTION_EVENTS_FILE", "")
	t.Setenv("MAKEWAND_CALL_BUDGET_LEASE_ID", "")
	// Python makes a one-call hold while the clock reads tomorrow; the operator repairs
	// the ledger. The finding: repair left the hold in tomorrow's bucket, so today's
	// capacity looked free and the holder's own call was later refused.
	lease := pythonBudgetLedger(t, path, `import time
from unittest.mock import patch
from makewand import call_budget as b
from makewand.execution_runtime import execution_context
real = time.time()
with patch("time.time", return_value=real + 86400), execution_context(task_id="holder"):
    hold = b.reserve_capacity(1, "review")
b.repair_clock(apply=True)
print(hold)`)
	other := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 10, TaskID: "other"})
	if _, err := Reserve(other, Metadata{Engine: "fake"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("another task took the held call: %v", err)
	}
	holder := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 10, TaskID: "holder", LeaseID: lease})
	attempt, err := Reserve(holder, Metadata{Engine: "fake", Stage: "review"})
	if err != nil {
		t.Fatalf("the holder could not claim its call: %v", err)
	}
	if err := attempt.Complete(Outcome{Status: Passed, Known: true}); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetPythonHoldUnderThePermittedLeadIsClaimableFromGo(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv(budgetTimezoneEnv, "UTC")
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", path)
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "100") // Python refuses an empty value
	t.Setenv("MAKEWAND_EXECUTION_EVENTS_FILE", "")
	t.Setenv("MAKEWAND_CALL_BUDGET_LEASE_ID", "")
	// A 60-second hold made while the ledger clock leads by the permitted 300 seconds.
	// Dated from wall time, the compaction of its own save used to delete it.
	lease := pythonBudgetLedger(t, path, `import json, time
from pathlib import Path
from makewand import call_budget as b
from makewand.execution_runtime import execution_context
b.complete(b.reserve("fake", "fast", task_id="seed"), True, 0)
ledger = Path(sys.argv[2])
data = json.loads(ledger.read_text())
data["scopes"]["clock"] = time.time() + 300
ledger.write_text(json.dumps(data))
with execution_context(task_id="holder"):
    print(b.reserve_capacity(1, "review", ttl_seconds=60))`)
	holder := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 10, TaskID: "holder", LeaseID: lease})
	attempt, err := Reserve(holder, Metadata{Engine: "fake", Stage: "review"})
	if err != nil {
		t.Fatalf("Go could not claim the fresh hold: %v", err)
	}
	if err := attempt.Complete(Outcome{Status: Passed, Known: true}); err != nil {
		t.Fatal(err)
	}
}

// The day-cap check and hold re-dating agree with makewand/layered_budget.py on one ledger.
func TestBudgetDayCapAndHoldRedatingMatchPython(t *testing.T) {
	t.Setenv(budgetTimezoneEnv, "UTC")
	const fixture = `{"attempts": [], "scopes": {
 "tasks": {"t": {"maximum": 100, "used": 0}},
 "projects": {"p:2026-10-14": {"maximum": 100, "used": 7}, "p:2026-10-15": {"maximum": 100, "used": 1},
              "p:2026-10-16": {"maximum": 4, "used": 1}, "q:2026-10-15": {"maximum": 100, "used": 50}},
 "machine": {"2026-10-14": {"maximum": 3, "used": 3}, "2026-10-15": {"maximum": 6, "used": 1},
             "2026-10-16": {"maximum": 5, "used": 1}},
 "day": "2026-10-16", "clock": 1792152000},
 "holds": {
 "today": {"remaining": 1, "created_at": 1792065600, "expires_at": 1792066200.25,
           "scopes": [["tasks", "t"], ["projects", "p:2026-10-15"], ["machine", "2026-10-15"]]},
 "latest": {"remaining": 2, "created_at": 1792152000.5, "expires_at": 1792152600.75,
            "scopes": [["tasks", "t"], ["projects", "p:2026-10-16"], ["machine", "2026-10-16"]]},
 "undated": {"remaining": 1, "expires_at": 1792411200.125, "scopes": null}}}`
	moments := []int64{1791979200, 1792065600, 1792108799, 1792108800, 1792152000}
	clocks := []float64{1791979200, 1792065600.5, 1792152300}
	keys := []any{[]any{"tasks", "t"}, []any{"projects", "p:2026-10-16"}, []any{"machine", "2026-10-16"}}
	python := `import copy, json
from makewand import layered_budget
data = json.loads(sys.argv[2])
moments, clocks, keys = json.loads(sys.argv[3]), json.loads(sys.argv[4]), json.loads(sys.argv[5])
available = [[layered_budget.available(data, keys, data["holds"], data["holds"].get(claim), now=now)
              for claim in ("", "today", "latest")] for now in moments]
redated = {}
for name, hold in data["holds"].items():
    for clock in clocks:
        copied = copy.deepcopy(hold)
        layered_budget.redate_hold(copied, clock)
        redated.setdefault(name, []).append([copied.get("created_at"), copied["expires_at"]])
print("PARITY " + json.dumps({"available": available, "redated": redated}))`
	encode := func(v any) string {
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	output, err := pythonCommand(t, python, fixture, encode(moments), encode(clocks), encode(keys)).CombinedOutput()
	if err != nil {
		t.Fatalf("python: %v %s", err, output)
	}
	text, found := strings.CutPrefix(strings.TrimSpace(string(output)), "PARITY ")
	if !found {
		t.Fatalf("no parity line in %s", output)
	}
	var want struct {
		Available [][]int                 `json:"available"`
		Redated   map[string][][]*float64 `json:"redated"`
	}
	if err := json.Unmarshal([]byte(text), &want); err != nil {
		t.Fatal(err)
	}
	decode := func() map[string]any {
		decoder := json.NewDecoder(strings.NewReader(fixture))
		decoder.UseNumber()
		var data map[string]any
		if err := decoder.Decode(&data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	data := decode()
	holds := data["holds"].(map[string]any)
	for i, moment := range moments {
		for j, claim := range []string{"", "today", "latest"} {
			claimed, _ := holds[claim].(map[string]any)
			got, err := scopeAvailable(data, keys, holds, claimed, time.Unix(moment, 0))
			python := want.Available[i][j]
			// Go refuses outright when holds exceed a cap; Python then leaves nothing either.
			if agree := err == nil && got == python || errors.Is(err, ErrBudgetExhausted) && python <= 0; !agree {
				t.Fatalf("at %d, claim %q: Go %d %v, Python %d", moment, claim, got, err, python)
			}
		}
	}
	for name := range holds {
		for k, clock := range clocks {
			hold := decode()["holds"].(map[string]any)[name].(map[string]any)
			redateHold(hold, ledgerNumber(hold["expires_at"]), clock)
			pair := want.Redated[name][k]
			_, dated := hold["created_at"]
			if dated != (pair[0] != nil) || dated && ledgerNumber(hold["created_at"]) != *pair[0] || ledgerNumber(hold["expires_at"]) != *pair[1] {
				t.Fatalf("hold %s at %v: Go %v, Python %v", name, clock, hold, pair)
			}
		}
	}
}

func TestBudgetAdmissionCapsAndPersistsTheLeadingClock(t *testing.T) {
	layeredFixture(t)
	leads, _ := recordSkewReports(t)
	data := map[string]any{}
	if err := initializeScopes(data); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	data["scopes"].(map[string]any)["clock"] = seconds(now.Add(2 * time.Hour))
	if _, err := scopeKeys(data, Config{TaskID: "heal", MaxCalls: 4}, now); err != nil {
		t.Fatal(err)
	}
	clock := ledgerNumber(data["scopes"].(map[string]any)["clock"])
	wall := float64(now.UnixNano()) / 1e9
	if clock < wall || clock > wall+clockSkewSlack+1e-6 {
		t.Fatalf("persisted clock leads real time by %.0fs", clock-wall)
	}
	if len(*leads) != 1 {
		t.Fatalf("skew reports: %v", *leads)
	}
	// A clock that steps back within the slack still cannot move the budget clock back.
	data["scopes"].(map[string]any)["clock"] = seconds(now)
	if _, err := scopeKeys(data, Config{TaskID: "heal", MaxCalls: 4}, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ledgerNumber(data["scopes"].(map[string]any)["clock"]) != wall {
		t.Fatal("budget clock moved backwards")
	}
	if len(*leads) != 1 {
		t.Fatalf("a lead within the slack was reported: %v", *leads)
	}
}

func TestBudgetDayUsesTheConfiguredZone(t *testing.T) {
	layeredFixture(t)
	before := time.Date(2026, 10, 15, 15, 59, 59, 0, time.UTC)
	for zone, want := range map[string][2]string{
		"Asia/Shanghai":    {"2026-10-15", "2026-10-16"},
		"UTC":              {"2026-10-15", "2026-10-15"},
		"America/New_York": {"2026-10-15", "2026-10-15"},
	} {
		t.Setenv(budgetTimezoneEnv, " "+zone+" ")
		for i, moment := range []time.Time{before, before.Add(time.Second)} {
			if got, err := budgetDay(moment); err != nil || got != want[i] {
				t.Fatalf("%s %v: %q %v, want %q", zone, moment, got, err, want[i])
			}
		}
	}
	for _, invalid := range []string{"Asia/Nowhere", "Local", "../etc/passwd", "Asia"} {
		t.Setenv(budgetTimezoneEnv, invalid)
		if _, err := budgetDay(before); err == nil || !strings.Contains(err.Error(), budgetTimezoneEnv) {
			t.Fatalf("%q accepted: %v", invalid, err)
		}
		data := map[string]any{}
		if err := initializeScopes(data); err != nil {
			t.Fatal(err)
		}
		if _, err := scopeKeys(data, Config{TaskID: "zone", MaxCalls: 4}, before); err == nil {
			t.Fatalf("admission accepted %q", invalid)
		}
	}
}

// budgetDayStamps are both sides of a midnight in each tested zone, including
// the New York DST changes and the half-hour offsets of Kolkata and St. John's.
var budgetDayStamps = []int64{
	1792058399, 1792058400, 1792079999, 1792080000, 1792088999, 1792089000,
	1792108799, 1792108800, 1792117799, 1792117800, 1792123199, 1792123200,
	1793505599, 1793505600, 1793595599, 1793595600, 1805083199, 1805083200,
}

func goBudgetDays(t *testing.T) []string {
	t.Helper()
	var days []string
	for _, stamp := range budgetDayStamps {
		day, err := budgetDay(time.Unix(stamp, 0))
		if err != nil {
			t.Fatal(err)
		}
		days = append(days, day)
	}
	return days
}

// TestBudgetDayProcessHelper prints this process's buckets: its local zone comes from TZ.
func TestBudgetDayProcessHelper(t *testing.T) {
	if os.Getenv("MAKEWAND_BUDGET_DAY_HELPER") != "1" {
		return
	}
	encoded, err := json.Marshal(goBudgetDays(t))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("BUDGET_DAYS " + string(encoded))
}

func budgetDaysFromOutput(t *testing.T, output []byte) []string {
	t.Helper()
	for _, line := range strings.Split(string(output), "\n") {
		if text, found := strings.CutPrefix(line, "BUDGET_DAYS "); found {
			var days []string
			if err := json.Unmarshal([]byte(text), &days); err != nil {
				t.Fatal(err)
			}
			return days
		}
	}
	t.Fatalf("no bucket line in %s", output)
	return nil
}

func TestBudgetDayMatchesPython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TZ does not select the Go local zone on Windows")
	}
	stamps, err := json.Marshal(budgetDayStamps)
	if err != nil {
		t.Fatal(err)
	}
	python := `import json
from makewand import layered_budget
print("BUDGET_DAYS " + json.dumps([layered_budget.day_bucket(s) for s in json.loads(sys.argv[2])]))`
	zones := []string{"Asia/Shanghai", "UTC", "America/New_York", "Asia/Kolkata", "Pacific/Kiritimati", "America/St_Johns"}
	for _, zone := range zones {
		if _, err := time.LoadLocation(zone); err != nil {
			t.Skipf("time zone database unavailable: %v", err)
		}
		// The override, whatever the local zone of either process.
		t.Setenv(budgetTimezoneEnv, zone)
		want := goBudgetDays(t)
		cmd := pythonCommand(t, python, string(stamps))
		cmd.Env = append(os.Environ(), "TZ=Pacific/Honolulu")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", zone, err, output)
		}
		if got := budgetDaysFromOutput(t, output); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s override: Python %v, Go %v", zone, got, want)
		}
		// The default: each process's local zone, set by TZ.
		t.Setenv(budgetTimezoneEnv, "")
		//nolint:gosec // G204: re-execute this test binary with a fixed helper-test selector.
		goCmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestBudgetDayProcessHelper$")
		goCmd.Env = append(os.Environ(), "MAKEWAND_BUDGET_DAY_HELPER=1", "TZ="+zone)
		goOutput, err := goCmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", zone, err, goOutput)
		}
		pyCmd := pythonCommand(t, python, string(stamps))
		pyCmd.Env = append(os.Environ(), "TZ="+zone)
		pyOutput, err := pyCmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", zone, err, pyOutput)
		}
		local := budgetDaysFromOutput(t, goOutput)
		if got := budgetDaysFromOutput(t, pyOutput); strings.Join(got, ",") != strings.Join(local, ",") {
			t.Fatalf("%s local zone: Python %v, Go %v", zone, got, local)
		}
		if strings.Join(local, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: local zone %v differs from the override %v", zone, local, want)
		}
	}
	// Both languages refuse the same invalid names.
	for _, invalid := range []string{"Asia/Nowhere", "Local", "../etc/passwd"} {
		cmd := pythonCommand(t, `from makewand import layered_budget, call_budget
try: layered_budget.day_bucket(0)
except call_budget.BudgetError as error: print("REFUSED", error)
else: print("ACCEPTED")`)
		cmd.Env = append(os.Environ(), budgetTimezoneEnv+"="+invalid)
		output, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "REFUSED") {
			t.Fatalf("Python accepted %q: %v %s", invalid, err, output)
		}
	}
}
