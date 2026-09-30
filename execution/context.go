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
	LedgerPath     string
	MaxCalls       int
	TaskID         string
	EventsPath     string
	LeaseID        string
	AccountRef     *string
	APIPolicy      string
	EventError     func(error)
	enforcedLedger string
	enforcedMax    int
}

type configKey struct{}
type stageKey struct{}

func ContextWithConfig(ctx context.Context, config Config) context.Context {
	if previous, ok := ctx.Value(configKey{}).(Config); ok {
		if previous.enforcedLedger != "" {
			config.enforcedLedger = previous.enforcedLedger
		} else if previous.LedgerPath != "" {
			config.enforcedLedger = previous.LedgerPath
		}
		if previous.enforcedMax > 0 {
			config.enforcedMax = previous.enforcedMax
		} else if previous.MaxCalls > 0 {
			config.enforcedMax = previous.MaxCalls
		}
		if config.LedgerPath == "" {
			config.LedgerPath = previous.LedgerPath
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
		resolved, err := resolvePath(*path)
		if err != nil {
			if path == &cfg.EventsPath {
				continue
			}
			return ctx, cfg, err
		}
		*path = resolved
	}
	for _, inherited := range []string{inheritedLedger, cfg.enforcedLedger} {
		if inherited == "" {
			continue
		}
		resolved, err := resolvePath(inherited)
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
	}
	if cfg.MaxCalls > 0 {
		cfg.enforcedMax = cfg.MaxCalls
	}
	return ContextWithConfig(ctx, cfg), cfg, nil
}

func resolvePath(path string) (string, error) {
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path[2:])
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return "", err
	}
	directory, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, filepath.Base(absolute)), nil
}
