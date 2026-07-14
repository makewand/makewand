#!/usr/bin/env bash
set -euo pipefail

REMOTE_HOST="${REMOTE_HOST:-user@192.168.10.16}"
REMOTE_DIR="${REMOTE_DIR:-/tmp/makewand-smoke}"
LOCAL_BIN="${LOCAL_BIN:-/tmp/makewand-local}"
LISTEN_ADDR="${LISTEN_ADDR:-0.0.0.0:18080}"
LISTEN_PORT="${LISTEN_ADDR##*:}"
SERVER_TOKEN="${SERVER_TOKEN:-mw-smoke-$(date +%s)}"
KEEP_SERVER="${KEEP_SERVER:-1}"
LOCAL_IP="${LOCAL_IP:-$(hostname -I | awk '{print $1}')}"
SERVER_URL="${SERVER_URL:-http://${LOCAL_IP}:${LISTEN_PORT}}"
SERVER_LOG="${SERVER_LOG:-/tmp/makewand-remote-smoke-server.log}"

server_pid=""

cleanup() {
  if [[ -n "${server_pid}" && "${KEEP_SERVER}" != "1" ]]; then
    kill "${server_pid}" >/dev/null 2>&1 || true
    wait "${server_pid}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

echo "==> Building makewand to ${LOCAL_BIN}"
go build -o "${LOCAL_BIN}" ./cmd/makewand

echo "==> Starting local server on ${LISTEN_ADDR}"
env MAKEWAND_SERVER_TOKEN="${SERVER_TOKEN}" "${LOCAL_BIN}" serve --listen "${LISTEN_ADDR}" >"${SERVER_LOG}" 2>&1 &
server_pid=$!

echo "==> Waiting for ${SERVER_URL}/v1/models"
ready=0
for _ in $(seq 1 30); do
  if curl -fsS -H "Authorization: Bearer ${SERVER_TOKEN}" "${SERVER_URL}/v1/models" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
if [[ "${ready}" != "1" ]]; then
  echo "server did not become ready; log follows:" >&2
  cat "${SERVER_LOG}" >&2 || true
  exit 1
fi

echo "==> Preparing remote client on ${REMOTE_HOST}:${REMOTE_DIR}"
ssh "${REMOTE_HOST}" "mkdir -p '${REMOTE_DIR}'"
scp "${LOCAL_BIN}" "${REMOTE_HOST}:${REMOTE_DIR}/makewand"
ssh "${REMOTE_HOST}" "chmod +x '${REMOTE_DIR}/makewand'"

echo "==> Remote doctor"
ssh "${REMOTE_HOST}" "cd '${REMOTE_DIR}' && env MAKEWAND_REMOTE_URL='${SERVER_URL}' MAKEWAND_REMOTE_TOKEN='${SERVER_TOKEN}' '${REMOTE_DIR}/makewand' doctor --modes balanced --json"

echo "==> Remote /v1/models"
ssh "${REMOTE_HOST}" "curl -sS -H 'Authorization: Bearer ${SERVER_TOKEN}' '${SERVER_URL}/v1/models'"
echo

echo "==> Remote /v1/chat/completions"
ssh "${REMOTE_HOST}" "cat > '${REMOTE_DIR}/request.json' <<'EOF'
{\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly: http smoke ok\"}]}
EOF"
ssh "${REMOTE_HOST}" "curl -sS -H 'Authorization: Bearer ${SERVER_TOKEN}' -H 'Content-Type: application/json' --data @'${REMOTE_DIR}/request.json' '${SERVER_URL}/v1/chat/completions'"
echo

echo "==> Remote CLI --print"
ssh "${REMOTE_HOST}" "cd '${REMOTE_DIR}' && env MAKEWAND_REMOTE_URL='${SERVER_URL}' MAKEWAND_REMOTE_TOKEN='${SERVER_TOKEN}' '${REMOTE_DIR}/makewand' --print --mode balanced --timeout 90s 'Reply with exactly: remote smoke ok'"

echo "==> Remote session round trip"
ssh "${REMOTE_HOST}" "cat > '${REMOTE_DIR}/session.json' <<'EOF'
{\"version\":1,\"project_path\":\"${REMOTE_DIR}\",\"saved_at\":\"2026-03-18T00:00:00Z\",\"messages\":[{\"role\":\"user\",\"content\":\"resume me\"},{\"role\":\"assistant\",\"content\":\"shared session ok\"}]}
EOF"
ssh "${REMOTE_HOST}" "curl -sS -X PUT -H 'Authorization: Bearer ${SERVER_TOKEN}' -H 'Content-Type: application/json' --data @'${REMOTE_DIR}/session.json' '${SERVER_URL}/v1/sessions/smoke-workspace'"
ssh "${REMOTE_HOST}" "curl -sS -H 'Authorization: Bearer ${SERVER_TOKEN}' '${SERVER_URL}/v1/sessions/smoke-workspace'"
echo

echo "==> Smoke test complete"
echo "Server URL: ${SERVER_URL}"
echo "Server token: ${SERVER_TOKEN}"
echo "Remote binary: ${REMOTE_HOST}:${REMOTE_DIR}/makewand"
if [[ "${KEEP_SERVER}" == "1" ]]; then
  echo "Server left running; log: ${SERVER_LOG}"
else
  echo "Server will be stopped on exit"
fi
