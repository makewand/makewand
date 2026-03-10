package tui

import (
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
)

func TestApplyBudgetRoutingPolicy_WarningDowngradesToFast(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MonthlyBudget = 1.0
	app := *NewApp(ModeChat, cfg, "")
	app.cost.Add("claude", 0.85)

	app = app.applyBudgetRoutingPolicy()

	if got := app.router.Mode(); got != model.ModeFast {
		t.Fatalf("router.Mode()=%v, want %v", got, model.ModeFast)
	}

	found := false
	for _, msg := range app.chat.messages {
		if strings.Contains(msg.Content, "Auto-switched mode") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected auto-switch notice in chat messages")
	}
}

func TestApplyBudgetRoutingPolicy_ExceededDowngradesToFast(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MonthlyBudget = 1.0
	app := *NewApp(ModeChat, cfg, "")
	app.router.SetMode(model.ModeBalanced)
	app.cost.Add("claude", 1.2)

	app = app.applyBudgetRoutingPolicy()

	if got := app.router.Mode(); got != model.ModeFast {
		t.Fatalf("router.Mode()=%v, want %v", got, model.ModeFast)
	}
}

func TestApplyBudgetRoutingPolicy_DeduplicatesWarnings(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MonthlyBudget = 1.0
	app := *NewApp(ModeChat, cfg, "")
	app.cost.Add("claude", 0.85)

	app = app.applyBudgetRoutingPolicy()
	firstCount := len(app.chat.messages)
	app = app.applyBudgetRoutingPolicy()
	secondCount := len(app.chat.messages)

	if secondCount != firstCount {
		t.Fatalf("budget warning duplicated: first=%d second=%d", firstCount, secondCount)
	}
}
