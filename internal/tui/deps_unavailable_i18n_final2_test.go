package tui

import (
	"testing"

	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
)

// Final round 2: the detail of tests skipped because the dependencies could
// not be downloaded is translated like the rest of the progress view.
func TestDepsRegistryUnreachableDetailIsTranslated_Final2(t *testing.T) {
	previous := i18n.GetLanguage()
	t.Cleanup(func() { i18n.SetLanguage(previous) })
	for _, tc := range []struct{ lang, want string }{
		{"en", "skipped (dependencies could not be downloaded)"},
		{"zh", "已跳过（依赖无法下载）"},
	} {
		app := newBuildAppForDepsTest(t) // NewApp applies the configured language
		i18n.SetLanguage(tc.lang)
		if got := i18n.Msg().BuildTestsDepsMissing; got != tc.want {
			t.Fatalf("[%s] BuildTestsDepsMissing = %q, want %q", tc.lang, got, tc.want)
		}
		app.pendingDepsPlan = &engine.ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install", "--ignore-scripts"}}
		model, _ := app.Update(depsInstallMsg{result: &engine.ExecResult{ExitCode: 1, Stderr: "npm error code ENOTFOUND"}, err: dependencyNetworkError()})
		app = model.(App)
		if step := app.progress.steps[stepTests]; step.Status != StepDone || step.Detail != tc.want {
			t.Fatalf("[%s] tests step = %+v, want done/%q", tc.lang, step, tc.want)
		}
	}
}
