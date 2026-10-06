# makewand Prelaunch Checklist

Use this checklist before shipping a release.

## 0) Prerequisites

- Go toolchain: `make` pins analyzers to the version in `go.mod`
  (`GOTOOLCHAIN=go<version>`), so a newer local Go (e.g. go1.27) is fine; the
  pinned toolchain is downloaded on first use.
- `golangci-lint` **v2.12.2** (the version pinned in `.github/workflows/ci.yml`):
  `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2`
- `govulncheck`: `go install golang.org/x/vuln/cmd/govulncheck@latest`
- `bubblewrap` (`bwrap`) for the sandbox tests. On Ubuntu 24.04, if
  `bwrap --unshare-net --ro-bind / / true` fails with
  `loopback: Failed RTM_NEWADDR: Operation not permitted`, AppArmor restricts
  unprivileged user namespaces; CI runs
  `sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0` (evaluate that
  trade-off before doing the same on a workstation).
- Provider CLIs / API keys configured for the modes you ship (used by `doctor`).

## 1) Run the release gate

```bash
make prelaunch
```

`make prelaunch` runs, in order (stops at the first failure):

1. `./scripts/check_secrets.sh` — built-in credential / private-address / home-path
   rules plus your private patterns from `~/.config/makewand/forbidden_*.txt`
2. `make test-scripts` — `scripts/test_check_secrets.sh` (scanner regression
   corpus) and `scripts/check_version.sh` (README, site, welcome card, CHANGELOG
   and Go default agree with `makewand.__version__`)
3. `make check-shell` — `bash -n` on `scripts/*.sh`, `site/install.sh`, `dispatch/*`
4. `make fmt-check` — `gofmt -l` on tracked Go files
5. `make lint` — `golangci-lint run ./...` under the go.mod toolchain
6. `bash ./scripts/prelaunch_gate.sh`:
   - `bash ./scripts/test_gate.sh` (all `go list ./...` packages, E2E first, then `go vet ./...`)
   - Python unit tests, dispatch regression tests, installer/release-bundle
     contract, offline benchmark harness
   - `go vet ./...`, `go build -trimpath -o build/makewand ./cmd/makewand`
   - `build/makewand doctor --strict --modes fast,balanced,power`
     (`MAKEWAND_DOCTOR_MODES` overrides the modes)
7. `make race` — `scripts/test_race.sh` (race detector on every package)
8. `make vuln` — `govulncheck ./...` under the go.mod toolchain

These cover every check of the CI `verify` job; `doctor --strict` is the
operator-only addition. CI additionally *requires* the live bubblewrap sandbox
test instead of skipping it when `bwrap` is unusable — reproduce that on Linux
with `MAKEWAND_REQUIRE_BWRAP=1 make prelaunch`. Individual targets (`make lint`,
`make vuln`, `make fmt-check`, `make test-scripts`, ...) can be run on their own
before pushing.

Before tagging, also run `bash scripts/check_version.sh --tag vX.Y.Z`: the
Release workflow refuses a tag that differs from `v` + `makewand.__version__`.

The Release workflow also requires the same-commit reusable native Windows
gate, including all Windows Go packages under `-race`, complete native Python
modules, and raw result artifacts. Interactive ordinary-user desktop acceptance
remains a separate human check: follow [Windows desktop verification](WINDOWS_DESKTOP_VERIFICATION.md)
and preserve its commit/binary-bound result. Neither hosted CI nor WinPE alone
certifies terminal interaction or a real provider.

## 2) Run live provider probe (recommended before production)

```bash
MAKEWAND_LIVE_SMOKE=1 MAKEWAND_DOCTOR_MODES=balanced,power make prelaunch
```

This adds `makewand doctor --strict --probe` to step 6 and spends real provider
quota.

Optional tuning:

- `MAKEWAND_PROBE_TIMEOUT=60s`
- `MAKEWAND_PROBE_RETRIES=2`
- `MAKEWAND_DOCTOR_MODES=all` (includes all three modes)
- `makewand doctor --probe` now classifies probe failures as `environment`, `configuration`, or `provider`.
  - `environment` / `configuration` probe failures are downgraded to `WARN` (to avoid mislabeling host/VPN/permission issues as product defects).
  - `provider` probe failures remain `FAIL`.

## 3) Proxy / VPN environments

If your machine requires a proxy (for example `127.0.0.1:7890`), export proxy vars before running:

```bash
export HTTP_PROXY=http://127.0.0.1:7890
export HTTPS_PROXY=http://127.0.0.1:7890
export ALL_PROXY=socks5://127.0.0.1:7890
```

Gemini CLI proxy behavior:

- default: if proxy env exists, makewand respects it
- `MAKEWAND_GEMINI_USE_PROXY=1`: always respect proxy env
- `MAKEWAND_GEMINI_BYPASS_PROXY=1`: force `NO_PROXY` for Google hosts

Custom provider prompt delivery:

- `prompt_mode: "stdin"` is the preferred safe path
- `prompt_mode: "arg"` still works, but `doctor` warns because argv-based prompt delivery is easier to misuse
- empty `prompt_mode` keeps legacy `{{prompt}}` / argv-append behavior for backward compatibility
- shell adapters such as `sh -c` / `bash -c` / `cmd /c` are allowed, but `setup` and `doctor` warn unless prompt delivery is moved to stdin

## 4) CI-friendly command

```bash
./build/makewand doctor --strict --json
```

Use `--probe` when CI runners have network + credentials configured.
