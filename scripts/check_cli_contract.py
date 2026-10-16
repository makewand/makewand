#!/usr/bin/env python3
"""Verify the command contract and, given a binary, every public command.

Without a binary, checks one source invariant: every subcommand registered
with argparse in makewand/cli.py appears in the command contract. Pass a
launcher binary to also execute the help-text contract against it.
"""
import argparse
import ast
import json
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
contract = json.loads((ROOT / "makewand" / "command_contract.json").read_text(encoding="utf-8"))


def _const_str(node):
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return node.value
    return None


def _iter_strings(node):
    text = _const_str(node)
    if text is not None:
        return [text]
    if isinstance(node, (ast.List, ast.Tuple, ast.Set)):
        out = []
        for elt in node.elts:
            out.extend(_iter_strings(elt))
        return out
    return []


def _tuple_field_strings(node, index):
    if not isinstance(node, (ast.List, ast.Tuple)):
        return []
    out = []
    for elt in node.elts:
        if isinstance(elt, (ast.List, ast.Tuple)) and index < len(elt.elts):
            text = _const_str(elt.elts[index])
            if text is not None:
                out.append(text)
    return out


def _bound_names(for_node, name):
    target = for_node.target
    if isinstance(target, ast.Name) and target.id == name:
        return _iter_strings(for_node.iter)
    if isinstance(target, ast.Tuple):
        for index, elt in enumerate(target.elts):
            if isinstance(elt, ast.Name) and elt.id == name:
                return _tuple_field_strings(for_node.iter, index)
    return []


class _SubcommandVisitor(ast.NodeVisitor):
    def __init__(self):
        self.stack = []
        self.names = set()
        self.unresolved = []

    def generic_visit(self, node):
        self.stack.append(node)
        super().generic_visit(node)
        self.stack.pop()

    def visit_Call(self, node):
        func = node.func
        if isinstance(func, ast.Attribute) and func.attr == "add_parser" and node.args:
            arg = node.args[0]
            text = _const_str(arg)
            if text is not None:
                self.names.add(text)
            elif isinstance(arg, ast.Name):
                found = []
                for ancestor in reversed(self.stack):
                    if isinstance(ancestor, ast.For):
                        found = _bound_names(ancestor, arg.id)
                        if found:
                            break
                if not found:
                    self.unresolved.append(f"{arg.id}:{getattr(arg, 'lineno', '?')}")
                self.names.update(found)
            else:
                self.unresolved.append(f"line {getattr(arg, 'lineno', '?')}")
        self.generic_visit(node)


def argparse_subcommands(cli_path: Path) -> set:
    tree = ast.parse(cli_path.read_text(encoding="utf-8"), filename=str(cli_path))
    visitor = _SubcommandVisitor()
    visitor.visit(tree)
    if visitor.unresolved:
        raise SystemExit("cannot resolve argparse subcommands: " + ", ".join(visitor.unresolved))
    if not visitor.names:
        raise SystemExit(f"no argparse subcommands found in {cli_path}")
    return visitor.names


def check_argparse_subcommands_in_contract() -> set:
    """Every subcommand Python's CLI registers must be listed in the contract."""
    registered = argparse_subcommands(ROOT / "makewand" / "cli.py")
    covered = set(contract["python_commands"]) | set(contract["native_go_commands"])
    missing = sorted(registered - covered)
    if missing:
        raise SystemExit(
            "Python argparse subcommands missing from the command contract: " + ", ".join(missing)
        )
    return registered


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", nargs="?", type=Path)
    parser.add_argument("--python-launcher", type=Path, help="Also verify the installed Python entry delegates native commands")
    args = parser.parse_args()
    registered = check_argparse_subcommands_in_contract()
    if args.binary is None:
        print(f"Command contract source check passed: {len(registered)} argparse subcommands are in the contract")
        return

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
    print(f"Command contract passed: {len(commands)} Python and {len(native_commands)} native commands, shared globals and {len(entries)} entries; {len(registered)} argparse subcommands covered")


if __name__ == "__main__":
    main()
