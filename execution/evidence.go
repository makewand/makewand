package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	ResolvedModel   string           `json:"resolved_model,omitempty"`
	Source          string           `json:"source"`
	ObservedAt      int64            `json:"observed_at_ms"`
	Windows         []EvidenceWindow `json:"windows"`
	Status          Status           `json:"result_status"`
	OutcomeKnown    bool             `json:"outcome_known"`
	ProcessExitCode *int             `json:"process_exit_code"`
	Tokens          *int64           `json:"tokens"`
	// Settled turns true only once the attempt's accounting is final: its
	// ledger settled it and released its account lease, or it has no ledger.
	// Retention prunes settled records only.
	Settled bool `json:"settled"`
}

type EvidenceWindow struct {
	Model       string  `json:"model"`
	Scope       string  `json:"scope,omitempty"`
	LimitID     string  `json:"limit_id,omitempty"`
	Key         string  `json:"window_key"`
	Minutes     int     `json:"window_minutes"`
	UsedPercent float64 `json:"used_percent"`
	ObservedAt  int64   `json:"observed_at_ms"`
	ResetAt     int64   `json:"reset_at_ms"`
}

// ErrEvidenceDegraded marks a first evidence write that failed before the
// provider started. The call proceeds without a record: nothing on disk can
// contradict its outcome, so it is degraded telemetry, not an unknown outcome.
var ErrEvidenceDegraded = errors.New("provider evidence unavailable before launch; continuing without a record")

var evidenceWarning sync.Once

// EvidenceDirectory is shared with the Python runner. Every attempt owns one
// atomic record; no bounded budget or optional telemetry switch is required.
func EvidenceDirectory() (string, error) {
	if value := os.Getenv("MAKEWAND_EXECUTION_EVIDENCE_DIR"); value != "" {
		return value, nil
	}
	cache, err := os.UserCacheDir()
	return filepath.Join(cache, "makewand", "execution-evidence"), err
}

// UpdateEvidence applies update and persists the record. Once a record exists
// or the provider may have started, a failed write is kept and turns the
// outcome UNKNOWN at completion. A failed first write before launch instead
// degrades this attempt to no record at all (ErrEvidenceDegraded); later
// updates are then no-ops.
func (a *Attempt) UpdateEvidence(update func(*ProviderEvidence)) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.evidenceDegraded {
		return nil
	}
	if a.evidence == nil {
		a.evidence = &ProviderEvidence{Schema: 1, TaskID: a.TaskID, AttemptID: a.ID, Engine: a.metadata.Engine,
			Source: "unavailable", ObservedAt: time.Now().UnixMilli(), Status: Unknown}
	}
	update(a.evidence)
	a.evidence.ObservedAt = time.Now().UnixMilli()
	err := a.saveEvidence()
	var published *accountingPublishedError
	if errors.As(err, &published) {
		a.evidencePersisted = true
	}
	switch {
	case err == nil:
		a.evidencePersisted = true
	case !a.evidencePersisted && !a.startCommitted:
		a.evidence, a.evidenceDegraded = nil, true
		err = fmt.Errorf("%w: %w", ErrEvidenceDegraded, err)
		if a.config.EventError != nil {
			a.config.EventError(err)
		} else {
			evidenceWarning.Do(func() {
				_, _ = fmt.Fprintln(os.Stderr, "warning: provider execution evidence unavailable; continuing without a record")
			})
		}
	default:
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
	err = withEvidenceLockedFile(ctx, path, directory, func(p *accountingPath) error {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		return writeEvidenceRecord(p, value)
	})
	if err == nil {
		appendEvidenceIndex(filepath.Dir(path), directory, a.evidence)
	}
	return err
}

// Keep a durable pending marker across record publication. Readers and
// retention ignore an uncertain version even if its final rename succeeded.
// Marker cleanup is optional once the record itself is durably committed.
func writeEvidenceRecord(p *accountingPath, value map[string]any) error {
	marker, err := openExecutionFile(p.root, p.name+".pending", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer marker.Close()
	if err := marker.Sync(); err != nil {
		return err
	}
	if err := syncDirectory(p.root); err != nil {
		return err
	}
	if err := writeLedgerFile(p, value); err != nil {
		return err
	}
	_ = retryAccountingIO(func() error { return p.root.Remove(p.name + ".pending") })
	_ = syncDirectory(p.root)
	return nil
}

// settleEvidence marks the record settled and prunes the store when due. It
// runs once the ledger settled the attempt and released its lease (or there
// is no ledger). A record that cannot be marked stays open and is never pruned.
func (a *Attempt) settleEvidence() error {
	if a.evidence == nil {
		return nil
	}
	if !a.evidence.Settled {
		a.evidence.Settled = true
		if err := a.saveEvidence(); err != nil {
			a.evidence.Settled = false
			return err
		}
	}
	if dir, err := EvidenceDirectory(); err == nil {
		_ = compactEvidence(dir, time.Now(), false)
	}
	return nil
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
	p, err := openAccountingPath(path, false)
	if err != nil {
		return ProviderEvidence{}, err
	}
	defer p.close()
	return readEvidenceAt(p.root, p.name)
}
