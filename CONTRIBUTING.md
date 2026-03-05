# Contributing to makewand

Thanks for your interest in improving makewand.

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
