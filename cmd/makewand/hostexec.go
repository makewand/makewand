package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/diag"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
)

// unsafeHostExecAuditFile is the JSONL audit log (under the config dir) that
// records every command executed directly on the host under the
// MAKEWAND_UNSAFE_HOST_EXEC opt-in.
const unsafeHostExecAuditFile = "unsafe_exec_audit.jsonl"

// Test seams: the environment opt-in check, the terminal interactivity probe,
// the acknowledgment line reader, and the persistence step. Production wiring
// stays in the var initializers; tests swap them to drive the interactive
// branches without a TTY.
var (
	unsafeHostExecRequested = func() bool {
		return os.Getenv("MAKEWAND_UNSAFE_HOST_EXEC") == "1"
	}
	unsafeHostExecInteractive = isInteractiveTerminal
	unsafeHostExecReadLine    = func() (string, error) {
		return bufio.NewReader(os.Stdin).ReadString('\n')
	}
	persistUnsafeHostExecAckFn = persistUnsafeHostExecAck
)

// resolveUnsafeHostExecAuth turns the MAKEWAND_UNSAFE_HOST_EXEC=1 request into
// an explicit engine authorization. The environment variable alone never
// authorizes host execution:
//
//   - variable unset: no authorization (engine stays fail-closed as before);
//   - variable set + valid recorded acknowledgment: authorized, with a
//     prominent per-session warning;
//   - variable set + no acknowledgment + interactive terminal: a one-time
//     responsibility prompt; accepting records the acknowledgment in config;
//   - variable set + no acknowledgment + non-interactive: refused (fail
//     closed) with guidance on how to acknowledge.
//
// Every execution under the returned authorization is appended to the audit
// log, not just the first.
func resolveUnsafeHostExecAuth(cfg *config.Config) engine.UnsafeHostExecAuthorization {
	if !unsafeHostExecRequested() {
		return engine.UnsafeHostExecAuthorization{}
	}

	i18n.SetLanguage(cfg.Language)
	msg := i18n.Msg()

	if cfg.UnsafeHostExecAckValid() {
		fmt.Fprintf(os.Stderr, "[makewand] %s\n", fmt.Sprintf(msg.UnsafeHostExecActiveWarning, cfg.UnsafeHostExecAckAt, unsafeHostExecAuditPathDisplay()))
		return authorizedUnsafeHostExec("config-ack")
	}

	if !unsafeHostExecInteractive() {
		fmt.Fprintf(os.Stderr, "[makewand] %s\n", msg.UnsafeHostExecAckNonInteractive)
		return engine.UnsafeHostExecAuthorization{}
	}

	fmt.Fprintf(os.Stderr, msg.UnsafeHostExecAckPrompt, config.UnsafeHostExecAckCurrentVersion)
	line, readErr := unsafeHostExecReadLine()
	if readErr != nil || !strings.EqualFold(strings.TrimSpace(line), "yes") {
		fmt.Fprintf(os.Stderr, "[makewand] %s\n", msg.UnsafeHostExecAckDeclined)
		return engine.UnsafeHostExecAuthorization{}
	}

	// Fail closed on ANY recording failure: an acknowledgment that cannot be
	// recorded (hostname unavailable, unreadable/corrupt config, save failure)
	// must not authorize host execution, and the user must not be told it was
	// recorded.
	if err := persistUnsafeHostExecAckFn(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "[makewand] %s\n", fmt.Sprintf(msg.UnsafeHostExecAckSaveFailed, err))
		return engine.UnsafeHostExecAuthorization{}
	}
	fmt.Fprintf(os.Stderr, "[makewand] %s\n", msg.UnsafeHostExecAckConfirmed)
	return authorizedUnsafeHostExec("interactive-ack")
}

// persistUnsafeHostExecAck durably records the acknowledgment on a FRESHLY
// loaded config (so runtime-only mutations of cfg — e.g. a --approval
// override — are not accidentally written to disk alongside it), and only
// AFTER that succeeds mirrors the recorded fields onto the caller's in-memory
// cfg. Ordering matters: a failed persist must leave cfg without any ack
// residue, because setup ignores the resolver's result, displays cfg's ack
// state, and re-saves cfg at the end — residue would resurrect an
// acknowledgment the user was just told failed (fail closed). A nil return
// means the acknowledgment is durably recorded; any error means neither the
// disk nor cfg carries it.
func persistUnsafeHostExecAck(cfg *config.Config) error {
	stored, loadErr := config.LoadWithOptions(config.LoadOptions{SkipCLIDetection: true})
	if loadErr != nil {
		return loadErr
	}
	if err := stored.RecordUnsafeHostExecAck(time.Now()); err != nil {
		return err
	}
	if err := config.Save(stored); err != nil {
		return err
	}
	cfg.UnsafeHostExecAckVersion = stored.UnsafeHostExecAckVersion
	cfg.UnsafeHostExecAckAt = stored.UnsafeHostExecAckAt
	cfg.UnsafeHostExecAckHost = stored.UnsafeHostExecAckHost
	return nil
}

func authorizedUnsafeHostExec(source string) engine.UnsafeHostExecAuthorization {
	return engine.UnsafeHostExecAuthorization{
		Acknowledged: true,
		Source:       source,
		Audit:        auditUnsafeHostExec,
	}
}

// auditUnsafeHostExec appends one host-execution record to the audit log.
// Audit failures are surfaced on stderr but do not block execution: the user
// already completed the explicit acknowledgment, and failing the build over a
// full disk would punish the acknowledged path harder than the sandboxed one.
func auditUnsafeHostExec(ev engine.UnsafeHostExecEvent) {
	dir, err := config.ConfigDir()
	if err != nil {
		diag.Stderr().WarnErr("unsafe host exec audit log unavailable", err)
		return
	}
	entry := struct {
		Time    string   `json:"time"`
		Context string   `json:"context"`
		Command string   `json:"command"`
		Args    []string `json:"args,omitempty"`
		Dir     string   `json:"dir"`
		Source  string   `json:"source"`
	}{
		Time:    time.Now().UTC().Format(time.RFC3339),
		Context: ev.Context,
		Command: ev.Command,
		Args:    ev.Args,
		Dir:     ev.Dir,
		Source:  ev.Source,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		diag.Stderr().WarnErr("unsafe host exec audit encode failed", err)
		return
	}
	path := filepath.Join(dir, unsafeHostExecAuditFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		diag.Stderr().WarnErr("unsafe host exec audit write failed", err)
		return
	}
	// The 0600 in OpenFile only applies on creation; tighten a pre-existing
	// wider-permission file so the audit log stays private as documented.
	if info, statErr := f.Stat(); statErr != nil {
		diag.Stderr().WarnErr("unsafe host exec audit stat failed", statErr)
	} else if info.Mode().Perm() != 0o600 {
		if chErr := f.Chmod(0o600); chErr != nil {
			diag.Stderr().WarnErr("unsafe host exec audit chmod failed", chErr)
		}
	}
	if _, writeErr := f.Write(append(data, '\n')); writeErr != nil {
		diag.Stderr().WarnErr("unsafe host exec audit write failed", writeErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		diag.Stderr().WarnErr("unsafe host exec audit close failed", closeErr)
	}
}

func unsafeHostExecAuditPathDisplay() string {
	if dir, err := config.ConfigDir(); err == nil {
		return filepath.Join(dir, unsafeHostExecAuditFile)
	}
	return unsafeHostExecAuditFile
}
