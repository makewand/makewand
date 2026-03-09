package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/makewand/makewand/internal/config"
)

func customProviderPromptLabel(cp config.CustomProvider) string {
	switch config.EffectiveCustomProviderPromptMode(cp) {
	case config.CustomPromptModeStdin:
		return "stdin"
	case config.CustomPromptModeArg:
		return "arg"
	default:
		return "legacy"
	}
}

func customProviderSafetyWarning(cp config.CustomProvider) string {
	name := strings.TrimSpace(cp.Name)
	if name == "" {
		name = strings.TrimSpace(cp.Command)
	}

	mode := config.EffectiveCustomProviderPromptMode(cp)
	if config.CustomProviderUsesShellAdapter(cp) && mode != config.CustomPromptModeStdin {
		return fmt.Sprintf("%s uses shell adapter %q with %s prompt delivery; prefer prompt_mode=\"stdin\" or remove the shell wrapper", name, filepath.Base(cp.Command), mode)
	}
	if mode == config.CustomPromptModeLegacy {
		return fmt.Sprintf("%s uses legacy argv/{{prompt}} prompt delivery; set prompt_mode=\"stdin\" to avoid shell-escaping footguns", name)
	}
	if mode == config.CustomPromptModeArg {
		return fmt.Sprintf("%s uses argv prompt delivery; prompt_mode=\"stdin\" is safer for untrusted text", name)
	}
	return ""
}

func customProviderDoctorCheck(cfg *config.Config) (doctorCheck, bool) {
	if cfg == nil {
		return doctorCheck{}, false
	}

	warnings := make([]string, 0, len(cfg.CustomProviders))
	safeCount := 0
	for _, cp := range cfg.CustomProviders {
		if !config.IsCustomProviderUsable(cp) {
			continue
		}
		if warning := customProviderSafetyWarning(cp); warning != "" {
			warnings = append(warnings, warning)
			continue
		}
		safeCount++
	}

	if len(warnings) > 0 {
		sort.Strings(warnings)
		return doctorCheck{
			Name:    "custom provider prompt safety",
			Status:  doctorWarn,
			Details: strings.Join(warnings, " | "),
		}, true
	}
	if safeCount > 0 {
		return doctorCheck{
			Name:    "custom provider prompt safety",
			Status:  doctorPass,
			Details: "all usable custom providers use stdin prompt delivery",
		}, true
	}
	return doctorCheck{}, false
}
