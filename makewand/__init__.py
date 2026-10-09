"""
Makewand - Unified Multi-Model AI Subscription Orchestrator (v3.2.0)
---------------------------------------------------------------------
Orchestrates mainstream local AI subscriptions (Antigravity, Claude Code,
Codex CLI, Grok Build, Muse Code, Aider), cloud APIs (DeepSeek, Qwen, GLM, Kimi),
and local models with explicit billing policy, dynamic tool topology, intelligent
quota adaptation, cross-model red-team verification, and automated repair loops.
"""

__version__ = "3.2.0"
__author__ = "Makewand Authors"

# Release bundles stamp both engines from the same tag. Source checkouts use
# the package version above. This file is installed data, never a cwd lookup.
from pathlib import Path as _Path
_release_version = _Path(__file__).with_name("VERSION")
if _release_version.is_file():
    __version__ = _release_version.read_text(encoding="utf-8").strip()
