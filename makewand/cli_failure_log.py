"""Bounded log of the CLI's own last lines when a dispatch ends UNKNOWN.

A provider CLI that exits nonzero without a catalogued message (quota, rate,
auth) is recorded as UNKNOWN, and nothing kept what it printed, so a missing
or stale cli_message_catalog.json pattern could not be found or fixed (Grok
Build's exhausted balance was recorded as UNKNOWN four times on 10-27/28).

Only stderr captured apart from the answer is read, and only the lines the
message catalog attributes to the CLI itself (health.cli_own_lines: Codex's
transcript sections with the prompt, reasoning, answer and tool output are
not), the last of them, shortened and with credential-like text redacted, in
an owner-only file next to the execution evidence directory that is cut back
to its newest half past a cap. Recording is best effort: it never waits for
the lock, never raises and never changes a result.
"""
import json
import os
import re
import time

from makewand import accounting_files, filelock

LOG_NAME = "unclassified-cli-failures.jsonl"
MAX_LINES = 20
MAX_LINE_CHARS = 300
MAX_BYTES = 256 * 1024

_SECRET_NAME = r"[A-Za-z0-9_.-]*(?:token|key|secret|password|passwd|cookie|authorization|credential)[A-Za-z0-9_.-]*"
_REDACTIONS = (
    # Named values and CLI flags, including quoted values containing spaces.
    (re.compile(r"(?i)(" + _SECRET_NAME
                + r""")(["']?(?:\s*[:=]\s*|\s+))(?:(?:bearer|basic|token)\s+)?(?:"(?:[^"\\\r\n]|\\.)*(?:"|\\?$)|'(?:[^'\\\r\n]|\\.)*(?:'|\\?$)|[^\s",;}]+)"""),
     r"\1\2[redacted]"),
    (re.compile(r"(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}"), r"\1 [redacted]"),
    (re.compile(r"(?i)\b([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@"), r"\1[redacted]@"),
    (re.compile(r"\b(?:sk|xai|ghp|gho|ghs|ghu|github_pat|glpat|hf|AIza|xox[abpr])[-_A-Za-z0-9]{12,}"), "[redacted]"),
    (re.compile(r"\bAKIA[0-9A-Z]{16}\b"), "[redacted]"),
    (re.compile(r"\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*"), "[redacted]"),
    (re.compile(r"[A-Za-z0-9+/=_-]{32,}"), "[redacted]"),
)


def log_path():
    from makewand import execution_evidence
    return execution_evidence.directory().parent / LOG_NAME


def redact(line):
    for pattern, replacement in _REDACTIONS:
        line = pattern.sub(replacement, line)
    return line


def tail_lines(engine, stderr):
    """The CLI's own last lines in `stderr`, or [] when stderr was merged with the answer."""
    if not stderr or getattr(stderr, "apart", True) is False:
        return []
    from makewand import health
    try:
        if not health.cli_catalog_entry(str(engine)):
            return []
        # run_subprocess already bounds the capture to 10 MiB. Keep its section
        # headers until classification, or a truncated transcript looks like
        # the CLI's own output (including marked lines in prompts/answers).
        own = list(health.cli_own_lines(str(engine), "", str(stderr)))
    except Exception:  # noqa: BLE001 - classification failure keeps nothing
        return []
    lines = [line.strip() for line in own if line.strip()][-MAX_LINES:]
    return [redact(line[:4 * MAX_LINE_CHARS])[:MAX_LINE_CHARS] for line in lines]


def record(engine, code, stderr):
    """Append one entry for an UNKNOWN exit; ignore every error."""
    try:
        lines = tail_lines(engine, stderr)
        if not lines:
            return
        entry = (json.dumps({"ts": time.time(), "engine": str(engine), "exit_code": code, "stderr_tail": lines},
                            ensure_ascii=False) + "\n").encode("utf-8")
        path = log_path()
        if path.is_symlink():
            return
        # Reuse the accounting opener's identity, owner, hardlink and Windows
        # DACL checks before writing diagnostics, including in an inherited
        # broadly-readable Windows cache directory.
        with accounting_files.pin(path) as files:
            descriptor = files.open(files.name, create=True, writable=True)
            try:
                try:
                    filelock.flock(descriptor, filelock.LOCK_EX | filelock.LOCK_NB)
                except OSError:
                    return  # another writer holds it: skip rather than wait
                try:
                    size = os.fstat(descriptor).st_size
                    os.lseek(descriptor, max(0, size - 1), os.SEEK_SET)
                    incomplete = size > 0 and os.read(descriptor, 1) != b"\n"
                    trim = size + len(entry) > MAX_BYTES
                    if trim or incomplete:
                        os.lseek(descriptor, max(0, size - MAX_BYTES), os.SEEK_SET)
                        kept = os.read(descriptor, min(size, MAX_BYTES))
                        if trim:
                            kept = kept[len(kept) // 2:]
                            kept = kept[kept.find(b"\n") + 1:] if b"\n" in kept else b""
                        # A partial last record must not swallow a new entry.
                        kept = kept[:kept.rfind(b"\n") + 1]
                        os.ftruncate(descriptor, 0)
                        os.lseek(descriptor, 0, os.SEEK_SET)
                        if kept:
                            os.write(descriptor, kept)
                    os.lseek(descriptor, 0, os.SEEK_END)
                    os.write(descriptor, entry)
                finally:
                    filelock.flock(descriptor, filelock.LOCK_UN)
            finally:
                os.close(descriptor)
    except Exception:  # noqa: BLE001 - diagnostics only, never part of a result
        return
