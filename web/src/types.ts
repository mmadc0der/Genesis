export interface Agent {
  id: string;
  instructions: string;
  cwd: string;
  home: string;
  user?: string;
  setup?: {
    groups?: string[];
    workspace?: string;
  };
  env: Record<string, string>;
  secrets: string[];
  presence: Presence;
}

export interface Rule {
  name: string;
  agent: string;
  match: Record<string, string>;
  presence: Presence;
}

export type Presence = "active" | "draft" | "active_only" | "unknown";

export interface Generation {
  digest: string;
  agents: Array<Omit<Agent, "presence">>;
  rules: Array<Omit<Rule, "presence">>;
  sync_configured?: boolean;
  syncing?: boolean;
}

export interface ControlState {
  listener: {
    reachable: boolean;
    ok: boolean;
    sync_configured: boolean;
    syncing: boolean;
    error?: string;
  };
  active: Generation | null;
  desired: Generation | null;
  desired_error?: string;
  drift: string;
}

export interface RunSummary {
  run_id: string;
  agent: string;
  rule: string;
  state: string;
  accepted_at?: string;
  ended_at?: string;
  last_seq: string;
  session_id?: string;
  cause_id?: string;
  cause_type?: string;
  finish_reason?: string;
  error?: string;
}

export interface RunDetail extends RunSummary {
  event_count: number;
  result?: unknown;
  stderr_tail?: string;
}

export interface LifecycleEvent {
  id?: string;
  type: string;
  time?: string;
  sequence: string;
  runid?: string;
  agentid?: string;
  rulefile?: string;
  sessionid?: string;
  data?: Record<string, unknown> | null;
}

export interface EventsPage {
  run_id: string;
  events: LifecycleEvent[];
  cursor: string;
  has_more: boolean;
}

export interface LiveFrame {
  op: string;
  topic?: string;
  run_id?: string;
  cursor?: string;
  event?: LifecycleEvent;
  run?: RunSummary;
  state?: {
    drift: string;
    active_digest?: string;
    desired_digest?: string;
    listener_ok: boolean;
    syncing: boolean;
    sync_configured: boolean;
    desired_error?: string;
  };
  error?: string;
}

export interface LiveOp {
  op: "subscribe" | "unsubscribe";
  topic: "run" | "runs" | "state";
  run_id?: string;
  after?: string;
}
