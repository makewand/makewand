package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

var ErrBudgetExhausted = errors.New("model call budget exhausted")
var ErrUnknownOutcome = errors.New("model call outcome unknown; automatic retry stopped")

const maximumLedgerBytes = 16 << 20

type UnknownOutcomeError struct{ Err error }

func (e *UnknownOutcomeError) Error() string {
	if errors.Is(e.Err, context.DeadlineExceeded) {
		return fmt.Sprintf("%s (timeout): %v", ErrUnknownOutcome, e.Err)
	}
	return fmt.Sprintf("%s: %v", ErrUnknownOutcome, e.Err)
}
func (e *UnknownOutcomeError) Unwrap() error        { return e.Err }
func (e *UnknownOutcomeError) Is(target error) bool { return target == ErrUnknownOutcome }

type Metadata struct {
	Engine   string
	Tier     string
	Model    *string
	Stage    string
	Readonly bool
}

type Outcome struct {
	Status         Status
	Known          bool
	Tokens         *int64
	MonetaryCost   *float64
	ErrorKind      *string
	ArtifactDigest *string
}

type Attempt struct {
	ID        string
	TaskID    string
	config    Config
	metadata  Metadata
	started   time.Time
	spanID    string
	provider  bool
	mu        sync.Mutex
	completed bool
}

// withLockedFile uses the same sidecar byte lock as Python's filelock module.
func withLockedFile(ctx context.Context, path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := openLockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		locked, err := tryLock(f)
		if err != nil {
			return err
		}
		if locked {
			break
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer func() { _ = unlock(f) }()
	return fn()
}

func readLedger(path string, maximum int) (map[string]any, error) {
	file, err := openLedgerFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if maximum <= 0 {
			return nil, fmt.Errorf("new model call ledger requires a positive maximum")
		}
		return map[string]any{"schema": json.Number("1"), "maximum": json.Number(strconv.Itoa(maximum)), "attempts": []any{}}, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maximumLedgerBytes {
		return nil, fmt.Errorf("model call ledger exceeds 16 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumLedgerBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maximumLedgerBytes {
		return nil, fmt.Errorf("model call ledger exceeds 16 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var ledger map[string]any
	if err := decoder.Decode(&ledger); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("invalid trailing budget ledger data")
	}
	schema, ok := jsonInteger(ledger["schema"])
	if _, attemptsOK := ledger["attempts"].([]any); schema != Schema || !ok || !attemptsOK {
		return nil, fmt.Errorf("invalid model call ledger schema or attempts")
	}
	if limit, valid := jsonInteger(ledger["maximum"]); !valid || limit <= 0 {
		return nil, fmt.Errorf("invalid model call ledger maximum")
	}
	identifiers := make(map[string]struct{})
	for _, raw := range ledger["attempts"].([]any) {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid model call ledger attempt")
		}
		id, ok := entry["id"].(string)
		if _, duplicate := identifiers[id]; !ok || id == "" || duplicate {
			return nil, fmt.Errorf("invalid or duplicate model call ledger attempt identity")
		}
		identifiers[id] = struct{}{}
	}
	return ledger, nil
}

func jsonInteger(value any) (int, bool) {
	n, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	result, err := strconv.Atoi(string(n))
	return result, err == nil
}

func saveLedger(path string, data map[string]any) error {
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	if len(encoded)+1 > maximumLedgerBytes {
		return fmt.Errorf("model call ledger exceeds 16 MiB")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".call-budget-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer func() { _ = os.Remove(temp) }()
	if _, err = file.Write(append(encoded, '\n')); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// #nosec G703 -- both paths belong to the explicit ledger directory; replacement is protected by its sidecar lock.
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// Reserve permanently counts a dispatch before it starts. Failed, canceled, or
// crashed attempts are never refunded. Unexpired workflow holds occupy capacity.
func Reserve(ctx context.Context, metadata Metadata) (*Attempt, error) {
	ctx, cfg, err := EnsureContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if metadata.Stage == "" {
		metadata.Stage = StageFromContext(ctx)
	}
	if !ValidIdentifier(metadata.Stage) {
		return nil, fmt.Errorf("invalid execution stage")
	}
	attempt := &Attempt{ID: NewID(), TaskID: cfg.TaskID, config: cfg, metadata: metadata, started: time.Now(), spanID: NewID(), provider: true}
	if cfg.LedgerPath != "" {
		err = withLockedFile(ctx, cfg.LedgerPath, func() error {
			data, err := readLedger(cfg.LedgerPath, cfg.MaxCalls)
			if err != nil {
				return err
			}
			maximum, _ := jsonInteger(data["maximum"])
			if cfg.MaxCalls > 0 && cfg.MaxCalls < maximum {
				maximum = cfg.MaxCalls
				data["maximum"] = maximum
				if err := saveLedger(cfg.LedgerPath, data); err != nil {
					return err
				}
			}
			attempts := data["attempts"].([]any)
			held := 0
			var claimed map[string]any
			if raw, exists := data["holds"]; exists {
				holds, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid budget holds")
				}
				for id, rawHold := range holds {
					if id == "" {
						return fmt.Errorf("invalid budget hold identity")
					}
					hold, ok := rawHold.(map[string]any)
					if !ok {
						return fmt.Errorf("invalid budget hold")
					}
					remaining, valid := jsonInteger(hold["remaining"])
					expiryNumber, expiryOK := hold["expires_at"].(json.Number)
					expires, expiryErr := expiryNumber.Float64()
					if !valid || remaining < 0 || !expiryOK || expiryErr != nil {
						return fmt.Errorf("invalid budget hold capacity or expiry")
					}
					if expires <= float64(time.Now().UnixNano())/1e9 {
						continue
					}
					if remaining > maximum-held {
						return fmt.Errorf("budget holds exceed maximum")
					}
					held += remaining
					if id == cfg.LeaseID && remaining > 0 {
						claimed = hold
					}
				}
			}
			if cfg.LeaseID != "" && claimed == nil {
				return fmt.Errorf("model call budget lease unavailable")
			}
			occupied := held
			if claimed != nil {
				occupied--
			}
			if len(attempts) >= maximum-occupied {
				return ErrBudgetExhausted
			}
			if claimed != nil {
				remaining, _ := jsonInteger(claimed["remaining"])
				claimed["remaining"] = remaining - 1
			}
			data["maximum"] = maximum
			data["attempts"] = append(attempts, map[string]any{
				"id": attempt.ID, "engine": metadata.Engine, "tier": metadata.Tier, "requested_model": metadata.Model, "readonly": metadata.Readonly,
				"benchmark_run": nullableString(os.Getenv("MAKEWAND_BENCHMARK_RUN_ID")), "started_at": float64(attempt.started.UnixNano()) / 1e9,
				"status": "started", "tokens": nil, "monetary_cost": nil, "task_id": cfg.TaskID, "stage": metadata.Stage,
				"account_ref": cfg.AccountRef, "api_policy": nullableString(cfg.APIPolicy), "result_status": nil, "outcome_known": false,
			})
			return saveLedger(cfg.LedgerPath, data)
		})
		if err != nil {
			return nil, fmt.Errorf("cannot reserve model task: %w", err)
		}
	}
	attempt.emitBestEffort("start", Outcome{}, nil)
	return attempt, nil
}

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// Complete uses a bounded independent context: cancellation cannot erase accounting.
func (a *Attempt) Complete(outcome Outcome) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.completed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if a.config.LedgerPath != "" {
		if err := withLockedFile(ctx, a.config.LedgerPath, func() error {
			data, err := readLedger(a.config.LedgerPath, a.config.MaxCalls)
			if err != nil {
				return err
			}
			for _, raw := range data["attempts"].([]any) {
				entry, ok := raw.(map[string]any)
				if !ok || entry["id"] != a.ID {
					continue
				}
				entry["status"] = "completed"
				entry["success"] = outcome.Status == Passed
				entry["seconds"] = time.Since(a.started).Seconds()
				entry["result_status"] = outcome.Status
				entry["outcome_known"] = outcome.Known
				entry["tokens"] = outcome.Tokens
				entry["monetary_cost"] = outcome.MonetaryCost
				return saveLedger(a.config.LedgerPath, data)
			}
			return fmt.Errorf("model task reservation disappeared")
		}); err != nil {
			return err
		}
	}
	duration := time.Since(a.started).Milliseconds()
	a.emitBestEffort("end", outcome, &duration)
	a.completed = true
	return nil
}
