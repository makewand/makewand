"""
Canonical constants for Makewand across Python modules.
"""

from typing import Set

# Canonical project directories to ignore during repository walks, manifest builds,
# symbol extraction, and workspace operations.
PROJECT_IGNORE_DIRS: Set[str] = {
    ".git",
    "node_modules",
    "vendor",
    "target",
    "dist",
    "build",
    ".pytest_cache",
    ".mypy_cache",
    ".ruff_cache",
    ".venv",
    "venv",
    "env",
    ".coverage",
    ".tox",
    ".idea",
    ".vscode",
    "site-packages",
    ".makewand_sandbox_home",
    ".hypothesis",
    ".nox",
    ".nyc_output",
    "htmlcov",
}

# Directories ignored by repository map parsing (including bytecode caches)
DEFAULT_IGNORE_DIRS: Set[str] = PROJECT_IGNORE_DIRS | {"__pycache__"}
