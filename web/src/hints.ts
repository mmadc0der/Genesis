import { RUN_LIST_LIMIT, type RuleRow, type RunRow, type Usage } from "./live";

// Everything a hint says is computed here from what control returned, so the
// words can be tested and the components only draw them.

export type Tone = "blue" | "green" | "red" | "amber" | "violet";

export interface Dot {
  // tone is absent when the slot stays empty, as for an idle agent.
  tone?: Tone;
  label: string;
  detail: string;
}

const RESYNC = "Sync to apply it.";

// isDrifted is true when the file on disk and the active listener disagree.
export function isDrifted(presence: string | undefined) {
  return presence === "draft" || presence === "active_only";
}

// invalid is control's desired_error: the files on disk do not load, so every
// row reads as "removed" even though no file is gone. Say that instead.
function driftDot(presence: string | undefined, invalid?: string): Dot | null {
  if (invalid && isDrifted(presence)) {
    return {
      tone: "amber",
      label: "Config invalid",
      detail: `Control cannot load the files on disk: ${invalid}. Nothing can sync until that is fixed.`,
    };
  }
  if (presence === "draft") {
    return {
      tone: "amber",
      label: "Not synced",
      detail: `New or edited on disk. The listener still uses the version it loaded. ${RESYNC}`,
    };
  }
  if (presence === "active_only") {
    return {
      tone: "amber",
      label: "Removed on disk",
      detail: `The file is gone, but the listener still has it active. ${RESYNC}`,
    };
  }
  return null;
}

function runWord(count: number) {
  return count === 1 ? "1 run" : `${count} runs`;
}

export function agentDot(presence: string | undefined, running: number, invalid?: string): Dot {
  const drift = driftDot(presence, invalid);
  if (running > 0) {
    const detail = `${runWord(running)} in progress.`;
    return {
      tone: "blue",
      label: "Running",
      detail: drift ? `${detail} Also ${drift.label.toLowerCase()}: ${drift.detail}` : detail,
    };
  }
  if (drift) return drift;
  if (presence === "unknown") {
    return { label: "Unknown", detail: "Control cannot read the listener, so the sync state is not known." };
  }
  return { label: "Idle", detail: "Active in the listener and waiting for an event. No run in progress." };
}

export function ruleDot(presence: string | undefined, running: number, total: number, invalid?: string): Dot {
  const drift = driftDot(presence, invalid);
  const history = total > 0 ? ` Started ${runWord(total)} so far.` : " No run started yet.";
  if (running > 0) {
    const detail = `${running === 1 ? "A run it started is" : `${running} runs it started are`} still going.${history}`;
    return {
      tone: "blue",
      label: "Triggering",
      detail: drift ? `${detail} Also ${drift.label.toLowerCase()}: ${drift.detail}` : detail,
    };
  }
  if (drift) return { ...drift, detail: `${drift.detail}${history}` };
  if (presence === "unknown") {
    return { label: "Unknown", detail: "Control cannot read the listener, so the sync state is not known." };
  }
  return { label: "Active", detail: `Active in the listener. No run it started is in progress.${history}` };
}

export function runDot(run: RunRow): Dot {
  if (run.state === "failed") {
    return { tone: "red", label: "Failed", detail: "The run ended in an error." };
  }
  if (run.cause_type === "dev.genesis.session.continue") {
    return { tone: "violet", label: "Continued", detail: "Resumed an earlier session after a review." };
  }
  if (run.state === "open") return { tone: "blue", label: "Running", detail: "The run has not ended yet." };
  if (run.state === "completed") return { tone: "green", label: "Completed", detail: "The run ended normally." };
  return { tone: "amber", label: run.state || "Unknown", detail: "The run ended in a state this view does not name." };
}

export function runningByAgent(runs: RunRow[]) {
  const counts = new Map<string, number>();
  for (const run of runs) {
    if (run.state !== "open") continue;
    counts.set(run.agent, (counts.get(run.agent) ?? 0) + 1);
  }
  return counts;
}

export function runningByRule(runs: RunRow[]) {
  const counts = new Map<string, number>();
  for (const run of runs) {
    if (run.state !== "open" || !run.rule) continue;
    counts.set(run.rule, (counts.get(run.rule) ?? 0) + 1);
  }
  return counts;
}

export function runsByRule(runs: RunRow[]) {
  const counts = new Map<string, number>();
  for (const run of runs) {
    if (!run.rule) continue;
    counts.set(run.rule, (counts.get(run.rule) ?? 0) + 1);
  }
  return counts;
}

export interface TokenPart {
  key: "cache_hit" | "cache_miss" | "output";
  label: string;
  tone: Tone;
  value: number;
  share: number;
}

export interface TokenBreakdown {
  total: number;
  parts: TokenPart[];
  // reasoning is counted inside output, so it is a note, not a fourth slice.
  reasoning: number;
}

// total is cache hit + cache miss + output, so the three parts fill the bar.
export function tokenBreakdown(usage: Usage): TokenBreakdown {
  const sum = usage.cache_hit + usage.cache_miss + usage.output;
  const share = (value: number) => (sum > 0 ? value / sum : 0);
  return {
    total: usage.total,
    reasoning: usage.reasoning,
    parts: [
      { key: "cache_hit", label: "Cache hit", tone: "green", value: usage.cache_hit, share: share(usage.cache_hit) },
      { key: "cache_miss", label: "Cache miss", tone: "amber", value: usage.cache_miss, share: share(usage.cache_miss) },
      { key: "output", label: "Output", tone: "blue", value: usage.output, share: share(usage.output) },
    ],
  };
}

export interface EventSummary {
  // triggers is the number of logged events that started a run. Control only
  // records an event when a rule matched it, so every logged event is one.
  triggers: number;
  running: number;
  completed: number;
  failed: number;
  byCause: { cause: string; count: number }[];
  // journalLines is the sum of run journal lengths: the lifecycle events the
  // runs themselves logged.
  journalLines: number;
  rules: { total: number; active: number; unsynced: number; fired: number };
  // capped is true when the run list may be shorter than the full history.
  capped: boolean;
}

export function eventSummary(runs: RunRow[], rules: RuleRow[]): EventSummary {
  const causes = new Map<string, number>();
  let running = 0;
  let completed = 0;
  let failed = 0;
  let journalLines = 0;
  for (const run of runs) {
    const cause = run.cause_type || "event";
    causes.set(cause, (causes.get(cause) ?? 0) + 1);
    if (run.state === "open") running += 1;
    else if (run.state === "failed") failed += 1;
    else if (run.state === "completed") completed += 1;
    const lines = Number(run.last_seq);
    if (Number.isFinite(lines) && lines > 0) journalLines += lines;
  }
  const fired = runsByRule(runs);
  return {
    triggers: runs.length,
    running,
    completed,
    failed,
    byCause: [...causes.entries()]
      .map(([cause, count]) => ({ cause, count }))
      .sort((left, right) => right.count - left.count || left.cause.localeCompare(right.cause)),
    journalLines,
    rules: {
      total: rules.length,
      active: rules.filter((rule) => !isDrifted(rule.presence)).length,
      unsynced: rules.filter((rule) => isDrifted(rule.presence)).length,
      fired: rules.filter((rule) => (fired.get(rule.name) ?? 0) > 0).length,
    },
    capped: runs.length >= RUN_LIST_LIMIT,
  };
}
