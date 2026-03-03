package tui

import (
	"fmt"

	"github.com/makewand/makewand/internal/model"
)

// applyBudgetRoutingPolicy emits budget warnings and auto-downgrades routing mode
// when spending pressure is high.
func (a App) applyBudgetRoutingPolicy() App {
	status := a.cost.BudgetStatus(a.cfg.MonthlyBudget)
	if status.Level == BudgetOK {
		a.lastBudgetNoticeLevel = BudgetOK
		return a
	}

	if status.Level != a.lastBudgetNoticeLevel {
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: a.cost.CheckBudget(a.cfg.MonthlyBudget),
		})
		a.lastBudgetNoticeLevel = status.Level
	}

	currentMode := model.ModeBalanced
	if a.router.ModeSet() {
		currentMode = a.router.Mode()
	}

	if status.Level == BudgetExceeded && currentMode != model.ModeFree {
		a.router.SetMode(model.ModeFree)
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("Budget exceeded. Auto-switched mode: %s -> %s", currentMode.String(), model.ModeFree.String()),
		})
		return a
	}

	if status.Level == BudgetWarning && (currentMode == model.ModePower || currentMode == model.ModeBalanced) {
		a.router.SetMode(model.ModeEconomy)
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("Budget warning. Auto-switched mode: %s -> %s", currentMode.String(), model.ModeEconomy.String()),
		})
	}

	return a
}
