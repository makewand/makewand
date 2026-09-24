"""
Providers package for Makewand.
"""

from makewand.providers.base import check_cli_installed, run_subprocess
from makewand.providers.agy import execute_agy_task, parse_agy_quota
from makewand.providers.claude import execute_claude_task, parse_claude_quota
from makewand.providers.codex import execute_codex_task, parse_codex_quota
from makewand.providers.muse import execute_muse_task, parse_muse_quota
from makewand.providers.grok import execute_grok_task, parse_grok_quota
from makewand.providers.local import execute_local_task, parse_local_quota, is_local_model_available, list_local_models, get_default_local_model
from makewand.providers.api_client import call_api_chat

__all__ = [
    "check_cli_installed",
    "run_subprocess",
    "execute_agy_task",
    "parse_agy_quota",
    "execute_claude_task",
    "parse_claude_quota",
    "execute_codex_task",
    "parse_codex_quota",
    "execute_muse_task",
    "parse_muse_quota",
    "execute_grok_task",
    "parse_grok_quota",
    "execute_local_task",
    "parse_local_quota",
    "is_local_model_available",
    "list_local_models",
    "get_default_local_model",
    "call_api_chat"
]
