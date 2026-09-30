"""Trusted process entry point for one daemon request or a detached server.

Invoked by absolute path with Python -I, so the target repository and the
client's PYTHONPATH cannot substitute Makewand's worker implementation.
"""

import io
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))


def execute_request(request):
    from makewand.cli import main
    from makewand.execution_runtime import execution_context
    sys.argv = ["makewand"] + request["argv"]
    sys.stdin = io.StringIO(request.get("stdin", ""))
    with execution_context(task_id=request.get("request_id"),
                           deadline_unix_ms=request.get("deadline_unix_ms")):
        main()
    return 0


def main():
    if sys.argv[1:] == ["--serve"]:
        from makewand.daemon import start_daemon
        return start_daemon(foreground=True)
    from makewand.daemon_context import MAX_REQUEST_BYTES
    line = sys.stdin.buffer.readline(MAX_REQUEST_BYTES + 1)
    if len(line) > MAX_REQUEST_BYTES or not line.endswith(b"\n"):
        print("invalid worker request", file=sys.stderr)
        return 2
    request = json.loads(line)
    return execute_request(request)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        sys.exit(130)
