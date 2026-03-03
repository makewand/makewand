package tui

import (
	"encoding/json"
	"os"
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
