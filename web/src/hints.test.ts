import { describe, expect, it } from "vitest";
import {
  agentDot,
  eventSummary,
  isDrifted,
  ruleDot,
  runDot,
  runningByAgent,
  runningByRule,
  tokenBreakdown,
} from "./hints";
import type { RuleRow, RunRow } from "./live";

function run(extra: Partial<RunRow>): RunRow {
  return { run_id: "gen_x", agent: "oracle", state: "completed", ...extra };
}

function rule(name: string, presence: string): RuleRow {
  return { name, agent: "oracle", match: {}, presence };
}

describe("isDrifted", () => {
  it("counts a file that differs or is gone, not an agent in step", () => {
    expect(isDrifted("draft")).toBe(true);
    expect(isDrifted("active_only")).toBe(true);
    expect(isDrifted("active")).toBe(false);
    expect(isDrifted("unknown")).toBe(false);
    expect(isDrifted(undefined)).toBe(false);
  });
});

describe("agentDot", () => {
  it("is empty and says idle for an agent in step with nothing running", () => {
    const dot = agentDot("active", 0);
    expect(dot.tone).toBeUndefined();
    expect(dot.label).toBe("Idle");
  });

  it("is blue while a run is going", () => {
    const dot = agentDot("active", 2);
    expect(dot.tone).toBe("blue");
    expect(dot.detail).toContain("2 runs");
  });

  it("is yellow for an edited or new agent, and says what to do", () => {
    const dot = agentDot("draft", 0);
    expect(dot.tone).toBe("amber");
    expect(dot.label).toBe("Not synced");
    expect(dot.detail).toContain("Sync");
  });

  it("is yellow for an agent whose file was removed", () => {
    expect(agentDot("active_only", 0)).toMatchObject({ tone: "amber", label: "Removed on disk" });
  });

  it("keeps blue for a running agent and still mentions the drift", () => {
    const dot = agentDot("draft", 1);
    expect(dot.tone).toBe("blue");
    expect(dot.detail.toLowerCase()).toContain("not synced");
  });

  it("does not guess when the listener cannot be read", () => {
    const dot = agentDot("unknown", 0);
    expect(dot.tone).toBeUndefined();
    expect(dot.label).toBe("Unknown");
  });
});

describe("invalid config", () => {
  const error = 'agents "a" and "b" share OS user "a"';

  it("says the config is invalid instead of calling every file removed", () => {
    const agent = agentDot("active_only", 0, error);
    expect(agent).toMatchObject({ tone: "amber", label: "Config invalid" });
    expect(agent.detail).toContain(error);
    expect(ruleDot("active_only", 0, 2, error).label).toBe("Config invalid");
  });

  it("leaves rows that are not drifted alone", () => {
    expect(agentDot("active", 0, error).label).toBe("Idle");
  });
});

describe("ruleDot", () => {
  it("is yellow when the rule file is new or changed and reports its history", () => {
    const dot = ruleDot("draft", 0, 3);
    expect(dot.tone).toBe("amber");
    expect(dot.detail).toContain("3 runs so far");
  });

  it("is blue while a run it started is going", () => {
    expect(ruleDot("active", 1, 4)).toMatchObject({ tone: "blue", label: "Triggering" });
  });

  it("is empty for a quiet active rule and says no run started when none did", () => {
    const dot = ruleDot("active", 0, 0);
    expect(dot.tone).toBeUndefined();
    expect(dot.detail).toContain("No run started yet");
  });
});

describe("runDot", () => {
  it("names each run state", () => {
    expect(runDot(run({ state: "failed" })).tone).toBe("red");
    expect(runDot(run({ state: "open" })).tone).toBe("blue");
    expect(runDot(run({ state: "completed" })).tone).toBe("green");
    expect(runDot(run({ cause_type: "dev.genesis.session.continue" })).tone).toBe("violet");
    expect(runDot(run({ state: "weird" })).tone).toBe("amber");
  });
});

describe("running counts", () => {
  const runs = [
    run({ agent: "a", state: "open", rule: "r1.yaml" }),
    run({ agent: "a", state: "open", rule: "r1.yaml" }),
    run({ agent: "b", state: "completed", rule: "r2.yaml" }),
    run({ agent: "c", state: "open" }),
  ];

  it("counts open runs by agent and by rule", () => {
    expect(runningByAgent(runs).get("a")).toBe(2);
    expect(runningByAgent(runs).get("b")).toBeUndefined();
    expect(runningByRule(runs).get("r1.yaml")).toBe(2);
    expect(runningByRule(runs).has("r2.yaml")).toBe(false);
  });
});

describe("tokenBreakdown", () => {
  it("splits total into hit, miss and output, and keeps reasoning as a note", () => {
    const breakdown = tokenBreakdown({ cache_hit: 600, cache_miss: 300, output: 100, reasoning: 40, total: 1000 });
    expect(breakdown.parts.map((part) => part.key)).toEqual(["cache_hit", "cache_miss", "output"]);
    expect(breakdown.parts.map((part) => part.share)).toEqual([0.6, 0.3, 0.1]);
    expect(breakdown.reasoning).toBe(40);
    expect(breakdown.total).toBe(1000);
  });

  it("does not divide by zero", () => {
    const breakdown = tokenBreakdown({ cache_hit: 0, cache_miss: 0, output: 0, reasoning: 0, total: 0 });
    expect(breakdown.parts.every((part) => part.share === 0)).toBe(true);
  });
});

describe("eventSummary", () => {
  const runs = [
    run({ state: "open", cause_type: "dev.genesis.user.message", rule: "a.yaml", last_seq: "10" }),
    run({ state: "completed", cause_type: "dev.genesis.agent.finished", rule: "b.yaml", last_seq: "30" }),
    run({ state: "completed", cause_type: "dev.genesis.agent.finished", rule: "b.yaml", last_seq: "5" }),
    run({ state: "failed", cause_type: "dev.genesis.agent.finished", rule: "b.yaml" }),
  ];
  const rules = [rule("a.yaml", "active"), rule("b.yaml", "active"), rule("c.yaml", "draft"), rule("d.yaml", "active")];

  it("counts the events that started runs, by cause and by outcome", () => {
    const summary = eventSummary(runs, rules);
    expect(summary.triggers).toBe(4);
    expect(summary.running).toBe(1);
    expect(summary.completed).toBe(2);
    expect(summary.failed).toBe(1);
    expect(summary.byCause[0]).toEqual({ cause: "dev.genesis.agent.finished", count: 3 });
    expect(summary.journalLines).toBe(45);
  });

  it("counts rules by sync state and by whether they ever started a run", () => {
    expect(eventSummary(runs, rules).rules).toEqual({ total: 4, active: 3, unsynced: 1, fired: 2 });
  });

  it("flags a list that may be cut short", () => {
    const many = Array.from({ length: 200 }, () => run({}));
    expect(eventSummary(many, []).capped).toBe(true);
    expect(eventSummary(runs, rules).capped).toBe(false);
  });
});
