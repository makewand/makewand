#!/usr/bin/env bash
# Regression tests for scripts/check_secrets.sh (eng-delivery#4).
#
# Plants one sensitive sample per throwaway git repository and asserts the
# scanner fails (exit 1) and names the file without echoing the full secret;
# then asserts a corpus of look-alike but safe content (CIDR ranges, docs
# placeholders, labelled mock keys, public PEM blocks) stays clean (exit 0).
#
# Sample secrets are assembled from fragments at runtime so this file never
# contains a literal credential and stays clean under the real repository scan.
#
# CHECK_SECRETS_SCRIPT=<path> runs the suite against another scanner build
# (used to prove the suite fails on the pre-fix scanner).
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCANNER="${CHECK_SECRETS_SCRIPT:-$ROOT_DIR/scripts/check_secrets.sh}"
[ -f "$SCANNER" ] || { echo "scanner not found: $SCANNER" >&2; exit 2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/test_check_secrets.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

# Hermetic environment: no maintainer config, no global git config.
export HOME="$WORK/home"
mkdir -p "$HOME"
unset XDG_CONFIG_HOME MAKEWAND_FORBIDDEN_NAMES MAKEWAND_FORBIDDEN_PATHS \
      MAKEWAND_FORBIDDEN_NAMES_FILE MAKEWAND_FORBIDDEN_PATHS_FILE || true
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
# Fixture repos are tiny: single-threaded git grep avoids thread start-up cost.
export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=grep.threads GIT_CONFIG_VALUE_0=1

PASS=0
FAIL=0
ok()  { PASS=$((PASS + 1)); echo "ok   - $*"; }
bad() { FAIL=$((FAIL + 1)); echo "FAIL - $*"; }

# Fragments (none of these match a rule on its own).
A20="Q7vK2mXr9LpT4sWd8NbZ"
A40="${A20}h3JcF6yUe1Ga5RoV0iMn"
A48="${A40}P2tS8kDq"
A36="${A20}h3JcF6yUe1Ga5RoV"
A35="${A20}h3JcF6yUe1Ga5Ro"
U16="Z7Q2M9R4T6W1X3Y5"

REPO_N=0
new_repo() {
    REPO_N=$((REPO_N + 1))
    REPO="$WORK/repo$REPO_N"
    mkdir -p "$REPO/scripts" "$REPO/docs"
    cp "$SCANNER" "$REPO/scripts/check_secrets.sh"
    chmod +x "$REPO/scripts/check_secrets.sh"
    printf '# fixture\n' > "$REPO/README.md"
    git -C "$REPO" init -q
    git -C "$REPO" add -A
    git -C "$REPO" -c commit.gpgsign=false commit -q -m base
}

# run_scan [args...] -> sets RC and OUT
run_scan() {
    RC=0
    OUT="$(cd "$REPO" && bash scripts/check_secrets.sh "$@" 2>&1)" || RC=$?
}

# expect_leak NAME RELPATH CONTENT [SECRET]
expect_leak() {
    local name="$1" rel="$2" content="$3" secret="${4:-}"
    new_repo
    mkdir -p "$(dirname "$REPO/$rel")"
    printf '%s\n' "$content" > "$REPO/$rel"
    git -C "$REPO" add -A
    run_scan
    if [ "$RC" -ne 1 ]; then
        bad "$name: expected exit 1, got $RC"
        printf '%s\n' "$OUT" | sed 's/^/     | /'
        return
    fi
    if ! printf '%s' "$OUT" | grep -qF -- "$rel"; then
        bad "$name: finding does not name $rel"
        printf '%s\n' "$OUT" | sed 's/^/     | /'
        return
    fi
    if [ -n "$secret" ] && printf '%s' "$OUT" | grep -qF -- "$secret"; then
        bad "$name: scanner echoed the full secret into its output"
        return
    fi
    ok "$name detected"
}

echo "# positive samples (each must be detected on its own)"
expect_leak "rsa-private-key"      docs/k1.md "$(printf -- '-----BEGIN %s PRIVATE %s-----' RSA KEY)"
expect_leak "openssh-private-key"  docs/k2.md "$(printf -- '-----BEGIN %s PRIVATE %s-----' OPENSSH KEY)"
expect_leak "pgp-private-key"      docs/k3.md "$(printf -- '-----BEGIN %s PRIVATE %s BLOCK-----' PGP KEY)"
S="sk-""ant-api03-${A40}";            expect_leak "anthropic-api03"  docs/a.md "ANTHROPIC_API_KEY=$S" "$S"
S="sk-""proj-${A40}_${A20}";          expect_leak "openai-project"   docs/b.md "OPENAI_API_KEY=\"$S\"" "$S"
S="sk-""svcacct-${A40}";              expect_leak "openai-svcacct"   docs/b2.md "key: $S" "$S"
S="sk-""${A48}";                      expect_leak "openai-legacy"    docs/c.md "key=$S" "$S"
S="AK""IA${U16}";                     expect_leak "aws-access-key"   deploy/aws.env.md "aws_access_key_id = $S" "$S"
S="gh""p_${A36}";                     expect_leak "github-classic"   docs/d.md "token $S" "$S"
S="gh""o_${A36}";                     expect_leak "github-oauth"     docs/d2.md "token $S" "$S"
S="github""_pat_11ABCDEFG0${A40}_${A20}"; expect_leak "github-fine-grained" docs/e.md "GH_TOKEN=$S" "$S"
S="gl""pat-${A20}";                   expect_leak "gitlab-pat"       docs/e2.md "$S" "$S"
S="xo""xb-1234567890-${A20}";         expect_leak "slack-bot-token"  docs/f.md "SLACK=$S" "$S"
S="https://hooks.slack.com/services/T0""1234567/B0""1234567/${A20}"; expect_leak "slack-webhook" docs/f2.md "$S" "$S"
S="AI""za${A35}";                     expect_leak "google-api-key"   site/g.js "const k='$S';" "$S"
S="ey""J${A40}.ey""J${A40}.sig";      expect_leak "jwt"              docs/h.md "Bearer $S" "$S"
expect_leak "private-ip-192.168"   docs/infra.md "Gitea lives at http://192.""168.10.16:3000/"
expect_leak "private-ip-10/8"      README.md    "staff test box: 10.""0.0.30."
expect_leak "private-ip-172.16/12" deploy/n.md  "db=172.""20.1.9"
expect_leak "linux-home-path"      docs/p.md    "cd /home/""jsmith/dev/makewand"
expect_leak "macos-home-path"      docs/p2.md   "see /Users/""jsmith/Library/Caches/x"

echo "# private patterns are appended from env/config, never required"
new_repo
printf 'internal codename %s\n' "zephyr""moon" > "$REPO/docs/n.md"
git -C "$REPO" add -A
RC=0; OUT="$(cd "$REPO" && MAKEWAND_FORBIDDEN_NAMES="zephyr""moon" bash scripts/check_secrets.sh 2>&1)" || RC=$?
if [ "$RC" -eq 1 ] && printf '%s' "$OUT" | grep -qF docs/n.md; then ok "env-provided private name detected"; else bad "env-provided private name not detected (rc=$RC)"; fi
mkdir -p "$HOME/.config/makewand"
printf '# private\n/srv/%s/\n' "ops""box" > "$HOME/.config/makewand/forbidden_paths.txt"
printf 'rsync to /srv/%s/data\n' "ops""box" > "$REPO/docs/n.md"
git -C "$REPO" add -A
run_scan
if [ "$RC" -eq 1 ] && printf '%s' "$OUT" | grep -qF docs/n.md; then ok "config-file private path detected"; else bad "config-file private path not detected (rc=$RC)"; fi
rm -rf "$HOME/.config"

echo "# sensitive tracked files"
new_repo
printf 'X=1\n' > "$REPO/.env"
git -C "$REPO" add -f .env
run_scan
if [ "$RC" -eq 1 ] && printf '%s' "$OUT" | grep -qF '.env'; then ok "tracked .env detected"; else bad "tracked .env not detected (rc=$RC)"; fi
new_repo
mkdir -p "$REPO/keys"; printf 'x\n' > "$REPO/keys/id_ed25519"
git -C "$REPO" add -f keys/id_ed25519
run_scan
if [ "$RC" -eq 1 ] && printf '%s' "$OUT" | grep -qF 'keys/id_ed25519'; then ok "tracked ssh key file detected"; else bad "tracked ssh key file not detected (rc=$RC)"; fi

echo "# ref mode scans history, not just the work tree"
new_repo
S="gh""p_${A36}"
printf 'token %s\n' "$S" > "$REPO/docs/old.md"
git -C "$REPO" add -A
git -C "$REPO" -c commit.gpgsign=false commit -q -m leak
LEAK_COMMIT="$(git -C "$REPO" rev-parse HEAD)"
git -C "$REPO" rm -q docs/old.md
git -C "$REPO" -c commit.gpgsign=false commit -q -m cleanup
run_scan
if [ "$RC" -eq 0 ]; then ok "clean work tree passes after removal"; else bad "clean work tree reported rc=$RC"; fi
run_scan "$LEAK_COMMIT"
if [ "$RC" -eq 1 ] && printf '%s' "$OUT" | grep -qF docs/old.md; then ok "ref scan finds leak in history"; else bad "ref scan missed leak (rc=$RC)"; fi

echo "# fail closed on scanner errors"
run_scan "no-such-ref-$$"
if [ "$RC" -eq 2 ]; then ok "unknown ref fails closed (exit 2)"; else bad "unknown ref returned $RC"; fi
mkdir -p "$WORK/not-a-repo/scripts"
cp "$SCANNER" "$WORK/not-a-repo/scripts/check_secrets.sh"
RC=0; OUT="$(cd "$WORK/not-a-repo" && GIT_CEILING_DIRECTORIES="$WORK" bash scripts/check_secrets.sh 2>&1)" || RC=$?
if [ "$RC" -ne 0 ] && ! printf '%s' "$OUT" | grep -q 'PASSED'; then ok "outside a git repo does not report PASSED"; else bad "outside a git repo returned $RC: $OUT"; fi

echo "# look-alike but safe content must not be flagged"
new_repo
cat > "$REPO/docs/clean.md" <<'CLEAN'
Trusted proxies: 10.0.0.0/8, 192.168.0.0/16 and 172.16.0.0/12 (CIDR ranges).
Loopback 127.0.0.1, wildcard 0.0.0.0, public resolver 8.8.8.8, version 1.10.0.5.
export ANTHROPIC_API_KEY=sk-ant-...
Fixture key sk-ant-mock-key-12345678901234567890 is labelled mock.
A risk-assessment-framework-for-teams-and-orgs and task-queue-worker-configuration-reference.
AIzaSyFakeGeminiTokenKey123 is too short to be a Google key; the AKIA prefix alone is prose.
-----BEGIN PUBLIC KEY-----
-----BEGIN CERTIFICATE-----
Tokens look like ghp_... or xoxb-... in the docs.
Paths: /home/alice/work/demo, /home/runner/work/makewand, ~/.config/makewand, $HOME/.local/bin, ${HOME}/x
CLEAN
printf 'package x\n\nvar addr = "10.0.0.5:8080" // test fixture\n' > "$REPO/docs/proxy_test.go"
printf 'KEY=replace-me\n' > "$REPO/.env.example"
git -C "$REPO" add -A
run_scan
if [ "$RC" -eq 0 ]; then
    ok "safe corpus passes"
else
    bad "safe corpus flagged (rc=$RC)"
    printf '%s\n' "$OUT" | sed 's/^/     | /'
fi

echo ""
echo "check_secrets regression: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
