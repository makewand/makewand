"""Pre-registered assertions run outside the candidate process."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from acceptance import check

CASES = [("", "."), ("a//./b/../c", "a/c"), ("a\\b\\..\\c", "a/c"),
 ("a/..", "."), (".../x", ".../x"), ("../secret", ValueError),
 ("a/../../secret", ValueError), ("/etc/passwd", ValueError),
 ("\\server\\share", ValueError), ("C:relative", ValueError), ("D:/data", ValueError),
 ("name\0suffix", ValueError)]
check(sys.argv[1], "paths.py", "normalize_relative_path", CASES)
