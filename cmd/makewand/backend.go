package main

import (
	"github.com/makewand/makewand/internal/config"
)

func hasUsableBackend(cfg *config.Config) bool {
	if cfg == nil {
		return config.HasRemoteBackend()
	}
	return cfg.HasAnyModel() || config.HasRemoteBackend()
}
