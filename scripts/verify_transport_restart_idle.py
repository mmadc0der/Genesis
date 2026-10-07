#!/usr/bin/env python3
"""Smoke-test transport restart when session.v3.jsonl ends on turn/end."""
from __future__ import annotations

import importlib.util
import json
import os
import sys
import time
from pathlib import Path

RUNNER = Path(os.environ.get("GENESIS_RUNNER", "/tmp/runner.py"))
DSH_HOME = os.environ["DSH_HOME"]
SESSION_ID = os.environ["SESSION_ID"]
CWD = os.environ.get("CWD", "/home/cpu-bench/workspace")
HOME = os.environ.get("HOME", "/home/cpu-bench")


def load_runner():
    spec = importlib.util.spec_from_file_location("genesis_runner", RUNNER)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {RUNNER}")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def main() -> int:
    runner = load_runner()
    invocation = {
        "rule": "verify-transport-restart",
        "agent": "verify",
        "run_id": "gen_verify_transport",
        "cwd": CWD,
        "home": HOME,
        "instructions": "Transport restart idle-turn verification.",
        "dsh_home": DSH_HOME,
        "session_id": SESSION_ID,
        "transport_restart": True,
        "env": {"LANG": "C.UTF-8", "PATH": "/usr/local/bin:/usr/bin:/bin"},
        "event": {
            "specversion": "1.0",
            "id": "evt_verify",
            "source": "urn:genesis:verify",
            "type": "dev.genesis.verify",
            "subject": "verify",
            "data": {},
        },
    }
    notifications = []

    def on_notification(n):
        notifications.append(getattr(n, "method", None))

    started = time.time()
    result = runner.execute(invocation, on_notification=on_notification)
    elapsed = time.time() - started
    print(json.dumps({"elapsed_s": round(elapsed, 2), "result": result, "notifications": len(notifications)}, indent=2))
    if result.get("finish_reason") != "completed":
        return 1
    if elapsed > 90:
        print("too slow", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
