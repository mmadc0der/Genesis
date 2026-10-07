#!/usr/bin/env python3
"""Per-type journal line size stats and data-field presence."""
from __future__ import annotations

import json
import os
import statistics
import sys
from collections import Counter, defaultdict

RUNS = sys.argv[1] if len(sys.argv) > 1 else "/var/lib/genesis/data/runs"


def main() -> None:
    stats: dict[str, dict] = defaultdict(
        lambda: {"count": 0, "bytes": [], "origins": Counter(), "data_keys": Counter()}
    )
    envelope_keys: Counter[str] = Counter()
    samples: dict[str, tuple[int, dict]] = {}
    chunk_sizes: list[int] = []

    for name in sorted(os.listdir(RUNS)):
        if not name.startswith("gen_"):
            continue
        path = os.path.join(RUNS, name, "events.jsonl")
        if not os.path.isfile(path):
            continue
        with open(path, "rb") as f:
            for raw in f:
                n = len(raw.rstrip(b"\n\r"))
                try:
                    e = json.loads(raw)
                except json.JSONDecodeError:
                    continue
                t = e.get("type") or "<missing>"
                s = stats[t]
                s["count"] += 1
                s["bytes"].append(n)
                s["origins"][e.get("origin") or ""] += 1
                for k in e:
                    if k != "data":
                        envelope_keys["envelope." + k] += 1
                data = e.get("data")
                if isinstance(data, str):
                    try:
                        data = json.loads(data)
                    except json.JSONDecodeError:
                        pass
                if isinstance(data, dict):
                    for k in data:
                        s["data_keys"][k] += 1
                    for k, v in data.items():
                        if isinstance(v, dict):
                            for sk in v:
                                s["data_keys"][f"{k}.{sk}"] += 1

                if t == "dev.genesis.run.chunk":
                    chunk_sizes.append(n)
                    if t not in samples and n < 2000:
                        samples[t] = (n, e)
                elif t not in samples:
                    samples[t] = (n, e)

    print("=== SIZE STATS (bytes per JSONL line, newline stripped) ===")
    rows = []
    for t, s in stats.items():
        b = s["bytes"]
        if not b:
            continue
        rows.append(
            (
                t,
                s["count"],
                sum(b) / len(b),
                statistics.median(b),
                min(b),
                max(b),
            )
        )
    rows.sort(key=lambda r: -r[1])
    for t, c, avg, med, mn, mx in rows:
        print(
            f"{t}\tcount={c}\tavg={avg:.1f}\tmedian={med:.0f}\tmin={mn}\tmax={mx}"
        )

    print("\n=== ORIGINS (top per type) ===")
    for t, c, *_ in rows:
        s = stats[t]
        top = s["origins"].most_common(3)
        print(f"{t}: {dict(top)}")

    print("\n=== DATA FIELD KEYS (per type, >=90% presence) ===")
    for t, c, *_ in rows:
        s = stats[t]
        keys = [
            (k, v)
            for k, v in s["data_keys"].most_common(40)
            if v >= c * 0.9
        ]
        if keys:
            parts = [f"{k}({v}/{c})" for k, v in keys]
            print(f"{t}: {', '.join(parts)}")

    print("\n=== CHUNK SIZE PERCENTILES ===")
    if chunk_sizes:
        chunk_sizes.sort()
        for p in (50, 90, 95, 99):
            i = min(int(len(chunk_sizes) * p / 100), len(chunk_sizes) - 1)
            print(f"p{p}={chunk_sizes[i]}")

    print("\n=== SAMPLE SHAPES ===")
    for t in sorted(samples):
        n, e = samples[t]
        slim = {k: e[k] for k in e if k != "data"}
        data = e.get("data")
        if isinstance(data, dict):
            data_summary = {}
            for k, v in list(data.items())[:15]:
                if isinstance(v, dict):
                    data_summary[k] = {sk: type(sv).__name__ for sk, sv in list(v.items())[:8]}
                elif isinstance(v, str) and len(v) > 60:
                    data_summary[k] = v[:60] + "..."
                else:
                    data_summary[k] = v
        else:
            data_summary = type(data).__name__
        print(f"\n--- {t} ({n} bytes) ---")
        print(json.dumps({"envelope": slim, "data": data_summary}, indent=2)[:3000])


def extras() -> None:
    """Optional-field breakdown for a few high-volume types."""
    from collections import Counter

    def scan(event_type: str, visit) -> tuple[int, Counter]:
        c: Counter = Counter()
        n = 0
        for name in os.listdir(RUNS):
            if not name.startswith("gen_"):
                continue
            path = os.path.join(RUNS, name, "events.jsonl")
            if not os.path.isfile(path):
                continue
            with open(path, "rb") as f:
                for raw in f:
                    e = json.loads(raw)
                    if e.get("type") != event_type:
                        continue
                    n += 1
                    visit(e, c)
        return n, c

    n, c = scan(
        "dev.genesis.run.tool",
        lambda e, c: (
            c.update(["has_name"] if e["data"].get("name") else ["no_name"]),
            c.update(["has_tool_call_id"] if e["data"].get("tool_call_id") else ["no_tool_call_id"]),
            c.update([f"phase={e['data'].get('phase', '')}"]),
        ),
    )
    print("\n=== TOOL OPTIONAL FIELDS ===")
    print(n, dict(c))

    n, c = scan(
        "dev.genesis.run.assistant",
        lambda e, c: c.update([f"phase={e['data'].get('phase', '')}"]),
    )
    print("\n=== ASSISTANT PHASES ===")
    print(n, dict(c))

    n, c = scan(
        "dev.genesis.run.turn",
        lambda e, c: (
            c.update([f"phase={e['data'].get('phase', '')}"]),
            c.update(["turn_failure"] if e["data"].get("turn_failure") else ["ok"]),
        ),
    )
    print("\n=== TURN ===")
    print(n, dict(c))

    n, c = scan(
        "dev.genesis.run.accepted",
        lambda e, c: c.update(["has_message"] if e["data"].get("message") else ["no_message"]),
    )
    print("\n=== ACCEPTED MESSAGE ===")
    print(n, dict(c))

    n, c = scan(
        "dev.genesis.run.restart",
        lambda e, c: [c.update([k]) for k in e["data"]],
    )
    print("\n=== RESTART DATA KEYS (counts) ===")
    print(n, dict(c))

    n, c = scan(
        "dev.genesis.run.chunk",
        lambda e, c: (
            c.update([f"frame={e['data'].get('frame', '')}"]),
            c.update([f"chunk_type={e['data'].get('chunk_type', '(absent)')}"]),
        ),
    )
    print("\n=== CHUNK FRAME / CHUNK_TYPE ===")
    for k, v in c.most_common(15):
        print(f"  {k}: {v}")


if __name__ == "__main__":
    main()
    extras()
