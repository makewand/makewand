package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
	"github.com/spf13/cobra"
)

type doctorStatus string

const (
	doctorPass doctorStatus = "pass"
	doctorWarn doctorStatus = "warn"
	doctorFail doctorStatus = "fail"
)

const (
	defaultDoctorProbeRetries = 1
	maxDoctorProbeErrorLen    = 220
)

type doctorProbeClassification string

const (
	probeClassHealthy       doctorProbeClassification = "healthy"
	probeClassEnvironment   doctorProbeClassification = "environment"
	probeClassConfiguration doctorProbeClassification = "configuration"
	probeClassProvider      doctorProbeClassification = "provider"
)

type doctorCheck struct {
	Name    string       `json:"name"`
	Status  doctorStatus `json:"status"`
	Details string       `json:"details,omitempty"`
}

type doctorTaskRoute struct {
	Task     string `json:"task"`
	Provider string `json:"provider,omitempty"`
	ModelID  string `json:"model_id,omitempty"`
	Error    string `json:"error,omitempty"`
}

type doctorModeReport struct {
	Mode   string            `json:"mode"`
	Status doctorStatus      `json:"status"`
	Routes []doctorTaskRoute `json:"routes"`
}

type doctorProbeReport struct {
	Mode           string                    `json:"mode"`
	Status         doctorStatus              `json:"status"`
	Classification doctorProbeClassification `json:"classification,omitempty"`
	Provider       string                    `json:"provider,omitempty"`
	ModelID        string                    `json:"model_id,omitempty"`
	Attempts       int                       `json:"attempts,omitempty"`
	DurationMS     int64                     `json:"duration_ms"`
	Error          string                    `json:"error,omitempty"`
}

type doctorReport struct {
	GeneratedAt         time.Time           `json:"generated_at"`
	ConfigPath          string              `json:"config_path,omitempty"`
	DetectedProviders   []string            `json:"detected_providers,omitempty"`
	Checks              []doctorCheck       `json:"checks"`
	ModeCoverage        []doctorModeReport  `json:"mode_coverage"`
	LiveProbe           []doctorProbeReport `json:"live_probe,omitempty"`
	Strict              bool                `json:"strict"`
	ProbeEnabled        bool                `json:"probe_enabled"`
	ProbeTimeoutSeconds int                 `json:"probe_timeout_seconds"`
	ProbeRetries        int                 `json:"probe_retries"`
}

type doctorOptions struct {
	modes        []model.UsageMode
	probe        bool
	probeTimeout time.Duration
	probeRetries int
	strict       bool
	jsonOutput   bool
}

func doctorCmd() *cobra.Command {
	var (
		modesFlag        string
		probeFlag        bool
		probeTimeoutFlag time.Duration
		probeRetriesFlag int
		strictFlag       bool
		jsonFlag         bool
	)

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run pre-launch health checks",
		Long: `Run configuration, routing coverage, and optional live provider probe checks.

Examples:
  makewand doctor
  makewand doctor --modes balanced,power
  makewand doctor --probe --strict`,
		RunE: func(cmd *cobra.Command, args []string) error {
			modes, err := parseDoctorModes(modesFlag)
			if err != nil {
				return err
			}
			if probeTimeoutFlag <= 0 {
				return fmt.Errorf("probe timeout must be positive")
			}
			if probeRetriesFlag < 0 {
				return fmt.Errorf("probe retries must be >= 0")
			}

			cfg, loadErr := config.Load()
			report, failCount, warnCount := runDoctor(cfg, loadErr, doctorOptions{
				modes:        modes,
				probe:        probeFlag,
				probeTimeout: probeTimeoutFlag,
				probeRetries: probeRetriesFlag,
				strict:       strictFlag,
				jsonOutput:   jsonFlag,
			})

			if jsonFlag {
				data, err := json.MarshalIndent(report, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
			} else {
				printDoctorReport(report)
			}

			if failCount > 0 {
				return fmt.Errorf("doctor found %d failing checks", failCount)
			}
			if strictFlag && warnCount > 0 {
				return fmt.Errorf("doctor found %d warnings in strict mode", warnCount)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&modesFlag, "modes", "all", "modes to verify: all or comma list (free,economy,balanced,power)")
	cmd.Flags().BoolVar(&probeFlag, "probe", false, "run live provider probe requests (network/API/CLI)")
	cmd.Flags().DurationVar(&probeTimeoutFlag, "probe-timeout", 45*time.Second, "timeout per live probe request")
	cmd.Flags().IntVar(&probeRetriesFlag, "probe-retries", defaultDoctorProbeRetries, "retry count per provider during live probe")
	cmd.Flags().BoolVar(&strictFlag, "strict", false, "treat warnings as failures")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "output report as JSON")
	return cmd
}

func parseDoctorModes(raw string) ([]model.UsageMode, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" || raw == "all" {
		return []model.UsageMode{
			model.ModeFree,
			model.ModeEconomy,
			model.ModeBalanced,
			model.ModePower,
		}, nil
	}

	seen := make(map[model.UsageMode]bool)
	modes := make([]model.UsageMode, 0, 4)
	for _, token := range strings.Split(raw, ",") {
		token = strings.TrimSpace(strings.ToLower(token))
		if token == "" {
			continue
		}
		m, ok := model.ParseUsageMode(token)
		if !ok {
			return nil, fmt.Errorf("invalid mode %q (expected free,economy,balanced,power)", token)
		}
		if seen[m] {
			continue
		}
		seen[m] = true
		modes = append(modes, m)
	}
	if len(modes) == 0 {
		return nil, fmt.Errorf("no valid modes provided")
	}
	return modes, nil
}

func runDoctor(cfg *config.Config, loadErr error, opts doctorOptions) (doctorReport, int, int) {
	report := doctorReport{
		GeneratedAt:         time.Now().UTC(),
		Checks:              make([]doctorCheck, 0, 8),
		ModeCoverage:        make([]doctorModeReport, 0, len(opts.modes)),
		LiveProbe:           make([]doctorProbeReport, 0, len(opts.modes)),
		Strict:              opts.strict,
		ProbeEnabled:        opts.probe,
		ProbeTimeoutSeconds: int(opts.probeTimeout.Seconds()),
		ProbeRetries:        opts.probeRetries,
	}

	if p, err := config.ConfigPath(); err == nil {
		report.ConfigPath = p
	} else {
		report.Checks = append(report.Checks, doctorCheck{
			Name:    "config path",
			Status:  doctorWarn,
			Details: err.Error(),
		})
	}

	if loadErr != nil {
		report.Checks = append(report.Checks, doctorCheck{
			Name:    "config load",
			Status:  doctorWarn,
			Details: loadErr.Error(),
		})
	} else {
		report.Checks = append(report.Checks, doctorCheck{
			Name:   "config load",
			Status: doctorPass,
		})
	}

	if cfg == nil {
		cfg = config.DefaultConfig()
	}

	report.DetectedProviders = detectConfiguredProviders(cfg)
	if check, ok := ollamaDoctorCheck(cfg.OllamaURL); ok {
		report.Checks = append(report.Checks, check)
	}
	if check, ok := customProviderDoctorCheck(cfg); ok {
		report.Checks = append(report.Checks, check)
	}
	if !cfg.HasAnyModel() {
		report.Checks = append(report.Checks, doctorCheck{
			Name:    "model configuration",
			Status:  doctorFail,
			Details: "no model configured; run 'makewand setup' first",
		})
	} else {
		report.Checks = append(report.Checks, doctorCheck{
			Name:    "model configuration",
			Status:  doctorPass,
			Details: strings.Join(report.DetectedProviders, ", "),
		})
	}

	configDir := ""
	if dir, err := config.ConfigDir(); err == nil {
		configDir = dir
	}

	for _, modeValue := range opts.modes {
		modeName := modeValue.String()
		cfgCopy := *cfg
		cfgCopy.UsageMode = modeName
		router := model.NewRouter(&cfgCopy)
		if configDir != "" {
			_ = router.LoadStats(configDir)
		}

		modeResult := doctorModeReport{
			Mode:   modeName,
			Status: doctorPass,
			Routes: make([]doctorTaskRoute, 0, len(doctorTasks())),
		}
		for _, tt := range doctorTasks() {
			route, err := router.Route(tt.task)
			if err != nil {
				modeResult.Status = doctorWarn
				modeResult.Routes = append(modeResult.Routes, doctorTaskRoute{
					Task:  tt.name,
					Error: err.Error(),
				})
				continue
			}
			modeResult.Routes = append(modeResult.Routes, doctorTaskRoute{
				Task:     tt.name,
				Provider: route.Actual,
				ModelID:  route.ModelID,
			})
		}
		report.ModeCoverage = append(report.ModeCoverage, modeResult)

		if !opts.probe {
			continue
		}

		probeResult := doctorProbeReport{
			Mode:           modeName,
			Status:         doctorPass,
			Classification: probeClassHealthy,
		}
		start := time.Now()
		prompt := []model.Message{{Role: "user", Content: "Reply with one short sentence: service healthy."}}
		system := "You are a health probe. Reply with one short sentence only."

		candidates := uniqueProbeProviders(modeResult.Routes)
		if len(candidates) == 0 {
			probeResult.Status = doctorFail
			probeResult.Classification = probeClassConfiguration
			probeResult.Error = "no probe candidate provider found"
			probeResult.DurationMS = time.Since(start).Milliseconds()
			report.LiveProbe = append(report.LiveProbe, probeResult)
			continue
		}

		attemptCount := 0
		failures := make([]doctorProbeFailure, 0, len(candidates)*(opts.probeRetries+1))
		success := false

	probeProvidersLoop:
		for _, providerName := range candidates {
			prov, err := router.Get(providerName)
			if err != nil {
				attemptCount++
				failures = append(failures, doctorProbeFailure{
					provider: providerName,
					attempt:  attemptCount,
					class:    classifyProbeError(err),
					err:      err,
				})
				continue
			}

			for try := 0; try <= opts.probeRetries; try++ {
				attemptCount++
				ctx, cancel := context.WithTimeout(context.Background(), opts.probeTimeout)
				content, usage, err := prov.Chat(ctx, prompt, system, model.MaxTokensForTask(model.TaskExplain))
				cancel()
				if err == nil && strings.TrimSpace(content) != "" {
					probeResult.Provider = providerName
					probeResult.ModelID = strings.TrimSpace(usage.Model)
					probeResult.Attempts = attemptCount
					success = true
					break probeProvidersLoop
				}
				if err == nil {
					err = fmt.Errorf("%s returned empty probe response", providerName)
				}
				class := classifyProbeError(err)
				failures = append(failures, doctorProbeFailure{
					provider: providerName,
					attempt:  attemptCount,
					class:    class,
					err:      err,
				})
				// Invalid config/auth/binary errors are non-retryable for this provider.
				if class == probeClassConfiguration {
					break
				}
			}
		}

		if !success {
			status, class, summary := evaluateProbeFailures(failures)
			probeResult.Status = status
			probeResult.Classification = class
			probeResult.Attempts = attemptCount
			probeResult.Error = summary
		}
		probeResult.DurationMS = time.Since(start).Milliseconds()
		report.LiveProbe = append(report.LiveProbe, probeResult)
	}

	failCount := 0
	warnCount := 0
	for _, c := range report.Checks {
		switch c.Status {
		case doctorFail:
			failCount++
		case doctorWarn:
			warnCount++
		}
	}
	for _, m := range report.ModeCoverage {
		switch m.Status {
		case doctorFail:
			failCount++
		case doctorWarn:
			warnCount++
		}
	}
	for _, p := range report.LiveProbe {
		switch p.Status {
		case doctorFail:
			failCount++
		case doctorWarn:
			warnCount++
		}
	}

	return report, failCount, warnCount
}

func detectConfiguredProviders(cfg *config.Config) []string {
	set := make(map[string]bool)

	for _, cli := range cfg.CLIs {
		name := strings.TrimSpace(cli.Name)
		if name != "" {
			set[name+" (cli)"] = true
		}
	}
	if cfg.ClaudeAPIKey != "" {
		set["claude (api)"] = true
	}
	if cfg.GeminiAPIKey != "" {
		set["gemini (api)"] = true
	}
	if cfg.OpenAIAPIKey != "" {
		set["openai (api)"] = true
	}
	if cfg.OllamaURL != "" {
		if check, ok := ollamaDoctorCheck(cfg.OllamaURL); ok && check.Status == doctorPass {
			set["ollama"] = true
		} else {
			set["ollama (blocked)"] = true
		}
	}
	for _, cp := range cfg.CustomProviders {
		if config.IsCustomProviderUsable(cp) {
			set[strings.TrimSpace(cp.Name)+" (custom)"] = true
		}
	}

	out := make([]string, 0, len(set))
	for name := range set {
		if strings.TrimSpace(name) != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func printDoctorReport(report doctorReport) {
	fmt.Println("makewand doctor")
	fmt.Printf("Generated at: %s\n", report.GeneratedAt.Format(time.RFC3339))
	if report.ConfigPath != "" {
		fmt.Printf("Config path: %s\n", report.ConfigPath)
	}
	if len(report.DetectedProviders) > 0 {
		fmt.Printf("Detected providers: %s\n", strings.Join(report.DetectedProviders, ", "))
	}
	fmt.Println()

	fmt.Println("Checks:")
	for _, c := range report.Checks {
		fmt.Printf("  [%s] %s", strings.ToUpper(string(c.Status)), c.Name)
		if c.Details != "" {
			fmt.Printf(" - %s", c.Details)
		}
		fmt.Println()
	}
	fmt.Println()

	fmt.Println("Mode coverage:")
	for _, m := range report.ModeCoverage {
		fmt.Printf("  [%s] %s\n", strings.ToUpper(string(m.Status)), m.Mode)
		for _, r := range m.Routes {
			if r.Error != "" {
				fmt.Printf("    - %s: %s\n", r.Task, r.Error)
				continue
			}
			if r.ModelID != "" {
				fmt.Printf("    - %s: %s (%s)\n", r.Task, r.Provider, r.ModelID)
			} else {
				fmt.Printf("    - %s: %s\n", r.Task, r.Provider)
			}
		}
	}
	if report.ProbeEnabled {
		fmt.Println()
		fmt.Println("Live probe:")
		for _, p := range report.LiveProbe {
			fmt.Printf("  [%s] %s (%dms)", strings.ToUpper(string(p.Status)), p.Mode, p.DurationMS)
			if p.Classification != "" && p.Classification != probeClassHealthy {
				fmt.Printf(" [%s]", p.Classification)
			}
			if p.Attempts > 0 {
				fmt.Printf(" attempts=%d", p.Attempts)
			}
			if p.Provider != "" {
				if p.ModelID != "" {
					fmt.Printf(" - %s (%s)", p.Provider, p.ModelID)
				} else {
					fmt.Printf(" - %s", p.Provider)
				}
			}
			if p.Error != "" {
				fmt.Printf(" - %s", p.Error)
			}
			fmt.Println()
		}
	}

	failCount := 0
	warnCount := 0
	for _, c := range report.Checks {
		if c.Status == doctorFail {
			failCount++
		}
		if c.Status == doctorWarn {
			warnCount++
		}
	}
	for _, m := range report.ModeCoverage {
		if m.Status == doctorFail {
			failCount++
		}
		if m.Status == doctorWarn {
			warnCount++
		}
	}
	for _, p := range report.LiveProbe {
		if p.Status == doctorFail {
			failCount++
		}
		if p.Status == doctorWarn {
			warnCount++
		}
	}

	fmt.Println()
	fmt.Printf("Summary: fail=%d warn=%d strict=%t probe=%t\n", failCount, warnCount, report.Strict, report.ProbeEnabled)
}

type doctorTask struct {
	name string
	task model.TaskType
}

func doctorTasks() []doctorTask {
	return []doctorTask{
		{name: "analyze", task: model.TaskAnalyze},
		{name: "explain", task: model.TaskExplain},
		{name: "code", task: model.TaskCode},
		{name: "review", task: model.TaskReview},
		{name: "fix", task: model.TaskFix},
	}
}

func uniqueProbeProviders(routes []doctorTaskRoute) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		name := strings.TrimSpace(r.Provider)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

type doctorProbeFailure struct {
	provider string
	attempt  int
	class    doctorProbeClassification
	err      error
}

func classifyProbeError(err error) doctorProbeClassification {
	if err == nil {
		return probeClassHealthy
	}

	switch model.ErrorKindOf(err) {
	case model.ErrorKindTimeout, model.ErrorKindNetwork, model.ErrorKindRateLimit:
		return probeClassEnvironment
	case model.ErrorKindAuth, model.ErrorKindConfig:
		return probeClassConfiguration
	case model.ErrorKindUnavailable:
		return probeClassProvider
	case model.ErrorKindProvider:
		return probeClassProvider
	}

	msg := strings.ToLower(strings.TrimSpace(err.Error()))

	switch {
	case strings.Contains(msg, "deadline exceeded"),
		strings.Contains(msg, "timed out"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "proxyconnect"),
		strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "network is unreachable"),
		strings.Contains(msg, "temporary failure in name resolution"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "tls handshake timeout"),
		strings.Contains(msg, "stream disconnected"),
		strings.Contains(msg, "transport channel closed"),
		strings.Contains(msg, "operation not permitted"),
		strings.Contains(msg, "permission denied"),
		strings.Contains(msg, "rate limit"),
		strings.Contains(msg, "quota"):
		return probeClassEnvironment
	case strings.Contains(msg, "not configured"),
		strings.Contains(msg, "is not available"),
		strings.Contains(msg, "api key"),
		strings.Contains(msg, "unauthorized"),
		strings.Contains(msg, "forbidden"),
		strings.Contains(msg, "authentication"),
		strings.Contains(msg, "command not found"),
		strings.Contains(msg, "no such file or directory"):
		return probeClassConfiguration
	default:
		return probeClassProvider
	}
}

func evaluateProbeFailures(failures []doctorProbeFailure) (doctorStatus, doctorProbeClassification, string) {
	if len(failures) == 0 {
		return doctorFail, probeClassProvider, "probe failed with unknown error"
	}

	hasProvider := false
	hasConfig := false
	for _, f := range failures {
		if f.class == probeClassProvider {
			hasProvider = true
		}
		if f.class == probeClassConfiguration {
			hasConfig = true
		}
	}

	if hasProvider {
		return doctorFail, probeClassProvider, summarizeProbeFailures(failures, probeClassProvider)
	}
	if hasConfig {
		return doctorWarn, probeClassConfiguration, summarizeProbeFailures(failures, probeClassConfiguration)
	}
	return doctorWarn, probeClassEnvironment, summarizeProbeFailures(failures, probeClassEnvironment)
}

func summarizeProbeFailures(failures []doctorProbeFailure, class doctorProbeClassification) string {
	providerSet := make(map[string]bool)
	providers := make([]string, 0, len(failures))
	causeSet := make(map[string]bool)
	causes := make([]string, 0, 3)
	for _, f := range failures {
		name := strings.TrimSpace(f.provider)
		if name != "" && !providerSet[name] {
			providerSet[name] = true
			providers = append(providers, name)
		}

		cause := compactProbeError(f.err)
		if cause == "" || causeSet[cause] {
			continue
		}
		causeSet[cause] = true
		causes = append(causes, cause)
		if len(causes) >= 3 {
			break
		}
	}
	sort.Strings(providers)

	classText := "provider"
	if class == probeClassEnvironment {
		classText = "environment"
	} else if class == probeClassConfiguration {
		classText = "configuration"
	}

	summary := fmt.Sprintf("%s issue across %d attempt(s)", classText, len(failures))
	if len(providers) > 0 {
		summary += fmt.Sprintf(" on %s", strings.Join(providers, ","))
	}
	if len(causes) > 0 {
		summary += fmt.Sprintf(": %s", strings.Join(causes, "; "))
	}
	return summary
}

func compactProbeError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return ""
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "operation not permitted"):
		return "operation not permitted"
	case strings.Contains(lower, "permission denied"):
		return "permission denied"
	case strings.Contains(lower, "proxyconnect"):
		return "proxy connect failure"
	case strings.Contains(lower, "deadline exceeded"):
		return "deadline exceeded"
	case strings.Contains(lower, "timed out"):
		return "timed out"
	case strings.Contains(lower, "timeout"):
		return "timeout"
	case strings.Contains(lower, "no such host"):
		return "no such host"
	case strings.Contains(lower, "network is unreachable"):
		return "network is unreachable"
	case strings.Contains(lower, "connection refused"):
		return "connection refused"
	case strings.Contains(lower, "stream disconnected"):
		return "stream disconnected"
	}

	lines := strings.Split(strings.ReplaceAll(msg, "\r\n", "\n"), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > maxDoctorProbeErrorLen {
			return line[:maxDoctorProbeErrorLen-3] + "..."
		}
		return line
	}
	if len(msg) > maxDoctorProbeErrorLen {
		return msg[:maxDoctorProbeErrorLen-3] + "..."
	}
	return msg
}
