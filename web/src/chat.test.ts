import { afterEach, describe, expect, it, vi } from "vitest";
import {
  buildThread,
  emitMessage,
  inlineParts,
  isWorking,
  sendFollowUp,
  sourceLabel,
  textBlocks,
  toolArguments,
  toolSummary,
  type ChatItem,
  type ChatRun,
  type ChatSession,
} from "./chat";

function item(partial: Partial<ChatItem> & Pick<ChatItem, "kind" | "role">): ChatItem {
  return { seq: "1", at: "2026-10-07T10:00:00Z", ...partial };
}

function run(id: string, state: string, items: ChatItem[], trigger: ChatRun["trigger"] = {}): ChatRun {
  return { run: { run_id: id, agent: "cpu-lead", state, accepted_at: "2026-10-07T10:00:00Z" }, trigger, items };
}

function session(runs: ChatRun[]): ChatSession {
  return { run_id: runs[0].run.run_id, agent: "cpu-lead", runs };
}

afterEach(() => vi.unstubAllGlobals());

describe("sourceLabel", () => {
  it("names the operator, an agent, or the raw source", () => {
    expect(sourceLabel("urn:genesis:control")).toBe("You");
    expect(sourceLabel("urn:genesis:agent:cpu-scout")).toBe("cpu-scout");
    expect(sourceLabel("urn:other:thing")).toBe("urn:other:thing");
    expect(sourceLabel(undefined)).toBe("event");
  });
});

describe("tool text", () => {
  it("shows a shell command as written and squeezes it onto one line", () => {
    const args = JSON.stringify({ command: "ls -la\necho   hi" });
    expect(toolSummary(args)).toBe("ls -la echo hi");
    expect(toolArguments(args)).toBe("ls -la\necho   hi");
  });

  it("pretty prints other arguments and survives bad JSON", () => {
    expect(toolArguments('{"path":"/a","mode":1}')).toBe('{\n  "path": "/a",\n  "mode": 1\n}');
    expect(toolSummary("not json")).toBe("not json");
    expect(toolArguments(undefined)).toBe("");
  });

  it("cuts a long summary", () => {
    expect(toolSummary(JSON.stringify({ command: "x".repeat(300) })).length).toBe(140);
  });
});

describe("buildThread", () => {
  it("opens a run with who started it and what they said", () => {
    const thread = buildThread(
      session([run("gen_a", "completed", [], { source: "urn:genesis:control", type: "dev.genesis.user.message", message: "Begin." })]),
    );
    expect(thread[0]).toMatchObject({ kind: "user", who: "You", mine: true, text: "Begin.", note: "" });
  });

  it("falls back to the event type when the run has no message", () => {
    const thread = buildThread(
      session([run("gen_a", "open", [], { source: "urn:genesis:agent:cpu-scout", type: "dev.genesis.agent.finished" })]),
    );
    expect(thread[0]).toMatchObject({ kind: "user", who: "cpu-scout", mine: false, text: "", note: "agent.finished" });
  });

  it("joins a tool call with its result and keeps the order of speech", () => {
    const thread = buildThread(
      session([
        run("gen_a", "completed", [
          item({ role: "assistant", kind: "reasoning", text: "plan" }),
          item({ seq: "2", role: "tool", kind: "call", name: "bash", call_id: "c1", text: JSON.stringify({ command: "ls" }) }),
          item({ seq: "3", role: "tool", kind: "result", call_id: "c1", text: "a\nb", failed: true }),
          item({ seq: "4", role: "assistant", kind: "text", text: "done" }),
        ]),
      ]),
    );
    expect(thread.map((entry) => entry.kind)).toEqual(["user", "thought", "tool", "agent", "status"]);
    expect(thread[2]).toMatchObject({ kind: "tool", name: "bash", summary: "ls", result: "a\nb", failed: true });
  });

  it("keeps a result whose call is missing, and shows errors", () => {
    const thread = buildThread(
      session([
        run("gen_a", "failed", [
          item({ role: "tool", kind: "result", call_id: "gone", text: "orphan" }),
          item({ seq: "2", role: "system", kind: "error", name: "Boom", text: "it broke" }),
        ]),
      ]),
    );
    expect(thread[1]).toMatchObject({ kind: "tool", name: "tool", result: "orphan" });
    expect(thread[2]).toMatchObject({ kind: "error", text: "it broke" });
    expect(thread[3]).toMatchObject({ kind: "status", failed: true, text: "Run failed" });
  });

  it("safely handles runs with null or missing items", () => {
    const rawRun = run("gen_a", "completed", []);
    // simulate JSON serialization where Items is null
    (rawRun as unknown as { items: null }).items = null;
    const thread = buildThread(session([rawRun]));
    expect(thread.length).toBe(2);
    expect(thread[0].kind).toBe("user");
    expect(thread[1].kind).toBe("status");
  });

  it("closes an ended run with tokens and retries, and leaves an open run open", () => {
    const ended = run("gen_a", "completed", []);
    ended.run.usage = { cache_hit: 0, cache_miss: 0, output: 0, reasoning: 0, total: 53588 };
    ended.retries = 2;
    const thread = buildThread(session([ended, run("gen_b", "open", [])]));
    expect(thread.filter((entry) => entry.kind === "status")).toEqual([
      expect.objectContaining({ text: "Run completed · 53,588 tokens · 2 retries", failed: false }),
    ]);
    expect(isWorking(session([ended, run("gen_b", "open", [])]))).toBe(true);
    expect(isWorking(session([ended]))).toBe(false);
  });
});

describe("text formatting", () => {
  it("sets fenced code apart and keeps an unclosed fence", () => {
    expect(textBlocks("before\n```sh\nls\n```\nafter")).toEqual([
      { kind: "text", text: "before" },
      { kind: "code", text: "ls" },
      { kind: "text", text: "after" },
    ]);
    expect(textBlocks("a\n```\nunfinished")).toEqual([
      { kind: "text", text: "a" },
      { kind: "code", text: "unfinished" },
    ]);
    expect(textBlocks("")).toEqual([]);
  });

  it("finds inline code and bold", () => {
    expect(inlineParts("run `ls` for **all** files")).toEqual([
      { kind: "text", text: "run " },
      { kind: "code", text: "ls" },
      { kind: "text", text: " for " },
      { kind: "bold", text: "all" },
      { kind: "text", text: " files" },
    ]);
    expect(inlineParts("plain")).toEqual([{ kind: "text", text: "plain" }]);
  });
});

describe("sendFollowUp", () => {
  it("posts the message to the run and returns the new run id", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ run_id: "gen_new" }), { status: 202 }));
    vi.stubGlobal("fetch", fetchMock);
    const result = await sendFollowUp("gen_a", "look again");
    expect(result).toEqual({ ok: true, message: "Sent.", runId: "gen_new" });
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/runs/gen_a/continue");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body)).toEqual({ message: "look again" });
  });

  it("reports control's reason when the follow-up is refused", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("The listener refused the follow-up.", { status: 409 })));
    expect(await sendFollowUp("gen_a", "x")).toEqual({ ok: false, message: "The listener refused the follow-up." });
  });

  it("reports an unreachable control", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("down")));
    expect(await sendFollowUp("gen_a", "x")).toEqual({ ok: false, message: "Control did not answer." });
  });
});

describe("emitMessage", () => {
  it("posts the message to /api/messages and returns the new run id", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ runs: [{ rule: "example.yaml", agent: "worker", run_id: "gen_created" }] }), {
        status: 202,
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const result = await emitMessage("example.yaml", "trigger run");
    expect(result).toEqual({ ok: true, message: "Event emitted.", runId: "gen_created" });
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/messages");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body)).toEqual({ rule: "example.yaml", message: "trigger run" });
  });

  it("handles 204 when no rule matched", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(null, { status: 204 })));
    const result = await emitMessage("example.yaml", "trigger run");
    expect(result).toEqual({ ok: false, message: "No agent run was started for this event." });
  });
});
