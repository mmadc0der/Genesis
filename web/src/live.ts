export interface Usage {
  cache_hit: number;
  cache_miss: number;
  output: number;
  reasoning: number;
  total: number;
}

export interface RunRow {
  run_id: string;
  agent: string;
  state: string;
  accepted_at?: string;
  ended_at?: string;
  session_id?: string;
  cause_type?: string;
  // rule is the rules.d file that started the run. last_seq is the length of
  // the run journal, which control serves as a string.
  rule?: string;
  last_seq?: string;
  usage?: Usage;
}

// RUN_LIST_LIMIT is how many runs the Wire asks control for. Counts built from
// the list stop growing there.
export const RUN_LIST_LIMIT = 200;

export interface LoopSpan {
  up: number;
  down: number;
}

function parseTime(value: string | undefined) {
  if (!value) return Number.NaN;
  return Date.parse(value);
}

export function loopSpan(runs: RunRow[], now: number): LoopSpan {
  const intervals: { start: number; end: number }[] = [];
  for (const run of runs) {
    const start = parseTime(run.accepted_at);
    if (!Number.isFinite(start)) continue;
    const ended = parseTime(run.ended_at);
    const end = run.state === "open" ? now : Number.isFinite(ended) ? Math.max(ended, start) : start;
    intervals.push({ start, end });
  }
  if (intervals.length === 0) return { up: 0, down: 0 };
  intervals.sort((left, right) => left.start - right.start);
  const merged: { start: number; end: number }[] = [];
  for (const interval of intervals) {
    const last = merged[merged.length - 1];
    if (!last || interval.start > last.end) merged.push({ ...interval });
    else last.end = Math.max(last.end, interval.end);
  }
  const up = merged.reduce((sum, interval) => sum + (interval.end - interval.start), 0);
  const span = Math.max(0, now - merged[0].start);
  return { up, down: Math.max(0, span - up) };
}

export interface ControlState {
  drift: string;
  // desired_error is set when the files on disk do not load (drift is then
  // desired_invalid).
  desired_error?: string;
  listener: {
    reachable: boolean;
    ok: boolean;
    syncing: boolean;
    error?: string;
  };
}

export interface AgentRow {
  id: string;
  presence: string;
  instructions?: string;
  cwd?: string;
  home?: string;
  user?: string;
  secrets?: string[];
  reasoning_effort?: string;
}

export interface RuleRow {
  name: string;
  agent: string;
  // presence is active, draft, active_only, or unknown; see docs/control.md.
  presence?: string;
  match: Record<string, string>;
}

export interface Snapshot {
  runs: RunRow[];
  state: ControlState | null;
  agents: AgentRow[];
  rules: RuleRow[];
}

const emptyUsage: Usage = { cache_hit: 0, cache_miss: 0, output: 0, reasoning: 0, total: 0 };

export function sumUsage(runs: RunRow[]): Usage {
  return runs.reduce((total, run) => {
    const usage = run.usage ?? emptyUsage;
    return {
      cache_hit: total.cache_hit + (usage.cache_hit || 0),
      cache_miss: total.cache_miss + (usage.cache_miss || 0),
      output: total.output + (usage.output || 0),
      reasoning: total.reasoning + (usage.reasoning || 0),
      total: total.total + (usage.total || 0),
    };
  }, emptyUsage);
}

export function shortCause(cause: string | undefined) {
  if (!cause) return "event";
  return cause.replace(/^dev\.genesis\./, "");
}

async function getJSON<T>(path: string): Promise<T> {
  const response = await fetch(path);
  if (!response.ok) throw new Error(`${path} ${response.status}`);
  return response.json() as Promise<T>;
}

export async function loadSnapshot(): Promise<Snapshot> {
  const [runs, state, agents, rules] = await Promise.all([
    getJSON<{ runs: RunRow[] }>(`/api/runs?limit=${RUN_LIST_LIMIT}`),
    getJSON<ControlState>("/api/state"),
    getJSON<{ agents: AgentRow[] }>("/api/agents"),
    getJSON<{ rules: RuleRow[] }>("/api/rules"),
  ]);
  return {
    runs: runs.runs ?? [],
    state,
    agents: agents.agents ?? [],
    rules: rules.rules ?? [],
  };
}
