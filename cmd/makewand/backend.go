package main

import (
	"errors"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/config"
)

func hasUsableBackend(cfg *config.Config) bool {
	if cfg == nil {
		return config.HasRemoteBackend()
	}
	return cfg.HasAnyModel() || (cfg.IsProviderEnabled("remote") && config.HasRemoteBackend())
}

func noUsableBackendError() error {
	return &execution.StatusError{Status: execution.Unverified, OutcomeKnown: true,
		Err: errors.New("no AI models or remote backend configured; run 'makewand setup' or set MAKEWAND_REMOTE_URL/MAKEWAND_REMOTE_TOKEN")}
}
