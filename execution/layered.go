package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The budget clock rule shared with makewand/layered_budget.py: scopes.clock is
// a ratchet against a clock that steps back, but it is trusted, and stored, at
// most clockSkewSlack seconds ahead of real time, so a ledger written while the
// clock was ahead cannot stall lease cleanup. Capacity holds are dated from that
// clock, and one created ahead of it is re-dated to it, keeping its duration.
// Daily buckets use the local zone or budgetTimezoneEnv (an IANA name); while
// the real day is earlier than the latest bucket, the daily caps count every
// bucket from today to that one.
const (
	clockSkewSlack    = 300.0
	budgetTimezoneEnv = "MAKEWAND_BUDGET_TIMEZONE"
	// maxHoldSeconds is the longest hold Python's reserve_capacity makes; a hold
	// without created_at counts as this long.
	maxHoldSeconds = 86400.0
)

// ledgerNow is the wall clock of the budget rules; tests move it.
var ledgerNow = time.Now

var clockSkewWarning, daySkewWarning sync.Once

// reportClockSkew and reportDaySkew warn once per process; tests replace them.
var reportClockSkew = func(lead float64) {
	clockSkewWarning.Do(func() {
		_, _ = fmt.Fprintf(os.Stderr, "warning: call budget clock is %.0fs ahead of real time; lease and hold expiry use at most %.0fs of it (repair: makewand budget clock --repair)\n", lead, clockSkewSlack)
	})
}
var reportDaySkew = func(today, latest string) {
	daySkewWarning.Do(func() {
		_, _ = fmt.Fprintf(os.Stderr, "warning: call budget day %s is ahead of today's bucket %s; counting into %s (repair: makewand budget clock --repair)\n", latest, today, latest)
	})
}

// effectiveClock is real time, or the persisted clock when it is ahead, by at most clockSkewSlack.
func effectiveClock(data map[string]any, now time.Time) float64 {
	wall := float64(now.UnixNano()) / 1e9
	scopes, _ := data["scopes"].(map[string]any)
	if scopes == nil {
		return wall
	}
	clock := ledgerNumber(scopes["clock"])
	if clock > wall+clockSkewSlack {
		reportClockSkew(clock - wall)
		return wall + clockSkewSlack
	}
	return max(wall, clock)
}

// redateHold re-dates a capacity hold created after clock to it, keeping its
// duration, as scopeKeys re-dates a lease: one made while the clock was ahead
// then lasts as long as asked from real time, not until real time catches up.
// It returns the hold's expiry.
func redateHold(hold map[string]any, expires, clock float64) float64 {
	created, duration := expires-maxHoldSeconds, maxHoldSeconds
	raw, dated := hold["created_at"]
	if dated {
		created = ledgerNumber(raw)
		duration = expires - created
	}
	if created <= clock {
		return expires
	}
	expires = clock + duration
	hold["expires_at"] = json.Number(strconv.FormatFloat(expires, 'f', -1, 64))
	if dated {
		hold["created_at"] = json.Number(strconv.FormatFloat(clock, 'f', -1, 64))
	}
	return expires
}

// budgetDay is the daily bucket of now: the local date, or the date in
// MAKEWAND_BUDGET_TIMEZONE. Python computes the same bucket.
func budgetDay(now time.Time) (string, error) {
	location := time.Local
	if name := strings.TrimSpace(os.Getenv(budgetTimezoneEnv)); name != "" {
		var err error
		location, err = time.LoadLocation(name)
		// "Local" is Go-only: Python's zoneinfo refuses it, so both refuse.
		if err != nil || name == "Local" {
			return "", fmt.Errorf("%s must name an IANA time zone such as Asia/Shanghai or UTC", budgetTimezoneEnv)
		}
	}
	return now.In(location).Format("2006-01-02"), nil
}

// Each scope owns its limit and count. One locked atomic file replacement is
// the commit point for all three, including workflow capacity holds.
func layeredEnabled(data map[string]any) bool {
	if data != nil && data["scopes"] != nil {
		return true
	}
	return os.Getenv("MAKEWAND_LAYERED_BUDGET") != "" || os.Getenv("MAKEWAND_MACHINE_DAILY_CALLS") != "" || os.Getenv("MAKEWAND_PROJECT_DAILY_CALLS") != ""
}
func scopeLimit(name string, fallback int) (int, error) {
	if text := os.Getenv(name); text != "" {
		n, err := strconv.Atoi(text)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%s requires a positive integer", name)
		}
		return n, nil
	}
	return fallback, nil
}
func initializeScopes(data map[string]any) error {
	n, err := scopeLimit("MAKEWAND_MACHINE_DAILY_CALLS", 1000)
	if err != nil {
		return err
	}
	data["maximum"] = json.Number(strconv.Itoa(n))
	data["scopes"] = map[string]any{"tasks": map[string]any{}, "projects": map[string]any{}, "machine": map[string]any{}, "day": "", "clock": json.Number("0")}
	return nil
}
func validateScopes(data map[string]any) error {
	s, ok := data["scopes"].(map[string]any)
	if !ok {
		return fmt.Errorf("invalid layered budget state")
	}
	if _, ok := s["day"].(string); !ok {
		return fmt.Errorf("invalid budget day")
	}
	clock, valid := s["clock"].(json.Number)
	clockValue, clockErr := clock.Float64()
	if !valid || clockErr != nil || math.IsNaN(clockValue) || math.IsInf(clockValue, 0) || clockValue < 0 {
		return fmt.Errorf("invalid budget clock")
	}
	if raw, exists := data["legacy_maximum"]; exists {
		n, valid := jsonInteger(raw)
		if !valid || n <= 0 {
			return fmt.Errorf("invalid legacy budget cap")
		}
		if raw, exists := data["legacy_used"]; exists {
			n, valid := jsonInteger(raw)
			if !valid || n < 0 {
				return fmt.Errorf("invalid legacy budget usage")
			}
		}
	}
	for _, group := range []string{"tasks", "projects", "machine"} {
		counters, ok := s[group].(map[string]any)
		if !ok {
			return fmt.Errorf("invalid budget scope map")
		}
		for _, raw := range counters {
			c, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid budget scope counter")
			}
			m, valid := jsonInteger(c["maximum"])
			u, used := jsonInteger(c["used"])
			if !valid || !used || m <= 0 || u < 0 {
				return fmt.Errorf("invalid budget scope counter")
			}
		}
	}
	entries := append([]any{}, data["attempts"].([]any)...)
	if holds, ok := data["holds"].(map[string]any); ok {
		for _, h := range holds {
			entries = append(entries, h)
		}
	}
	for _, raw := range entries {
		e, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid scoped entry")
		}
		if e["scopes"] == nil {
			continue
		}
		keys, ok := e["scopes"].([]any)
		if !ok || len(keys) != 3 {
			return fmt.Errorf("invalid budget scope references")
		}
		for i, raw := range keys {
			pair, ok := raw.([]any)
			if !ok || len(pair) != 2 {
				return fmt.Errorf("invalid scope reference")
			}
			group, ok := pair[0].(string)
			if !ok || group != []string{"tasks", "projects", "machine"}[i] {
				return fmt.Errorf("invalid scope group")
			}
			key, ok := pair[1].(string)
			if !ok || s[group].(map[string]any)[key] == nil {
				return fmt.Errorf("missing scope counter")
			}
		}
	}
	return nil
}
func DefaultBudgetPath() (string, error) {
	root := os.Getenv("MAKEWAND_CONFIG_DIR")
	if root == "" {
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, ".config")
		}
		root = filepath.Join(base, "makewand")
	}
	return filepath.Join(root, "call-budget.json"), nil
}
func projectRef() (string, error) {
	root := os.Getenv("MAKEWAND_PROJECT_ROOT")
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(root))
	return hex.EncodeToString(hash[:])[:32], nil
}
func scopeKeys(data map[string]any, cfg Config, now time.Time) ([]any, error) {
	s := data["scopes"].(map[string]any)
	day, err := budgetDay(now)
	if err != nil {
		return nil, err
	}
	// A clock that moved back keeps counting into the latest bucket already seen:
	// refusing would stop every dispatch, an earlier bucket would reset counts.
	if latest := s["day"].(string); day < latest {
		reportDaySkew(day, latest)
		day = latest
	}
	s["day"] = day
	// Persisting the capped clock heals a skewed ledger on its next admission.
	clock := effectiveClock(data, now)
	s["clock"] = json.Number(strconv.FormatFloat(clock, 'f', -1, 64))
	// A lease dated past what this clock allows was written while the clock was
	// ahead. Date it like a fresh heartbeat: a live owner renews it, a dead
	// owner's lease expires one TTL from now instead of when real time catches up.
	// A hold created after this clock keeps its duration from it. Reserve
	// persists both even when it refuses the call.
	leases, _ := data["leases"].(map[string]any)
	for _, raw := range leases {
		lease := raw.(map[string]any)
		if ledgerNumber(lease["expires_at"]) > clock+leaseTTL.Seconds() {
			lease["expires_at"] = json.Number(strconv.FormatFloat(clock+leaseTTL.Seconds(), 'f', -1, 64))
		}
	}
	holds, _ := data["holds"].(map[string]any)
	for _, raw := range holds {
		hold := raw.(map[string]any)
		redateHold(hold, ledgerNumber(hold["expires_at"]), clock)
	}
	project, err := projectRef()
	if err != nil {
		return nil, err
	}
	task := cfg.TaskID
	p, err := scopeLimit("MAKEWAND_PROJECT_DAILY_CALLS", 200)
	if err != nil {
		return nil, err
	}
	m, err := scopeLimit("MAKEWAND_MACHINE_DAILY_CALLS", 1000)
	if err != nil {
		return nil, err
	}
	t := cfg.MaxCalls
	if t == 0 {
		t = 200
	}
	keys := []any{[]any{"tasks", task}, []any{"projects", project + ":" + day}, []any{"machine", day}}
	limits := []int{t, p, m}
	for i, raw := range keys {
		pair := raw.([]any)
		counters := s[pair[0].(string)].(map[string]any)
		key := pair[1].(string)
		c, exists := counters[key].(map[string]any)
		if !exists {
			c = map[string]any{"maximum": json.Number(strconv.Itoa(limits[i])), "used": json.Number("0")}
			counters[key] = c
		}
		limit, _ := jsonInteger(c["maximum"])
		c["maximum"] = json.Number(strconv.Itoa(min(limit, limits[i])))
		c["last_used"] = s["clock"]
	}
	return keys, nil
}
func containsScope(raw any, pair []any) bool {
	keys, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, raw := range keys {
		k, ok := raw.([]any)
		if ok && len(k) == 2 && k[0] == pair[0] && k[1] == pair[1] {
			return true
		}
	}
	return false
}

// daySpan lists the buckets one scope's cap counts. While the real day is
// earlier than the latest bucket (the clock moved back), the calls charged into
// the later buckets were made today: a daily scope counts every bucket from
// today to the latest.
func daySpan(counters map[string]any, group, key, today, latest string) []string {
	span := []string{key}
	if group == "tasks" || today >= latest {
		return span
	}
	prefix := key[:strings.LastIndex(key, ":")+1]
	for k := range counters {
		cut := strings.LastIndex(k, ":") + 1
		if k != key && k[:cut] == prefix && k[cut:] >= today && k[cut:] <= latest {
			span = append(span, k)
		}
	}
	return span
}
func containsAnyScope(raw any, group string, span []string) bool {
	for _, key := range span {
		if containsScope(raw, []any{group, key}) {
			return true
		}
	}
	return false
}
func scopeAvailable(data map[string]any, keys []any, holds map[string]any, claim map[string]any, now time.Time) (int, error) {
	available := math.MaxInt
	s := data["scopes"].(map[string]any)
	today, err := budgetDay(now)
	if err != nil {
		return 0, err
	}
	latest, _ := s["day"].(string)
	for _, raw := range keys {
		pair := raw.([]any)
		group := pair[0].(string)
		counters := s[group].(map[string]any)
		span := daySpan(counters, group, pair[1].(string), today, latest)
		limit, used := math.MaxInt, 0
		for _, key := range span {
			c := counters[key].(map[string]any)
			m, _ := jsonInteger(c["maximum"])
			u, _ := jsonInteger(c["used"])
			limit = min(limit, m)
			// A sum past MaxInt must not wrap around into free capacity.
			if u > math.MaxInt-used {
				used = math.MaxInt
			} else {
				used += u
			}
		}
		held := 0
		for _, raw := range holds {
			h := raw.(map[string]any)
			if containsAnyScope(h["scopes"], group, span) {
				n, _ := jsonInteger(h["remaining"])
				if n > limit-held {
					return 0, ErrBudgetExhausted
				}
				held += n
			}
		}
		if claim != nil && containsAnyScope(claim["scopes"], group, span) {
			held--
		}
		available = min(available, limit-used-held)
	}
	if raw, exists := data["legacy_maximum"]; exists {
		legacyUsed, _ := jsonInteger(data["legacy_used"])
		available -= legacyUsed
		limit, ok := jsonInteger(raw)
		if !ok || limit <= 0 {
			return 0, fmt.Errorf("invalid legacy limit")
		}
		occupied, _ := jsonInteger(data["archived_used"])
		for _, raw := range data["attempts"].([]any) {
			if raw.(map[string]any)["status"] != "released" {
				occupied++
			}
		}
		for _, raw := range holds {
			n, _ := jsonInteger(raw.(map[string]any)["remaining"])
			occupied += n
		}
		if claim != nil {
			occupied--
		}
		available = min(available, limit-occupied)
	}
	return available, nil
}
func scopeSpend(data map[string]any, raw any, delta int) {
	keys, _ := raw.([]any)
	s := data["scopes"].(map[string]any)
	for _, raw := range keys {
		pair := raw.([]any)
		c := s[pair[0].(string)].(map[string]any)[pair[1].(string)].(map[string]any)
		n, _ := jsonInteger(c["used"])
		c["used"] = json.Number(strconv.Itoa(n + delta))
		c["last_used"] = effectiveClock(data, ledgerNow())
	}
}
