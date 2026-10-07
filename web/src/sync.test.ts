import { afterEach, describe, expect, it, vi } from "vitest";
import { driftItems, postSync } from "./sync";

describe("driftItems", () => {
  it("lists only what differs, agents before rules, in name order", () => {
    const items = driftItems(
      [
        { id: "zeta", presence: "draft" },
        { id: "alpha", presence: "active_only" },
        { id: "calm", presence: "active" },
      ],
      [
        { name: "b.yaml", agent: "alpha", match: {}, presence: "draft" },
        { name: "a.yaml", agent: "alpha", match: {}, presence: "active" },
      ],
    );
    expect(items).toEqual([
      { kind: "agent", name: "alpha", note: "removed on disk" },
      { kind: "agent", name: "zeta", note: "new or edited" },
      { kind: "rule", name: "b.yaml", note: "new or edited" },
    ]);
  });

  it("is empty when everything is in step or unknown", () => {
    expect(driftItems([{ id: "a", presence: "unknown" }], [])).toEqual([]);
  });
});

describe("postSync", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("posts an empty JSON object and reports success", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true });
    vi.stubGlobal("fetch", fetchMock);
    expect(await postSync()).toEqual({ ok: true, message: "Synced." });
    expect(fetchMock).toHaveBeenCalledWith("/api/sync", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
  });

  it("returns the listener's reason when it refuses", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ ok: false, status: 409, text: async () => " sync already running\n" }),
    );
    expect(await postSync()).toEqual({ ok: false, message: "sync already running" });
  });

  it("names the status when the body is empty", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 502, text: async () => "" }));
    expect((await postSync()).message).toBe("Sync failed (502).");
  });

  it("survives a network failure", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("down")));
    expect(await postSync()).toEqual({ ok: false, message: "Control did not answer." });
  });
});
