package execution

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const ledgerRetention = 48 * time.Hour

func ledgerNumber(value any) float64 {
	switch n := value.(type) {
	case json.Number:
		f, _ := n.Float64()
		return f
	case float64:
		return n
	}
	return 0
}

func retentionNumber(value any) bool {
	switch value.(type) {
	case json.Number, float64:
		n := ledgerNumber(value)
		return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0
	}
	return false
}

// compactLedger runs under the ledger lock before encoding. It never removes
// an unsettled attempt or any attempt with a remaining lifecycle lease.
func compactLedger(data map[string]any) {
	now := effectiveClock(data, ledgerNow())
	scopes, _ := data["scopes"].(map[string]any)
	cutoff := now - ledgerRetention.Seconds()
	leases, _ := data["leases"].(map[string]any)
	archived, _ := jsonInteger(data["archived_used"])
	attempts, ok := data["attempts"].([]any)
	if !ok {
		// Not a loaded call-budget ledger (those always carry attempts): leave it alone.
		return
	}
	// Most transactions have no expired history. Reuse the decoded slice
	// rather than allocating and copying it again on both read and save.
	kept := attempts[:0]
	for _, raw := range attempts {
		entry := raw.(map[string]any)
		settled := entry["status"] == "completed" || entry["status"] == "released"
		if settled && entry["finished_at"] == nil {
			// Older Go completions recorded start + elapsed seconds. A start
			// timestamp alone does not prove when a long job finished.
			started, elapsed := ledgerNumber(entry["started_at"]), ledgerNumber(entry["seconds"])
			if entry["status"] == "completed" && retentionNumber(entry["started_at"]) && retentionNumber(entry["seconds"]) {
				entry["finished_at"] = started + elapsed
			} else {
				entry["finished_at"] = now
			}
		}
		finished := ledgerNumber(entry["finished_at"])
		if settled && leases[entry["id"].(string)] == nil && finished >= 0 && !math.IsNaN(finished) && !math.IsInf(finished, 0) && finished < cutoff {
			if entry["status"] != "released" {
				if archived < int(^uint(0)>>1) {
					archived++
				}
			}
		} else {
			kept = append(kept, entry)
		}
	}
	clear(attempts[len(kept):])
	data["attempts"] = kept
	if scopes == nil || data["legacy_maximum"] != nil {
		data["archived_used"] = json.Number(strconv.Itoa(archived))
	}
	holds, _ := data["holds"].(map[string]any)
	for key, raw := range holds {
		if ledgerNumber(raw.(map[string]any)["expires_at"]) <= now {
			delete(holds, key)
		}
	}
	limits, _ := data["lease_limits"].(map[string]any)
	if len(limits) > 0 {
		activeKeys := map[string]bool{}
		for _, raw := range leases {
			for _, key := range raw.(map[string]any)["keys"].([]any) {
				activeKeys[key.(string)] = true
			}
		}
		for key := range limits {
			if !activeKeys[key] {
				delete(limits, key)
			}
		}
	}
	if scopes == nil {
		return
	}
	referenced := map[string]map[string]bool{"tasks": {}, "projects": {}, "machine": {}}
	entries := append([]any{}, kept...)
	for _, raw := range holds {
		entries = append(entries, raw)
	}
	for _, raw := range entries {
		keys, _ := raw.(map[string]any)["scopes"].([]any)
		for _, raw := range keys {
			pair := raw.([]any)
			referenced[pair[0].(string)][pair[1].(string)] = true
		}
	}
	for _, group := range []string{"tasks", "projects", "machine"} {
		counters := scopes[group].(map[string]any)
		for key, raw := range counters {
			counter := raw.(map[string]any)
			if referenced[group][key] {
				if counter["last_used"] == nil {
					counter["last_used"] = now
				}
				continue
			}
			if counter["last_used"] == nil {
				counter["last_used"] = now
			}
			day := key[strings.LastIndex(key, ":")+1:]
			if (group != "tasks" && day < scopes["day"].(string)) || (group == "tasks" && ledgerNumber(counter["last_used"]) < cutoff) {
				delete(counters, key)
			}
		}
	}
}

// Validate optional retention fields before compaction can remove anything.
// Malformed holds must fail closed even when their expiry appears to be old.
func validateRetention(data map[string]any) error {
	checkTime := func(value any) bool {
		n, ok := value.(json.Number)
		f, err := n.Float64()
		return ok && err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0
	}
	for _, raw := range data["attempts"].([]any) {
		entry := raw.(map[string]any)
		if value := entry["finished_at"]; value != nil && !checkTime(value) {
			return fmt.Errorf("invalid attempt finish timestamp")
		}
	}
	if raw, exists := data["holds"]; exists {
		holds, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid budget holds")
		}
		for id, raw := range holds {
			hold, ok := raw.(map[string]any)
			if !ok || id == "" {
				return fmt.Errorf("invalid budget hold")
			}
			remaining, valid := jsonInteger(hold["remaining"])
			if !valid || remaining < 0 || !checkTime(hold["expires_at"]) {
				return fmt.Errorf("invalid budget hold capacity or expiry")
			}
		}
	}
	if scopes, ok := data["scopes"].(map[string]any); ok {
		for _, group := range []string{"tasks", "projects", "machine"} {
			counters, _ := scopes[group].(map[string]any)
			for _, raw := range counters {
				counter, _ := raw.(map[string]any)
				if value := counter["last_used"]; value != nil && !checkTime(value) {
					return fmt.Errorf("invalid scope last-used timestamp")
				}
			}
		}
	}
	return nil
}
