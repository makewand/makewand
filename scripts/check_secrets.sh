#!/usr/bin/env bash
# Makewand Pre-Push & Open-Source Secret Scanner
# Scans git working tree, staged index, or a target branch for sensitive private paths, project names, and credentials.
set -euo pipefail

TARGET_REF="${1:-}"

if [ -n "$TARGET_REF" ]; then
    echo "=== Running Makewand Secret Scan on git ref: $TARGET_REF ==="
    GREP_ARGS=("$TARGET_REF" "--")
else
    echo "=== Running Makewand Secret Scan on working tree / index ==="
    GREP_ARGS=("--")
fi

SCAN_FAIL=0

# 1. Patterns that must NEVER appear in open-source commits
FORBIDDEN_NAMES=(
    "adlims"
    "dibiaoshui"
    "zainadiao"
    "whereifish"
    "yufenlei"
    "shineiyun"
    "watersmap"
)

# 2. Hardcoded local path patterns
FORBIDDEN_PATHS=(
    "/home/user/"
    "/mnt/data/"
)

# 3. Credential & secret regex patterns
CREDENTIAL_REGEXES=(
    "sk-[a-zA-Z0-9]{20,}"
    "sk-ant-[a-zA-Z0-9]{20,}"
    "ghp_[a-zA-Z0-9]{20,}"
    "-----BEGIN[ A-Z0-9_-]*PRIVATE KEY-----"
    "eyJ[a-zA-Z0-9_-]{20,}\.eyJ[a-zA-Z0-9_-]{20,}"
)

# 1. Scan for forbidden names
echo "1. Scanning files for private project names..."
for name in "${FORBIDDEN_NAMES[@]}"; do
    if git grep -I -i -n --fixed-strings "$name" "${GREP_ARGS[@]}" ':!scripts/check_secrets.sh' 2>/dev/null; then
        echo "❌ [SECURITY LEAK] Found forbidden private project reference: '$name'"
        SCAN_FAIL=1
    fi
done

# 2. Scan for hardcoded host paths
echo "2. Scanning files for hardcoded host paths..."
for path_pat in "${FORBIDDEN_PATHS[@]}"; do
    if git grep -I -n --fixed-strings "$path_pat" "${GREP_ARGS[@]}" ':!scripts/check_secrets.sh' 2>/dev/null; then
        echo "❌ [SECURITY LEAK] Found forbidden host absolute path: '$path_pat'"
        SCAN_FAIL=1
    fi
done

# 3. Scan for credentials
echo "3. Scanning files for credential patterns..."
for cred_pat in "${CREDENTIAL_REGEXES[@]}"; do
    if git grep -I -E -n "$cred_pat" "${GREP_ARGS[@]}" ':!scripts/check_secrets.sh' 2>/dev/null; then
        echo "❌ [SECURITY LEAK] Potential credential or token pattern matched: '$cred_pat'"
        SCAN_FAIL=1
    fi
done

# 4. Check for forbidden tracked files
echo "4. Checking repository index for sensitive files..."
FORBIDDEN_FILES=(
    ".env"
    "settings.local.json"
    "security_best_practices_report.md"
)
for f in "${FORBIDDEN_FILES[@]}"; do
    if git ls-files | grep -E "(^|/)$f$" 2>/dev/null; then
        echo "❌ [SECURITY LEAK] Tracked sensitive file found in repository index: '$f'"
        SCAN_FAIL=1
    fi
done

FORBIDDEN_DIRS=(
    ".claude"
    ".codex"
    ".gemini"
    ".makewand_sandbox_home"
)
for d in "${FORBIDDEN_DIRS[@]}"; do
    if git ls-files | grep -E "(^|/)$d/" 2>/dev/null; then
        echo "❌ [SECURITY LEAK] Tracked sensitive directory found in repository index: '$d'"
        SCAN_FAIL=1
    fi
done

if [ "$SCAN_FAIL" -ne 0 ]; then
    echo ""
    echo "🚨 [ABORT] Secret / Private data scan FAILED. Clean up the above findings before pushing to public Git!"
    exit 1
fi

echo "✔ All open-source security & sanitization checks PASSED! Safe for public release."
exit 0
