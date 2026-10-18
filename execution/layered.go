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
	"time"
)

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
	day := now.In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	if day < s["day"].(string) {
		return nil, fmt.Errorf("budget clock moved back across an Asia/Shanghai day")
	}
	s["day"] = day
	old, _ := s["clock"].(json.Number).Float64()
	s["clock"] = json.Number(strconv.FormatFloat(max(old, float64(now.UnixNano())/1e9), 'f', -1, 64))
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
func scopeAvailable(data map[string]any, keys []any, holds map[string]any, claim map[string]any) (int, error) {
	available := int(^uint(0) >> 1)
	s := data["scopes"].(map[string]any)
	for _, raw := range keys {
		pair := raw.([]any)
		c := s[pair[0].(string)].(map[string]any)[pair[1].(string)].(map[string]any)
		limit, _ := jsonInteger(c["maximum"])
		used, _ := jsonInteger(c["used"])
		held := 0
		for _, raw := range holds {
			h := raw.(map[string]any)
			if containsScope(h["scopes"], pair) {
				n, _ := jsonInteger(h["remaining"])
				if n > limit-held {
					return 0, ErrBudgetExhausted
				}
				held += n
			}
		}
		if claim != nil && containsScope(claim["scopes"], pair) {
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
		c["last_used"] = max(float64(time.Now().UnixNano())/1e9, ledgerNumber(s["clock"]))
	}
}
