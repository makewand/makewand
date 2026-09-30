#!/usr/bin/env python3
"""Verify every public command on a source launcher or extracted binary."""
import argparse
import json
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
contract = json.loads((ROOT / "makewand" / "command_contract.json").read_text())
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("binary", type=Path)
parser.add_argument("--python-launcher", type=Path, help="Also verify the installed Python entry delegates native commands")
args = parser.parse_args()
binary = str(args.binary.resolve())
commands = set(contract["python_commands"]) - set(contract["native_go_commands"])
entries = [("Go", [binary])]
if args.python_launcher:
    entries.append(("Python", [sys.executable, "-I", str(args.python_launcher.resolve())]))
native_commands = set(contract["native_go_commands"]) - set(contract["python_commands"])
for label, entry in entries:
    for command in sorted(commands):
        result = subprocess.run([*entry, command, "--help"], text=True,
                                capture_output=True, timeout=20, check=True)
        if not result.stdout.startswith(f"usage: makewand {command}"):
            raise SystemExit(f"{label} entry command {command} did not reach its registered Python parser")
    for prefix in (("-C", "."), ("--cwd=." ,), ("--daemon",), ("--no-daemon",),
                   ("--max-model-calls", "1"), ("--call-budget-file", "unused-budget.json"),
                   ("--workflow", "auto"), ("--risk", "high"), ("--total-timeout", "30"),
                   ("--judge-reserve-seconds", "5")):
        result = subprocess.run([*entry, *prefix, "repomap", "--help"], text=True,
                                capture_output=True, timeout=20, check=True)
        if not result.stdout.startswith("usage: makewand repomap"):
            raise SystemExit(f"{label} entry global flags {prefix} hid the repomap command")
    for command in sorted(native_commands):
        for prefix in ((), ("-C", "."), ("--cwd=." ,), ("--repo-trust", "untrusted"),
                       ("--max-model-calls", "1"), ("--call-budget-file", "unused-budget.json")):
            result = subprocess.run([*entry, *prefix, command, "--help"], text=True,
                                    capture_output=True, timeout=20, check=True)
            if f"makewand {command}" not in result.stdout or "Usage:" not in result.stdout:
                raise SystemExit(f"{label} entry command {command} did not reach its registered Go parser")
    for prefix in (("--daemon",), ("-f", "unused-prompt.txt"), ("--workflow", "race"),
                   ("--risk", "high"), ("--total-timeout", "30"), ("--judge-reserve-seconds", "5")):
        result = subprocess.run([*entry, *prefix, "serve", "--help"], text=True,
                                capture_output=True, timeout=20)
        if result.returncode == 0:
            raise SystemExit(f"{label} entry silently accepted unsupported task flags {prefix} for serve")
print(f"Command contract passed: {len(commands)} Python and {len(native_commands)} native commands, shared globals and {len(entries)} entries")
