package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/model"
)

type resourceCandidateProvider struct {
	name                string
	calls, active, peak *atomic.Int32
	cost                float64
	wait                bool
}

func (p *resourceCandidateProvider) Name() string      { return p.name }
func (p *resourceCandidateProvider) IsAvailable() bool { return true }
func (p *resourceCandidateProvider) Chat(ctx context.Context, _ []model.Message, _ string, _ int) (string, model.Usage, error) {
	p.calls.Add(1)
	current := p.active.Add(1)
	defer p.active.Add(-1)
	for previous := p.peak.Load(); current > previous; previous = p.peak.Load() {
		if p.peak.CompareAndSwap(previous, current) {
			break
		}
	}
	if p.wait {
		<-ctx.Done()
		return "", model.Usage{Provider: p.name}, ctx.Err()
	}
	select {
	case <-time.After(5 * time.Millisecond):
	case <-ctx.Done():
		return "", model.Usage{Provider: p.name}, ctx.Err()
	}
	return "candidate output", model.Usage{Provider: p.name, Cost: p.cost}, nil
}
func (p *resourceCandidateProvider) ChatStream(context.Context, []model.Message, string, int) (<-chan model.StreamChunk, error) {
	ch := make(chan model.StreamChunk)
	close(ch)
	return ch, nil
}

func candidateResourceRouter(t *testing.T, cost float64, wait bool) (*model.Router, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	calls, active, peak := &atomic.Int32{}, &atomic.Int32{}, &atomic.Int32{}
	providers := map[string]model.ProviderEntry{}
	for _, name := range []string{"claude", "codex", "gemini"} {
		providers[name] = model.ProviderEntry{Provider: &resourceCandidateProvider{name: name, calls: calls, active: active, peak: peak, cost: cost, wait: wait}, Access: model.AccessSubscription}
	}
	r, err := model.NewRouterFromConfig(model.RouterConfig{Providers: providers, UsageMode: "balanced"})
	if err != nil {
		t.Fatal(err)
	}
	return r, calls, peak
}

func TestCandidateCountConcurrencyAndSoftBudget(t *testing.T) {
	for _, test := range []struct {
		name    string
		options CandidateSelectionOptions
		calls   int32
		peak    int32
	}{{"single", CandidateSelectionOptions{MaxCandidates: 1}, 1, 1}, {"sequential", CandidateSelectionOptions{MaxConcurrent: 1}, 3, 1}, {"concurrency-two", CandidateSelectionOptions{MaxConcurrent: 2}, 3, 2}, {"budget", CandidateSelectionOptions{MaxCostUSD: .2}, 1, 1}} {
		t.Run(test.name, func(t *testing.T) {
			r, calls, peak := candidateResourceRouter(t, .3, false)
			ctx := ContextWithCandidateSelectionOptions(context.Background(), test.options)
			selection := RunCandidateSelection(ctx, r, nil, model.PhaseCode, nil, "", nil)
			if calls.Load() != test.calls || peak.Load() != test.peak {
				t.Fatalf("calls/peak=%d/%d", calls.Load(), peak.Load())
			}
			if selection.Verified {
				t.Fatal("resource tuning bypassed human approval")
			}
			if test.options.MaxCostUSD > 0 && selection.Usage.Cost != .3 {
				t.Fatalf("actual overrun was not retained: %v", selection.Usage.Cost)
			}
		})
	}
}

type candidateWorkspaceObservation struct {
	path string
	err  error
}

type workspaceEditingCandidateProvider struct {
	resourceCandidateProvider
	ready    *sync.WaitGroup
	observed chan candidateWorkspaceObservation
}

func (p *workspaceEditingCandidateProvider) Chat(ctx context.Context, _ []model.Message, _ string, _ int) (string, model.Usage, error) {
	dir, ok := model.WorkDirFromContext(ctx)
	var observationErr error
	if !ok {
		observationErr = errors.New("candidate did not receive an isolated work directory")
	} else if data, err := os.ReadFile(filepath.Join(dir, "input.txt")); err != nil || string(data) != "original" {
		observationErr = fmt.Errorf("candidate observed modified input: %q, %v", data, err)
	} else if _, err := os.Stat(filepath.Join(dir, ".env")); !os.IsNotExist(err) {
		observationErr = errors.New("candidate received a secret dotenv file")
	}
	// Every candidate reads its starting contents before any candidate edits.
	p.ready.Done()
	p.ready.Wait()
	if observationErr == nil {
		file, err := os.OpenFile(filepath.Join(dir, "input.txt"), os.O_WRONLY, 0)
		if err == nil {
			_, err = file.WriteAt([]byte(p.name), 0)
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
		}
		observationErr = err
	}
	p.observed <- candidateWorkspaceObservation{path: dir, err: observationErr}
	// An ordinary synthetic error skips command verification while exercising
	// actual candidate cloning, in-place provider edits, and workspace cleanup.
	return "", model.Usage{Provider: p.name}, errors.New("synthetic provider completed its isolation check")
}

func TestCandidateSelectionIsolatesActualProviderWorkspaces(t *testing.T) {
	dir := t.TempDir()
	for path, content := range map[string]string{"input.txt": "original", ".env": "SYNTHETIC_SECRET=value"} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	project, err := OpenProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ready sync.WaitGroup
	ready.Add(3)
	observed := make(chan candidateWorkspaceObservation, 3)
	providers := map[string]model.ProviderEntry{}
	for _, name := range []string{"claude", "codex", "gemini"} {
		providers[name] = model.ProviderEntry{Provider: &workspaceEditingCandidateProvider{resourceCandidateProvider: resourceCandidateProvider{name: name}, ready: &ready, observed: observed}, Access: model.AccessSubscription}
	}
	r, err := model.NewRouterFromConfig(model.RouterConfig{Providers: providers, UsageMode: "balanced"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := ContextWithCandidateSelectionOptions(context.Background(), CandidateSelectionOptions{MaxCandidates: 3, MaxConcurrent: 3})
	selection := RunCandidateSelection(ctx, r, project, model.PhaseCode, nil, "", nil)
	if selection.Verified {
		t.Fatal("synthetic failed providers became verified")
	}
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		observation := <-observed
		if observation.err != nil {
			t.Fatal(observation.err)
		}
		if observation.path == dir || seen[observation.path] {
			t.Fatal("candidate workspaces were shared")
		}
		seen[observation.path] = true
		if _, err := os.Stat(observation.path); !os.IsNotExist(err) {
			t.Fatal("candidate temporary workspace leaked")
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "input.txt"))
	if err != nil || string(data) != "original" {
		t.Fatalf("provider mutated the source project: %q, %v", data, err)
	}
}

func TestCandidateEnvironmentAndTimeout(t *testing.T) {
	t.Setenv("MAKEWAND_CANDIDATES", "1")
	t.Setenv("MAKEWAND_CANDIDATE_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_CANDIDATE_TIMEOUT", "20ms")
	t.Setenv("MAKEWAND_CANDIDATE_BUDGET_USD", "0")
	r, calls, peak := candidateResourceRouter(t, 0, true)
	started := time.Now()
	selection := RunCandidateSelection(context.Background(), r, nil, model.PhaseCode, nil, "", nil)
	if time.Since(started) > time.Second || calls.Load() != 1 || peak.Load() != 1 || selection.Verified {
		t.Fatal("timeout/count control failed")
	}
	t.Setenv("MAKEWAND_CANDIDATE_BUDGET_USD", "NaN")
	if result := RunCandidateSelection(context.Background(), r, nil, model.PhaseCode, nil, "", nil); result.Err == nil {
		t.Fatal("invalid budget failed open")
	}
}
