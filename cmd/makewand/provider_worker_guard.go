package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/makewand/makewand/execution"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The generated contract lists complete command paths and audited flags. New
// commands, descendants, aliases and flags fail closed until explicitly audited.
type providerWorkerQuery struct {
	Engine  string
	MaxArgs int
	Flags   map[string]bool // true means the flag consumes a value
}

func providerWorkerQueryMatches(argv []string, path string, rule providerWorkerQuery) bool {
	words := strings.Fields(path)
	position, arguments := 0, 0
	literal := false
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if !literal && arg == "--" {
			literal = true
			continue
		}
		if !literal && strings.HasPrefix(arg, "-") && arg != "-" {
			flag, _, equals := strings.Cut(arg, "=")
			// -Cdir is the only audited attached short option.
			attached := strings.HasPrefix(arg, "-C") && len(arg) > 2
			if attached {
				flag = "-C"
			}
			takesValue, ok := rule.Flags[flag]
			if !ok {
				return false
			}
			if takesValue && !equals && !attached {
				i++
				if i >= len(argv) {
					return false
				}
			}
			continue
		}
		if position < len(words) {
			// A command after -- is a literal argument, never a command.
			if literal || arg != words[position] {
				return false
			}
			position++
		} else {
			arguments++
		}
	}
	return position == len(words) && arguments <= rule.MaxArgs
}

func providerWorkerRefusal(command string) error {
	return &execution.StatusError{Status: execution.InvalidRequest, OutcomeKnown: true,
		Err: fmt.Errorf("❌ makewand 正作为另一 makewand 任务派发的 AI 子进程运行（MAKEWAND_PROVIDER_WORKER=1），已拒绝递归或未经审计的调用 `%s`。仅允许白名单中的只读查询与参数；管理操作、探测、provider 启动及隐式写入均被禁止。请直接完成当前分配的任务。", command)}
}

// Check before delegation/flag parsing as well: help and completion may bypass
// Cobra's pre-run hook. This is an inherited-environment misuse guard, not a
// security boundary against a worker deliberately removing its marker.
func refuseProviderWorkerArgs(argv []string) error {
	if os.Getenv("MAKEWAND_PROVIDER_WORKER") != "1" {
		return nil
	}
	if len(argv) == 1 && providerWorkerStaticArgs[argv[0]] {
		return nil
	}
	for path, rule := range providerWorkerQueries {
		if providerWorkerQueryMatches(argv, path, rule) {
			return nil
		}
	}
	return providerWorkerRefusal("makewand")
}

// Recheck the resolved Go command and changed flags, including inherited flags,
// so direct command-tree callers also get a default-deny guard.
func refuseProviderWorkerDispatch(cmd *cobra.Command) error {
	if os.Getenv("MAKEWAND_PROVIDER_WORKER") != "1" {
		return nil
	}
	path := strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
	rule, ok := providerWorkerQueries[path]
	if !ok || rule.Engine != "go" {
		return providerWorkerRefusal(cmd.CommandPath())
	}
	var refusal error
	check := func(flag *pflag.Flag) {
		if _, ok := rule.Flags["--"+flag.Name]; !ok {
			refusal = providerWorkerRefusal(cmd.CommandPath())
		}
	}
	cmd.Flags().Visit(check)
	cmd.InheritedFlags().Visit(check)
	for parent := cmd.Parent(); parent != nil; parent = parent.Parent() {
		parent.PersistentFlags().Visit(check)
	}
	return refusal
}
