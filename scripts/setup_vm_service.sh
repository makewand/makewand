#!/usr/bin/env bash
set -euo pipefail

# One-shot VM bootstrap for running makewand as a remote service.
#
# Recommended usage on the VM:
#   sudo env OPENAI_API_KEY=... MAKEWAND_BIN_SOURCE=/tmp/makewand \
#     bash ./scripts/setup_vm_service.sh
#
# Optional overrides:
#   MAKEWAND_USER=makewand
#   MAKEWAND_INSTALL_PATH=/usr/local/bin/makewand
#   MAKEWAND_WORKDIR=/srv/makewand/workspace
#   MAKEWAND_DATA_DIR=/var/lib/makewand
#   MAKEWAND_ETC_DIR=/etc/makewand
#   MAKEWAND_LISTEN=127.0.0.1:8080
#   MAKEWAND_WORKSPACE_PREFIXES=repo-
#   MAKEWAND_ALLOWED_PROVIDERS=codex
#   MAKEWAND_ALLOWED_MODES=balanced
#   MAKEWAND_RUNNER_TOKEN=...
#   MAKEWAND_VIEWER_TOKEN=...
#   MAKEWAND_CREATE_VIEWER_TOKEN=1

if [[ "${EUID}" -ne 0 ]]; then
  echo "run this script as root (for example via sudo)" >&2
  exit 1
fi

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required command: $1" >&2
    exit 1
  }
}

trim() {
  local value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

json_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

json_array_from_csv() {
  local csv="$1"
  local first=1
  local part
  local out="["
  IFS=',' read -r -a parts <<< "$csv"
  for part in "${parts[@]}"; do
    part="$(trim "$part")"
    if [[ -z "$part" ]]; then
      continue
    fi
    if [[ $first -eq 0 ]]; then
      out+=", "
    fi
    out+="\"$(json_escape "$part")\""
    first=0
  done
  out+="]"
  printf '%s' "$out"
}

generate_token() {
  od -An -N20 -tx1 /dev/urandom | tr -d ' \n'
}

resolve_bin_source() {
  if [[ -n "${MAKEWAND_BIN_SOURCE:-}" ]]; then
    printf '%s' "$MAKEWAND_BIN_SOURCE"
    return
  fi

  if [[ -x /tmp/makewand ]]; then
    printf '%s' /tmp/makewand
    return
  fi
  if [[ -x ./build/makewand ]]; then
    printf '%s' ./build/makewand
    return
  fi
  if [[ -x ./makewand ]]; then
    printf '%s' ./makewand
    return
  fi
  if command -v makewand >/dev/null 2>&1; then
    command -v makewand
    return
  fi
  printf '%s' ""
}

MAKEWAND_USER="${MAKEWAND_USER:-makewand}"
MAKEWAND_INSTALL_PATH="${MAKEWAND_INSTALL_PATH:-/usr/local/bin/makewand}"
MAKEWAND_WORKDIR="${MAKEWAND_WORKDIR:-/srv/makewand/workspace}"
MAKEWAND_DATA_DIR="${MAKEWAND_DATA_DIR:-/var/lib/makewand}"
MAKEWAND_HOME="${MAKEWAND_HOME:-${MAKEWAND_DATA_DIR}/home}"
MAKEWAND_CONFIG_DIR="${MAKEWAND_CONFIG_DIR:-${MAKEWAND_DATA_DIR}/config}"
MAKEWAND_ETC_DIR="${MAKEWAND_ETC_DIR:-/etc/makewand}"
MAKEWAND_ENV_FILE="${MAKEWAND_ENV_FILE:-${MAKEWAND_ETC_DIR}/makewand.env}"
MAKEWAND_AUTH_FILE="${MAKEWAND_AUTH_FILE:-${MAKEWAND_ETC_DIR}/server_auth.json}"
MAKEWAND_SERVICE_FILE="${MAKEWAND_SERVICE_FILE:-/etc/systemd/system/makewand.service}"
MAKEWAND_LISTEN="${MAKEWAND_LISTEN:-127.0.0.1:8080}"
MAKEWAND_WORKSPACE_PREFIXES="${MAKEWAND_WORKSPACE_PREFIXES:-repo-}"
MAKEWAND_ALLOWED_PROVIDERS="${MAKEWAND_ALLOWED_PROVIDERS:-codex}"
MAKEWAND_ALLOWED_MODES="${MAKEWAND_ALLOWED_MODES:-balanced}"
MAKEWAND_CREATE_VIEWER_TOKEN="${MAKEWAND_CREATE_VIEWER_TOKEN:-1}"
MAKEWAND_RUNNER_TOKEN="${MAKEWAND_RUNNER_TOKEN:-}"
MAKEWAND_VIEWER_TOKEN="${MAKEWAND_VIEWER_TOKEN:-}"

need_cmd install
need_cmd sed
need_cmd systemctl
need_cmd curl

if [[ -z "${OPENAI_API_KEY:-}" && -z "${ANTHROPIC_API_KEY:-}" && -z "${GEMINI_API_KEY:-}" ]]; then
  echo "set at least one provider API key in the environment before running this script" >&2
  echo "recommended minimum for the default codex-only policy: OPENAI_API_KEY=..." >&2
  exit 1
fi

BIN_SOURCE="$(resolve_bin_source)"
if [[ -z "$BIN_SOURCE" && ! -x "$MAKEWAND_INSTALL_PATH" ]]; then
  echo "could not find a makewand binary; set MAKEWAND_BIN_SOURCE=/path/to/makewand" >&2
  exit 1
fi
if [[ -n "$BIN_SOURCE" && ! -x "$BIN_SOURCE" ]]; then
  echo "MAKEWAND_BIN_SOURCE is not executable: $BIN_SOURCE" >&2
  exit 1
fi

if [[ -z "$MAKEWAND_RUNNER_TOKEN" ]]; then
  MAKEWAND_RUNNER_TOKEN="$(generate_token)"
fi
if [[ "$MAKEWAND_CREATE_VIEWER_TOKEN" == "1" && -z "$MAKEWAND_VIEWER_TOKEN" ]]; then
  MAKEWAND_VIEWER_TOKEN="$(generate_token)"
fi

if ! id -u "$MAKEWAND_USER" >/dev/null 2>&1; then
  useradd -m -r -s /bin/bash "$MAKEWAND_USER"
fi
MAKEWAND_GROUP="$(id -gn "$MAKEWAND_USER")"

install -d -m 0755 /usr/local/bin
install -d -o "$MAKEWAND_USER" -g "$MAKEWAND_GROUP" -m 0750 "$MAKEWAND_WORKDIR"
install -d -o "$MAKEWAND_USER" -g "$MAKEWAND_GROUP" -m 0750 "$MAKEWAND_DATA_DIR"
install -d -o "$MAKEWAND_USER" -g "$MAKEWAND_GROUP" -m 0750 "$MAKEWAND_HOME"
install -d -o "$MAKEWAND_USER" -g "$MAKEWAND_GROUP" -m 0750 "$MAKEWAND_CONFIG_DIR"
install -d -o root -g "$MAKEWAND_GROUP" -m 0750 "$MAKEWAND_ETC_DIR"

if [[ -n "$BIN_SOURCE" ]]; then
  install -m 0755 "$BIN_SOURCE" "$MAKEWAND_INSTALL_PATH"
fi

runner_scopes_json='["models:read", "chat:invoke", "sessions:read", "sessions:write"]'
viewer_scopes_json='["models:read", "sessions:read"]'
workspace_prefixes_json="$(json_array_from_csv "$MAKEWAND_WORKSPACE_PREFIXES")"
allowed_providers_json="$(json_array_from_csv "$MAKEWAND_ALLOWED_PROVIDERS")"
allowed_modes_json="$(json_array_from_csv "$MAKEWAND_ALLOWED_MODES")"

auth_tmp="$(mktemp)"
viewer_block=""
if [[ -n "$MAKEWAND_VIEWER_TOKEN" ]]; then
  viewer_block=$(
    cat <<EOF
,
    {
      "token": "$(json_escape "$MAKEWAND_VIEWER_TOKEN")",
      "description": "read-only session viewer",
      "scopes": ${viewer_scopes_json},
      "workspace_prefixes": ${workspace_prefixes_json},
      "allowed_providers": ${allowed_providers_json},
      "allowed_modes": ${allowed_modes_json}
    }
EOF
  )
fi

cat >"$auth_tmp" <<EOF
{
  "tokens": [
    {
      "token": "$(json_escape "$MAKEWAND_RUNNER_TOKEN")",
      "description": "interactive remote client",
      "scopes": ${runner_scopes_json},
      "workspace_prefixes": ${workspace_prefixes_json},
      "allowed_providers": ${allowed_providers_json},
      "allowed_modes": ${allowed_modes_json}
    }${viewer_block}
  ]
}
EOF
install -o root -g "$MAKEWAND_GROUP" -m 0640 "$auth_tmp" "$MAKEWAND_AUTH_FILE"
rm -f "$auth_tmp"

env_tmp="$(mktemp)"
cat >"$env_tmp" <<EOF
HOME=${MAKEWAND_HOME}
MAKEWAND_CONFIG_DIR=${MAKEWAND_CONFIG_DIR}
MAKEWAND_SERVER_AUTH_CONFIG=${MAKEWAND_AUTH_FILE}
EOF
if [[ -n "${OPENAI_API_KEY:-}" ]]; then
  printf 'OPENAI_API_KEY=%s\n' "$OPENAI_API_KEY" >>"$env_tmp"
fi
if [[ -n "${ANTHROPIC_API_KEY:-}" ]]; then
  printf 'ANTHROPIC_API_KEY=%s\n' "$ANTHROPIC_API_KEY" >>"$env_tmp"
fi
if [[ -n "${GEMINI_API_KEY:-}" ]]; then
  printf 'GEMINI_API_KEY=%s\n' "$GEMINI_API_KEY" >>"$env_tmp"
fi
install -o root -g "$MAKEWAND_GROUP" -m 0640 "$env_tmp" "$MAKEWAND_ENV_FILE"
rm -f "$env_tmp"

service_tmp="$(mktemp)"
cat >"$service_tmp" <<EOF
[Unit]
Description=makewand remote server
After=network.target

[Service]
User=${MAKEWAND_USER}
Group=${MAKEWAND_GROUP}
WorkingDirectory=${MAKEWAND_WORKDIR}
EnvironmentFile=${MAKEWAND_ENV_FILE}
ExecStart=${MAKEWAND_INSTALL_PATH} serve --listen ${MAKEWAND_LISTEN} --data-dir ${MAKEWAND_DATA_DIR}
Restart=always
RestartSec=3
UMask=0077

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${MAKEWAND_DATA_DIR} ${MAKEWAND_WORKDIR}

[Install]
WantedBy=multi-user.target
EOF
install -o root -g root -m 0644 "$service_tmp" "$MAKEWAND_SERVICE_FILE"
rm -f "$service_tmp"

systemctl daemon-reload
systemctl enable --now makewand.service

if ! systemctl --quiet is-active makewand.service; then
  echo "makewand.service failed to start" >&2
  journalctl -u makewand --no-pager -n 100 >&2 || true
  exit 1
fi

ready=0
for _ in $(seq 1 15); do
  if curl -fsS "http://${MAKEWAND_LISTEN}/health" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
if [[ "$ready" != "1" ]]; then
  echo "makewand.service started but /health did not become ready in time" >&2
  journalctl -u makewand --no-pager -n 100 >&2 || true
  exit 1
fi

cat <<EOF
makewand VM setup complete.

Service:
  sudo systemctl status makewand --no-pager
  sudo journalctl -u makewand -f

Files:
  Binary: ${MAKEWAND_INSTALL_PATH}
  Env: ${MAKEWAND_ENV_FILE}
  Auth: ${MAKEWAND_AUTH_FILE}
  Data: ${MAKEWAND_DATA_DIR}
  Workspace: ${MAKEWAND_WORKDIR}

Tokens:
  Runner token: ${MAKEWAND_RUNNER_TOKEN}
EOF

if [[ -n "$MAKEWAND_VIEWER_TOKEN" ]]; then
  printf '  Viewer token: %s\n' "$MAKEWAND_VIEWER_TOKEN"
fi

cat <<EOF

Client tunnel:
  ssh -N -L 8080:127.0.0.1:8080 <user>@<vm-ip>

Client environment:
  export MAKEWAND_REMOTE_URL=http://127.0.0.1:8080
  export MAKEWAND_REMOTE_TOKEN=${MAKEWAND_RUNNER_TOKEN}
  makewand doctor --modes balanced
EOF
