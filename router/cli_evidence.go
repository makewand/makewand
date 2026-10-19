package router

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/makewand/makewand/execution"
)

// Only protocol envelopes are inspected; model message bodies are never
// recursively searched, so a quoted rate_limits object is not evidence.
func collectCLIProtocol(e *execution.ProviderEvidence, raw []byte) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	sessionRef := ""
	for scanner.Scan() {
		var obj map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &obj) != nil {
			continue
		}
		var kind string
		_ = json.Unmarshal(obj["type"], &kind)
		var payload map[string]json.RawMessage
		_ = json.Unmarshal(obj["payload"], &payload)
		var fields map[string]json.RawMessage
		switch {
		case e.Engine == "codex" && (kind == "session_meta" || kind == "turn_context"):
			fields = payload
		case e.Engine == "claude" && (kind == "system" || kind == "result"):
			fields = obj
		case e.Engine == "claude" && kind == "assistant":
			_ = json.Unmarshal(obj["message"], &fields)
		}
		ref := codexEventAccountRef(obj)
		if e.Engine != "codex" {
			ref = ""
		}
		if kind == "session_meta" {
			sessionRef = ref
		}
		if ref == "" {
			ref = sessionRef
		}
		if ref != "" && ref != e.AccountRef {
			continue
		}
		var model string
		_ = json.Unmarshal(fields["model"], &model)
		if execution.ValidIdentifier(model) {
			e.Model = model
		}
		if e.Engine == "claude" && kind == "result" {
			var models map[string]json.RawMessage
			if json.Unmarshal(obj["modelUsage"], &models) == nil && len(models) == 1 {
				for name := range models {
					if execution.ValidIdentifier(name) {
						e.Model = name
					}
				}
			}
		}
		if e.Engine == "codex" && kind == "turn.completed" || e.Engine == "claude" && kind == "result" {
			var usage struct {
				Input  *int64 `json:"input_tokens"`
				Output *int64 `json:"output_tokens"`
			}
			if json.Unmarshal(obj["usage"], &usage) == nil && usage.Input != nil && usage.Output != nil && *usage.Input >= 0 && *usage.Output >= 0 && *usage.Input <= math.MaxInt64-*usage.Output {
				count := *usage.Input + *usage.Output
				e.Tokens = &count
			}
		}
		if e.Engine != "codex" || !e.IdentityKnown {
			continue
		}
		var payloadKind string
		_ = json.Unmarshal(payload["type"], &payloadKind)
		if kind != "rate_limits.updated" && (kind != "event_msg" || payloadKind != "token_count") {
			continue
		}
		var stamp string
		_ = json.Unmarshal(obj["timestamp"], &stamp)
		at := parseTime(stamp)
		if at.IsZero() || at.After(time.Now().Add(time.Minute)) {
			continue
		}
		rl := findRateLimits(obj)
		if rl == nil {
			continue
		}
		scoped := rl.Model
		if scoped == "" && rl.LimitID != "codex" {
			scoped = rl.LimitID
		}
		if scoped != "" && !execution.ValidIdentifier(scoped) {
			continue
		}
		for slot, w := range []*codexWindow{rl.Primary, rl.Secondary} {
			if w == nil || w.UsedPercent == nil || *w.UsedPercent < 0 || *w.UsedPercent > 100 {
				continue
			}
			reset := parseUnixOrTime(w.ResetsAt)
			if w.ResetsAt != nil && reset.IsZero() {
				continue
			}
			minutes := w.windowMinutes()
			if minutes < 0 || minutes > 525600 {
				continue
			}
			key := scoped + "|" + strconv.Itoa(minutes)
			if minutes == 0 {
				key += "|unknown_" + strconv.Itoa(slot)
			}
			resetMS := int64(0)
			if !reset.IsZero() {
				resetMS = reset.UnixMilli()
			}
			candidate := execution.EvidenceWindow{Model: scoped, Key: key, Minutes: minutes, UsedPercent: *w.UsedPercent, ObservedAt: at.UnixMilli(), ResetAt: resetMS}
			found := false
			for i, old := range e.Windows {
				if old.Key == key {
					if old.ObservedAt <= candidate.ObservedAt {
						e.Windows[i] = candidate
					}
					found = true
					break
				}
			}
			if !found && len(e.Windows) < 32 {
				e.Windows = append(e.Windows, candidate)
			}
		}
	}
}

// Read only the selected provider's private root, never alternate logins.
func (h *cliPrivateHome) collectEvidence() {
	if h.attempt == nil {
		return
	}
	_ = h.attempt.UpdateEvidence(func(e *execution.ProviderEvidence) {
		if e.Engine != "codex" && e.Engine != "claude" {
			return
		}
		root := h.evidenceRoot
		if e.Source == "cli_json" || e.Source == "private_home+cli_json" {
			e.Source = "private_home+cli_json"
		} else {
			e.Source = "private_home"
		}
		if root == "" {
			return
		}
		raw, problem := cliReadPrivateCredential(h.dir, h.identityPath)
		// Unreadable/replaced private metadata cannot retain the admitted
		// identity. Both providers must re-establish it from the frozen copy.
		var identity struct {
			Tokens struct {
				AccountID string `json:"account_id"`
			} `json:"tokens"`
			OAuth struct {
				AccountID string `json:"accountUuid"`
			} `json:"oauthAccount"`
		}
		id := ""
		if problem == "" && json.Unmarshal(raw, &identity) == nil {
			id = identity.Tokens.AccountID
			if e.Engine == "claude" {
				id = identity.OAuth.AccountID
			}
		}
		ref := quotaAccountRef(e.Engine, id)
		if e.AccountRef != ref {
			e.Windows = nil
		}
		e.AccountRef, e.IdentityKnown, e.PoolRef = ref, ref != "", ""
		if ref != "" {
			e.PoolRef = ref + ":subscription"
		}
		logs := "sessions"
		if e.Engine == "claude" {
			logs = "projects"
		}
		count := 0
		_ = filepath.WalkDir(filepath.Join(root, logs), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			count++
			if count > 256 {
				return fs.SkipAll
			}
			if entry.Type()&os.ModeSymlink != 0 {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			data, problem := cliReadPrivateCredential(root, path)
			if problem == "" {
				collectCLIProtocol(e, data)
			}
			return nil
		})
	})
}

// Ephemeral stdout is inspected before returning. Message text is excluded by
// collectCLIProtocol; absent metadata stays unknown, never copied from argv.
func recordCLIOutput(attempt *execution.Attempt, raw []byte) {
	_ = attempt.UpdateEvidence(func(e *execution.ProviderEvidence) {
		collectCLIProtocol(e, raw)
		if e.Source == "private_home" || e.Source == "private_home+cli_json" {
			e.Source = "private_home+cli_json"
		} else {
			e.Source = "cli_json"
		}
	})
}

func initializeCLIEvidence(attempt *execution.Attempt, provider string) error {
	err := attempt.UpdateEvidence(func(e *execution.ProviderEvidence) {
		if provider != "codex" && provider != "claude" {
			return
		}
		identity := readQuotaIdentity(provider, selectedQuotaRoot(provider))
		e.AccountRef, e.IdentityKnown, e.PoolRef = identity.AccountRef, identity.IdentityKnown, identity.PoolRef
		e.Source = "selected_login"
	})
	if err != nil {
		return &execution.UnknownOutcomeError{Err: err}
	}
	return nil
}

func mergeExecutionQuota(q ProviderQuota) ProviderQuota {
	if !q.IdentityKnown {
		return q
	}
	dir, err := execution.EvidenceDirectory()
	if err != nil {
		return q
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return q
	}
	// A bounded newest-first history prevents an old completed dispatch from
	// hiding a newer observation made by an interrupted one.
	type evidenceFile struct {
		path string
		at   time.Time
	}
	var files []evidenceFile
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err == nil {
			files = append(files, evidenceFile{filepath.Join(dir, entry.Name()), info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.After(files[j].at) })
	if len(files) > 64 {
		files = files[:64]
	}
	for _, file := range files {
		record, err := execution.ReadEvidence(file.path)
		if err != nil || record.Engine != q.Provider || !record.IdentityKnown || record.AccountRef != q.AccountRef || record.PoolRef != q.PoolRef {
			continue
		}
		next := q
		next.Windows = nil
		next.FiveHourPct, next.WeeklyPct, next.ThirtyDayPct, next.ScopedPct = nil, nil, nil, nil
		next.SourceAt = time.Time{}
		for _, w := range record.Windows {
			at := time.UnixMilli(w.ObservedAt)
			reset := time.Time{}
			if w.ResetAt > 0 {
				reset = time.UnixMilli(w.ResetAt)
			}
			next.Windows = append(next.Windows, QuotaWindow{PoolRef: record.PoolRef, Model: w.Model, WindowKey: w.Key, WindowMinutes: w.Minutes, UsedPercent: w.UsedPercent, SourceAt: at, ResetAt: reset})
			if at.After(next.SourceAt) {
				next.SourceAt = at
			}
		}
		q = mergeQuotaObservations(q, next)
	}
	return q
}

func recordCLIExit(attempt *execution.Attempt, cmd *exec.Cmd) {
	_ = attempt.UpdateEvidence(func(e *execution.ProviderEvidence) {
		if cmd.ProcessState != nil {
			code := cmd.ProcessState.ExitCode()
			e.ProcessExitCode = &code
		}
	})
}
