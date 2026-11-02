"""What a task was allowed stays visible; one repository shares one daily limit."""
try:
    import _isolation
except ImportError:
    from tests import _isolation

import json

import pytest

from makewand import call_budget as budget, layered_budget, shared_capacity
try:
    from test_verified_issues_r2 import isolated
except ImportError:  # python3 -m unittest tests.<module>
    from tests.test_verified_issues_r2 import isolated


def _scoped_ledger():
    data = {"attempts": []}
    layered_budget.initialize(data)
    return data


def _linked_repository(root):
    repository = root / "repository"
    metadata = repository / ".git" / "worktrees" / "linked"
    metadata.mkdir(parents=True)
    (metadata / "commondir").write_text("../..\n")
    (repository / "nested" / "directory").mkdir(parents=True)
    worktree = root / "linked"
    worktree.mkdir()
    (worktree / ".git").write_text(f"gitdir: {metadata}\n")
    return repository, worktree


def test_task_without_a_stated_limit_records_the_default(isolated, monkeypatch):
    monkeypatch.delenv("MAKEWAND_MAX_MODEL_CALLS")
    data = _scoped_ledger()
    layered_budget.scopes(data, "plain", None)
    assert data["scopes"]["tasks"]["plain"] == {
        "maximum": layered_budget.DEFAULT_PROJECT, "used": 0, "initial_maximum": layered_budget.DEFAULT_PROJECT,
        "maximum_source": "default", "last_used": data["scopes"]["clock"]}


def test_a_later_smaller_limit_keeps_what_the_task_was_given(isolated, monkeypatch):
    monkeypatch.delenv("MAKEWAND_MAX_MODEL_CALLS")
    data = _scoped_ledger()
    layered_budget.scopes(data, "batch", 300)
    layered_budget.scopes(data, "batch", 1)
    counter = data["scopes"]["tasks"]["batch"]
    assert (counter["maximum"], counter["initial_maximum"], counter["maximum_source"]) == (1, 300, "stated")


def test_limit_from_the_environment_counts_as_stated(isolated, monkeypatch):
    monkeypatch.setenv("MAKEWAND_MAX_MODEL_CALLS", "7")
    data = _scoped_ledger()
    layered_budget.scopes(data, "inherited", None)
    counter = data["scopes"]["tasks"]["inherited"]
    assert (counter["maximum"], counter["initial_maximum"], counter["maximum_source"]) == (7, 7, "stated")


def test_only_task_counters_carry_provenance(isolated):
    data = _scoped_ledger()
    keys = layered_budget.scopes(data, "plain", 3)
    assert "initial_maximum" not in data["scopes"]["projects"][keys[1][1]]
    assert "maximum_source" not in data["scopes"]["machine"][keys[2][1]]


def test_repository_directories_and_worktrees_share_one_project(isolated, tmp_path, monkeypatch):
    monkeypatch.delenv("MAKEWAND_PROJECT_ROOT")
    repository, worktree = _linked_repository(tmp_path)
    expected = shared_capacity.project(repository)[0]
    for directory in (repository, repository / "nested" / "directory", worktree):
        monkeypatch.chdir(directory)
        assert layered_budget.project_ref() == expected
    other = tmp_path / "other"
    (other / ".git").mkdir(parents=True)
    monkeypatch.chdir(other)
    assert layered_budget.project_ref() != expected


def test_project_root_override_still_groups_directories(isolated, tmp_path, monkeypatch):
    repository, worktree = _linked_repository(tmp_path)
    monkeypatch.setenv("MAKEWAND_PROJECT_ROOT", str(tmp_path / "chosen"))
    seen = set()
    for directory in (repository, worktree):
        monkeypatch.chdir(directory)
        seen.add(layered_budget.project_ref())
    assert seen == {shared_capacity.project(tmp_path / "chosen")[0]}


def test_changing_directory_does_not_escape_the_project_daily_limit(isolated, tmp_path, monkeypatch):
    # A batch that gave every call its own worktree never reached the limit.
    monkeypatch.delenv("MAKEWAND_PROJECT_ROOT")
    monkeypatch.setenv("MAKEWAND_PROJECT_DAILY_CALLS", "2")
    monkeypatch.setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "100")
    monkeypatch.setenv("MAKEWAND_POOL_CONCURRENCY", "100")
    repository, worktree = _linked_repository(tmp_path)
    monkeypatch.chdir(repository / "nested")
    budget.reserve("codex", "fast", task_id="first")
    monkeypatch.chdir(worktree)
    budget.reserve("codex", "fast", task_id="second")
    monkeypatch.chdir(repository)
    with pytest.raises(budget.BudgetError, match="exhausted"):
        budget.reserve("codex", "fast", task_id="third")
    ledger = json.loads((tmp_path / "calls.json").read_text())
    assert {attempt["scopes"][1][1] for attempt in ledger["attempts"]} == {
        shared_capacity.project(repository)[0] + ":" + ledger["scopes"]["day"]}


def test_a_shadow_clone_counts_against_the_repository_a_run_started_from(isolated, tmp_path, monkeypatch):
    monkeypatch.delenv("MAKEWAND_PROJECT_ROOT")
    monkeypatch.setenv("MAKEWAND_PROJECT_DAILY_CALLS", "1")
    monkeypatch.setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "100")
    monkeypatch.setenv("MAKEWAND_POOL_CONCURRENCY", "100")
    repository, _ = _linked_repository(tmp_path)
    for name in ("shadow-coder", "shadow-reviewer"):
        (tmp_path / name / ".git").mkdir(parents=True)
    monkeypatch.chdir(repository)
    budget.reserve("codex", "fast", task_id="coder", working_directory=tmp_path / "shadow-coder")
    with pytest.raises(budget.BudgetError, match="exhausted"):
        budget.reserve("codex", "fast", task_id="reviewer", working_directory=tmp_path / "shadow-reviewer")


def test_a_daily_limit_is_not_lowered_for_another_repository(isolated, tmp_path, monkeypatch):
    monkeypatch.delenv("MAKEWAND_PROJECT_ROOT")
    monkeypatch.setenv("MAKEWAND_PROJECT_DAILY_CALLS", "1")
    monkeypatch.setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "100")
    monkeypatch.setenv("MAKEWAND_POOL_CONCURRENCY", "100")
    for name in ("first", "second"):
        (tmp_path / name / ".git").mkdir(parents=True)
        monkeypatch.chdir(tmp_path / name)
        budget.reserve("codex", "fast", task_id=name)
