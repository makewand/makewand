"""Versioned command ownership shared by launchers and release validation."""
import json
from pathlib import Path

_CONTRACT = json.loads(Path(__file__).with_suffix(".json").read_text(encoding="utf-8"))
if _CONTRACT.get("schema") != 1:
    raise RuntimeError("Unsupported Makewand command contract")

PYTHON_COMMANDS = frozenset(_CONTRACT["python_commands"])
NATIVE_GO_COMMANDS = frozenset(_CONTRACT["native_go_commands"])
DELEGATED_PYTHON_COMMANDS = PYTHON_COMMANDS - NATIVE_GO_COMMANDS
PYTHON_VALUE_FLAGS = frozenset(_CONTRACT["python_value_flags"])
SHARED_VALUE_FLAGS = frozenset(_CONTRACT["shared_value_flags"])
PYTHON_BOOLEAN_FLAGS = frozenset(_CONTRACT["python_boolean_flags"])
