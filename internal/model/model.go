// Package model is a thin adapter that re-exports the router package types
// and provides NewRouter(cfg) for config-based construction.
package model

import "github.com/makewand/makewand/router"

// Type aliases — these allow existing internal code to use model.X
// interchangeably with router.X without any changes.
type (
	Provider       = router.Provider
	Message        = router.Message
	StreamChunk    = router.StreamChunk
	Usage          = router.Usage
	RouteResult    = router.RouteResult
	EnsembleResult = router.EnsembleResult
	Router         = router.Router
	RouterConfig   = router.RouterConfig
	ProviderEntry  = router.ProviderEntry
	TaskType       = router.TaskType
	UsageMode      = router.UsageMode
	AccessType     = router.AccessType
	ModelTier      = router.ModelTier
	BuildPhase     = router.BuildPhase
	BuildStrategy  = router.BuildStrategy
	ErrorKind      = router.ErrorKind
	ProviderError  = router.ProviderError
	TraceSink      = router.TraceSink
	TraceEvent     = router.TraceEvent
	TraceCandidate = router.TraceCandidate
	CLIProvider    = router.CLIProvider
	CLIInfo        = router.CLIInfo
	Claude         = router.Claude
	Gemini         = router.Gemini
	OpenAI         = router.OpenAI

	TransientCLIError = router.TransientCLIError
	ProviderFactory   = router.ProviderFactory
	SSEEventHandler   = router.SSEEventHandler
)

// Re-export constants.
const (
	TaskAnalyze = router.TaskAnalyze
	TaskCode    = router.TaskCode
	TaskReview  = router.TaskReview
	TaskExplain = router.TaskExplain
	TaskFix     = router.TaskFix

	ModeFast     = router.ModeFast
	ModeBalanced = router.ModeBalanced
	ModePower    = router.ModePower

	AccessFree         = router.AccessFree
	AccessLocal        = router.AccessLocal
	AccessSubscription = router.AccessSubscription
	AccessAPI          = router.AccessAPI

	TierCheap   = router.TierCheap
	TierMid     = router.TierMid
	TierPremium = router.TierPremium

	PhasePlan   = router.PhasePlan
	PhaseCode   = router.PhaseCode
	PhaseReview = router.PhaseReview
	PhaseFix    = router.PhaseFix

	ErrorKindUnknown     = router.ErrorKindUnknown
	ErrorKindTimeout     = router.ErrorKindTimeout
	ErrorKindCanceled    = router.ErrorKindCanceled
	ErrorKindRateLimit   = router.ErrorKindRateLimit
	ErrorKindAuth        = router.ErrorKindAuth
	ErrorKindConfig      = router.ErrorKindConfig
	ErrorKindNetwork     = router.ErrorKindNetwork
	ErrorKindProvider    = router.ErrorKindProvider
	ErrorKindUnavailable = router.ErrorKindUnavailable
	ErrorKindTransport   = router.ErrorKindNetwork // alias for backward compat

	DefaultContextBudget = router.DefaultContextBudget
)

// Re-export functions.
var (
	NewRouterFromConfig      = router.NewRouterFromConfig
	NewClaude                = router.NewClaude
	NewGemini                = router.NewGemini
	NewOpenAI                = router.NewOpenAI
	NewClaudeCLI             = router.NewClaudeCLI
	NewGeminiCLI             = router.NewGeminiCLI
	NewCodexCLI              = router.NewCodexCLI
	NewCommandCLI            = router.NewCommandCLI
	ClassifyTask             = router.ClassifyTask
	EstimateCost             = router.EstimateCost
	ParseUsageMode           = router.ParseUsageMode
	MaxTokensForTask         = router.MaxTokensForTask
	ContextBudgetForProvider = router.ContextBudgetForProvider
	ContextBudgetForMode     = router.ContextBudgetForMode
	LoadUserOverrides        = router.LoadUserOverrides
	ContextWithTask          = router.ContextWithTask
	TaskFromContext          = router.TaskFromContext
	ContextWithSystem        = router.ContextWithSystem
	SystemFromContext        = router.SystemFromContext
	ContextWithModel         = router.ContextWithModel
	ModelFromContext          = router.ModelFromContext
	RegisterProviderFactory  = router.RegisterProviderFactory
	ErrorKindOf              = router.ErrorKindOf
	IsRetryableProviderError = router.IsRetryableProviderError
	DetectCLIs               = router.DetectCLIs
	DetectCLIsJSON           = router.DetectCLIsJSON
	ParseAccessType          = router.ParseAccessType
)
