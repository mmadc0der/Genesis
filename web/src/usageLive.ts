import { useSyncExternalStore } from "react";
import type { Usage } from "./live";

export interface UsagePush {
  cache_hit: number;
  cache_miss: number;
  output: number;
  reasoning: number;
  total: number;
  snapshot?: boolean;
  runs?: Record<string, Usage>;
}

const empty: Usage = { cache_hit: 0, cache_miss: 0, output: 0, reasoning: 0, total: 0 };

type UsageState = {
  global: Usage;
  runs: Record<string, Usage>;
};

let state: UsageState = { global: empty, runs: {} };
const listeners = new Set<() => void>();

function emit() {
  listeners.forEach((listener) => listener());
}

export function applyUsagePush(push: UsagePush) {
  const global: Usage = {
    cache_hit: push.cache_hit || 0,
    cache_miss: push.cache_miss || 0,
    output: push.output || 0,
    reasoning: push.reasoning || 0,
    total: push.total || 0,
  };
  const runs = push.snapshot ? {} : { ...state.runs };
  for (const [id, row] of Object.entries(push.runs ?? {})) {
    runs[id] = {
      cache_hit: row.cache_hit || 0,
      cache_miss: row.cache_miss || 0,
      output: row.output || 0,
      reasoning: row.reasoning || 0,
      total: row.total || 0,
    };
  }
  state = { global, runs };
  emit();
}

export function useUsageLive(): UsageState {
  return useSyncExternalStore(
    (listener) => {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    () => state,
  );
}

export function sessionTokens(runs: Record<string, Usage>, runIDs: string[], fallback: number): number {
  let seen = false;
  let total = 0;
  for (const id of runIDs) {
    const row = runs[id];
    if (!row) continue;
    seen = true;
    total += row.total || 0;
  }
  return seen ? total : fallback;
}
