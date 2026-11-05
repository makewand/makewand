# Contributing to makewand

Thanks for your interest in improving makewand.

## Maintenance mode and frozen components

Since 2026-10-24 makewand is in maintenance mode: no new features; correctness,
security, recovery and compatibility fixes only. The day-to-day core is dispatch
(`makewand <provider>` / `review`), `route`, the call budget and outcome ledgers,
`mark` and the sandbox.

The Go TUI, the multi-tenant server suite, the Go engine and router, native
release packaging and the Windows gate were removed from the main line on
2026-11-05. They stay in the tag `archive/pre-trim-20261105`. The Go code left
here is the `makewand-server quota` reader in `internal/quota`.

These components are **frozen** (2026-10-27): they keep their code and tests,
but only defects that lose data or break security are fixed; ordinary bugs,
features and refactors are not taken.

- Race / ensemble / merge / DAG: `race`, `ensemble`, `merge`, `makewand/ensemble.py`, `makewand/merger.py`, `makewand/task_dag.py`
- Website `site/`
- `benchmarks/`

## Development setup

1. Clone the repository and enter it.
2. Install Go from `go.mod` requirements.
3. Build once to verify toolchain:

```bash
go build -trimpath -o build/makewand ./cmd/makewand
```

## Quality checks

Run these before opening a PR:

```bash
make test
go vet ./...
```

## Commit and PR guidance

- Keep commits focused and minimal.
- Write clear commit messages.
- Include tests when behavior changes.
- Update docs when user-facing behavior changes.

PR checklist:

- [ ] Tests pass locally
- [ ] `go vet` passes
- [ ] Docs updated (if needed)
- [ ] Release impact noted (if needed)

## Security issues

Do not report security vulnerabilities in public issues.
Use the process in [SECURITY.md](SECURITY.md).
