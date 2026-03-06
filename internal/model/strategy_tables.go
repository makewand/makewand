package model

// modelTable maps (provider, tier) → model ID.
// For CLI providers, model ID is ignored (CLI uses subscription default),
// but we still need entries for the routing logic.
var modelTable = map[string]map[ModelTier]string{
	"claude": {
		TierCheap:   "claude-haiku-4-5-20251001",
		TierMid:     "claude-sonnet-4-20250514",
		TierPremium: "claude-opus-4-20250514",
	},
	"codex": {
		TierCheap:   "codex-cli", // subscription CLI — cost is zero
		TierMid:     "codex-cli",
		TierPremium: "codex-cli",
	},
	"openai": {
		TierCheap:   "gpt-4o-mini",
		TierMid:     "gpt-4o",
		TierPremium: "gpt-4o",
	},
	"gemini": {
		TierCheap:   "gemini-2.5-flash",
		TierMid:     "gemini-2.5-flash",
		TierPremium: "gemini-2.5-pro",
	},
	"ollama": {
		TierCheap:   "gemma3:4b",
		TierMid:     "gemma3:27b",
		TierPremium: "llama4:16x17b",
	},
}

// strategyEntry defines the tier and provider preference order for a task.
type strategyEntry struct {
	Tier      ModelTier
	Providers []string
}

// strategyTable maps (UsageMode, TaskType) → strategyEntry.
var strategyTable = map[UsageMode]map[TaskType]strategyEntry{
	ModeFree: {
		TaskAnalyze: {TierCheap, []string{"gemini", "ollama"}},
		TaskExplain: {TierCheap, []string{"gemini", "ollama"}},
		TaskCode:    {TierCheap, []string{"gemini", "ollama"}},
		TaskFix:     {TierCheap, []string{"gemini", "ollama"}},
		TaskReview:  {TierCheap, []string{"gemini", "ollama"}},
	},
	ModeEconomy: {
		TaskAnalyze: {TierCheap, []string{"gemini", "ollama", "claude", "codex", "openai"}},
		TaskExplain: {TierCheap, []string{"gemini", "ollama", "claude", "codex", "openai"}},
		TaskCode:    {TierCheap, []string{"gemini", "claude", "codex", "openai", "ollama"}},
		TaskFix:     {TierCheap, []string{"codex", "claude", "gemini", "openai", "ollama"}},
		TaskReview:  {TierCheap, []string{"gemini", "ollama", "claude", "codex", "openai"}},
	},
	ModeBalanced: {
		TaskAnalyze: {TierMid, []string{"gemini", "claude", "codex", "openai", "ollama"}},
		TaskExplain: {TierMid, []string{"gemini", "claude", "codex", "openai", "ollama"}},
		TaskCode:    {TierMid, []string{"claude", "codex", "openai", "gemini", "ollama"}},
		TaskFix:     {TierMid, []string{"codex", "claude", "openai", "gemini", "ollama"}},
		TaskReview:  {TierMid, []string{"gemini", "claude", "codex", "openai", "ollama"}},
	},
	ModePower: {
		TaskAnalyze: {TierPremium, []string{"claude", "codex", "openai", "gemini", "ollama"}},
		TaskExplain: {TierPremium, []string{"claude", "codex", "openai", "gemini", "ollama"}},
		TaskCode:    {TierPremium, []string{"claude", "codex", "openai", "gemini", "ollama"}},
		TaskFix:     {TierPremium, []string{"claude", "codex", "openai", "gemini", "ollama"}},
		TaskReview:  {TierPremium, []string{"claude", "codex", "openai", "gemini", "ollama"}},
	},
}

// costEntry holds per-million-token pricing.
type costEntry struct {
	Input  float64 // $/MTok input
	Output float64 // $/MTok output
}

// costTable maps model ID → pricing.
var costTable = map[string]costEntry{
	"claude-haiku-4-5-20251001": {0.25, 1.25},
	"claude-sonnet-4-20250514":  {3.0, 15.0},
	"claude-opus-4-20250514":    {15.0, 75.0},
	"gpt-4o-mini":               {0.15, 0.60},
	"gpt-4o":                    {2.50, 10.0},
	"gemini-2.5-flash":          {0, 0},
	"gemini-2.5-pro":            {1.25, 10.0},
	"codex-cli":                 {0, 0},
	"llama3.2":                  {0, 0},
	"gemma3:4b":                 {0, 0},
	"gemma3:27b":                {0, 0},
	"llama4:16x17b":             {0, 0},
}

// BuildPhase represents a phase in the multi-model build pipeline.
type BuildPhase int

const (
	PhasePlan   BuildPhase = iota // requirements analysis / planning
	PhaseCode                     // code generation
	PhaseReview                   // cross-model code review
	PhaseFix                      // error fixing
)

// BuildStrategy defines the provider preference for a build phase.
type BuildStrategy struct {
	Tier      ModelTier
	Primary   string   // preferred provider
	Fallbacks []string // fallback order
}

// buildStrategyTable maps (UsageMode, BuildPhase) → BuildStrategy.
// Review and Fix Primary must differ from Code — cross-model perspective is the core value.
var buildStrategyTable = map[UsageMode]map[BuildPhase]BuildStrategy{
	ModeFree: {
		PhasePlan:   {TierCheap, "gemini", []string{"ollama"}},
		PhaseCode:   {TierCheap, "gemini", []string{"ollama"}},
		PhaseReview: {TierCheap, "ollama", []string{"gemini"}},
		PhaseFix:    {TierCheap, "ollama", []string{"gemini"}},
	},
	ModeEconomy: {
		PhasePlan:   {TierCheap, "gemini", []string{"ollama", "claude", "codex"}},
		PhaseCode:   {TierCheap, "gemini", []string{"claude", "codex", "ollama"}},
		PhaseReview: {TierCheap, "claude", []string{"ollama", "gemini", "codex"}},
		PhaseFix:    {TierCheap, "codex", []string{"claude", "gemini", "ollama"}},
	},
	ModeBalanced: {
		PhasePlan:   {TierMid, "gemini", []string{"claude", "codex", "ollama"}},
		PhaseCode:   {TierMid, "claude", []string{"codex", "gemini", "ollama"}},
		PhaseReview: {TierMid, "gemini", []string{"codex", "ollama", "claude"}},
		PhaseFix:    {TierMid, "codex", []string{"claude", "gemini", "ollama"}},
	},
	ModePower: {
		PhasePlan:   {TierPremium, "gemini", []string{"claude", "codex", "ollama"}},
		PhaseCode:   {TierPremium, "claude", []string{"codex", "gemini", "ollama"}},
		PhaseReview: {TierPremium, "gemini", []string{"codex", "ollama", "claude"}},
		PhaseFix:    {TierPremium, "codex", []string{"claude", "gemini", "ollama"}},
	},
}
