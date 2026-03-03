package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/model"
)

func loadTraceEvents(t *testing.T, path string) []model.TraceEvent {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	if len(data) == 0 {
		return nil
	}

	var events []model.TraceEvent
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] != '\n' {
			continue
		}
		line := data[start:i]
		start = i + 1
		if len(line) == 0 {
			continue
		}
		var evt model.TraceEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			t.Fatalf("unmarshal trace line: %v", err)
		}
		events = append(events, evt)
	}
	return events
}

func requireTraceEvent(t *testing.T, events []model.TraceEvent, eventName string, check func(model.TraceEvent) bool) model.TraceEvent {
	t.Helper()
	for _, evt := range events {
		if evt.Event != eventName {
			continue
		}
		if check == nil || check(evt) {
			return evt
		}
	}
	t.Fatalf("trace event %q not found", eventName)
	return model.TraceEvent{}
}

func TestExecTrace_DepsDeclineIncludesPlanAndDecision(t *testing.T) {
	app := newBuildAppForDepsTest(t)

	tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
	sink, err := newJSONLTraceSink(tracePath)
	if err != nil {
		t.Fatalf("newJSONLTraceSink: %v", err)
	}
	app.router.SetTraceSink(sink)

	modelAfterStart, cmd := app.startDepsPhase()
	app = modelAfterStart.(App)
	if cmd != nil {
		t.Fatal("startDepsPhase should wait for confirmation")
	}

	modelAfterKey, confirmCmd := app.handleDepsConfirmKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	app = modelAfterKey.(App)
	if confirmCmd == nil {
		t.Fatal("decline key should produce confirm message command")
	}

	modelAfterConfirm, _ := app.Update(confirmCmd())
	app = modelAfterConfirm.(App)
	_ = app

	if err := sink.Close(); err != nil {
		t.Fatalf("close sink: %v", err)
	}

	events := loadTraceEvents(t, tracePath)
	requireTraceEvent(t, events, "pipeline.exec.plan_detected", func(evt model.TraceEvent) bool {
		return evt.Phase == "deps" && evt.Command == "npm" && len(evt.Args) >= 1 && evt.Args[0] == "install"
	})
	declined := requireTraceEvent(t, events, "pipeline.exec.declined", func(evt model.TraceEvent) bool {
		return evt.Phase == "deps" && evt.Command == "npm" && evt.Approved != nil
	})
	if *declined.Approved {
		t.Fatal("declined event should have approved=false")
	}
}

func TestExecTrace_TestsApprovalAndSuccessIncludesResult(t *testing.T) {
	app := newBuildAppForDepsTest(t)

	tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
	sink, err := newJSONLTraceSink(tracePath)
	if err != nil {
		t.Fatalf("newJSONLTraceSink: %v", err)
	}
	app.router.SetTraceSink(sink)

	modelAfterStart, _ := app.startDepsPhase()
	app = modelAfterStart.(App)

	modelAfterDepsKey, depsConfirmCmd := app.handleDepsConfirmKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	app = modelAfterDepsKey.(App)
	if depsConfirmCmd == nil {
		t.Fatal("accept deps key should return confirmation command")
	}

	modelAfterDepsConfirm, _ := app.Update(depsConfirmCmd())
	app = modelAfterDepsConfirm.(App)

	modelAfterDeps, _ := app.Update(depsInstallMsg{result: &engine.ExecResult{ExitCode: 0, Duration: 1200 * time.Millisecond}})
	app = modelAfterDeps.(App)

	modelAfterTestsKey, testsConfirmCmd := app.handleTestsConfirmKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	app = modelAfterTestsKey.(App)
	if testsConfirmCmd == nil {
		t.Fatal("accept tests key should return confirmation command")
	}

	modelAfterTestsConfirm, _ := app.Update(testsConfirmCmd())
	app = modelAfterTestsConfirm.(App)

	modelAfterTestsRun, _ := app.Update(testRunMsg{result: &engine.ExecResult{ExitCode: 0, Duration: 1500 * time.Millisecond}})
	app = modelAfterTestsRun.(App)
	_ = app

	if err := sink.Close(); err != nil {
		t.Fatalf("close sink: %v", err)
	}

	events := loadTraceEvents(t, tracePath)
	confirmed := requireTraceEvent(t, events, "pipeline.exec.confirmed", func(evt model.TraceEvent) bool {
		return evt.Phase == "tests" && evt.Command == "npm" && len(evt.Args) == 1 && evt.Args[0] == "test" && evt.Approved != nil
	})
	if !*confirmed.Approved {
		t.Fatal("tests confirmed event should have approved=true")
	}

	succeeded := requireTraceEvent(t, events, "pipeline.exec.succeeded", func(evt model.TraceEvent) bool {
		return evt.Phase == "tests" && evt.Command == "npm" && evt.ExitCode != nil
	})
	if *succeeded.ExitCode != 0 {
		t.Fatalf("tests succeeded exit_code=%d, want 0", *succeeded.ExitCode)
	}
}
