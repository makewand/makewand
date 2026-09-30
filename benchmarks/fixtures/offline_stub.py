#!/usr/bin/env python3
"""Write explicit offline solutions/failures; never invoke any provider.

These writers validate the independent acceptance protocol only. Their output
is not evidence of model quality, routing quality or subscription cost.
"""
import argparse
import json
import shutil
from pathlib import Path

ROOT = Path(__file__).resolve().parent


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--case", required=True)
    parser.add_argument("--workspace", type=Path, default=Path.cwd())
    parser.add_argument("--mode", choices=("correct", "wrong", "early-exit", "mutating", "timeout"), default="correct")
    args = parser.parse_args()
    if Path(args.case).name != args.case or args.case in (".", ".."):
        parser.error("case must be a registered fixture name")
    fixture = ROOT / args.case
    metadata = json.loads((fixture / "metadata.json").read_text(encoding="utf-8"))
    if args.mode == "correct":
        for source in sorted((fixture / "offline_solution").glob("*.py")):
            shutil.copy2(source, args.workspace / source.name)
    else:
        definitions = {}
        for module, function in metadata["public_apis"]:
            if args.mode == "wrong":
                body = f"def {function}(values):\n    return None\n"
            elif args.mode == "mutating":
                body = f"def {function}(values):\n    if not values: return []\n    values.clear()\n    raise ValueError('input was mutated')\n"
            elif args.mode == "early-exit":
                body = "raise SystemExit(0)\n"
            else:
                body = "import time\ntime.sleep(60)\n"
            definitions.setdefault(module, []).append(body)
        for module, parts in definitions.items():
            (args.workspace / module).write_text("\n".join(parts), encoding="utf-8")
    print(f"offline stub: {args.mode}, case={args.case}; not model evidence")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
