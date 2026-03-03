package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/model"
)

type jsonlTraceSink struct {
	mu sync.Mutex
	f  *os.File
}

func newJSONLTraceSink(path string) (*jsonlTraceSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	return &jsonlTraceSink{f: f}, nil
}

func (s *jsonlTraceSink) Trace(event model.TraceEvent) {
	b, err := json.Marshal(event)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.f.Write(b)
	_, _ = s.f.Write([]byte("\n"))
}

func (s *jsonlTraceSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

type debugTraceSink struct {
	file  *jsonlTraceSink
	route *routeDebugState
}

func (s *debugTraceSink) Trace(event model.TraceEvent) {
	if s.file != nil {
		s.file.Trace(event)
	}
	if s.route != nil {
		s.route.Observe(event)
	}
}

func (s *debugTraceSink) Close() error {
	if s.file == nil {
		return nil
	}
	return s.file.Close()
}

type routeDebugState struct {
	mu      sync.RWMutex
	summary string
}

func newRouteDebugState() *routeDebugState {
	return &routeDebugState{}
}

func (s *routeDebugState) Observe(event model.TraceEvent) {
	if line := summarizeRouteEvent(event); line != "" {
		s.mu.Lock()
		s.summary = line
		s.mu.Unlock()
	}
}

func (s *routeDebugState) Summary() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.summary
}

func summarizeRouteEvent(event model.TraceEvent) string {
	switch event.Event {
	case "route_mode_candidates", "build_adaptive_selected":
		if len(event.Candidates) == 0 {
			return ""
		}
		var top []string
		for i, c := range event.Candidates {
			if i >= 3 {
				break
			}
			label := c.Name
			if c.ModelID != "" {
				label += ":" + c.ModelID
			}
			top = append(top, fmt.Sprintf("%s(score=%.2f fail=%.2f)", label, c.ThompsonScore, c.FailureRate))
		}
		return "Route candidates: " + strings.Join(top, " > ")
	case "route_selected", "build_route_selected":
		if event.Selected == "" {
			return ""
		}
		requested := event.Requested
		if requested == "" {
			requested = event.Selected
		}
		extra := ""
		if event.ModelID != "" {
			extra = " model=" + event.ModelID
		}
		return fmt.Sprintf("Route selected: %s -> %s (fallback=%t)%s", requested, event.Selected, event.IsFallback, extra)
	case "route_candidate_skipped", "chat_fallback_skipped":
		if event.Selected == "" {
			return ""
		}
		detail := event.Detail
		if detail == "" {
			detail = event.Error
		}
		if detail == "" {
			detail = "skipped"
		}
		return fmt.Sprintf("Route skip: %s (%s)", event.Selected, detail)
	case "circuit_opened":
		if event.Selected == "" || event.Detail == "" {
			return ""
		}
		return fmt.Sprintf("Circuit opened: %s (%s)", event.Selected, event.Detail)
	default:
		return ""
	}
}

func emitExecTrace(
	router *model.Router,
	event string,
	phase string,
	plan *engine.ExecPlan,
	approved *bool,
	result *engine.ExecResult,
	err error,
	detail string,
) {
	if router == nil {
		return
	}

	trace := model.TraceEvent{
		Timestamp: time.Now().UTC(),
		Event:     event,
		Phase:     phase,
		Detail:    detail,
	}

	if plan != nil {
		trace.ExecKind = plan.Kind
		trace.Detector = plan.Detector
		trace.Command = plan.Command
		if len(plan.Args) > 0 {
			trace.Args = append([]string(nil), plan.Args...)
		}
	}

	if approved != nil {
		trace.Approved = approved
	}

	if result != nil {
		exitCode := result.ExitCode
		trace.ExitCode = &exitCode
		trace.DurationMS = result.Duration.Milliseconds()
	}

	if err != nil {
		trace.Error = err.Error()
	}

	router.EmitTrace(trace)
}

func boolPtr(v bool) *bool {
	return &v
}
