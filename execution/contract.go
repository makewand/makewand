// Package execution defines Makewand's cross-language execution and admission contract.
package execution

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
)

type StatusError struct {
	Status       Status
	OutcomeKnown bool
	Err          error
}

func (e *StatusError) Error() string { return e.Err.Error() }
func (e *StatusError) Unwrap() error { return e.Err }

func ErrorStatus(err error) Status {
	if err == nil {
		return Passed
	}
	var statusError *StatusError
	if errors.As(err, &statusError) && statusError.Status.Valid() {
		return statusError.Status
	}
	switch {
	case errors.Is(err, ErrBudgetExhausted):
		return BudgetExhausted
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout
	case errors.Is(err, context.Canceled):
		return Cancelled
	case errors.Is(err, ErrUnknownOutcome):
		return Unknown
	}
	// Providers expose their classification through an interface so this shared
	// protocol package does not depend on any router or transport implementation.
	var classified interface{ ExecutionStatus() Status }
	if errors.As(err, &classified) {
		if status := classified.ExecutionStatus(); status.Valid() {
			return status
		}
	}
	return InternalError
}

const Schema = 1

type Status string

const (
	Passed             Status = "PASSED"
	InternalError      Status = "INTERNAL_ERROR"
	InvalidRequest     Status = "INVALID_REQUEST"
	Failed             Status = "FAILED"
	Unverified         Status = "UNVERIFIED"
	Cancelled          Status = "CANCELLED"
	BudgetExhausted    Status = "BUDGET_EXHAUSTED"
	ApplyConflict      Status = "APPLY_CONFLICT"
	SandboxUnavailable Status = "SANDBOX_UNAVAILABLE"
	Timeout            Status = "TIMEOUT"
	Unknown            Status = "UNKNOWN"
)

func (s Status) ExitCode() int {
	switch s {
	case Passed:
		return 0
	case InternalError:
		return 1
	case InvalidRequest:
		return 2
	case Failed:
		return 10
	case Unverified:
		return 11
	case Cancelled:
		return 12
	case BudgetExhausted:
		return 13
	case ApplyConflict:
		return 14
	case SandboxUnavailable:
		return 15
	case Timeout:
		return 16
	case Unknown:
		return 17
	default:
		return 1
	}
}

func (s Status) Valid() bool {
	switch s {
	case Passed, InternalError, InvalidRequest, Failed, Unverified, Cancelled, BudgetExhausted, ApplyConflict, SandboxUnavailable, Timeout, Unknown:
		return true
	default:
		return false
	}
}

// Nullable measurements remain null until a transport supplies authoritative data.
type Request struct {
	Schema         int     `json:"schema"`
	TaskID         string  `json:"task_id"`
	Stage          string  `json:"stage"`
	Engine         string  `json:"engine"`
	Tier           string  `json:"tier"`
	Model          *string `json:"model"`
	AccountRef     *string `json:"account_ref"`
	Readonly       bool    `json:"readonly"`
	RepoTrust      string  `json:"repo_trust"`
	APIPolicy      string  `json:"api_policy"`
	DeadlineUnixMS *int64  `json:"deadline_unix_ms"`
	TimeoutMS      *int64  `json:"timeout_ms"`
	BudgetFile     *string `json:"budget_file"`
	MaxModelCalls  *int    `json:"max_model_calls"`
	Workflow       *string `json:"workflow"`
	Risk           *string `json:"risk"`
	Prompt         *string `json:"prompt"`
	CWD            *string `json:"cwd"`
}

type Result struct {
	Schema         int      `json:"schema"`
	TaskID         string   `json:"task_id"`
	AttemptID      *string  `json:"attempt_id"`
	Stage          string   `json:"stage"`
	Engine         string   `json:"engine"`
	Status         Status   `json:"status"`
	ExitCode       int      `json:"exit_code"`
	AccountRef     *string  `json:"account_ref"`
	Readonly       bool     `json:"readonly"`
	DurationMS     *int64   `json:"duration_ms"`
	ArtifactDigest *string  `json:"artifact_digest"`
	Tokens         *int64   `json:"tokens"`
	MonetaryCost   *float64 `json:"monetary_cost"`
	ErrorKind      *string  `json:"error_kind"`
	OutcomeKnown   bool     `json:"outcome_known"`
	Output         *string  `json:"output"`
	Error          *string  `json:"error"`
}

func (r Request) Validate() error {
	if r.Schema != Schema || !ValidIdentifier(r.TaskID) || !ValidIdentifier(r.Stage) || !ValidIdentifier(r.Engine) || !ValidIdentifier(r.Tier) {
		return fmt.Errorf("invalid execution request schema, identity, stage, or engine")
	}
	if r.TimeoutMS != nil && *r.TimeoutMS <= 0 || r.MaxModelCalls != nil && *r.MaxModelCalls <= 0 {
		return fmt.Errorf("execution timeout and model call maximum must be positive")
	}
	if r.RepoTrust != "trusted" && r.RepoTrust != "untrusted" || r.APIPolicy != "subscription_only" && r.APIPolicy != "allow_paid" {
		return fmt.Errorf("invalid execution trust or API policy")
	}
	if r.DeadlineUnixMS != nil && *r.DeadlineUnixMS <= 0 {
		return fmt.Errorf("execution deadline must be positive")
	}
	if r.AccountRef != nil && !ValidIdentifier(*r.AccountRef) {
		return fmt.Errorf("invalid execution account reference")
	}
	for _, value := range []*string{r.Model, r.BudgetFile, r.Prompt, r.CWD} {
		if value != nil && strings.ContainsRune(*value, '\x00') {
			return fmt.Errorf("execution text contains NUL")
		}
	}
	if r.Workflow != nil && !oneOf(*r.Workflow, "auto", "single", "pipeline", "race", "review", "direct") {
		return fmt.Errorf("invalid execution workflow")
	}
	if r.Risk != nil && !oneOf(*r.Risk, "auto", "low", "medium", "high", "unknown") {
		return fmt.Errorf("invalid execution risk")
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, option := range allowed {
		if value == option {
			return true
		}
	}
	return false
}

func validateMeasurements(duration, tokens, rss *int64, cost *float64) error {
	for _, value := range []*int64{duration, tokens, rss} {
		if value != nil && *value < 0 {
			return fmt.Errorf("execution measurements must be nonnegative")
		}
	}
	if cost != nil && (*cost < 0 || math.IsNaN(*cost) || math.IsInf(*cost, 0)) {
		return fmt.Errorf("execution monetary cost must be finite and nonnegative")
	}
	return nil
}

func (r Result) Validate() error {
	if r.Schema != Schema || !ValidIdentifier(r.TaskID) || !ValidIdentifier(r.Stage) || !ValidIdentifier(r.Engine) || !r.Status.Valid() || r.ExitCode != r.Status.ExitCode() {
		return fmt.Errorf("invalid execution result schema, identity, status, or exit code")
	}
	if r.AttemptID != nil && !ValidIdentifier(*r.AttemptID) {
		return fmt.Errorf("invalid execution attempt identity")
	}
	if r.AccountRef != nil && !ValidIdentifier(*r.AccountRef) || r.ErrorKind != nil && !ValidIdentifier(*r.ErrorKind) {
		return fmt.Errorf("invalid execution account or error kind")
	}
	if r.Status == Unknown && r.OutcomeKnown {
		return fmt.Errorf("unknown execution result cannot claim a known outcome")
	}
	if !validDigest(r.ArtifactDigest) {
		return fmt.Errorf("invalid execution artifact digest")
	}
	for _, value := range []*string{r.Output, r.Error} {
		if value != nil && strings.ContainsRune(*value, '\x00') {
			return fmt.Errorf("execution result text contains NUL")
		}
	}
	return validateMeasurements(r.DurationMS, r.Tokens, nil, r.MonetaryCost)
}

func validDigest(digest *string) bool {
	if digest == nil {
		return true
	}
	if len(*digest) != 64 {
		return false
	}
	for _, c := range *digest {
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' {
			continue
		}
		return false
	}
	return true
}
