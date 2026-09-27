#!/usr/bin/env python3
"""Copy the stdlib-only engine into a relocatable release bundle."""
import shutil
import sys
from pathlib import Path

source = Path(__file__).resolve().parent.parent
output = Path(sys.argv[1]).resolve() / "lib" / "makewand" / "python"
version = sys.argv[2]
output.mkdir(parents=True, exist_ok=True)
shutil.copytree(source / "makewand", output / "makewand", dirs_exist_ok=True,
                ignore=shutil.ignore_patterns("__pycache__", "*.pyc", "*.pyo"))
(output / "bin").mkdir(exist_ok=True)
shutil.copy2(source / "bin" / "makewand", output / "bin" / "makewand")
(output / "makewand" / "VERSION").write_text(version + "\n", encoding="utf-8")
shutil.copy2(source / "LICENSE", output.parents[2] / "LICENSE")
(output.parents[2] / "INSTALL.txt").write_text(
    "Requires Python 3.9+ on PATH (python3 or python). Keep lib/ beside the Go "
    "executable; move or install the entire directory. No pip dependencies are "
    "required. Provider CLIs and project test tools are installed separately.\n",
    encoding="utf-8",
)
