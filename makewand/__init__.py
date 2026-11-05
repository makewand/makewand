"""
Makewand - dispatch to the coding CLIs signed in on this machine (v4.0.0)
---------------------------------------------------------------------
Sends tasks and independent reviews to local subscriptions (Antigravity, Claude
Code, Codex CLI, Grok Build, Muse Code, Aider), with sandboxing, call budgets,
quota awareness and a record of accepted results. Cloud APIs and local models
are available under an explicit billing policy.
"""

__version__ = "4.0.0"
__author__ = "Makewand Authors"

# Release bundles stamp both engines from the same tag. Source checkouts use
# the package version above. This file is installed data, never a cwd lookup.
from pathlib import Path as _Path
_release_version = _Path(__file__).with_name("VERSION")
if _release_version.is_file():
    __version__ = _release_version.read_text(encoding="utf-8").strip()
