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
- `MAKEWAND_PROBE_RETRIES=2`
- `MAKEWAND_DOCTOR_MODES=all` (includes `free` mode)
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

Remote Ollama behavior:

- default: only localhost / 127.0.0.1 / ::1 are allowed
- `MAKEWAND_OLLAMA_ALLOW_REMOTE=1`: allow remote Ollama hosts explicitly

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
