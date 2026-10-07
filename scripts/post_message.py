#!/usr/bin/env python3
"""POST a control-panel operator message (rule + message)."""
from __future__ import annotations

import json
import sys
import urllib.error
import urllib.request

BASE = "http://127.0.0.1:8790"


def main() -> int:
    if len(sys.argv) < 3:
        print("usage: post_message.py <rule.yaml> <message>", file=sys.stderr)
        return 2
    rule, message = sys.argv[1], sys.argv[2]
    body = json.dumps({"message": message, "rule": rule}).encode()
    req = urllib.request.Request(
        f"{BASE}/api/messages",
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            print(resp.status, resp.read().decode())
    except urllib.error.HTTPError as e:
        print(e.code, e.read().decode(), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
