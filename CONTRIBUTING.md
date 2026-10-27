# Contributing to makewand

Thanks for your interest in improving makewand.

## Maintenance mode and frozen components

Since 2026-10-24 makewand is in maintenance mode: no new features; correctness,
security, recovery and compatibility fixes only. The day-to-day core is dispatch
(`makewand <provider>` / `review`), `route`, the call budget and outcome ledgers,
`mark` and the sandbox.

These components are **frozen** (2026-10-27): they keep their code and tests,
but only defects that lose data or break security are fixed; ordinary bugs,
features and refactors are not taken.

- Multi-tenant server suite: `server*` packages, the `serve` command and `cmd/server-drill`
- Terminal UI: `internal/tui`
- Race / ensemble / merge / DAG: `race`, `ensemble`, `merge`, `makewand/ensemble.py`, `makewand/merger.py`, `makewand/task_dag.py`
- Website `site/` and the Homebrew/Scoop channels (`scripts/publish_homebrew_tap.sh`, `scripts/publish_scoop_bucket.sh`)
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
go test ./...
go vet ./...
make prelaunch
```

Optional live probe (requires credentials/network):

```bash
MAKEWAND_LIVE_SMOKE=1 MAKEWAND_DOCTOR_MODES=balanced,power make prelaunch
```

`go test ./...` connects to nothing outside this host. Test steps that reach
the internet (the dependency-install network's DNS and public egress checks, a
real `npm install` against its registry) run only when asked:

```bash
MAKEWAND_TEST_LIVE_NETWORK=1 go test ./internal/engine -run 'DepsNet|TestFullBuildPipeline'
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
