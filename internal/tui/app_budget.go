package tui

import (
	"fmt"
	"time"

	"github.com/makewand/makewand/internal/model"
)

// applyBudgetRoutingPolicy emits budget warnings and auto-downgrades routing mode
// when spending pressure is high. The budget is measured against month-to-date
// pay-as-you-go spend (persisted across sessions and /clear), not the current
// conversation total.
func (a App) applyBudgetRoutingPolicy() App {
	status := a.monthly.BudgetStatus(time.Now(), a.cfg.MonthlyBudget)
	if status.Level == BudgetOK {
		a.lastBudgetNoticeLevel = BudgetOK
		return a
	}

	if status.Level != a.lastBudgetNoticeLevel {
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: budgetMessage(status),
		})
		a.lastBudgetNoticeLevel = status.Level
	}

	currentMode := model.ModeBalanced
	if a.router.ModeSet() {
		currentMode = a.router.Mode()
	}

	if status.Level == BudgetExceeded && currentMode != model.ModeFast {
		a.router.SetMode(model.ModeFast)
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("Budget exceeded. Auto-switched mode: %s -> %s", currentMode.String(), model.ModeFast.String()),
		})
		return a
	}

	if status.Level == BudgetWarning && (currentMode == model.ModePower || currentMode == model.ModeBalanced) {
		a.router.SetMode(model.ModeFast)
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("Budget warning. Auto-switched mode: %s -> %s", currentMode.String(), model.ModeFast.String()),
		})
	}

	return a
}
