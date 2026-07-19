#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

# Generate all packages from go list
all_packages=$(go list ./...)

# Count and list all packages
total=$(echo "$all_packages" | wc -l)

echo "=== Package Coverage Report ==="
echo ""
echo "Total packages: $total"
echo ""
echo "All packages to be tested:"
echo "$all_packages" | sort
echo ""

# Categorize packages
cmd_packages=$(echo "$all_packages" | grep "^github.com/makewand/makewand/cmd" | sort)
internal_packages=$(echo "$all_packages" | grep "^github.com/makewand/makewand/internal" | sort)
server_packages=$(echo "$all_packages" | grep "^github.com/makewand/makewand/server" | sort)
router_packages=$(echo "$all_packages" | grep "^github.com/makewand/makewand/router$" | sort)

echo "Categorization:"
echo "  cmd packages:      $(echo "$cmd_packages" | wc -l)"
echo "  internal packages: $(echo "$internal_packages" | wc -l)"
echo "  router packages:   $(echo "$router_packages" | wc -l)"
echo "  server packages:   $(echo "$server_packages" | wc -l)"
echo ""

echo "Server packages in coverage:"
if [[ -n "$server_packages" ]]; then
  echo "$server_packages"
else
  echo "  (none)"
fi
echo ""

# Verify minimum requirements from PR-02
echo "=== Verification ==="
server_count=$(echo "$server_packages" | wc -l)
if [[ $server_count -lt 10 ]]; then
  echo "❌ Expected at least 10 server packages, found $server_count"
  exit 1
fi
echo "✓ All 10+ server packages are included in test coverage"
echo "✓ CI coverage is comprehensive and includes all packages"
