# makewand Prelaunch Checklist

Use this checklist before shipping a release.

## 1) Run static gate

```bash
make prelaunch
```

This runs:

- `go test ./...`
- `go vet ./...`
- build binary
- `makewand doctor --strict --modes economy,balanced,power`

## 2) Run live provider probe (recommended before production)

```bash
MAKEWAND_LIVE_SMOKE=1 MAKEWAND_DOCTOR_MODES=balanced,power make prelaunch
```

Optional tuning:

- `MAKEWAND_PROBE_TIMEOUT=60s`
- `MAKEWAND_DOCTOR_MODES=all` (includes `free` mode)

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

## 4) CI-friendly command

```bash
./build/makewand doctor --strict --json
```

Use `--probe` when CI runners have network + credentials configured.
