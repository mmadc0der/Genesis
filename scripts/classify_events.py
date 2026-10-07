#!/usr/bin/env python3
"""Classify Genesis run journal events (events.jsonl)."""
from __future__ import annotations

import json
import os
import sys
from collections import Counter, defaultdict

RUNS_DIR = sys.argv[1] if len(sys.argv) > 1 else "/var/lib/genesis/data/runs"


def load_events():
    by_type = Counter()
    by_agent = Counter()
    by_origin = Counter()
    by_rule = Counter()
    by_run = []
    lifecycle = Counter()
    sdk_mapped = Counter()
    chunks = 0
    chunk_frames = Counter()
    total = 0
    bytes_total = 0
    per_run_type: dict[str, Counter] = defaultdict(Counter)

    for name in sorted(os.listdir(RUNS_DIR)):
        if not name.startswith("gen_"):
            continue
        path = os.path.join(RUNS_DIR, name, "events.jsonl")
        if not os.path.isfile(path):
            continue
        run_total = 0
        run_bytes = os.path.getsize(path)
        last_agent = ""
        last_rule = ""
        with open(path, "rb") as f:
            for raw in f:
                bytes_total += len(raw)
                line = raw.decode("utf-8", errors="replace").strip()
                if not line:
                    continue
                total += 1
                run_total += 1
                try:
                    e = json.loads(line)
                except json.JSONDecodeError:
                    by_type["<parse_error>"] += 1
                    continue
                t = e.get("type") or "<missing_type>"
                by_type[t] += 1
                per_run_type[name][t] += 1
                agent = e.get("agentid") or ""
                rule = e.get("rulefile") or ""
                if agent:
                    last_agent = agent
                    by_agent[agent] += 1
                if rule:
                    last_rule = rule
                    by_rule[rule] += 1
                origin = e.get("origin") or ""
                if origin:
                    by_origin[origin] += 1
                if t.startswith("dev.genesis.run."):
                    lifecycle[t.removeprefix("dev.genesis.run.")] += 1
                if t == "dev.genesis.run.chunk":
                    chunks += 1
                    if isinstance(data, dict):
                        frame = data.get("frame")
                        if frame:
                            chunk_frames[str(frame)] += 1
                        raw = data.get("raw")
                        if isinstance(raw, dict):
                            payload = raw.get("payload")
                            if isinstance(payload, dict):
                                ft = payload.get("type")
                                if ft:
                                    chunk_frames[f"payload.type={ft}"] += 1
                data = e.get("data")
                if isinstance(data, dict) and "raw" in data:
                    raw = data["raw"]
                    if isinstance(raw, dict):
                        method = raw.get("method") or ""
                        if method:
                            sdk_mapped[f"data.raw.method={method}"] += 1
                        ev = raw.get("payload")
                        if isinstance(ev, dict):
                            inner = ev.get("event")
                            if isinstance(inner, dict):
                                it = inner.get("type") or ""
                                if it:
                                    sdk_mapped[f"sdk.event.type={it}"] += 1
        by_run.append(
            {
                "run_id": name,
                "events": run_total,
                "bytes": run_bytes,
                "agent": last_agent,
                "rule": last_rule,
            }
        )

    return {
        "total": total,
        "bytes_total": bytes_total,
        "chunks": chunks,
        "chunk_frames": chunk_frames,
        "by_type": by_type,
        "by_agent": by_agent,
        "by_origin": by_origin,
        "by_rule": by_rule,
        "lifecycle": lifecycle,
        "sdk_mapped": sdk_mapped,
        "by_run": sorted(by_run, key=lambda r: -r["events"]),
        "per_run_type": per_run_type,
    }


def pct(n: int, total: int) -> str:
    if total == 0:
        return "0%"
    return f"{100.0 * n / total:.1f}%"


def main() -> None:
    s = load_events()
    total = s["total"]
    print(f"TOTAL_EVENTS={total}")
    print(f"TOTAL_BYTES={s['bytes_total']}")
    print(f"CHUNK_EVENTS={s['chunks']} ({pct(s['chunks'], total)} of all events)")
    print()
    print("=== Chunk payload types (top 15) ===")
    cf = Counter({k: v for k, v in s["chunk_frames"].items() if k.startswith("payload.type=")})
    for k, v in cf.most_common(15):
        print(f"  {k.removeprefix('payload.type='):25s} {v:8d}  {pct(v, s['chunks'])} of chunks")
    print()

    print("=== By lifecycle type (dev.genesis.run.*) ===")
    for k, v in s["lifecycle"].most_common():
        print(f"  {k:20s} {v:8d}  {pct(v, total)}")
    print()

    print("=== By full event type (top 25) ===")
    for k, v in s["by_type"].most_common(25):
        print(f"  {k:45s} {v:8d}  {pct(v, total)}")
    print()

    print("=== By agent ===")
    for k, v in s["by_agent"].most_common():
        print(f"  {k:20s} {v:8d}  {pct(v, total)}")
    print()

    print("=== By origin ===")
    for k, v in s["by_origin"].most_common():
        print(f"  {k:30s} {v:8d}  {pct(v, total)}")
    print()

    print("=== SDK inner event types (from mapped notifications, top 20) ===")
    inner = Counter({k: v for k, v in s["sdk_mapped"].items() if k.startswith("sdk.event.type=")})
    for k, v in inner.most_common(20):
        label = k.removeprefix("sdk.event.type=")
        print(f"  {label:30s} {v:8d}  {pct(v, total)}")
    print()

    print("=== Top runs by event count ===")
    for r in s["by_run"][:12]:
        print(
            f"  {r['run_id']}  {r['events']:7d}  {r['agent']:12s}  {r['rule'][:40]}"
        )
    print()

    # Bucket summary for "picture"
    buckets = Counter()
    for t, c in s["by_type"].items():
        if t == "dev.genesis.run.chunk":
            buckets["streaming chunks (on_chunk)"] += c
        elif t == "dev.genesis.run.assistant":
            buckets["assistant stream (journaled)"] += c
        elif t == "dev.genesis.run.tool":
            buckets["tool lifecycle"] += c
        elif t == "dev.genesis.run.turn":
            buckets["turn boundaries"] += c
        elif t == "dev.genesis.run.retry":
            buckets["LLM retries"] += c
        elif t in (
            "dev.genesis.run.accepted",
            "dev.genesis.run.start",
            "dev.genesis.run.session.created",
            "dev.genesis.run.result",
            "dev.genesis.run.end",
            "dev.genesis.run.restart",
            "dev.genesis.run.error",
        ):
            buckets["run shell (start/end/result/…)"] += c
        else:
            buckets["other / ingress"] += c
    print("=== Coarse buckets ===")
    for k, v in buckets.most_common():
        print(f"  {k:35s} {v:8d}  {pct(v, total)}")


if __name__ == "__main__":
    main()
