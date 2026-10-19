package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ProviderEvidence contains only allowlisted runner observations. Requested
// models and admission account references are deliberately separate from them.
type ProviderEvidence struct {
	Schema          int              `json:"schema"`
	TaskID          string           `json:"task_id"`
	AttemptID       string           `json:"attempt_id"`
	Engine          string           `json:"engine"`
	AccountRef      string           `json:"account_ref,omitempty"`
	IdentityKnown   bool             `json:"identity_known"`
	PoolRef         string           `json:"pool_ref,omitempty"`
	Model           string           `json:"actual_model,omitempty"`
	Source          string           `json:"source"`
	ObservedAt      int64            `json:"observed_at_ms"`
	Windows         []EvidenceWindow `json:"windows"`
	Status          Status           `json:"result_status"`
	OutcomeKnown    bool             `json:"outcome_known"`
	ProcessExitCode *int             `json:"process_exit_code"`
	Tokens          *int64           `json:"tokens"`
}

type EvidenceWindow struct {
	Model       string  `json:"model"`
	Key         string  `json:"window_key"`
	Minutes     int     `json:"window_minutes"`
	UsedPercent float64 `json:"used_percent"`
	ObservedAt  int64   `json:"observed_at_ms"`
	ResetAt     int64   `json:"reset_at_ms"`
}

// EvidenceDirectory is shared with the Python runner. Every attempt owns one
// atomic record; no bounded budget or optional telemetry switch is required.
func EvidenceDirectory() (string, error) {
	if value := os.Getenv("MAKEWAND_EXECUTION_EVIDENCE_DIR"); value != "" {
		return value, nil
	}
	cache, err := os.UserCacheDir()
	return filepath.Join(cache, "makewand", "execution-evidence"), err
}

func (a *Attempt) UpdateEvidence(update func(*ProviderEvidence)) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.evidence == nil {
		a.evidence = &ProviderEvidence{Schema: 1, TaskID: a.TaskID, AttemptID: a.ID, Engine: a.metadata.Engine,
			Source: "unavailable", ObservedAt: time.Now().UnixMilli(), Status: Unknown}
	}
	update(a.evidence)
	a.evidence.ObservedAt = time.Now().UnixMilli()
	err := a.saveEvidence()
	if err != nil {
		a.evidenceError = err
	}
	return err
}

func (a *Attempt) saveEvidence() error {
	if a.evidence == nil {
		return nil
	}
	dir, err := EvidenceDirectory()
	if err != nil {
		return err
	}
	path, directory, err := resolvePathIdentity(filepath.Join(dir, a.ID+".json"), nil)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(a.evidence)
	if err != nil {
		return err
	}
	ctx, cancel := a.evidenceWriteContext()
	defer cancel()
	return withLockedFile(ctx, path, directory, func(p *accountingPath) error {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		return writeLedgerFile(p, value)
	})
}

func (a *Attempt) evidenceWriteContext() (context.Context, context.CancelFunc) {
	if a.evidenceContext != nil {
		return context.WithCancel(a.evidenceContext)
	}
	// Evidence is durable accounting, not best-effort telemetry. Use the ledger
	// settlement budget and keep caller cancellation from erasing the outcome.
	return context.WithTimeout(context.Background(), accountingWriteTimeout)
}

// ReadEvidence rejects non-regular and oversized records. Quota consumers must
// additionally match account/pool and check each window's original timestamp.
func ReadEvidence(path string) (ProviderEvidence, error) {
	var value ProviderEvidence
	p, err := openAccountingPath(path, false)
	if err != nil {
		return value, err
	}
	defer p.close()
	file, err := openExecutionFile(p.root, p.name, os.O_RDONLY)
	if err != nil {
		return value, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() > 1<<20 {
		return value, fmt.Errorf("invalid evidence file")
	}
	err = json.NewDecoder(io.LimitReader(file, 1<<20+1)).Decode(&value)
	if err == nil && (value.Schema != 1 || !ValidIdentifier(value.AttemptID) || !ValidIdentifier(value.Engine)) {
		err = fmt.Errorf("invalid provider evidence")
	}
	return value, err
}
