package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/makewand/makewand/internal/privatefile"
)

var ErrBudgetExhausted = errors.New("model call budget exhausted")
var ErrConcurrencyBusy = errors.New("account concurrency capacity exhausted")
var ErrUnknownOutcome = errors.New("model call outcome unknown; automatic retry stopped")

type capacityFallbackKey struct{}

// ContextWithCapacityFallback checks for another usable provider only after
// busy admission. Successful dispatches never probe their fallback providers.
func ContextWithCapacityFallback(ctx context.Context, available func() bool) context.Context {
	return context.WithValue(ctx, capacityFallbackKey{}, available)
}

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
	Pending  bool
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
	ID                string
	TaskID            string
	config            Config
	metadata          Metadata
	started           time.Time
	spanID            string
	provider          bool
	mu                sync.Mutex
	completed         bool
	startCommitted    bool
	evidence          *ProviderEvidence
	evidenceError     error
	leaseStop         chan struct{}
	leaseStopOnce     sync.Once
	releaseRetryOnce  sync.Once
	completeRetryOnce sync.Once
}

// withLockedFile uses the same sidecar byte lock as Python's filelock module.
func withLockedFile(ctx context.Context, path string, expected os.FileInfo, fn func(*accountingPath) error) error {
	p, _, err := accountingPathForIdentity(path, expected)
	if err != nil {
		return err
	}
	defer p.close()
	f, err := openExecutionFile(p.root, p.name+".lock", os.O_CREATE|os.O_RDWR)
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
	return fn(p)
}

func readLedger(path *accountingPath, maximum int) (map[string]any, error) {
	data, err := readLedgerBytes(path)
	if errors.Is(err, os.ErrNotExist) {
		if maximum <= 0 && !layeredEnabled(nil) {
			return nil, fmt.Errorf("new model call ledger requires a positive maximum")
		}
		data := map[string]any{"schema": json.Number("1"), "maximum": json.Number(strconv.Itoa(maximum)), "attempts": []any{}}
		if layeredEnabled(nil) {
			if err := initializeScopes(data); err != nil {
				return nil, err
			}
		}
		return data, nil
	}
	if err != nil {
		return nil, err
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
	if err := validateLeases(ledger); err != nil {
		return nil, err
	}
	if ledger["scopes"] != nil {
		if err := validateScopes(ledger); err != nil {
			return nil, err
		}
	}
	if raw, exists := ledger["archived_used"]; exists {
		if n, valid := jsonInteger(raw); !valid || n < 0 {
			return nil, fmt.Errorf("invalid archived call count")
		}
	}
	if err := validateRetention(ledger); err != nil {
		return nil, err
	}
	compactLedger(ledger)
	return ledger, nil
}

// Close the image before decoding or replacing it. Callers hold the sidecar
// lock; a reader never needs to retain a handle across an atomic replacement.
func readLedgerBytes(path *accountingPath) ([]byte, error) {
	file, err := openExecutionFile(path.root, path.name, os.O_RDONLY)
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
	return data, nil
}

func jsonInteger(value any) (int, bool) {
	n, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	result, err := strconv.Atoi(string(n))
	return result, err == nil
}

func saveLedger(path *accountingPath, data map[string]any) error {
	compactLedger(data)
	return writeLedgerFile(path, data)
}

// writeLedgerFile atomically replaces the file with data. Evidence records use
// it directly: they are not call-budget ledgers and must never be compacted.
func writeLedgerFile(path *accountingPath, data map[string]any) error {
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	if len(encoded)+1 > maximumLedgerBytes {
		return fmt.Errorf("model call ledger exceeds 16 MiB")
	}
	temp := ".call-budget-" + NewID()
	var file *os.File
	err = retryAccountingIO(func() error {
		var err error
		file, err = path.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		return err
	})
	if err != nil {
		return err
	}
	defer func() { _ = path.root.Remove(temp) }()
	if err := retryAccountingIO(func() error { return privatefile.Tighten(file) }); err != nil {
		_ = file.Close()
		return fmt.Errorf("protect model call ledger: %w", err)
	}
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
	if err := retryAccountingIO(func() error { return path.root.Rename(temp, path.name) }); err != nil {
		return err
	}
	return syncDirectory(path.root)
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
	waitUntil, waitErr := leaseWaitDeadline(ctx, time.Now())
	if waitErr != nil {
		return nil, waitErr
	}
	attempt := &Attempt{ID: NewID(), TaskID: cfg.TaskID, config: cfg, metadata: metadata, started: time.Now(), spanID: NewID(), provider: true, startCommitted: !metadata.Pending}
	if cfg.LedgerPath != "" {
		delay := 50 * time.Millisecond
		wasBusy := false
		for {
			err = withLockedFile(ctx, cfg.LedgerPath, cfg.ledgerDirectory, func(path *accountingPath) error {
				data, err := readLedger(path, cfg.MaxCalls)
				if err != nil {
					return err
				}
				if layeredEnabled(nil) && data["scopes"] == nil {
					legacy := data["maximum"]
					if err := initializeScopes(data); err != nil {
						return err
					}
					data["legacy_maximum"] = legacy
					legacyUsed, _ := jsonInteger(data["archived_used"])
					for _, raw := range data["attempts"].([]any) {
						if raw.(map[string]any)["status"] != "released" {
							legacyUsed++
						}
					}
					data["legacy_used"] = json.Number(strconv.Itoa(legacyUsed))
					if err := saveLedger(path, data); err != nil {
						return err
					}
				}
				maximum, _ := jsonInteger(data["maximum"])
				if data["scopes"] == nil && cfg.MaxCalls > 0 && cfg.MaxCalls < maximum {
					maximum = cfg.MaxCalls
					data["maximum"] = json.Number(strconv.Itoa(maximum))
					if err := saveLedger(path, data); err != nil {
						return err
					}
				}
				attempts := data["attempts"].([]any)
				held := 0
				var claimed map[string]any
				active := map[string]any{}
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
						now := float64(time.Now().UnixNano()) / 1e9
						if scopes, ok := data["scopes"].(map[string]any); ok {
							clock, _ := scopes["clock"].(json.Number).Float64()
							now = max(now, clock)
						}
						if expires <= now {
							continue
						}
						active[id] = hold
						if data["scopes"] == nil && remaining > maximum-held {
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
				var keys []any
				if data["scopes"] != nil {
					keys, err = scopeKeys(data, cfg, time.Now())
					if err != nil {
						return err
					}
					if err := saveLedger(path, data); err != nil {
						return err
					}
					if claimed != nil && !containsScope(claimed["scopes"], keys[0].([]any)) {
						return fmt.Errorf("capacity hold belongs to another task")
					}
					available, err := scopeAvailable(data, keys, active, claimed)
					if err != nil {
						return err
					}
					if available <= 0 {
						return ErrBudgetExhausted
					}
					scopeSpend(data, keys, 1)
				} else {
					used, _ := jsonInteger(data["archived_used"])
					for _, raw := range attempts {
						if raw.(map[string]any)["status"] != "released" {
							used++
						}
					}
					if used >= maximum-occupied {
						return ErrBudgetExhausted
					}
				}
				if data["scopes"] != nil {
					lease, err := admitLease(data, metadata.Engine, cfg.AccountRef)
					if err != nil {
						return err
					}
					data["leases"].(map[string]any)[attempt.ID] = lease
				}
				if claimed != nil {
					remaining, _ := jsonInteger(claimed["remaining"])
					claimed["remaining"] = json.Number(strconv.Itoa(remaining - 1))
				}
				data["maximum"] = json.Number(strconv.Itoa(maximum))
				data["attempts"] = append(data["attempts"].([]any), map[string]any{
					"id": attempt.ID, "engine": metadata.Engine, "tier": metadata.Tier, "requested_model": metadata.Model, "readonly": metadata.Readonly,
					"benchmark_run": nullableString(os.Getenv("MAKEWAND_BENCHMARK_RUN_ID")), "started_at": float64(attempt.started.UnixNano()) / 1e9,
					"status": func() string {
						if metadata.Pending {
							return "reserved"
						}
						return "started"
					}(), "scopes": keys, "tokens": nil, "monetary_cost": nil, "task_id": cfg.TaskID, "stage": metadata.Stage,
					"account_ref": cfg.AccountRef, "api_policy": nullableString(cfg.APIPolicy), "result_status": nil, "outcome_known": false,
				})
				return saveLedger(path, data)
			})
			if wasBusy && errors.Is(err, context.DeadlineExceeded) {
				err = ErrConcurrencyBusy
			}
			if !errors.Is(err, ErrConcurrencyBusy) {
				break
			}
			wasBusy = true
			if available, _ := ctx.Value(capacityFallbackKey{}).(func() bool); available != nil && available() {
				break
			}
			if !time.Now().Before(waitUntil) {
				if errors.Is(ctx.Err(), context.Canceled) {
					return nil, context.Canceled
				}
				break
			}
			// The ledger lock has been released; repeat the complete atomic check.
			timer := time.NewTimer(min(delay, time.Until(waitUntil)))
			select {
			case <-ctx.Done():
				timer.Stop()
				if errors.Is(ctx.Err(), context.Canceled) {
					return nil, context.Canceled
				}
				return nil, fmt.Errorf("cannot reserve model task: %w", ErrConcurrencyBusy)
			case <-timer.C:
			}
			if !time.Now().Before(waitUntil) {
				if errors.Is(ctx.Err(), context.Canceled) {
					return nil, context.Canceled
				}
				break
			}
			delay = min(delay*2, time.Second)
		}
		if err != nil {
			return nil, fmt.Errorf("cannot reserve model task: %w", err)
		}
	}
	attempt.leaseStop = make(chan struct{})
	if cfg.LedgerPath != "" {
		go attempt.heartbeat()
	}
	attempt.emitBestEffort("start", Outcome{}, nil)
	return attempt, nil
}

// Capacity waits are capped and leave time for dispatch/cleanup. Routing skips
// the wait entirely when another usable provider remains.
var leaseWaitDecimal = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func leaseWaitDeadline(ctx context.Context, now time.Time) (time.Time, error) {
	deadline, bounded := ctx.Deadline()
	value := os.Getenv("MAKEWAND_LEASE_WAIT_SECONDS")
	if value == "" {
		value = "30"
	}
	value = strings.Trim(value, " \t\r\n\v\f")
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || !leaseWaitDecimal.MatchString(value) || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds*float64(time.Second) >= math.Ldexp(1, 63) {
		return time.Time{}, fmt.Errorf("MAKEWAND_LEASE_WAIT_SECONDS must be a finite nonnegative decimal fitting int64 nanoseconds")
	}
	waitUntil := now.Add(time.Duration(seconds * float64(time.Second)))
	if bounded {
		remaining := max(time.Duration(0), deadline.Sub(now))
		latest := deadline.Add(-min(time.Second, remaining/2))
		if latest.Before(waitUntil) {
			waitUntil = latest
		}
	}
	return waitUntil, nil
}

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// Complete uses a bounded independent context: cancellation cannot erase accounting.
func (a *Attempt) Complete(outcome Outcome) error {
	// Retries outlive this call; own measurements rather than retaining the
	// caller's mutable usage storage after returning an accounting error.
	if outcome.Tokens != nil {
		value := *outcome.Tokens
		outcome.Tokens = &value
	}
	if outcome.MonetaryCost != nil {
		value := *outcome.MonetaryCost
		outcome.MonetaryCost = &value
	}
	if outcome.ErrorKind != nil {
		value := *outcome.ErrorKind
		outcome.ErrorKind = &value
	}
	if outcome.ArtifactDigest != nil {
		value := *outcome.ArtifactDigest
		outcome.ArtifactDigest = &value
	}
	err := a.complete(outcome)
	if err != nil && a != nil {
		a.completeRetryOnce.Do(func() { go a.retrySettlement(func() error { return a.complete(outcome) }) })
	}
	return err
}

func (a *Attempt) complete(outcome Outcome) error {
	if a == nil {
		return nil
	}
	defer a.StopLease()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.completed {
		return nil
	}
	var evidenceErr error
	if a.evidence != nil {
		if outcome.Tokens == nil {
			outcome.Tokens = a.evidence.Tokens
		}
		a.evidence.Status, a.evidence.OutcomeKnown, a.evidence.Tokens = outcome.Status, outcome.Known, outcome.Tokens
		evidenceErr = a.evidenceError
		if evidenceErr == nil {
			evidenceErr = a.saveEvidence()
		}
		if evidenceErr != nil {
			outcome.Status, outcome.Known = Unknown, false
			a.evidence.Status, a.evidence.OutcomeKnown = Unknown, false
			evidenceErr = fmt.Errorf("provider evidence unavailable: %w", evidenceErr)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if a.config.LedgerPath != "" {
		if err := withLockedFile(ctx, a.config.LedgerPath, a.config.ledgerDirectory, func(path *accountingPath) error {
			data, err := readLedger(path, a.config.MaxCalls)
			if err != nil {
				return err
			}
			for _, raw := range data["attempts"].([]any) {
				entry, ok := raw.(map[string]any)
				if !ok || entry["id"] != a.ID {
					continue
				}
				if entry["status"] == "completed" || entry["status"] == "released" {
					return nil
				}
				if entry["status"] == "reserved" {
					if entry["scopes"] != nil {
						scopeSpend(data, entry["scopes"], -1)
					}
					entry["status"] = "released"
				} else {
					entry["status"] = "completed"
				}
				entry["finished_at"] = float64(time.Now().UnixNano()) / 1e9
				entry["success"] = outcome.Status == Passed
				entry["seconds"] = time.Since(a.started).Seconds()
				entry["result_status"] = outcome.Status
				entry["outcome_known"] = outcome.Known
				entry["tokens"] = outcome.Tokens
				entry["monetary_cost"] = outcome.MonetaryCost
				settleLease(data, a.ID)
				return saveLedger(path, data)
			}
			return errReservationDisappeared
		}); err != nil {
			return errors.Join(evidenceErr, err)
		}
	}
	duration := time.Since(a.started).Milliseconds()
	a.emitBestEffort("end", outcome, &duration)
	a.completed = true
	return evidenceErr
}

// StopLease gives up renewal even when the ledger is temporarily unavailable.
// Settlement retries below still refund the reservation once it is writable.
func (a *Attempt) StopLease() {
	if a != nil && a.leaseStop != nil {
		a.leaseStopOnce.Do(func() { close(a.leaseStop) })
	}
}

var errReservationDisappeared = errors.New("model task reservation disappeared")

// Each operation has at most one retry loop. Each write has a five-second
// lock budget; backoff caps IO while a live daemon waits for ledger recovery.
// This never launches a provider or renews an abandoned account's heartbeat.
func (a *Attempt) retrySettlement(settle func() error) {
	for delay := 100 * time.Millisecond; ; delay = min(delay*2, 30*time.Second) {
		time.Sleep(delay)
		err := settle()
		if err == nil || errors.Is(err, errReservationDisappeared) {
			return
		}
	}
}

// Start commits the dispatch boundary; ReleaseUnstarted only releases a
// reservation that has not crossed it. Both are repeat-safe across processes.
func (a *Attempt) Start() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	err := a.transition(false, false)
	if err == nil {
		a.startCommitted = true
	} else if !a.startCommitted {
		// No caller has crossed the dispatch boundary. A failed directory
		// fsync may have persisted "started" already; refund that state too.
		err = errors.Join(err, a.release(true))
	}
	return err
}
func (a *Attempt) ReleaseUnstarted() error {
	return a.release(false)
}
func (a *Attempt) release(beforeDispatch bool) error {
	err := a.transition(true, beforeDispatch)
	if err != nil && a != nil {
		a.releaseRetryOnce.Do(func() { go a.retrySettlement(func() error { return a.transition(true, beforeDispatch) }) })
	}
	return err
}
func (a *Attempt) transition(release, beforeDispatch bool) error {
	stopLease := release
	if release && a != nil {
		defer func() {
			if stopLease {
				a.StopLease()
			}
		}()
	}
	if a == nil || a.config.LedgerPath == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return withLockedFile(ctx, a.config.LedgerPath, a.config.ledgerDirectory, func(path *accountingPath) error {
		data, err := readLedger(path, a.config.MaxCalls)
		if err != nil {
			return err
		}
		for _, raw := range data["attempts"].([]any) {
			e := raw.(map[string]any)
			if e["id"] != a.ID {
				continue
			}
			if e["status"] != "reserved" && (!release || !beforeDispatch || e["status"] != "started") {
				if !release && e["status"] != "started" {
					return fmt.Errorf("cannot start a released or settled reservation")
				}
				// Public ReleaseUnstarted is a no-op for a running provider.
				if release && e["status"] == "started" {
					stopLease = false
				}
				return nil
			}
			if release {
				if e["scopes"] != nil {
					scopeSpend(data, e["scopes"], -1)
				}
				e["status"] = "released"
				e["finished_at"] = float64(time.Now().UnixNano()) / 1e9
				if leases, ok := data["leases"].(map[string]any); ok {
					delete(leases, a.ID)
				}
			} else {
				e["status"] = "started"
			}
			return saveLedger(path, data)
		}
		return errReservationDisappeared
	})
}

// InitializeBudget prepares the shared file without consuming a dispatch.
func InitializeBudget(ctx context.Context, cfg Config) error {
	return withLockedFile(ctx, cfg.LedgerPath, nil, func(path *accountingPath) error {
		data, err := readLedger(path, cfg.MaxCalls)
		if err != nil {
			return err
		}
		return saveLedger(path, data)
	})
}
