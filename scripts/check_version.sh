#!/usr/bin/env bash
# Version consistency gate (eng-delivery#9).
#
# Single source of truth: `__version__` in makewand/__init__.py. The source
# installer stamps it into the Go binary (scripts/install_source.py) and release builds
# stamp the tag, which must be "v" + __version__. Everything that still has to
# repeat the number (README title, website, welcome card, CHANGELOG) is checked
# against it here, so a bump that misses a copy fails `make test`, CI and the
# release workflow instead of shipping mixed versions (v3.0.1/v3.0.2 shipped with
# __version__ still at 3.0.0).
#
#   scripts/check_version.sh                 # consistency of the checkout
#   scripts/check_version.sh --tag v3.2.0    # ... and the release tag matches
#   scripts/check_version.sh --root DIR      # check another tree (tests)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TAG=""

usage() {
    sed -n '2,15p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
    case "$1" in
        --tag) [ $# -ge 2 ] || { usage >&2; exit 2; }; TAG="$2"; shift 2 ;;
        --tag=*) TAG="${1#--tag=}"; shift ;;
        --root) [ $# -ge 2 ] || { usage >&2; exit 2; }; ROOT="$2"; shift 2 ;;
        --root=*) ROOT="${1#--root=}"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
done

cd "$ROOT"

ERRORS=0
fail() { echo "✗ $*" >&2; ERRORS=$((ERRORS + 1)); }
pass() { echo "✓ $*"; }

INIT="makewand/__init__.py"
[ -f "$INIT" ] || { echo "✗ $INIT not found under $ROOT" >&2; exit 2; }
VERSION="$(sed -nE 's/^__version__[[:space:]]*=[[:space:]]*["'\'']([^"'\'']+)["'\''].*$/\1/p' "$INIT" | head -n 1)"
if [ -z "$VERSION" ]; then
    echo "✗ cannot read __version__ from $INIT" >&2
    exit 2
fi
if ! [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]]; then
    fail "$INIT: __version__ '$VERSION' is not semantic (X.Y.Z)"
fi
echo "Source version (makewand/__init__.py): $VERSION"

# check_tokens FILE [MAX_LINE]: every vX.Y.Z token (optionally only within the
# first MAX_LINE lines) must be v$VERSION. Files with no token pass, so a copy
# that switches to reading __version__ at runtime keeps passing.
check_tokens() {
    local file="$1" max_line="${2:-}" hits bad
    if [ ! -f "$file" ]; then
        fail "$file: missing"
        return
    fi
    if [ -n "$max_line" ]; then
        hits="$(head -n "$max_line" "$file" | grep -noE '(^|[^A-Za-z0-9._-])v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?' || true)"
    else
        hits="$(grep -noE '(^|[^A-Za-z0-9._-])v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?' "$file" || true)"
    fi
    bad="$(printf '%s\n' "$hits" | awk -F: -v want="v$VERSION" 'NF >= 2 {
        tok = $2; for (i = 3; i <= NF; i++) tok = tok ":" $i
        sub(/^[^v]*/, "", tok)
        if (tok != want) printf "%s:%s ", $1, tok
    }')"
    if [ -n "$bad" ]; then
        fail "$file: expected v$VERSION, found line:token ${bad% }"
    else
        pass "$file"
    fi
}

check_tokens "$INIT"
check_tokens README.md 1
if ! head -n 1 README.md | grep -qF "v$VERSION"; then
    fail "README.md: title line must name v$VERSION"
fi
for f in site/index.html site/docs.html site/main.js; do
    [ -e "$f" ] && check_tokens "$f"
done
check_tokens makewand/interactive.py
[ -e tests/test_interactive.py ] && check_tokens tests/test_interactive.py

# Go: builds without ldflags must identify as "dev", never as a stale release.
BUILDINFO="internal/buildinfo/buildinfo.go"
if grep -qE '^[[:space:]]*Version[[:space:]]*=[[:space:]]*"dev"' "$BUILDINFO" 2>/dev/null; then
    pass "$BUILDINFO default Version is \"dev\" (release/source builds stamp it via -ldflags)"
else
    fail "$BUILDINFO: default Version must stay \"dev\"; stamp releases with -ldflags instead"
fi

# The shell entry delegates to the transaction that checks the frozen Python
# engine's version, stamps it into Go, and rejects a mixed engine version.
if grep -qE '^exec python3 -I "\$SCRIPT_ROOT/scripts/install_source.py" "\$SCRIPT_ROOT"$' scripts/install.sh 2>/dev/null; then
    pass "scripts/install.sh delegates to the frozen source installer"
else
    fail "scripts/install.sh must delegate to scripts/install_source.py"
fi
if python3 - scripts/install_source.py <<'PY'
import ast
import sys
from pathlib import Path

try:
    tree = ast.parse(Path(sys.argv[1]).read_text(encoding="utf-8"))
except (OSError, SyntaxError):
    sys.exit(1)

def same(node, expression):
    return ast.dump(node) == ast.dump(ast.parse(expression, mode="eval").body)

assignments = {target.id: node.value for node in ast.walk(tree)
               if isinstance(node, ast.Assign) for target in node.targets
               if isinstance(target, ast.Name)}
python_version = assignments.get("python_version")
python_probe = python_version is not None and any(
    isinstance(node, ast.Call) and same(node.func, "subprocess.check_output")
    and node.args and isinstance(node.args[0], ast.List)
    and all(any(same(argument, expression) for argument in node.args[0].elts)
            for expression in ('sys.executable', '"-I"', '"--version"', 'str(stage / "bin/makewand")'))
    for node in ast.walk(python_version))
derived_version = "version" in assignments and same(assignments["version"], 'python_version[len("makewand "): ]')
stamped_build = any(
    isinstance(node, ast.Call) and same(node.func, "subprocess.run")
    and node.args and isinstance(node.args[0], ast.List)
    and all(any(same(argument, expression) for argument in node.args[0].elts)
            for expression in ('"go"', '"build"', '"-ldflags"',
                               '"-X github.com/makewand/makewand/internal/buildinfo.Version=" + version'))
    for node in ast.walk(tree))
version_gate = any(
    isinstance(node, ast.If) and same(node.test, 'native_version != "makewand version " + version')
    and any(isinstance(action, ast.Raise) and isinstance(action.exc, ast.Call)
            and same(action.exc.func, "RuntimeError") for action in node.body)
    for node in ast.walk(tree))
sys.exit(0 if python_probe and derived_version and stamped_build and version_gate else 1)
PY
then
    pass "scripts/install_source.py stamps and enforces the frozen Python/Go version contract"
else
    fail "scripts/install_source.py must derive Go buildinfo.Version from the frozen Python engine and reject version mismatches"
fi

# Keep a Changelog: the current version needs a released section.
if grep -qE "^## \[$(printf '%s' "$VERSION" | sed 's/[.]/\\./g')\]" CHANGELOG.md 2>/dev/null; then
    pass "CHANGELOG.md has a [$VERSION] section"
else
    fail "CHANGELOG.md: missing '## [$VERSION]' section"
fi

if [ -n "$TAG" ]; then
    if [ "$TAG" = "v$VERSION" ]; then
        pass "release tag $TAG matches __version__"
    else
        fail "release tag '$TAG' does not match v$VERSION from $INIT (bump __version__ and the copies above before tagging)"
    fi
fi

if [ "$ERRORS" -ne 0 ]; then
    echo "Version consistency check FAILED ($ERRORS problem(s))." >&2
    exit 1
fi
echo "Version consistency check passed (v$VERSION)."
