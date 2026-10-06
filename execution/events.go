package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

var ErrInvalidStage = errors.New("invalid execution stage")

// Event deliberately excludes prompts, paths, raw errors, and credentials.
type Event struct {
	Schema         int      `json:"schema"`
	EventID        string   `json:"event_id"`
	TaskID         string   `json:"task_id"`
	BenchmarkRun   *string  `json:"benchmark_run"`
	Stage          string   `json:"stage"`
	Event          string   `json:"event"`
	Engine         *string  `json:"engine"`
	AttemptID      *string  `json:"attempt_id"`
	Readonly       bool     `json:"readonly"`
	Status         *Status  `json:"status"`
	StartUnixMS    int64    `json:"start_unix_ms"`
	DurationMS     *int64   `json:"duration_ms"`
	ArtifactDigest *string  `json:"artifact_digest"`
	ErrorKind      *string  `json:"error_kind"`
	Tokens         *int64   `json:"tokens"`
	MonetaryCost   *float64 `json:"monetary_cost"`
	PeakRSSBytes   *int64   `json:"peak_rss_bytes"`
	AccountRef     *string  `json:"account_ref"`
}

func (a *Attempt) emit(kind string, outcome Outcome, duration *int64) error {
	if a.config.EventsPath == "" {
		return nil
	}
	stage := a.metadata.Stage
	if a.provider {
		stage = "provider"
	}
	event := Event{Schema: Schema, EventID: a.spanID, TaskID: a.TaskID, BenchmarkRun: nullableString(os.Getenv("MAKEWAND_BENCHMARK_RUN_ID")), Stage: stage,
		Event: kind, Engine: nullableString(a.metadata.Engine), AttemptID: &a.ID, Readonly: a.metadata.Readonly, StartUnixMS: a.started.UnixMilli(), DurationMS: duration,
		ArtifactDigest: outcome.ArtifactDigest, ErrorKind: outcome.ErrorKind, Tokens: outcome.Tokens, MonetaryCost: outcome.MonetaryCost, AccountRef: a.config.AccountRef}
	if kind == "end" {
		event.Status = &outcome.Status
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	return withLockedFile(ctx, a.config.EventsPath, a.config.eventsDirectory, func(path *accountingPath) error {
		file, err := openExecutionFile(path.root, path.name, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(append(encoded, '\n'))
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	})
}

func (a *Attempt) emitBestEffort(kind string, outcome Outcome, duration *int64) {
	if err := a.emit(kind, outcome, duration); err != nil {
		if a.config.EventError != nil {
			a.config.EventError(err)
		} else {
			_, _ = fmt.Fprintln(os.Stderr, "warning: execution event recorder unavailable")
		}
	}
}

func (e Event) Validate() error {
	if e.Schema != Schema || !ValidIdentifier(e.EventID) || !ValidIdentifier(e.TaskID) || !ValidIdentifier(e.Stage) || !oneOf(e.Event, "start", "end") || e.StartUnixMS < 0 {
		return fmt.Errorf("invalid execution event schema, identity, stage, or timestamp")
	}
	if e.Engine != nil && !ValidIdentifier(*e.Engine) || e.AttemptID != nil && !ValidIdentifier(*e.AttemptID) {
		return fmt.Errorf("invalid execution event engine or attempt")
	}
	if e.Status != nil && !e.Status.Valid() {
		return fmt.Errorf("invalid execution event status")
	}
	if e.Event == "start" && (e.Status != nil || e.DurationMS != nil) || e.Event == "end" && (e.Status == nil || e.DurationMS == nil) {
		return fmt.Errorf("execution event terminal fields do not match its event kind")
	}
	for _, value := range []*string{e.BenchmarkRun, e.AccountRef, e.ErrorKind} {
		if value != nil && !ValidIdentifier(*value) {
			return fmt.Errorf("invalid execution event reference")
		}
	}
	if !validDigest(e.ArtifactDigest) {
		return fmt.Errorf("invalid execution event artifact digest")
	}
	return validateMeasurements(e.DurationMS, e.Tokens, e.PeakRSSBytes, e.MonetaryCost)
}

// StartStage records a non-provider stage without consuming a model call slot.
func StartStage(ctx context.Context, stage, engine string, readonly bool) (*Attempt, error) {
	return StartStageForAttempt(ctx, stage, engine, readonly, NewID())
}

func StartStageForAttempt(ctx context.Context, stage, engine string, readonly bool, attemptID string) (*Attempt, error) {
	_, cfg, err := EnsureContext(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidIdentifier(stage) {
		return nil, ErrInvalidStage
	}
	cfg.LedgerPath = ""
	if !ValidIdentifier(attemptID) {
		return nil, ErrInvalidStage
	}
	a := &Attempt{ID: attemptID, TaskID: cfg.TaskID, config: cfg, metadata: Metadata{Stage: stage, Engine: engine, Readonly: readonly}, started: time.Now(), spanID: NewID()}
	a.emitBestEffort("start", Outcome{}, nil)
	return a, nil
}
