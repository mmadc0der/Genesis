#!/usr/bin/env python3
import json
import os
import sys

runs_dir = sys.argv[1] if len(sys.argv) > 1 else "/var/lib/genesis/data/runs"
for name in sorted(os.listdir(runs_dir)):
    if not name.startswith("gen_"):
        continue
    path = os.path.join(runs_dir, name, "events.jsonl")
    if not os.path.isfile(path):
        continue
    last = None
    with open(path) as f:
        for line in f:
            if line.strip():
                last = json.loads(line)
    if not last:
        continue
    data = last.get("data") or {}
    state = data.get("state", "")
    et = last.get("type", "")
    agent = last.get("agentid", "")
    print(f"{name}\t{agent}\t{et}\tstate={state}")
