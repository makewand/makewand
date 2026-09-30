#!/usr/bin/env python3
"""Offline gate for canonical request, outcome and metadata-only event fixtures."""
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))
from makewand.execution_contract import ExecutionRequest, ExecutionResult, STATUS_CODES  # noqa: E402
from makewand.telemetry import validate_event  # noqa: E402


def main():
    contract = json.loads((ROOT / "makewand/execution_contract.json").read_text(encoding="utf-8"))
    if contract.get("schema") != 1 or contract.get("status_codes") != STATUS_CODES:
        raise ValueError("execution outcome contract drift")
    for kind, decoder in (("request", ExecutionRequest.from_dict), ("result", ExecutionResult.from_dict)):
        fixture = contract["fixtures"][kind]
        if decoder(fixture).to_dict() != fixture:
            raise ValueError(f"{kind} execution contract drift")
    validate_event(contract["fixtures"]["event"])
    print("Execution contract passed: schema 1 request, result and safe event fixtures")


if __name__ == "__main__":
    main()
