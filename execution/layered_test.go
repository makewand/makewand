package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func layeredFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
	t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "0")
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "100")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "100")
	t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "10")
	t.Setenv("MAKEWAND_PROJECT_DAILY_CALLS", "10")
	t.Setenv("MAKEWAND_PROJECT_ROOT", t.TempDir())
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", "")
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "")
	t.Setenv("MAKEWAND_BUDGET_TIMEZONE", "")
	return filepath.Join(t.TempDir(), "calls.json")
}
func TestLayeredIndependentTaskLimits(t *testing.T) {
	path := layeredFixture(t)
	small := ContextWithConfig(context.Background(), Config{LedgerPath: path, MaxCalls: 1, TaskID: "small"})
	if _, err := Reserve(small, Metadata{Engine: "fake"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(small, Metadata{Engine: "fake"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatal(err)
	}
	large := ContextWithConfig(context.Background(), Config{LedgerPath: path, MaxCalls: 5, TaskID: "large"})
	for range 5 {
		if _, err := Reserve(large, Metadata{Engine: "fake"}); err != nil {
			t.Fatal(err)
		}
	}
	if readTestLedger(t, path)["maximum"] != float64(10) {
		t.Fatal("task maximum changed machine limit")
	}
}
func TestLayeredReleaseAndRepeatSettlement(t *testing.T) {
	path := layeredFixture(t)
	ctx := ContextWithConfig(context.Background(), Config{LedgerPath: path, MaxCalls: 1, TaskID: "task"})
	a, err := Reserve(ctx, Metadata{Engine: "fake", Pending: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseUnstarted(); err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseUnstarted(); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(); err == nil {
		t.Fatal("released reservation restarted")
	}
	a, err = Reserve(ctx, Metadata{Engine: "fake", Pending: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseUnstarted(); err != nil {
		t.Fatal(err)
	}
	if err := a.Complete(Outcome{Status: Unknown, Known: false}); err != nil {
		t.Fatal(err)
	}
	if err := a.Complete(Outcome{Status: Passed, Known: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(ctx, Metadata{Engine: "fake"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatal(err)
	}
}
func TestLayeredBusyAdmissionPersistsLowerLimitWithoutSpending(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 5, TaskID: "busy-limit"})
	first, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Complete(Outcome{Status: Passed, Known: true}) })
	lower := ContextWithConfig(ctx, Config{MaxCalls: 3})
	if _, err := Reserve(lower, Metadata{Engine: "codex"}); !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatalf("busy admission: %v", err)
	}
	// A refusal must persist the tighter cap, without charging any scope.
	// Returning to the larger caller configuration cannot undo that cap.
	for _, engine := range []string{"grok", "muse"} {
		a, err := Reserve(ctx, Metadata{Engine: engine})
		if err != nil {
			t.Fatalf("busy admission spent capacity: %v", err)
		}
		t.Cleanup(func() { _ = a.Complete(Outcome{Status: Passed, Known: true}) })
	}
	if _, err := Reserve(ctx, Metadata{Engine: "agy"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("busy admission lost the lower limit: %v", err)
	}
}

// A caller refused as busy must not persist its lower lease cap for others.
func TestLayeredBusyRefusalDoesNotLowerSharedLeaseLimit(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "2")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "2")
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 10, TaskID: "busy-lease-limit"})
	first, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Complete(Outcome{Status: Passed, Known: true}) })
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	if _, err := Reserve(ctx, Metadata{Engine: "codex"}); !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatalf("lower-limit caller should be busy: %v", err)
	}
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "2")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "2")
	second, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatalf("a busy refusal lowered the shared lease limit: %v", err)
	}
	t.Cleanup(func() { _ = second.Complete(Outcome{Status: Passed, Known: true}) })
}

func TestLayeredShanghaiDayAndClockRollback(t *testing.T) {
	layeredFixture(t)
	t.Setenv("MAKEWAND_BUDGET_TIMEZONE", "Asia/Shanghai")
	_, days := recordSkewReports(t)
	data := map[string]any{}
	if err := initializeScopes(data); err != nil {
		t.Fatal(err)
	}
	cfg := Config{TaskID: "clock", MaxCalls: 4}
	before := time.Date(2026, 10, 15, 15, 59, 59, 0, time.UTC)
	a, err := scopeKeys(data, cfg, before)
	if err != nil {
		t.Fatal(err)
	}
	b, err := scopeKeys(data, cfg, before.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if a[2].([]any)[1] == b[2].([]any)[1] {
		t.Fatal("Shanghai midnight did not change daily scope")
	}
	// Back across that midnight: admitted, still counting into the later day.
	c, err := scopeKeys(data, cfg, before)
	if err != nil {
		t.Fatalf("backward day refused: %v", err)
	}
	if c[2].([]any)[1] != b[2].([]any)[1] || c[1].([]any)[1] != b[1].([]any)[1] || len(*days) != 1 {
		t.Fatalf("backward day reopened an earlier bucket: %v %v", c, *days)
	}
}

func TestMixedLayeredGoPythonProcessesShareHardLimit(t *testing.T) {
	t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "100")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "100")
	t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "13")
	t.Setenv("MAKEWAND_PROJECT_DAILY_CALLS", "100")
	t.Setenv("MAKEWAND_PROJECT_ROOT", t.TempDir())
	TestMixedGoPythonProcessesShareHardLimit(t)
}

func TestTaskCounterKeepsWhatItWasGiven(t *testing.T) {
	layeredFixture(t)
	data := map[string]any{}
	if err := initializeScopes(data); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := scopeKeys(data, Config{TaskID: "plain"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := scopeKeys(data, Config{TaskID: "batch", MaxCalls: 300}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := scopeKeys(data, Config{TaskID: "batch", MaxCalls: 1}, now); err != nil {
		t.Fatal(err)
	}
	tasks := data["scopes"].(map[string]any)["tasks"].(map[string]any)
	check := func(task string, maximum, initial int64, source string) {
		t.Helper()
		c := tasks[task].(map[string]any)
		got, _ := jsonInteger(c["maximum"])
		first, _ := jsonInteger(c["initial_maximum"])
		if int64(got) != maximum || int64(first) != initial || c["maximum_source"] != source {
			t.Fatalf("%s: %v", task, c)
		}
	}
	check("plain", 200, 200, "default")
	check("batch", 1, 300, "stated")
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS_DEFAULTED", "1")
	if _, err := scopeKeys(data, Config{TaskID: "named", MaxCalls: 3}, now); err != nil {
		t.Fatal(err)
	}
	check("named", 3, 3, "default")
}

func TestGitFileWithoutCommonDirIsItsOwnRepository(t *testing.T) {
	t.Setenv("MAKEWAND_PROJECT_ROOT", "")
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	module := filepath.Join(parent, "module")
	gitDirectory := filepath.Join(parent, ".git", "modules", "module")
	for _, directory := range []string{filepath.Join(module, "nested"), gitDirectory} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(parent, ".git", "HEAD"), []byte("ref: refs/heads/master\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, ".git"), []byte("gitdir: "+gitDirectory+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, directory := range []string{parent, module, filepath.Join(module, "nested")} {
		id, _, err := capacityProject(directory)
		if err != nil {
			t.Fatal(err)
		}
		ids[directory] = id
	}
	if ids[module] != ids[filepath.Join(module, "nested")] || ids[module] == ids[parent] {
		t.Fatalf("project keys: %v", ids)
	}
}

func TestWorktreesOfASeparateGitDirectoryShareAProjectOfTheirOwn(t *testing.T) {
	t.Setenv("MAKEWAND_PROJECT_ROOT", "")
	root := t.TempDir()
	ids := map[string]string{}
	for _, name := range []string{"first", "second"} {
		directory := filepath.Join(root, "meta", name+".git")
		linked := filepath.Join(directory, "worktrees", "linked")
		if err := os.MkdirAll(linked, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(linked, "commondir"), []byte("../..\n"), 0600); err != nil {
			t.Fatal(err)
		}
		for tree, target := range map[string]string{"main": directory, "linked": linked} {
			path := filepath.Join(root, name, tree)
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: "+target+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			id, _, err := capacityProject(path)
			if err != nil {
				t.Fatal(err)
			}
			ids[name+"/"+tree] = id
		}
	}
	if ids["first/main"] != ids["first/linked"] || ids["second/main"] != ids["second/linked"] || ids["first/main"] == ids["second/main"] {
		t.Fatalf("project keys: %v", ids)
	}
}

func TestGitFilePointingAtNothingIsPassedOver(t *testing.T) {
	t.Setenv("MAKEWAND_PROJECT_ROOT", "")
	repository := filepath.Join(t.TempDir(), "repository")
	pruned := filepath.Join(repository, "pruned")
	for _, directory := range []string{filepath.Join(repository, ".git"), pruned} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, ".git", "HEAD"), []byte("ref: refs/heads/master\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pruned, ".git"), []byte("gitdir: "+filepath.Join(repository, "gone")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prunedID, _, err := capacityProject(pruned)
	if err != nil {
		t.Fatal(err)
	}
	repositoryID, _, err := capacityProject(repository)
	if err != nil {
		t.Fatal(err)
	}
	if prunedID != repositoryID {
		t.Fatal("a stale Git file gave its directory a project of its own")
	}
}

func TestEmptyGitDirectoryIsNotARepository(t *testing.T) {
	t.Setenv("MAKEWAND_PROJECT_ROOT", "")
	root := t.TempDir()
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	for _, directory := range []string{filepath.Join(root, ".git"), first, second} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	firstID, _, err := capacityProject(first)
	if err != nil {
		t.Fatal(err)
	}
	secondID, _, err := capacityProject(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstID == secondID {
		t.Fatal("an empty .git directory joined unrelated directories into one project")
	}
}

func TestProjectDailyLimitFollowsTheRepository(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_PROJECT_ROOT", "")
	t.Setenv("MAKEWAND_PROJECT_DAILY_CALLS", "2")
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	metadata := filepath.Join(repository, ".git", "worktrees", "linked")
	nested := filepath.Join(repository, "nested")
	worktree := filepath.Join(root, "linked")
	shadow := filepath.Join(root, "shadow")
	other := filepath.Join(root, "other")
	for _, directory := range []string{metadata, nested, worktree, filepath.Join(shadow, ".git"), filepath.Join(other, ".git")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, directory := range []string{repository, shadow, other} {
		if err := os.WriteFile(filepath.Join(directory, ".git", "HEAD"), []byte("ref: refs/heads/master\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(metadata, "commondir"), []byte("../..\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+metadata+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reserve := func(task, directory, workingDir string) error {
		t.Chdir(directory)
		ctx := ContextWithConfig(context.Background(), Config{LedgerPath: path, MaxCalls: 3, TaskID: task})
		attempt, err := Reserve(ctx, Metadata{Engine: "fake", WorkingDir: workingDir})
		if err == nil {
			t.Cleanup(func() { _ = attempt.Complete(Outcome{Status: Passed, Known: true}) })
		}
		return err
	}
	if err := reserve("first", nested, ""); err != nil {
		t.Fatal(err)
	}
	if err := reserve("second", worktree, ""); err != nil {
		t.Fatal(err)
	}
	if err := reserve("third", repository, shadow); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("a shadow clone escaped the repository's limit: %v", err)
	}
	if err := reserve("fourth", other, ""); err != nil {
		t.Fatalf("another repository shared the exhausted limit: %v", err)
	}
}
