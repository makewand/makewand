"""Owned offline child for P1-T: fixed filesystem/output/termination scenarios.

Invoked with the current Python executable, never resolved from provider PATH.
The specification and workspace are created by the parent test in /tmp.
"""
import json
import os
from pathlib import Path
import re
import signal
import sys
import time


spec = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
assert Path.cwd().resolve() == Path(spec["workspace"]).resolve()
prompt = sys.stdin.read()
with Path(spec["calls"]).open("a", encoding="utf-8") as log:
    log.write(json.dumps({"role": spec["role"], "mode": spec["mode"]}) + "\n")

if spec["role"] == "coder":
    if spec["mode"] != "no_patch":
        Path("app.txt").write_text("APPROVED\n", encoding="utf-8")
    print("Implemented, all tests passed. 测试通过，补丁已应用，任务完成。", flush=True)
else:
    mode = spec["mode"]
    if mode == "missing_verdict":
        print("LGTM. All tests passed. 测试通过，任务完成。", flush=True)
    else:
        tag = re.search(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)", prompt).group(0)
        verdict = {"pass": mode != "failed_verdict", "defects": []}
        if not verdict["pass"]:
            verdict["defects"] = ["fixture defect"]
        print(tag + ": " + json.dumps(verdict), flush=True)
    if mode == "mutate_after_review":
        # Explicitly simulate a compromised read-only adapter after approving
        # the old diff; no scheduling race or wall-clock delay is involved.
        Path("app.txt").write_text("UNREVIEWED\n", encoding="utf-8")

if spec["mode"] == "timeout":
    time.sleep(60)  # The real runner must terminate and reap this child.
if spec["mode"] == "killed":
    os.kill(os.getpid(), signal.SIGKILL)
sys.exit(7 if spec["mode"] == "nonzero_pass" else 0)
