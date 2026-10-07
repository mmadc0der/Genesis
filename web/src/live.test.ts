import { describe, expect, it } from "vitest";
import { loopSpan, shortCause, sumUsage, type RunRow } from "./live";

const minute = 60_000;
const t0 = Date.parse("2026-10-05T10:00:00Z");

function run(partial: Partial<RunRow>): RunRow {
  return { run_id: "gen_x", agent: "lead", state: "completed", ...partial };
}

describe("shortCause", () => {
  it("drops the dev.genesis prefix and names a missing cause", () => {
    expect(shortCause("dev.genesis.session.continue")).toBe("session.continue");
    expect(shortCause(undefined)).toBe("event");
  });
});

describe("sumUsage", () => {
  it("adds fields and tolerates runs without usage", () => {
    const total = sumUsage([
      run({ usage: { cache_hit: 3, cache_miss: 2, output: 1, reasoning: 1, total: 7 } }),
      run({}),
      run({ usage: { cache_hit: 1, cache_miss: 0, output: 4, reasoning: 0, total: 5 } }),
    ]);
    expect(total).toEqual({ cache_hit: 4, cache_miss: 2, output: 5, reasoning: 1, total: 12 });
  });
});

describe("loopSpan", () => {
  it("is empty with no timed runs", () => {
    expect(loopSpan([], t0)).toEqual({ up: 0, down: 0 });
    expect(loopSpan([run({})], t0)).toEqual({ up: 0, down: 0 });
  });

  it("merges overlapping runs and counts the gap as down", () => {
    const runs = [
      run({ accepted_at: new Date(t0).toISOString(), ended_at: new Date(t0 + 10 * minute).toISOString() }),
      run({ accepted_at: new Date(t0 + 5 * minute).toISOString(), ended_at: new Date(t0 + 15 * minute).toISOString() }),
      run({ accepted_at: new Date(t0 + 30 * minute).toISOString(), ended_at: new Date(t0 + 40 * minute).toISOString() }),
    ];
    expect(loopSpan(runs, t0 + 60 * minute)).toEqual({ up: 25 * minute, down: 35 * minute });
  });

  it("counts an open run as up until now", () => {
    const runs = [run({ state: "open", accepted_at: new Date(t0).toISOString() })];
    expect(loopSpan(runs, t0 + 20 * minute)).toEqual({ up: 20 * minute, down: 0 });
  });
});
