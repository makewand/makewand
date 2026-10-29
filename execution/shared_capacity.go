package execution

// This protocol is shared with makewand/shared_capacity.py. The shared lock is
// always acquired before a budget lock. Call counts stay in their own ledger.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type hostCapacityPolicy struct {
	Schema      int            `json:"schema"`
	GlobalMax   int            `json:"global_max"`
	ProjectMax  int            `json:"project_max"`
	ToolMax     int            `json:"tool_max"`
	WaitSeconds int            `json:"wait_seconds"`
	Providers   map[string]int `json:"providers"`
}

type capacityEntry struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Ledger    string         `json:"ledger"`
	Attempt   string         `json:"attempt"`
	Project   string         `json:"project"`
	Label     string         `json:"label"`
	TaskID    string         `json:"task_id"`
	StartedAt float64        `json:"started_at"`
	Limits    []int          `json:"limits"`
	Lease     map[string]any `json:"lease"`
	Settled   bool           `json:"settled,omitempty"`
}

type sharedCapacityState struct {
	Schema       int                       `json:"schema"`
	Active       map[string]*capacityEntry `json:"active"`
	Queue        []*capacityEntry          `json:"queue"`
	Turn         int64                     `json:"turn,omitempty"`
	ProjectTurns map[string]int64          `json:"project_turns,omitempty"`
}

type sharedCapacityGate struct {
	Project string
	Label   string
	Policy  *hostCapacityPolicy
}

func capacityPath(name string) (string, error) {
	path, err := DefaultBudgetPath()
	return filepath.Join(filepath.Dir(path), name), err
}

func decodeCapacity(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid trailing capacity data")
	}
	return nil
}

func readCapacityFile(value string, target any) error {
	value, err := canonicalAccountingPath(value)
	if err != nil {
		return err
	}
	path, err := openAccountingPath(value, false)
	if err != nil {
		return err
	}
	defer path.close()
	raw, err := readLedgerBytes(path)
	if err != nil {
		return err
	}
	return decodeCapacity(raw, target)
}

func loadCapacityPolicy() (*hostCapacityPolicy, error) {
	value, err := capacityPath("capacity-policy.json")
	if err != nil {
		return nil, err
	}
	var policy hostCapacityPolicy
	if err := readCapacityFile(value, &policy); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("cannot read host capacity policy: %w", err)
	}
	valid := func(n int) bool { return n > 0 && n <= 86400 }
	if policy.Schema != 1 || !valid(policy.GlobalMax) || !valid(policy.ProjectMax) || !valid(policy.ToolMax) || !valid(policy.WaitSeconds) || policy.Providers == nil {
		return nil, fmt.Errorf("invalid host capacity policy")
	}
	for name, maximum := range policy.Providers {
		if name == "" || leaseProvider(name) != name || !valid(maximum) {
			return nil, fmt.Errorf("invalid host provider capacity")
		}
	}
	return &policy, nil
}

func capacityProject(workingDir string) (string, string, error) {
	root := os.Getenv("MAKEWAND_PROJECT_ROOT")
	explicit := root != ""
	if !explicit {
		root = workingDir
	}
	if !explicit {
		if root == "" {
			var err error
			root, err = os.Getwd()
			if err != nil {
				return "", "", err
			}
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", err
	}
	if !explicit {
		for parent := root; ; parent = filepath.Dir(parent) {
			marker := filepath.Join(parent, ".git")
			if capacityDirectory(marker) {
				root = parent
				break
			}
			if raw, err := readCapacityText(marker); err == nil && strings.HasPrefix(strings.TrimSpace(raw), "gitdir: ") {
				dir := strings.TrimSpace(raw)[8:]
				if !filepath.IsAbs(dir) {
					dir = filepath.Join(parent, dir)
				}
				if common, err := readCapacityText(filepath.Join(dir, "commondir")); err == nil {
					common = strings.TrimSpace(common)
					if !filepath.IsAbs(common) {
						common = filepath.Join(dir, common)
					}
					if resolved, err := filepath.EvalSymlinks(common); err == nil {
						root = filepath.Dir(resolved)
						break
					}
				}
			}
			if filepath.Dir(parent) == parent {
				break
			}
		}
	}
	hash := sha256.Sum256([]byte(root))
	return hex.EncodeToString(hash[:])[:32], filepath.Base(root), nil
}

func readCapacityText(value string) (string, error) {
	value, err := canonicalAccountingPath(value)
	if err != nil {
		return "", err
	}
	path, err := openAccountingPath(value, false)
	if err != nil {
		return "", err
	}
	defer path.close()
	// Unlike budget readers, Git metadata and the import registry must not
	// have their permissions changed. Still pin the parent and refuse leaf
	// symlinks, devices and FIFOs before reading any bytes.
	file, err := path.root.OpenFile(path.name, accountingOpenFlags(os.O_RDONLY), 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maximumLedgerBytes {
		return "", fmt.Errorf("invalid capacity text file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maximumLedgerBytes+1))
	if len(raw) > maximumLedgerBytes {
		return "", fmt.Errorf("capacity text exceeds 16 MiB")
	}
	return string(raw), err
}

func capacityDirectory(value string) bool {
	path, err := openAccountingPath(value, false)
	if err != nil {
		return false
	}
	defer path.close()
	info, err := path.root.Stat(path.name)
	return err == nil && info.IsDir()
}

func capacityLimits(engine string, policy *hostCapacityPolicy) ([]int, error) {
	maximum := policy.Providers[strings.TrimSuffix(leaseProvider(engine), ":api")]
	if maximum == 0 {
		maximum = 1
	}
	limits := []int{maximum, maximum, maximum}
	for i, name := range []string{"MAKEWAND_PROVIDER_CONCURRENCY", "MAKEWAND_ACCOUNT_CONCURRENCY", "MAKEWAND_POOL_CONCURRENCY"} {
		value, err := scopeLimit(name, maximum)
		if err != nil {
			return nil, err
		}
		limits[i] = min(maximum, value)
	}
	return limits, nil
}

func capacityLive(lease map[string]any, completed bool) bool {
	expires, _ := lease["expires_at"].(json.Number).Float64()
	stopped := completed && !lease["launch_pending"].(bool)
	return !((!leaseProcessAlive(lease["owner"].(map[string]any), false) || stopped) && !leaseWorkersAlive(lease) && expires <= float64(time.Now().UnixNano())/1e9)
}

func capacitySource(value string) (map[string]*capacityEntry, map[string]map[string]any, error) {
	var data map[string]any
	if err := readCapacityFile(value, &data); err != nil {
		return nil, nil, err
	}
	if err := validateLeases(data); err != nil {
		return nil, nil, err
	}
	entries, ok := data["attempts"].([]any)
	if !ok {
		return nil, nil, fmt.Errorf("invalid capacity source attempts")
	}
	attempts := map[string]map[string]any{}
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("invalid capacity source attempt")
		}
		id, ok := entry["id"].(string)
		if !ok || id == "" || attempts[id] != nil {
			return nil, nil, fmt.Errorf("invalid capacity source identity")
		}
		attempts[id] = entry
	}
	rows := map[string]*capacityEntry{}
	leases, _ := data["leases"].(map[string]any)
	for identifier, raw := range leases {
		lease := raw.(map[string]any)
		entry := attempts[identifier]
		if !capacityLive(lease, entry["status"] == "completed") {
			if entry == nil {
				entry = map[string]any{}
				attempts[identifier] = entry
			}
			entry["_capacity_reaped"] = true
			continue
		}
		project, _ := entry["capacity_project"].(string)
		if project == "" {
			project = "legacy:" + filepath.Dir(value)
			if scopes, ok := entry["scopes"].([]any); ok {
				for _, raw := range scopes {
					if scope, ok := raw.([]any); ok && len(scope) == 2 && scope[0] == "projects" {
						project, _ = scope[1].(string)
					}
				}
			}
		}
		label, _ := entry["capacity_label"].(string)
		if label == "" {
			label = "existing task"
		}
		started := ledgerNumber(entry["started_at"])
		if math.IsNaN(started) || math.IsInf(started, 0) || started < 0 {
			started = 0
		}
		taskID, _ := entry["task_id"].(string)
		limits := []int{math.MaxInt, math.MaxInt, math.MaxInt}
		if saved, ok := data["lease_limits"].(map[string]any); ok {
			for i, key := range lease["keys"].([]any) {
				if n, ok := jsonInteger(saved[key.(string)]); ok {
					limits[i] = n
				}
			}
		}
		hash := sha256.Sum256([]byte(value + "\x00" + identifier))
		rows[identifier] = &capacityEntry{ID: hex.EncodeToString(hash[:]), Kind: "model", Ledger: value, Attempt: identifier, Project: project, Label: label, TaskID: taskID, StartedAt: started, Limits: limits, Lease: lease}
	}
	return rows, attempts, nil
}

func validateCapacityState(state *sharedCapacityState) error {
	if state.Schema != 1 || state.Active == nil || state.Queue == nil {
		return fmt.Errorf("invalid shared capacity state")
	}
	if state.Turn < 0 {
		return fmt.Errorf("invalid project queue turn")
	}
	for _, turn := range state.ProjectTurns {
		if turn < 0 {
			return fmt.Errorf("invalid project queue turn")
		}
	}
	rows := append([]*capacityEntry{}, state.Queue...)
	for _, row := range state.Active {
		rows = append(rows, row)
	}
	for _, row := range rows {
		if row == nil || (row.Kind != "model" && row.Kind != "tool") || row.ID == "" || row.Project == "" || len(row.Limits) != 3 || row.Lease == nil {
			return fmt.Errorf("invalid shared capacity entry")
		}
		for _, limit := range row.Limits {
			if limit <= 0 {
				return fmt.Errorf("invalid shared capacity limit")
			}
		}
		if err := validateLeases(map[string]any{"leases": map[string]any{row.ID: row.Lease}}); err != nil {
			return err
		}
	}
	return nil
}

func refreshCapacity(state *sharedCapacityState, selected string) {
	paths := map[string]bool{selected: true}
	if path, err := DefaultBudgetPath(); err == nil {
		paths[resolvedLedgerPath(path)] = true
	}
	// The old registry is an import source only. New admissions are durably
	// registered under the shared lock before any provider callback can run.
	if path, err := capacityPath(ledgerRegistryName); err == nil {
		if text, err := readCapacityText(path); err == nil {
			for _, line := range strings.Split(text, "\n") {
				var row struct {
					Path string `json:"path"`
				}
				if json.Unmarshal([]byte(line), &row) == nil && row.Path != "" {
					paths[row.Path] = true
				}
			}
		}
	}
	for _, row := range state.Active {
		if row.Kind == "model" {
			paths[row.Ledger] = true
		}
	}
	for value := range paths {
		value, err := canonicalAccountingPath(value)
		if err != nil {
			continue
		}
		rows, attempts, err := capacitySource(value)
		if err != nil {
			continue // an unreadable active source never creates free capacity
		}
		for key, old := range state.Active {
			if old.Kind != "model" || old.Ledger != value || rows[old.Attempt] != nil {
				continue
			}
			status := attempts[old.Attempt]["status"]
			if status == "completed" || status == "released" || attempts[old.Attempt]["_capacity_reaped"] == true {
				delete(state.Active, key)
			}
		}
		for _, row := range rows {
			state.Active[row.ID] = row
		}
	}
	for key, row := range state.Active {
		if row.Kind == "tool" && !capacityLive(row.Lease, row.Settled) {
			delete(state.Active, key)
		}
	}
	queue := make([]*capacityEntry, 0, len(state.Queue))
	for _, row := range state.Queue {
		expires, _ := row.Lease["expires_at"].(json.Number).Float64()
		if expires > float64(time.Now().UnixNano())/1e9 && leaseProcessAlive(row.Lease["owner"].(map[string]any), false) {
			queue = append(queue, row)
		}
	}
	state.Queue = queue
}

func capacityFits(ticket *capacityEntry, state *sharedCapacityState, policy *hostCapacityPolicy) bool {
	if ticket.Kind == "tool" {
		used := 0
		for _, row := range state.Active {
			if row.Kind == "tool" {
				used++
			}
		}
		if used >= policy.ToolMax {
			return false
		}
		if runtime.GOOS == "linux" {
			raw, err := os.ReadFile("/proc/meminfo")
			if err != nil {
				return false
			}
			for _, line := range strings.Split(string(raw), "\n") {
				var kb uint64
				if _, err := fmt.Sscanf(line, "MemAvailable: %d kB", &kb); err == nil {
					return kb >= 32*1024*1024
				}
			}
			return false
		}
		return true
	}
	models := map[string]*capacityEntry{}
	for id, row := range state.Active {
		if row.Kind == "model" {
			models[id] = row
		}
	}
	if len(models) >= policy.GlobalMax {
		return false
	}
	projectUsed := 0
	for _, row := range models {
		if row.Project == ticket.Project {
			projectUsed++
		}
		if ticket.TaskID != "" && row.TaskID == ticket.TaskID {
			return false
		}
	}
	if projectUsed >= policy.ProjectMax {
		return false
	}
	wanted := ticket.Lease["keys"].([]any)
	provider := strings.TrimSuffix(wanted[0].(string), ":provider")
	for index, key := range wanted {
		used, maximum := 0, ticket.Limits[index]
		for _, row := range models {
			keys := row.Lease["keys"].([]any)
			conflicts := index == 0 && strings.TrimSuffix(provider, ":api") == strings.TrimSuffix(strings.TrimSuffix(keys[0].(string), ":provider"), ":api")
			for _, existing := range keys {
				if existing == key {
					conflicts = true
				}
			}
			if index > 0 && wanted[0] == keys[0] && (wanted[1] == provider+":unknown" || keys[1] == provider+":unknown") {
				conflicts = true
			}
			if conflicts {
				used++
				maximum = min(maximum, row.Limits[index])
			}
		}
		if used >= maximum {
			return false
		}
	}
	return true
}

func saveCapacity(path *accountingPath, state *sharedCapacityState) error {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if len(raw) >= maximumLedgerBytes {
		return fmt.Errorf("shared capacity exceeds 16 MiB")
	}
	return replaceAccountingFile(path, append(raw, '\n'))
}

func capacityDiagnostic(state *sharedCapacityState, ticket *capacityEntry, policy *hostCapacityPolicy) string {
	running := []string{}
	models, tools := 0, 0
	for _, row := range state.Active {
		if row.Kind == "model" {
			models++
		} else {
			tools++
		}
		age := "age unknown"
		if row.StartedAt > 0 {
			age = fmt.Sprintf("%.0fs", max(0, float64(time.Now().UnixNano())/1e9-row.StartedAt))
		}
		running = append(running, fmt.Sprintf("%q (%s, %s)", row.Label, strings.TrimSuffix(row.Lease["keys"].([]any)[0].(string), ":provider"), age))
	}
	sort.Strings(running)
	return fmt.Sprintf("shared capacity busy: models=%d/%d; tools=%d/%d; project=%q; queue=%d; occupied: %s", models, policy.GlobalMax, tools, policy.ToolMax, ticket.Label, len(state.Queue), strings.Join(running, ", "))
}

func withSharedCapacity(ctx context.Context, policy *hostCapacityPolicy, cfg Config, metadata Metadata, attemptID string, fn func(*sharedCapacityGate) error) error {
	if policy == nil {
		return fn(nil)
	}
	value, err := capacityPath("shared-capacity.json")
	if err != nil {
		return err
	}
	project, label, err := capacityProject(metadata.WorkingDir)
	if err != nil {
		return err
	}
	limits, err := capacityLimits(metadata.Engine, policy)
	if err != nil {
		return err
	}
	now := float64(time.Now().UnixNano()) / 1e9
	ticket := &capacityEntry{ID: attemptID, Kind: "model", Ledger: cfg.LedgerPath, Project: project, Label: label, TaskID: cfg.TaskID, StartedAt: now, Limits: limits,
		Lease: map[string]any{"keys": leaseKeys(metadata.Engine, cfg.AccountRef), "owner": leaseProcessRef(os.Getpid()), "workers": []any{}, "launch_pending": false, "heartbeat_at": json.Number(fmt.Sprint(now)), "expires_at": json.Number(fmt.Sprint(now + 30))}}
	return withLockedFile(ctx, value, nil, func(path *accountingPath) error {
		state := sharedCapacityState{Schema: 1, Active: map[string]*capacityEntry{}, Queue: []*capacityEntry{}}
		raw, err := readLedgerBytes(path)
		if err == nil {
			err = decodeCapacity(raw, &state)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := validateCapacityState(&state); err != nil {
			return err
		}
		refreshCapacity(&state, cfg.LedgerPath)
		found := false
		for _, row := range state.Queue {
			if row.ID == attemptID {
				row.Lease["expires_at"] = ticket.Lease["expires_at"]
				ticket = row
				found = true
				break
			}
		}
		if !found {
			state.Queue = append(state.Queue, ticket)
		}
		var eligible *capacityEntry
		for _, row := range state.Queue {
			if row.Kind == "model" && capacityFits(row, &state, policy) {
				if eligible == nil || state.ProjectTurns[row.Project] < state.ProjectTurns[eligible.Project] {
					eligible = row
				}
			}
		}
		if eligible == nil || eligible.ID != attemptID {
			if err := saveCapacity(path, &state); err != nil {
				return err
			}
			return fmt.Errorf("%w: %s", ErrConcurrencyBusy, capacityDiagnostic(&state, ticket, policy))
		}
		if err := fn(&sharedCapacityGate{Project: project, Label: label, Policy: policy}); err != nil {
			return err
		}
		rows, _, err := capacitySource(cfg.LedgerPath)
		if err != nil {
			return err
		}
		row := rows[attemptID]
		if row == nil {
			return fmt.Errorf("admitted model call has no capacity lease")
		}
		state.Active[row.ID] = row
		state.Turn++
		if state.ProjectTurns == nil {
			state.ProjectTurns = map[string]int64{}
		}
		state.ProjectTurns[row.Project] = state.Turn
		queue := make([]*capacityEntry, 0, len(state.Queue))
		for _, row := range state.Queue {
			if row.ID != attemptID {
				queue = append(queue, row)
			}
		}
		state.Queue = queue
		return saveCapacity(path, &state)
	})
}

func cancelCapacityTicket(policy *hostCapacityPolicy, identifier string) {
	if policy == nil {
		return
	}
	path, err := capacityPath("shared-capacity.json")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = withLockedFile(ctx, path, nil, func(path *accountingPath) error {
		raw, err := readLedgerBytes(path)
		if err != nil {
			return err
		}
		var state sharedCapacityState
		if err := decodeCapacity(raw, &state); err != nil {
			return err
		}
		if err := validateCapacityState(&state); err != nil {
			return err
		}
		queue := make([]*capacityEntry, 0, len(state.Queue))
		for _, row := range state.Queue {
			if row.ID != identifier {
				queue = append(queue, row)
			}
		}
		state.Queue = queue
		return saveCapacity(path, &state)
	})
}
