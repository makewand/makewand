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
	// Counts are never reset: the latest bucket keeps its limit.
	if err := reserve("after-3"); err != nil {
		t.Fatal(err)
	}
	if err := reserve("after-4"); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("latest bucket limit not enforced: %v", err)
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
