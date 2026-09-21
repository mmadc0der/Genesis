import { describe, expect, it } from "vitest";
import { buildMessage, driftLabel, eventLine, maxCursor, mergeEvents, shortDigest } from "./model";
import type { LifecycleEvent } from "./types";

function event(sequence: string, type: string, data?: Record<string, unknown>): LifecycleEvent {
  return { sequence, type, data };
}

describe("control panel model", () => {
  it("shortens digests and names drift", () => {
    expect(shortDigest("sha256:0123456789abcdef")).toBe("0123456789ab");
    expect(shortDigest(undefined)).toBe("—");
    expect(driftLabel("in_sync")).toBe("Active");
    expect(driftLabel("draft")).toBe("Draft");
    expect(driftLabel("listener_unavailable")).toBe("Listener down");
  });

  it("merges journal events by sequence without duplicates", () => {
    const first = [event("1", "dev.genesis.run.accepted"), event("2", "dev.genesis.run.turn", { phase: "start" })];
    const merged = mergeEvents(first, [
      event("2", "dev.genesis.run.turn", { phase: "start" }),
      event("3", "dev.genesis.run.result", { final_response: "done" }),
    ]);
    expect(merged.map((item) => item.sequence)).toEqual(["1", "2", "3"]);
    expect(maxCursor(merged)).toBe("3");
  });

  it("renders settled results and does not invent token text from raw notifications", () => {
    expect(eventLine(event("4", "dev.genesis.run.result", { final_response: "bench is clean" })).text).toBe(
      "bench is clean",
    );
    const raw = eventLine(
      event("5", "dev.genesis.run.tool", {
        phase: "call",
        name: "read",
        raw: { method: "session.event", payload: { event: { type: "assistant/message", text: "hidden stream" } } },
      }),
    );
    expect(raw.text).toBe("Tool call read");
    expect(raw.text).not.toContain("hidden stream");
  });

  it("builds a message for a selected rule without a bearer token", () => {
    const body = buildMessage({
      message: "summarize the workspace",
      rule: "example.yaml",
      type: "dev.genesis.run",
      source: "urn:genesis:example",
    });
    expect(body).toEqual({
      message: "summarize the workspace",
      rule: "example.yaml",
      type: "dev.genesis.run",
      source: "urn:genesis:example",
    });
    expect(JSON.stringify(body)).not.toContain("Bearer");
    expect(JSON.stringify(body)).not.toContain("sync-token");
  });
});
