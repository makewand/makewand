package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	LedgerPath        string
	MaxCalls          int
	TaskID            string
	EventsPath        string
	LeaseID           string
	AccountRef        *string
	APIPolicy         string
	EventError        func(error)
	enforcedLedger    string
	enforcedMax       int
	ledgerDirectory   os.FileInfo
	eventsDirectory   os.FileInfo
	enforcedDirectory os.FileInfo
}

type configKey struct{}
type stageKey struct{}

func ContextWithConfig(ctx context.Context, config Config) context.Context {
	if previous, ok := ctx.Value(configKey{}).(Config); ok {
		if previous.enforcedLedger != "" {
			config.enforcedLedger = previous.enforcedLedger
			config.enforcedDirectory = previous.enforcedDirectory
		} else if previous.LedgerPath != "" {
			config.enforcedLedger = previous.LedgerPath
			config.enforcedDirectory = previous.ledgerDirectory
		}
		if previous.enforcedMax > 0 {
			config.enforcedMax = previous.enforcedMax
		} else if previous.MaxCalls > 0 {
			config.enforcedMax = previous.MaxCalls
		}
		if config.LedgerPath == "" {
			config.LedgerPath = previous.LedgerPath
		}
		if config.LedgerPath == previous.LedgerPath {
			config.ledgerDirectory = previous.ledgerDirectory
		}
		if config.EventsPath == previous.EventsPath {
			config.eventsDirectory = previous.eventsDirectory
		}
		if config.MaxCalls == 0 || config.enforcedMax > 0 && config.MaxCalls > config.enforcedMax {
			config.MaxCalls = config.enforcedMax
		}
	}
	return context.WithValue(ctx, configKey{}, config)
}

func ContextWithStage(ctx context.Context, stage string) context.Context {
	return context.WithValue(ctx, stageKey{}, stage)
}

func StageFromContext(ctx context.Context) string {
	stage, _ := ctx.Value(stageKey{}).(string)
	if ValidIdentifier(stage) {
		return stage
	}
	return "provider"
}

func ValidIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:-", c) {
			continue
		}
		return false
	}
	return true
}

func NewID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic("cannot obtain execution identity randomness")
	}
	return hex.EncodeToString(bytes[:])
}

// EnsureContext resolves configuration once so fallback attempts and stages share identity.
func EnsureContext(ctx context.Context) (context.Context, Config, error) {
	cfg, _ := ctx.Value(configKey{}).(Config)
	inheritedLedger := os.Getenv("MAKEWAND_CALL_BUDGET_FILE")
	if cfg.LedgerPath == "" {
		cfg.LedgerPath = inheritedLedger
	}
	if cfg.EventsPath == "" {
		cfg.EventsPath = os.Getenv("MAKEWAND_EXECUTION_EVENTS_FILE")
	}
	if cfg.TaskID == "" {
		cfg.TaskID = os.Getenv("MAKEWAND_TASK_ID")
	}
	if cfg.LeaseID == "" {
		cfg.LeaseID = os.Getenv("MAKEWAND_CALL_BUDGET_LEASE_ID")
	}
	if value := os.Getenv("MAKEWAND_MAX_MODEL_CALLS"); value != "" {
		maximum, err := strconv.Atoi(value)
		if err != nil || maximum <= 0 {
			return ctx, cfg, fmt.Errorf("MAKEWAND_MAX_MODEL_CALLS must be a positive integer")
		}
		if cfg.MaxCalls == 0 || cfg.MaxCalls > maximum {
			cfg.MaxCalls = maximum
		}
	}
	if cfg.MaxCalls < 0 {
		return ctx, cfg, fmt.Errorf("model call maximum must be positive")
	}
	if cfg.APIPolicy == "" {
		cfg.APIPolicy = os.Getenv("MAKEWAND_API_POLICY")
	}
	if cfg.MaxCalls > 0 && cfg.LedgerPath == "" {
		return ctx, cfg, fmt.Errorf("model call maximum requires a persistent budget file")
	}
	if !ValidIdentifier(cfg.TaskID) {
		cfg.TaskID = NewID()
	}
	for _, path := range []*string{&cfg.LedgerPath, &cfg.EventsPath} {
		if *path == "" {
			continue
		}
		expected := cfg.ledgerDirectory
		if path == &cfg.EventsPath {
			expected = cfg.eventsDirectory
		}
		resolved, directory, err := resolvePathIdentity(*path, expected)
		if err != nil {
			if path == &cfg.EventsPath {
				continue
			}
			return ctx, cfg, err
		}
		*path = resolved
		if path == &cfg.EventsPath {
			cfg.eventsDirectory = directory
		} else {
			cfg.ledgerDirectory = directory
		}
	}
	for index, inherited := range []string{inheritedLedger, cfg.enforcedLedger} {
		if inherited == "" {
			continue
		}
		var expected os.FileInfo
		if index == 1 {
			expected = cfg.enforcedDirectory
		}
		resolved, _, err := resolvePathIdentity(inherited, expected)
		if err != nil {
			return ctx, cfg, err
		}
		if resolved != cfg.LedgerPath {
			return ctx, cfg, fmt.Errorf("cannot replace inherited model call budget ledger")
		}
	}
	if cfg.enforcedMax > 0 && (cfg.MaxCalls == 0 || cfg.MaxCalls > cfg.enforcedMax) {
		cfg.MaxCalls = cfg.enforcedMax
	}
	if cfg.LedgerPath != "" {
		cfg.enforcedLedger = cfg.LedgerPath
		cfg.enforcedDirectory = cfg.ledgerDirectory
	}
	if cfg.MaxCalls > 0 {
		cfg.enforcedMax = cfg.MaxCalls
	}
	// cfg already contains the inherited limits and the canonical directory
	// identities. Reapplying ContextWithConfig here would overwrite those with
	// an unresolved alias from the original context.
	return context.WithValue(ctx, configKey{}, cfg), cfg, nil
}

func resolvePathIdentity(path string, expected os.FileInfo) (string, os.FileInfo, error) {
	p, directory, err := accountingPathForIdentity(path, expected)
	if err != nil {
		return "", nil, err
	}
	defer p.close()
	return filepath.Join(p.root.Name(), p.name), directory, nil
}
