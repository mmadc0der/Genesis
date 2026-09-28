export interface Agent {
  id: string;
  instructions: string;
  cwd: string;
  home: string;
  user?: string;
  max_parallel?: number;
  setup?: {
    groups?: string[];
    workspace?: string;
  };
  env: Record<string, string>;
  secrets: string[];
  github?: GitHubGrant;
  presence: Presence;
}

export interface GitHubGrant {
  repository: string;
  git: string;
  permissions?: Record<string, string>;
  credential: string;
}

export interface Rule {
  name: string;
  agent: string;
  match: Record<string, string>;
  presence: Presence;
}

export interface Repository {
  id: string;
  provider: string;
  org: string;
  name: string;
  lifecycle: { remove: string; existing: string };
  settings: {
    visibility: string;
    description?: string;
    default_branch: string;
    features: { issues: boolean; wiki: boolean; projects: boolean };
    merge: {
      allow_squash: boolean;
      allow_merge_commit: boolean;
      allow_rebase: boolean;
      delete_branch_on_merge: boolean;
    };
  };
  bootstrap?: { template: string };
  actions: { enabled: boolean; allowed: string; selected?: string[] };
  secrets?: {
    repository?: string[];
    environments?: Array<{ name: string; secrets?: string[] }>;
  };
  protection?: {
    ruleset: {
      name: string;
      required_approving_reviews: number;
      dismiss_stale_reviews: boolean;
      required_checks?: string[];
      strict_checks: boolean;
    };
  };
  identities?: Array<{ name: string; role: string }>;
  presence: Presence;
  observation?: string;
  observed?: RepositoryObserved;
  drift?: RepositoryDrift[];
}

export interface RepositoryObserved {
  id: string;
  org: string;
  name: string;
  repository_id: number;
  node_id: string;
  observed_at?: string;
  visibility: string;
  description: string;
  default_branch: string;
  archived: boolean;
  features: { issues: boolean; wiki: boolean; projects: boolean };
  merge: {
    allow_squash: boolean;
    allow_merge_commit: boolean;
    allow_rebase: boolean;
    delete_branch_on_merge: boolean;
  };
  actions_status: string;
  actions_enabled?: boolean;
  actions_allowed?: string;
  ruleset_status: string;
  branch_status: string;
}

export interface RepositoryDrift {
  id: string;
  field: string;
  desired?: string;
  observed?: string;
  status: string;
  reason?: string;
}

export type Presence = "active" | "draft" | "active_only" | "unknown";

export interface Generation {
  digest: string;
  agents: Array<Omit<Agent, "presence">>;
  rules: Array<Omit<Rule, "presence">>;
  repositories?: Array<Omit<Repository, "presence">>;
  repositories_active?: boolean;
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
