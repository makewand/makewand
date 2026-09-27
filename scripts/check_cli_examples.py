#!/usr/bin/env python3
"""Parse every public website Makewand example using the installed CLI.

--help prevents execution, provider calls, or server startup. Unknown options
are checked against the actual command help as argparse exits early on --help.
"""
import re
import shlex
import subprocess
import sys
from pathlib import Path

root = Path(__file__).resolve().parent.parent
binary = sys.argv[1]
count = 0
for page in (root / "site" / "docs.html", root / "site" / "index.html", root / "site" / "main.js"):
    for line in page.read_text(encoding="utf-8").splitlines():
        line = re.sub(r"<[^>]+>", "", line).strip().removeprefix("$ ")
        if "~/project$ " in line:
            line = line.split("~/project$ ", 1)[1]
        if not line.startswith("makewand "):
            continue
        args = shlex.split(line)
        result = subprocess.run([binary, *args[1:], "--help"], capture_output=True, text=True)
        if result.returncode:
            raise SystemExit(f"{page.name}: invalid example {line!r}: {result.stderr}")
        options = re.findall(r"(?<!\w)--[\w-]+", result.stdout)
        for arg in args[1:]:
            if arg.startswith("--") and arg.split("=", 1)[0] not in options:
                raise SystemExit(f"{page.name}: unsupported option {arg!r}: {line}")
        count += 1
if not count:
    raise SystemExit("No website CLI examples found")
print(f"Validated {count} website CLI examples")
