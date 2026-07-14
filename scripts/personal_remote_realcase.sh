#!/usr/bin/env bash
set -euo pipefail

REMOTE_HOST="${REMOTE_HOST:-user@192.168.10.16}"
REMOTE_DIR="${REMOTE_DIR:-/tmp/makewand-smoke}"
REMOTE_CASE_DIR="${REMOTE_CASE_DIR:-/tmp/makewand-realcases-py}"
LOCAL_BIN="${LOCAL_BIN:-/tmp/makewand-local-realcase}"
LISTEN_ADDR="${LISTEN_ADDR:-0.0.0.0:18084}"
LISTEN_PORT="${LISTEN_ADDR##*:}"
START_SERVER="${START_SERVER:-1}"
SERVER_TOKEN="${SERVER_TOKEN:-mw-realcase-$(date +%s)}"
KEEP_SERVER="${KEEP_SERVER:-0}"
LOCAL_IP="${LOCAL_IP:-$(hostname -I | awk '{print $1}')}"
SERVER_URL="${SERVER_URL:-http://${LOCAL_IP}:${LISTEN_PORT}}"
SERVER_LOG="${SERVER_LOG:-/tmp/makewand-realcase-server.log}"
EXPLAIN_MODE="${EXPLAIN_MODE:-balanced}"
REVIEW_MODE="${REVIEW_MODE:-balanced}"
FIX_MODE="${FIX_MODE:-power}"
FIX_PROVIDER="${FIX_PROVIDER:-gemini}"
FIX_TRANSPORT="${FIX_TRANSPORT:-cli}"
RUN_ANALYSIS_STEPS="${RUN_ANALYSIS_STEPS:-0}"

server_pid=""
tmp_dir="$(mktemp -d)"

cleanup() {
  rm -rf "${tmp_dir}"
  if [[ -n "${server_pid}" && "${KEEP_SERVER}" != "1" ]]; then
    kill "${server_pid}" >/dev/null 2>&1 || true
    wait "${server_pid}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

write_file() {
  local path="$1"
  mkdir -p "$(dirname "${path}")"
  cat >"${path}"
}

assert_nonempty_payload() {
  local label="$1"
  local output="$2"
  local payload

  payload="$(printf '%s\n' "${output}" | sed '/^\[makewand\] provider=/d;/^$/d')"
  if [[ -z "${payload}" ]]; then
    fail "${label} returned no payload"
  fi
}

payload_of() {
  printf '%s\n' "$1" | sed '/^\[makewand\] provider=/d;/^$/d'
}

copy_to_remote() {
  local local_path="$1"
  local remote_path="$2"
  scp "${local_path}" "${REMOTE_HOST}:${remote_path}" >/dev/null
}

run_remote_makewand_capture() {
  local mode="$1"
  local prompt_path="$2"

  ssh "${REMOTE_HOST}" "cd '${REMOTE_CASE_DIR}' && env MAKEWAND_REMOTE_URL='${SERVER_URL}' MAKEWAND_REMOTE_TOKEN='${SERVER_TOKEN}' '${REMOTE_DIR}/makewand' --print --mode '${mode}' --timeout 240s \"\$(cat '${prompt_path}')\"" 2>&1
}

run_remote_makewand_with_payload() {
  local label="$1"
  local mode="$2"
  local prompt_path="$3"
  local output=""
  local payload=""
  local attempt

  for attempt in 1 2 3; do
    output="$(run_remote_makewand_capture "${mode}" "${prompt_path}" || true)"
    payload="$(printf '%s\n' "${output}" | sed '/^\[makewand\] provider=/d;/^$/d')"
    if [[ -n "${payload}" ]]; then
      printf '%s\n' "${output}"
      return 0
    fi
    echo "${label} attempt ${attempt} returned no payload; retrying" >&2
    sleep 2
  done

  printf '%s\n' "${output}"
  return 1
}

write_fix_request_json() {
  local provider="$1"
  local prompt_path="$2"
  local request_path="$3"

  python3 - "${provider}" "${prompt_path}" "${request_path}" <<'PY'
import json
import sys

provider, prompt_path, request_path = sys.argv[1:]
with open(prompt_path, "r", encoding="utf-8") as fh:
    prompt = fh.read()

payload = {
    "model": provider,
    "messages": [
        {
            "role": "user",
            "content": prompt,
        }
    ],
}

with open(request_path, "w", encoding="utf-8") as fh:
    json.dump(payload, fh)
PY
}

run_remote_cli_fix_attempt() {
  local mode="$1"
  local prompt_path="$2"
  local candidate_path="$3"

  ssh "${REMOTE_HOST}" "cd '${REMOTE_CASE_DIR}' && env MAKEWAND_REMOTE_URL='${SERVER_URL}' MAKEWAND_REMOTE_TOKEN='${SERVER_TOKEN}' '${REMOTE_DIR}/makewand' --print --mode '${mode}' --timeout 240s \"\$(cat '${prompt_path}')\" > '${candidate_path}'" 2>&1
}

run_remote_http_fix_attempt() {
  local provider="$1"
  local request_path="$2"
  local response_path="$3"
  local candidate_path="$4"

  ssh "${REMOTE_HOST}" "cd '${REMOTE_CASE_DIR}' && curl -fsS -H 'Authorization: Bearer ${SERVER_TOKEN}' -H 'Content-Type: application/json' --data @'${request_path}' '${SERVER_URL}/v1/chat/completions' > '${response_path}' && python3 - '${response_path}' '${candidate_path}' <<'PY'
import json
import sys

response_path, candidate_path = sys.argv[1:]
with open(response_path, 'r', encoding='utf-8') as fh:
    data = json.load(fh)

content = data['choices'][0]['message']['content'].strip()
fence = chr(96) * 3
marker = '--- FILE:'

if marker in content and fence in content:
    start = content.find(fence)
    end = content.find(fence, start + len(fence))
    if start != -1 and end != -1:
        block = content[start + len(fence):end].lstrip()
        if block.startswith('python\n'):
            block = block[len('python\n'):]
        content = block.strip()
elif content.startswith(fence):
    block = content[len(fence):].lstrip()
    if block.startswith('python\n'):
        block = block[len('python\n'):]
    end = block.find(fence)
    if end != -1:
        block = block[:end]
    content = block.strip()

with open(candidate_path, 'w', encoding='utf-8') as fh:
    if content:
        fh.write(content)
        if not content.endswith('\n'):
            fh.write('\n')
PY" 2>&1
}

run_fix_attempt() {
  local prompt_local="$1"
  local prompt_remote="$2"
  local request_local="$3"
  local request_remote="$4"
  local response_remote="$5"
  local candidate_remote="$6"

  copy_to_remote "${prompt_local}" "${prompt_remote}"
  if [[ "${FIX_TRANSPORT}" == "http" ]]; then
    write_fix_request_json "${FIX_PROVIDER}" "${prompt_local}" "${request_local}"
    copy_to_remote "${request_local}" "${request_remote}"
    run_remote_http_fix_attempt "${FIX_PROVIDER}" "${request_remote}" "${response_remote}" "${candidate_remote}"
    return
  fi
  run_remote_cli_fix_attempt "${FIX_MODE}" "${prompt_remote}" "${candidate_remote}"
}

run_fix_attempt_with_retries() {
  local prompt_local="$1"
  local prompt_remote="$2"
  local request_local="$3"
  local request_remote="$4"
  local response_remote="$5"
  local candidate_remote="$6"
  local label="$7"
  local output=""
  local attempt

  for attempt in 1 2 3; do
    output="$(run_fix_attempt "${prompt_local}" "${prompt_remote}" "${request_local}" "${request_remote}" "${response_remote}" "${candidate_remote}" || true)"
    if candidate_is_nonempty "${candidate_remote}" && candidate_is_valid_python "${candidate_remote}"; then
      printf '%s\n' "${output}"
      return 0
    fi
    echo "${label} retry ${attempt} returned no usable candidate; retrying" >&2
    sleep 2
  done

  printf '%s\n' "${output}"
  return 1
}

run_remote_tests() {
  ssh "${REMOTE_HOST}" "cd '${REMOTE_CASE_DIR}' && python3 -m unittest -q" 2>&1
}

candidate_is_nonempty() {
  local candidate_path="$1"
  ssh "${REMOTE_HOST}" "test -s '${candidate_path}'"
}

candidate_is_valid_python() {
  local candidate_path="$1"
  ssh "${REMOTE_HOST}" "python3 -m py_compile '${candidate_path}' >/dev/null 2>&1"
}

apply_candidate_and_test() {
  local candidate_path="$1"
  ssh "${REMOTE_HOST}" "cd '${REMOTE_CASE_DIR}' && cp '${candidate_path}' '${REMOTE_CASE_DIR}/pricing.py' && python3 -m unittest -q" 2>&1
}

warm_server() {
  local strict="${1:-0}"
  local request_path="${tmp_dir}/warmup.request.json"
  local response=""
  local attempt

  if [[ "${FIX_TRANSPORT}" == "http" ]]; then
    write_file "${request_path}" <<EOF
{"model":"${FIX_PROVIDER}","mode":"${FIX_MODE}","messages":[{"role":"user","content":"Fix the failing Python test. Reply with exactly: warmup ok"}]}
EOF
  else
    write_file "${request_path}" <<EOF
{"mode":"${FIX_MODE}","messages":[{"role":"user","content":"Fix the failing Python test. Reply with exactly: warmup ok"}]}
EOF
  fi

  echo "==> Warming server routing path"
  for attempt in 1 2 3 4 5; do
    response="$(curl -fsS -H "Authorization: Bearer ${SERVER_TOKEN}" -H "Content-Type: application/json" --data @"${request_path}" "${SERVER_URL}/v1/chat/completions" 2>&1 || true)"
    if grep -Fq '"warmup ok"' <<<"${response}"; then
      return 0
    fi
    echo "server warmup attempt ${attempt} did not return the expected payload; retrying" >&2
    sleep 2
  done

  if [[ "${strict}" == "1" ]]; then
    echo "server warmup failed; last response follows:" >&2
    printf '%s\n' "${response}" >&2
    if [[ -f "${SERVER_LOG}" ]]; then
      echo "server log follows:" >&2
      cat "${SERVER_LOG}" >&2 || true
    fi
    exit 1
  fi

  echo "warning: server warmup did not return the expected payload" >&2
}

echo "==> Building makewand to ${LOCAL_BIN}"
go build -o "${LOCAL_BIN}" ./cmd/makewand

if [[ "${START_SERVER}" == "1" ]]; then
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
fi

warm_server "${START_SERVER}"

echo "==> Preparing remote client on ${REMOTE_HOST}:${REMOTE_DIR}"
ssh "${REMOTE_HOST}" "mkdir -p '${REMOTE_DIR}' '${REMOTE_CASE_DIR}'"
scp "${LOCAL_BIN}" "${REMOTE_HOST}:${REMOTE_DIR}/makewand" >/dev/null
ssh "${REMOTE_HOST}" "chmod +x '${REMOTE_DIR}/makewand' && command -v python3 >/dev/null"

write_file "${tmp_dir}/pricing.py" <<'EOF'
def calculate_total(items, coupon_pct=0, tax_rate=0.08):
    subtotal = 0
    for item in items:
        subtotal += item["price"]

    if coupon_pct:
        subtotal = subtotal - coupon_pct

    total = subtotal * (1 + tax_rate)
    return round(total, 2)
EOF

write_file "${tmp_dir}/test_pricing.py" <<'EOF'
import unittest

from pricing import calculate_total


class PricingTests(unittest.TestCase):
    def test_empty_cart(self):
        self.assertEqual(calculate_total([]), 0)

    def test_quantity_and_tax(self):
        items = [
            {"price": 12.5, "qty": 2},
            {"price": 5.0, "qty": 1},
        ]
        self.assertEqual(calculate_total(items, tax_rate=0.1), 33.0)

    def test_percentage_coupon(self):
        items = [
            {"price": 20.0, "qty": 1},
            {"price": 20.0, "qty": 1},
        ]
        self.assertEqual(calculate_total(items, coupon_pct=10, tax_rate=0), 36.0)

    def test_fractional_average_case(self):
        items = [
            {"price": 9.99, "qty": 3},
        ]
        self.assertEqual(calculate_total(items, coupon_pct=0, tax_rate=0), 29.97)


if __name__ == "__main__":
    unittest.main()
EOF

echo "==> Uploading real-case fixture"
copy_to_remote "${tmp_dir}/pricing.py" "${REMOTE_CASE_DIR}/pricing.py"
copy_to_remote "${tmp_dir}/test_pricing.py" "${REMOTE_CASE_DIR}/test_pricing.py"

echo "==> Baseline failing tests"
baseline_output="$(run_remote_tests || true)"
printf '%s\n' "${baseline_output}"
if ! grep -q 'FAILED (failures=3)' <<<"${baseline_output}"; then
  fail "expected the baseline fixture to fail with 3 failing tests"
fi

if [[ "${RUN_ANALYSIS_STEPS}" == "1" ]]; then
  write_file "${tmp_dir}/explain.prompt" <<'EOF'
The tests in test_pricing.py are failing. Explain the concrete root causes in pricing.py in 3 short bullets.
EOF
  copy_to_remote "${tmp_dir}/explain.prompt" "${REMOTE_CASE_DIR}/explain.prompt"

  echo "==> Real-case explanation"
  explain_output="$(run_remote_makewand_with_payload "explanation" "${EXPLAIN_MODE}" "${REMOTE_CASE_DIR}/explain.prompt" || true)"
  printf '%s\n' "${explain_output}"
  if [[ -z "$(payload_of "${explain_output}")" ]]; then
    echo "warning: explanation returned no payload after retries" >&2
  fi

  write_file "${tmp_dir}/review.prompt" <<'EOF'
Please review pricing.py for real bugs only. Focus on behavioral issues, not style. Keep it to 3 short bullets.
EOF
  copy_to_remote "${tmp_dir}/review.prompt" "${REMOTE_CASE_DIR}/review.prompt"

  echo "==> Real-case review"
  review_output="$(run_remote_makewand_with_payload "review" "${REVIEW_MODE}" "${REMOTE_CASE_DIR}/review.prompt" || true)"
  printf '%s\n' "${review_output}"
  if [[ -z "$(payload_of "${review_output}")" ]]; then
    echo "warning: review returned no payload after retries" >&2
  fi
fi

write_file "${tmp_dir}/fix-attempt-1.prompt" <<'EOF'
Rewrite pricing.py so python3 -m unittest -q passes. Output only Python source code for pricing.py. No markdown. No prose. The first line must be: def calculate_total(items, coupon_pct=0, tax_rate=0.08):
EOF

echo "==> Real-case fix attempt 1"
attempt1_output="$(run_fix_attempt_with_retries "${tmp_dir}/fix-attempt-1.prompt" "${REMOTE_CASE_DIR}/fix-attempt-1.prompt" "${tmp_dir}/fix-attempt-1.request.json" "${REMOTE_CASE_DIR}/fix-attempt-1.request.json" "${REMOTE_CASE_DIR}/fix-attempt-1.response.json" "${REMOTE_CASE_DIR}/pricing_candidate.py" "fix attempt 1" || true)"
printf '%s\n' "${attempt1_output}"

if candidate_is_nonempty "${REMOTE_CASE_DIR}/pricing_candidate.py"; then
  attempt1_tests="$(apply_candidate_and_test "${REMOTE_CASE_DIR}/pricing_candidate.py" || true)"
else
  attempt1_tests="candidate generation failed or returned empty content"
fi
printf '%s\n' "${attempt1_tests}"

if ! grep -q '^OK$' <<<"${attempt1_tests}"; then
  write_file "${tmp_dir}/fix-attempt-2.prompt" <<'EOF'
pricing.py still fails the real-case tests. Items are dicts with keys price and qty, not quantity. Rewrite pricing.py so python3 -m unittest -q passes. Output only Python source code for pricing.py. No markdown. No prose. The first line must be: def calculate_total(items, coupon_pct=0, tax_rate=0.08):
EOF

  echo "==> Real-case fix attempt 2"
  attempt2_output="$(run_fix_attempt_with_retries "${tmp_dir}/fix-attempt-2.prompt" "${REMOTE_CASE_DIR}/fix-attempt-2.prompt" "${tmp_dir}/fix-attempt-2.request.json" "${REMOTE_CASE_DIR}/fix-attempt-2.request.json" "${REMOTE_CASE_DIR}/fix-attempt-2.response.json" "${REMOTE_CASE_DIR}/pricing_candidate.py" "fix attempt 2" || true)"
  printf '%s\n' "${attempt2_output}"

  if candidate_is_nonempty "${REMOTE_CASE_DIR}/pricing_candidate.py"; then
    final_tests="$(apply_candidate_and_test "${REMOTE_CASE_DIR}/pricing_candidate.py" || true)"
  else
    final_tests="candidate generation failed or returned empty content"
  fi
else
  final_tests="${attempt1_tests}"
fi

if ! grep -q '^OK$' <<<"${final_tests}"; then
  write_file "${tmp_dir}/fix-attempt-3.prompt" <<'EOF'
Rewrite pricing.py to satisfy this exact behavior: each item is a dict with keys price and qty; subtotal is the sum of price * qty for every item; coupon_pct is a percentage discount applied before tax; an empty item list must return 0.0; final result must be rounded to 2 decimals. Output only Python source code for pricing.py. No markdown. No prose. The first line must be: def calculate_total(items, coupon_pct=0, tax_rate=0.08):
EOF

  echo "==> Real-case fix attempt 3"
  attempt3_output="$(run_fix_attempt_with_retries "${tmp_dir}/fix-attempt-3.prompt" "${REMOTE_CASE_DIR}/fix-attempt-3.prompt" "${tmp_dir}/fix-attempt-3.request.json" "${REMOTE_CASE_DIR}/fix-attempt-3.request.json" "${REMOTE_CASE_DIR}/fix-attempt-3.response.json" "${REMOTE_CASE_DIR}/pricing_candidate.py" "fix attempt 3" || true)"
  printf '%s\n' "${attempt3_output}"

  if candidate_is_nonempty "${REMOTE_CASE_DIR}/pricing_candidate.py"; then
    final_tests="$(apply_candidate_and_test "${REMOTE_CASE_DIR}/pricing_candidate.py" || true)"
  else
    final_tests="candidate generation failed or returned empty content"
  fi
fi

echo "==> Final test run"
printf '%s\n' "${final_tests}"
if ! grep -q '^OK$' <<<"${final_tests}"; then
  fail "real-case fix regression did not produce a passing test run"
fi

write_file "${tmp_dir}/session.json" <<'EOF'
{"version":1,"project_path":"/tmp/makewand-realcases-py","saved_at":"2026-03-18T00:30:00Z","messages":[{"role":"user","content":"Fix pricing.py so the tests pass"},{"role":"assistant","content":"Use price * qty, apply coupon as a percentage before tax, and return 0.0 for an empty cart."}]}
EOF

echo "==> Cross-device session round trip"
curl -fsS -X PUT -H "Authorization: Bearer ${SERVER_TOKEN}" -H "Content-Type: application/json" --data @"${tmp_dir}/session.json" "${SERVER_URL}/v1/sessions/pricing-realcase" >/dev/null
session_output="$(ssh "${REMOTE_HOST}" "curl -fsS -H 'Authorization: Bearer ${SERVER_TOKEN}' '${SERVER_URL}/v1/sessions/pricing-realcase'")"
printf '%s\n' "${session_output}"
if ! grep -Fq 'price * qty' <<<"${session_output}"; then
  fail "session round trip did not return the expected assistant message"
fi

echo "==> Real-case regression complete"
echo "Server URL: ${SERVER_URL}"
echo "Server token: ${SERVER_TOKEN}"
echo "Remote binary: ${REMOTE_HOST}:${REMOTE_DIR}/makewand"
echo "Remote case dir: ${REMOTE_CASE_DIR}"
if [[ "${START_SERVER}" == "1" && "${KEEP_SERVER}" == "1" ]]; then
  echo "Server left running; log: ${SERVER_LOG}"
fi
