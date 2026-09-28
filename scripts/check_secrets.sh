#!/usr/bin/env bash
# Makewand Pre-Push & Open-Source Secret Scanner
#
# Scans the tracked working tree / index (default) or a git ref for credentials,
# private-network addresses, workstation home paths and sensitive tracked files.
#
#   scripts/check_secrets.sh            # scan tracked files in the work tree
#   scripts/check_secrets.sh <git-ref>  # scan a commit/branch/tag instead
#
# Built-in rules never depend on local configuration, so CI enforces exactly
# what a maintainer's workstation enforces. Private patterns (internal project
# names, workstation paths) are *appended* from config/env and never replace
# the built-in rules:
#   ${MAKEWAND_FORBIDDEN_NAMES_FILE:-$XDG_CONFIG_HOME/makewand/forbidden_patterns.txt}
#   ${MAKEWAND_FORBIDDEN_PATHS_FILE:-$XDG_CONFIG_HOME/makewand/forbidden_paths.txt}
#   MAKEWAND_FORBIDDEN_NAMES / MAKEWAND_FORBIDDEN_PATHS (comma/space separated)
#
# Findings are reported as path:line plus a redacted preview, so the scanner
# never republishes a leaked secret into (public) CI logs.
#
# Exit codes: 0 = clean, 1 = findings, 2 = scanner error (fails closed: a git
# usage error must never be mistaken for "no match").
#
# Regression tests: scripts/test_check_secrets.sh (run by `make test`).
# Portable to bash 3.2 (macOS): no mapfile, no associative arrays, no ${x,,}.
set -euo pipefail

SELF_PATH="scripts/check_secrets.sh"
TARGET_REF="${1:-}"

if ! git rev-parse --git-dir >/dev/null 2>&1; then
    echo "error: check_secrets.sh must run inside a git repository" >&2
    exit 2
fi

REF_ARGS=()
if [ -n "$TARGET_REF" ]; then
    if ! git rev-parse --verify --quiet "${TARGET_REF}^{tree}" >/dev/null; then
        echo "error: unknown git ref: $TARGET_REF" >&2
        exit 2
    fi
    echo "=== Running Makewand Secret Scan on git ref: $TARGET_REF ==="
    REF_ARGS=("$TARGET_REF")
else
    echo "=== Running Makewand Secret Scan on working tree / index ==="
fi

SCAN_FAIL=0
SCAN_ERROR=0
ERR_FILE="$(mktemp "${TMPDIR:-/tmp}/check_secrets.XXXXXX")"
trap 'rm -f "$ERR_FILE"' EXIT

CONFIG_BASE="${XDG_CONFIG_HOME:-${HOME:-}/.config}/makewand"

# Test fixtures legitimately contain private addresses (10.0.0.5 in a
# trusted-proxy test, ...). The private-network rule skips them; credential
# rules still scan every file.
TEST_EXCLUDES=(
    ':(exclude)*_test.go'
    ':(exclude)tests/'
    ':(exclude)testdata/'
    ':(exclude)*/testdata/*'
    ':(exclude)scripts/test_check_secrets.sh'
)

# ---------------------------------------------------------------------------
# Built-in rules: NAME|SCOPE|ERE  (SCOPE: all = every tracked file,
# nontest = skip test fixtures). Patterns are always passed with `git grep -e`
# so a leading "-" (PEM headers) can never be parsed as an option.
# ---------------------------------------------------------------------------
BUILTIN_RULES=(
    'private-key|all|-----BEGIN[ A-Z0-9_-]*PRIVATE KEY( BLOCK)?-----'
    'anthropic-api-key|all|sk-ant-[A-Za-z0-9_-]{20,}'
    'openai-project-key|all|sk-(proj|svcacct|admin)-[A-Za-z0-9_-]{20,}'
    'openai-legacy-key|all|(^|[^A-Za-z0-9_-])sk-[A-Za-z0-9]{20,}'
    'aws-access-key-id|all|(^|[^A-Za-z0-9])(AKIA|ASIA|ABIA|ACCA|A3T[A-Z0-9])[A-Z0-9]{16}([^A-Za-z0-9]|$)'
    'github-token|all|gh[pousr]_[A-Za-z0-9]{20,}'
    'github-fine-grained-pat|all|github_pat_[A-Za-z0-9_]{22,}'
    'gitlab-token|all|glpat-[A-Za-z0-9_-]{20,}'
    'slack-token|all|xox[baprs]-[A-Za-z0-9-]{10,}'
    'slack-webhook|all|hooks\.slack\.com/services/T[A-Za-z0-9_]+/B[A-Za-z0-9_]+/[A-Za-z0-9_]+'
    'google-api-key|all|AIza[0-9A-Za-z_-]{35}'
    'jwt|all|eyJ[A-Za-z0-9_-]{20,}\.eyJ[A-Za-z0-9_-]{20,}'
    'private-network-ipv4|nontest|(^|[^0-9A-Za-z.])(10(\.[0-9]{1,3}){3}|192\.168(\.[0-9]{1,3}){2}|172\.(1[6-9]|2[0-9]|3[01])(\.[0-9]{1,3}){2})(/[0-9]{1,2})?([^0-9/]|$)'
    'workstation-home-path|all|(^|[^A-Za-z0-9_.~$-])/(home|Users)/[A-Za-z0-9._-]+/'
)

# Conventional placeholder account names in documentation/tests.
PLACEHOLDER_USERS=" alice bob carol dave eve runner username yourname you me example someone name foo jdoe "

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

# redact TOKEN: short preview + length, never the full value.
redact() {
    local t="$1"
    local n=${#t}
    local keep=6
    if [ "$n" -le 16 ]; then keep=4; fi
    printf '%s...[%d chars]' "${t:0:keep}" "$n"
}

# is_allowed RULE TOKEN -> 0 when the hit is a known-safe placeholder.
is_allowed() {
    local rule="$1" token="$2" lc user
    case "$rule" in
        private-network-ipv4)
            # CIDR ranges in docs/config ("10.0.0.0/8") describe networks,
            # not hosts.
            if [[ "$token" =~ [0-9]/[0-9] ]]; then return 0; fi
            return 1
            ;;
        workstation-home-path)
            user="$(printf '%s' "$token" | sed -E 's#^.*/(home|Users)/([^/]+)/.*$#\2#')"
            case "$PLACEHOLDER_USERS" in *" $(lower "$user") "*) return 0 ;; esac
            return 1
            ;;
        name:*|path:*)
            return 1
            ;;
    esac
    # Credential fixtures explicitly labelled as fake.
    lc="$(lower "$token")"
    case "$lc" in
        *mock*|*fake*|*dummy*|*placeholder*|*redacted*) return 0 ;;
    esac
    return 1
}

# run_rule NAME MODE PATTERN [pathspec...]
#   MODE: E (extended regex), F (fixed string), Fi (fixed, case-insensitive)
run_rule() {
    local name="$1" mode="$2" pattern="$3"
    shift 3
    local flags=(-I -n -o)
    case "$mode" in
        E) flags+=(-E) ;;
        F) flags+=(-F) ;;
        Fi) flags+=(-F -i) ;;
    esac
    local out rc=0
    out="$(git grep "${flags[@]}" -e "$pattern" ${REF_ARGS[@]+"${REF_ARGS[@]}"} -- \
        ":(exclude)${SELF_PATH}" "$@" 2>"$ERR_FILE")" || rc=$?
    if [ "$rc" -ge 2 ]; then
        echo "   [SCANNER ERROR] rule '$name' could not run (git grep exit $rc):" >&2
        sed 's/^/     /' "$ERR_FILE" >&2
        SCAN_ERROR=1
        return 0
    fi
    [ "$rc" -eq 1 ] && return 0
    local hit loc token reported=0
    while IFS= read -r hit; do
        [ -n "$hit" ] || continue
        # git grep -n -o prints [ref:]path:line:match
        if [[ "$hit" =~ ^(.*:[0-9]+):(.*)$ ]]; then
            loc="${BASH_REMATCH[1]}"
            token="${BASH_REMATCH[2]}"
        else
            loc="?"
            token="$hit"
        fi
        if is_allowed "$name" "$token"; then
            continue
        fi
        echo "   $loc: $(redact "$token")"
        reported=1
    done <<< "$out"
    if [ "$reported" -eq 1 ]; then
        echo "❌ [SECURITY LEAK] rule '$name' matched (see locations above)"
        SCAN_FAIL=1
    fi
    return 0
}

# read_list_file FILE -> one trimmed, non-comment entry per line
read_list_file() {
    local file="$1" line
    [ -f "$file" ] || return 0
    while IFS= read -r line || [ -n "$line" ]; do
        line="$(printf '%s' "$line" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
        if [ -n "$line" ] && [[ ! "$line" =~ ^# ]]; then
            printf '%s\n' "$line"
        fi
    done < "$file"
}

# 1. Private project names (optional, appended from local config or env)
FORBIDDEN_NAMES=()
NAMES_FILE="${MAKEWAND_FORBIDDEN_NAMES_FILE:-${CONFIG_BASE}/forbidden_patterns.txt}"
while IFS= read -r entry; do
    if [ -n "$entry" ]; then FORBIDDEN_NAMES+=("$entry"); fi
done < <(read_list_file "$NAMES_FILE")
if [ -n "${MAKEWAND_FORBIDDEN_NAMES:-}" ]; then
    IFS=', ' read -r -a extra_names <<< "$MAKEWAND_FORBIDDEN_NAMES"
    for entry in ${extra_names[@]+"${extra_names[@]}"}; do
        if [ -n "$entry" ]; then FORBIDDEN_NAMES+=("$entry"); fi
    done
fi

# 2. Private workstation paths (optional, appended from local config or env)
FORBIDDEN_PATHS=()
PATHS_FILE="${MAKEWAND_FORBIDDEN_PATHS_FILE:-${CONFIG_BASE}/forbidden_paths.txt}"
while IFS= read -r entry; do
    if [ -n "$entry" ]; then FORBIDDEN_PATHS+=("$entry"); fi
done < <(read_list_file "$PATHS_FILE")
if [ -n "${MAKEWAND_FORBIDDEN_PATHS:-}" ]; then
    IFS=', ' read -r -a extra_paths <<< "$MAKEWAND_FORBIDDEN_PATHS"
    for entry in ${extra_paths[@]+"${extra_paths[@]}"}; do
        if [ -n "$entry" ]; then FORBIDDEN_PATHS+=("$entry"); fi
    done
fi

echo "1. Scanning files for private project names (local config)..."
if [ ${#FORBIDDEN_NAMES[@]} -gt 0 ]; then
    for name in "${FORBIDDEN_NAMES[@]}"; do
        run_rule "name:$(redact "$name")" Fi "$name"
    done
else
    echo "   (No private project names configured; built-in rules still apply.)"
fi

echo "2. Scanning files for private host paths (local config)..."
if [ ${#FORBIDDEN_PATHS[@]} -gt 0 ]; then
    for path_pat in "${FORBIDDEN_PATHS[@]}"; do
        run_rule "path:$(redact "$path_pat")" F "$path_pat"
    done
else
    echo "   (No private host paths configured; built-in rules still apply.)"
fi

echo "3. Scanning files for credentials, private addresses and home paths (built-in)..."
for rule in "${BUILTIN_RULES[@]}"; do
    rule_name="${rule%%|*}"
    rest="${rule#*|}"
    rule_scope="${rest%%|*}"
    rule_regex="${rest#*|}"
    if [ "$rule_scope" = "nontest" ]; then
        run_rule "$rule_name" E "$rule_regex" "${TEST_EXCLUDES[@]}"
    else
        run_rule "$rule_name" E "$rule_regex"
    fi
done

# 4. Sensitive files must never be tracked.
echo "4. Checking repository index for sensitive files..."
if [ -n "$TARGET_REF" ]; then
    TRACKED="$(git ls-tree -r --name-only "$TARGET_REF")" || { echo "   [SCANNER ERROR] git ls-tree failed" >&2; SCAN_ERROR=1; TRACKED=""; }
else
    TRACKED="$(git ls-files)" || { echo "   [SCANNER ERROR] git ls-files failed" >&2; SCAN_ERROR=1; TRACKED=""; }
fi
FORBIDDEN_FILE_REGEXES=(
    '(^|/)\.env$'
    '(^|/)\.env\.[^/]+$'
    '(^|/)settings\.local\.json$'
    '(^|/)[^/]*security_best_practices_report[^/]*\.md$'
    '(^|/)id_(rsa|dsa|ecdsa|ed25519)$'
    '(^|/)\.(netrc|pypirc)$'
    '\.(p12|pfx|key)$'
)
# Documented templates are fine.
SAFE_FILE_REGEX='(^|/)\.env\.(example|sample|template)$'
for f in "${FORBIDDEN_FILE_REGEXES[@]}"; do
    hits="$(printf '%s\n' "$TRACKED" | grep -E -e "$f" | grep -Ev -e "$SAFE_FILE_REGEX" || true)"
    if [ -n "$hits" ]; then
        printf '%s\n' "$hits" | sed 's/^/   /'
        echo "❌ [SECURITY LEAK] Tracked sensitive file matching '$f'"
        SCAN_FAIL=1
    fi
done

FORBIDDEN_DIRS=(
    '\.claude'
    '\.codex'
    '\.gemini'
    '\.makewand_sandbox_home'
)
for d in "${FORBIDDEN_DIRS[@]}"; do
    hits="$(printf '%s\n' "$TRACKED" | grep -E -e "(^|/)${d}/" || true)"
    if [ -n "$hits" ]; then
        printf '%s\n' "$hits" | sed 's/^/   /'
        echo "❌ [SECURITY LEAK] Tracked sensitive directory found in repository index: '${d//\\/}'"
        SCAN_FAIL=1
    fi
done

if [ "$SCAN_ERROR" -ne 0 ]; then
    echo ""
    echo "🚨 [ERROR] The secret scanner itself failed; refusing to report a clean result."
    exit 2
fi

if [ "$SCAN_FAIL" -ne 0 ]; then
    echo ""
    echo "🚨 [ABORT] Secret / Private data scan FAILED. Clean up the above findings before pushing to public Git!"
    exit 1
fi

echo "✔ All open-source security & sanitization checks PASSED! Safe for public release."
exit 0
