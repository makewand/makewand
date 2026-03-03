package model

import "time"

// TraceSink receives structured routing/selection trace events.
type TraceSink interface {
	Trace(event TraceEvent)
}

// TraceEvent captures one routing or provider-attempt event.
type TraceEvent struct {
	Timestamp  time.Time        `json:"timestamp"`
	Event      string           `json:"event"`
	Mode       string           `json:"mode,omitempty"`
	Task       string           `json:"task,omitempty"`
	Phase      string           `json:"phase,omitempty"`
	Requested  string           `json:"requested,omitempty"`
	Selected   string           `json:"selected,omitempty"`
	ModelID    string           `json:"model_id,omitempty"`
	ExecKind   string           `json:"exec_kind,omitempty"`
	Detector   string           `json:"detector,omitempty"`
	Command    string           `json:"command,omitempty"`
	Args       []string         `json:"args,omitempty"`
	Approved   *bool            `json:"approved,omitempty"`
	ExitCode   *int             `json:"exit_code,omitempty"`
	IsFallback bool             `json:"is_fallback,omitempty"`
	DurationMS int64            `json:"duration_ms,omitempty"`
	Candidates []TraceCandidate `json:"candidates,omitempty"`
	Error      string           `json:"error,omitempty"`
	Detail     string           `json:"detail,omitempty"`
}

// TraceCandidate captures one candidate score and ranking input.
type TraceCandidate struct {
	Name          string  `json:"name"`
	ModelID       string  `json:"model_id,omitempty"`
	Access        string  `json:"access,omitempty"`
	Order         int     `json:"order"`
	UseCount      int     `json:"use_count"`
	FailureRate   float64 `json:"failure_rate"`
	Requests      int     `json:"requests"`
	ThompsonScore float64 `json:"thompson_score"`
}

func taskTypeName(task TaskType) string {
	switch task {
	case TaskAnalyze:
		return "analyze"
	case TaskCode:
		return "code"
	case TaskReview:
		return "review"
	case TaskExplain:
		return "explain"
	case TaskFix:
		return "fix"
	default:
		return "unknown"
	}
}

func buildPhaseName(phase BuildPhase) string {
	switch phase {
	case PhasePlan:
		return "plan"
	case PhaseCode:
		return "code"
	case PhaseReview:
		return "review"
	case PhaseFix:
		return "fix"
	default:
		return "unknown"
	}
}

func accessTypeName(at AccessType) string {
	switch at {
	case AccessFree:
		return "free"
	case AccessLocal:
		return "local"
	case AccessSubscription:
		return "subscription"
	case AccessAPI:
		return "api"
	default:
		return "unknown"
	}
}

func toTraceCandidates(candidates []candidate) []TraceCandidate {
	out := make([]TraceCandidate, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, TraceCandidate{
			Name:          c.name,
			ModelID:       c.modelID,
			Access:        accessTypeName(c.access),
			Order:         c.order,
			UseCount:      c.useCount,
			FailureRate:   c.failureRate,
			Requests:      c.requests,
			ThompsonScore: c.thompsonScore,
		})
	}
	return out
}
