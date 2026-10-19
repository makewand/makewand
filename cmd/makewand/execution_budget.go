package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/makewand/makewand/execution"
	"github.com/spf13/cobra"
)

// Resolve the stable machine ledger before any Router is built. Task limits
// live in their own scope and never overwrite the daily limits.
func configureExecutionBudget(cmd *cobra.Command) error {
	maximumText := os.Getenv("MAKEWAND_MAX_MODEL_CALLS")
	inheritedMaximum := 0
	if maximumText != "" {
		var err error
		inheritedMaximum, err = strconv.Atoi(maximumText)
		if err != nil || inheritedMaximum <= 0 {
			return fmt.Errorf("inherited model call maximum must be a positive integer")
		}
	}
	if cmd.Flags().Changed("max-model-calls") || cmd.InheritedFlags().Changed("max-model-calls") {
		maximumText = strconv.Itoa(rootMaxModelCallsFlag)
	}
	maximum := 0
	if maximumText != "" {
		var err error
		maximum, err = strconv.Atoi(maximumText)
		if err != nil || maximum <= 0 {
			return fmt.Errorf("model call maximum must be a positive integer")
		}
		if inheritedMaximum > 0 && maximum > inheritedMaximum {
			maximum = inheritedMaximum
			maximumText = strconv.Itoa(maximum)
		}
	}
	path := os.Getenv("MAKEWAND_CALL_BUDGET_FILE")
	if cmd.Flags().Changed("call-budget-file") || cmd.InheritedFlags().Changed("call-budget-file") {
		if rootCallBudgetFileFlag == "" {
			return fmt.Errorf("--call-budget-file requires a nonempty path")
		}
		path = rootCallBudgetFileFlag
	}
	if maximum > 0 && path == "" {
		var err error
		path, err = execution.DefaultBudgetPath()
		if err != nil {
			return err
		}
	}
	if path != "" {
		if err := os.Setenv("MAKEWAND_LAYERED_BUDGET", "1"); err != nil {
			return err
		}
	}

	_, cfg, err := execution.EnsureContext(execution.ContextWithConfig(cmd.Context(), execution.Config{LedgerPath: path, MaxCalls: maximum}))
	if err != nil {
		return err
	}
	if cfg.LedgerPath != "" {
		if err := execution.InitializeBudget(cmd.Context(), cfg); err != nil {
			return err
		}
	}
	path = cfg.LedgerPath
	if path != "" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("resolve model call budget: %w", err)
		}
		if err := os.Setenv("MAKEWAND_CALL_BUDGET_FILE", absolute); err != nil {
			return err
		}
	}
	if maximumText != "" {
		if err := os.Setenv("MAKEWAND_MAX_MODEL_CALLS", maximumText); err != nil {
			return err
		}
	}
	if !execution.ValidIdentifier(os.Getenv("MAKEWAND_TASK_ID")) {
		if err := os.Setenv("MAKEWAND_TASK_ID", execution.NewID()); err != nil {
			return err
		}
	}
	return nil
}
