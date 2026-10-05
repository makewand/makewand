package model

import (
	"context"
	"github.com/makewand/makewand/internal/testfixture"
	"os"
	"path/filepath"
	"testing"

	"github.com/makewand/makewand/internal/config"
)

func TestDisabledProvidersCannotEnterStaticOrDynamicRoutes(t *testing.T) {
	t.Setenv("MAKEWAND_API_POLICY", "allow_paid")
	cfg := policyConfig(t)
	cfg.EnabledProviders = map[string]bool{"claude": false, "agy": false, "codex": false, "custom": false, "local": false}
	sentinel := filepath.Join(t.TempDir(), "executed")
	bin := testfixture.WriteCLI(t, "fake-cli", testfixture.Spec{Sentinel: sentinel, Stdout: "fixture\n"})
	cfg.CLIs = []config.CLITool{{Name: "claude", BinPath: bin}, {Name: "codex", BinPath: bin}, {Name: "gemini", BinPath: bin}, {Name: "agy", BinPath: bin}}
	cfg.CustomProviders = []config.CustomProvider{{Name: "claude-api", Command: bin}, {Name: "anthropic", Command: bin}, {Name: "custom", Command: bin}, {Name: "ollama", Command: bin, Access: "local"}}
	r, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasAnyModel() {
		t.Fatal("disabled providers treated as a usable backend")
	}
	if available := r.Available(); len(available) != 0 {
		t.Fatalf("disabled provider registered: %v", available)
	}
	for _, name := range []string{"claude", "claude-api", "anthropic", "gemini", "gemini-api", "codex", "openai", "custom", "ollama"} {
		if _, err := r.Get(name); err == nil {
			t.Errorf("Get(%s) bypassed disable", name)
		}
		if _, err := r.RouteProvider(name, PhaseCode); err == nil {
			t.Errorf("RouteProvider(%s) bypassed disable", name)
		}
	}
	for _, mode := range []UsageMode{ModeFast, ModeBalanced, ModePower} {
		r.SetMode(mode)
		if _, err := r.Route(TaskCode); err == nil {
			t.Errorf("mode %v route succeeded with disabled pool", mode)
		}
		if _, _, _, err := r.ChatBest(context.Background(), PhaseCode, []Message{{Role: "user", Content: "must not dispatch"}}, ""); err == nil {
			t.Errorf("mode %v dynamic selection bypassed disable", mode)
		}
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("disabled CLI executed", err)
	}
}
