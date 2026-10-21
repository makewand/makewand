package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const leaseTTL = 30 * time.Second

// leaseProvider is a capacity namespace, independent of adapter/display names.
func leaseProvider(engine string) string {
	engine = strings.ToLower(strings.TrimSpace(engine))
	base := strings.TrimSuffix(engine, ":api")
	suffix := strings.TrimPrefix(engine, base)
	// Keep in sync with config.PROVIDER_ALIASES; parity is tested with Python.
	aliases := map[string]string{
		"gemini": "agy", "google": "agy", "antigravity": "agy",
		"anthropic": "claude", "openai": "codex", "xai": "grok", "meta": "muse",
		"ollama": "local", "dashscope": "qwen", "aliyun": "qwen", "zhipu": "glm",
		"moonshot": "kimi", "silicon": "siliconflow", "deepseek-coder": "deepseek", "deepseek-chat": "deepseek",
	}
	base = strings.TrimSuffix(base, "-api")
	if canonical, ok := aliases[base]; ok {
		base = canonical
	}
	return base + suffix
}

func leaseKeys(engine string, account *string) []any {
	engine = leaseProvider(engine)
	if account != nil {
		if provider, _, api := strings.Cut(*account, ":api:"); api && leaseProvider(provider) == strings.TrimSuffix(engine, ":api") {
			engine = strings.TrimSuffix(engine, ":api") + ":api"
			account = nil
		}
	}
	ref := engine + ":unknown"
	if account != nil && strings.Contains(*account, ":account:") {
		ref = *account
	}
	return []any{engine + ":provider", ref, ref + ":subscription"}
}
func validateLeases(data map[string]any) error {
	if raw, exists := data["leases"]; exists {
		leases, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid account lease ledger")
		}
		for _, raw := range leases {
			l, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid account lease")
			}
			keys, ok := l["keys"].([]any)
			if !ok || len(keys) != 3 {
				return fmt.Errorf("invalid account lease keys")
			}
			for _, k := range keys {
				if text, ok := k.(string); !ok || text == "" {
					return fmt.Errorf("invalid lease key")
				}
			}
			owner, ok := l["owner"].(map[string]any)
			if !ok {
				return fmt.Errorf("invalid account lease owner")
			}
			workers, ok := l["workers"].([]any)
			if !ok {
				return fmt.Errorf("invalid account lease workers")
			}
			for _, raw := range append([]any{owner}, workers...) {
				ref, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid lease process")
				}
				pid, ok := jsonInteger(ref["pid"])
				if !ok || pid <= 0 {
					return fmt.Errorf("invalid lease pid")
				}
			}
			if _, ok := l["launch_pending"].(bool); !ok {
				return fmt.Errorf("invalid lease fence")
			}
			expires, valid := l["expires_at"].(json.Number)
			value, err := expires.Float64()
			if !valid || err != nil || math.IsInf(value, 0) || math.IsNaN(value) || value < 0 {
				return fmt.Errorf("invalid lease expiry")
			}
		}
	}
	if raw, exists := data["lease_limits"]; exists {
		m, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid lease limits")
		}
		for _, raw := range m {
			n, ok := jsonInteger(raw)
			if !ok || n <= 0 {
				return fmt.Errorf("invalid lease limit")
			}
		}
	}
	return nil
}

// leaseClock dates lease expiry: a persisted clock far ahead of real time would
// keep a dead owner's lease unexpired until real time caught up.
func leaseClock(data map[string]any) float64 {
	return effectiveClock(data, ledgerNow())
}
func leaseWorkersAlive(l map[string]any) bool {
	for _, raw := range l["workers"].([]any) {
		ref := raw.(map[string]any)
		// Only the supervised Windows Job runner may set this marker. Its
		// non-inheritable handle dies with the lease owner, killing descendants.
		if runtime.GOOS == "windows" && ref["job"] == "owner-kill-on-close" && !leaseProcessAlive(l["owner"].(map[string]any), false) {
			continue
		}
		if leaseProcessAlive(ref, true) {
			return true
		}
	}
	return false
}
func admitLease(data map[string]any, engine string, account *string) (map[string]any, error) {
	keys := leaseKeys(engine, account)
	engine = strings.TrimSuffix(keys[0].(string), ":provider")
	leases, ok := data["leases"].(map[string]any)
	if !ok {
		leases = map[string]any{}
		data["leases"] = leases
	}
	now := leaseClock(data)
	completed := map[string]bool{}
	if attempts, ok := data["attempts"].([]any); ok {
		for _, raw := range attempts {
			e := raw.(map[string]any)
			if e["status"] == "completed" {
				completed[e["id"].(string)] = true
			}
		}
	}
	for id, raw := range leases {
		l := raw.(map[string]any)
		expires, _ := l["expires_at"].(json.Number).Float64()
		stoppedAttempt := completed[id] && !l["launch_pending"].(bool)
		if expires <= now && (stoppedAttempt || !leaseProcessAlive(l["owner"].(map[string]any), false)) && !leaseWorkersAlive(l) {
			delete(leases, id)
		}
	}
	limits, ok := data["lease_limits"].(map[string]any)
	if !ok {
		limits = map[string]any{}
		data["lease_limits"] = limits
	}
	for i, key := range keys {
		limit, err := scopeLimit([]string{"MAKEWAND_PROVIDER_CONCURRENCY", "MAKEWAND_ACCOUNT_CONCURRENCY", "MAKEWAND_POOL_CONCURRENCY"}[i], []int{64, 1, 1}[i])
		if err != nil {
			return nil, err
		}
		if old, ok := jsonInteger(limits[key.(string)]); ok {
			limit = min(limit, old)
		}
		limits[key.(string)] = json.Number(strconv.Itoa(limit))
		used := 0
		for _, raw := range leases {
			l := raw.(map[string]any)
			existing := l["keys"].([]any)
			conflict := false
			for _, k := range existing {
				if k == key {
					conflict = true
					break
				}
			}
			if i > 0 && existing[0] == keys[0] && (keys[1] == engine+":unknown" || existing[1] == engine+":unknown") {
				conflict = true
			}
			if conflict {
				used++
			}
		}
		if used >= limit {
			return nil, ErrConcurrencyBusy
		}
	}
	return map[string]any{"keys": keys, "owner": leaseProcessRef(os.Getpid()), "workers": []any{}, "heartbeat_at": now, "expires_at": now + leaseTTL.Seconds(), "launch_pending": false}, nil
}
func settleLease(data map[string]any, id string) {
	leases, _ := data["leases"].(map[string]any)
	l, ok := leases[id].(map[string]any)
	if ok && !l["launch_pending"].(bool) && !leaseWorkersAlive(l) {
		delete(leases, id)
	}
}
func (a *Attempt) updateLease(action string, pid int) error {
	if a == nil || a.config.LedgerPath == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return withLockedFile(ctx, a.config.LedgerPath, a.config.ledgerDirectory, func(path *accountingPath) error {
		if action == "heartbeat" {
			select {
			case <-a.leaseStop:
				return nil
			default:
			}
		}
		data, err := readLedger(path, a.config.MaxCalls)
		if err != nil {
			return err
		}
		leases, _ := data["leases"].(map[string]any)
		l, ok := leases[a.ID].(map[string]any)
		if !ok {
			return nil
		}
		now := leaseClock(data)
		l["heartbeat_at"] = now
		l["expires_at"] = now + leaseTTL.Seconds()
		switch action {
		case "launch":
			l["launch_pending"] = true
		case "worker", "job_worker":
			ref := leaseProcessRef(pid)
			if action == "job_worker" && runtime.GOOS == "windows" {
				ref["job"] = "owner-kill-on-close"
			}
			l["workers"] = append(l["workers"].([]any), ref)
			l["launch_pending"] = ref["token"] == nil
		case "launch_failed":
			l["launch_pending"] = false
		case "stopped":
			// Caller has joined its OS process tree. Live lifecycle evidence still wins.
			if !leaseWorkersAlive(l) || leasePlatformUnsupported() {
				l["workers"] = []any{}
				l["launch_pending"] = false
			}
		case "heartbeat":
		default:
			return fmt.Errorf("invalid lease update")
		}
		if action == "stopped" {
			for _, raw := range data["attempts"].([]any) {
				e := raw.(map[string]any)
				if e["id"] == a.ID && e["status"] == "completed" {
					settleLease(data, a.ID)
					break
				}
			}
		}
		return saveLedger(path, data)
	})
}
func (a *Attempt) BeginLaunch() error {
	if err := a.Start(); err != nil {
		return err
	}
	return a.updateLease("launch", 0)
}
func (a *Attempt) RegisterWorker(pid int) error { return a.updateLease("worker", pid) }

// RegisterJobWorker is only for runners using a no-breakaway, owner-held Job.
func (a *Attempt) RegisterJobWorker(pid int) error { return a.updateLease("job_worker", pid) }
func (a *Attempt) cleanupLease(action string) error {
	var err error
	for retry := 0; retry < 3; retry++ {
		if err = a.updateLease(action, 0); err == nil {
			return nil
		}
		if retry < 2 {
			time.Sleep(time.Duration(retry+1) * 50 * time.Millisecond)
		}
	}
	return err
}
func (a *Attempt) LaunchFailed() error  { return a.cleanupLease("launch_failed") }
func (a *Attempt) WorkerStopped() error { return a.cleanupLease("stopped") }
func (a *Attempt) heartbeat() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.leaseStop:
			return
		case <-ticker.C:
			_ = a.updateLease("heartbeat", 0)
		}
	}
}
