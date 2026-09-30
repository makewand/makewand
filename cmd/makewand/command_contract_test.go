package main

import (
	"slices"
	"testing"
)

func TestAllPythonCommandsHaveWorkingDelegation(t *testing.T) {
	for command := range pythonOrchestratorCmds {
		for _, prefix := range [][]string{{}, {"-C", "/tmp"}, {"--cwd=/tmp"}, {"--protect", "test_policy.py"}, {"--protect=test_policy.py"}, {"--daemon"}, {"-f", "task.txt"}, {"--no-daemon", "--repo-trust", "untrusted"}} {
			args := append(slices.Clone(prefix), command, "--help")
			plan, err := planPythonDelegation(args)
			if err != nil || plan == nil || plan.subcmd != command {
				t.Fatalf("%q: %+v %v", args, plan, err)
			}
		}
	}
}

func TestMissingPythonGlobalValueIsRejected(t *testing.T) {
	for flag := range pythonGlobalValueFlags {
		if _, err := planPythonDelegation([]string{flag}); err == nil {
			t.Fatalf("missing value for %s was accepted", flag)
		}
	}
}

func TestPromptFileFlagsSelectPythonWithoutNamedCommand(t *testing.T) {
	for _, args := range [][]string{{"-f", "task.txt"}, {"--no-daemon", "fix bug"}} {
		plan, err := planPythonDelegation(args)
		if err != nil || plan == nil || plan.subcmd != "run" {
			t.Fatalf("%q: %+v %v", args, plan, err)
		}
	}
	if plan, err := planPythonDelegation([]string{"-C", "/tmp", "serve"}); err != nil || plan != nil {
		t.Fatalf("native -C routed to Python: %+v %v", plan, err)
	}
}

func TestBudgetFlagsAreSharedByNativeAndPythonCommands(t *testing.T) {
	for _, command := range []string{"explain", "serve", "doctor", "chat"} {
		if plan, err := planPythonDelegation([]string{"--max-model-calls", "1", "--call-budget-file", "ledger.json", command}); err != nil || plan != nil {
			t.Fatalf("native %s delegated: %+v %v", command, plan, err)
		}
	}
	if plan, err := planPythonDelegation([]string{"--max-model-calls", "1", "run", "task"}); err != nil || plan == nil || plan.subcmd != "run" {
		t.Fatalf("Python budget flag lost: %+v %v", plan, err)
	}
	for _, maximum := range []string{"0", "-1", "invalid"} {
		if _, err := planPythonDelegation([]string{"--max-model-calls", maximum, "explain"}); err == nil {
			t.Fatalf("invalid max accepted: %q", maximum)
		}
	}
}
