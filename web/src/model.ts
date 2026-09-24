import type { Agent, ControlState, LifecycleEvent, Repository, Rule, RunSummary } from "./types";

export interface Settled<T> {
  ok: boolean;
  value?: T;
  error?: string;
}

export interface PanelSnapshot {
  state: ControlState | null;
  agents: Agent[];
  rules: Rule[];
  repositories: Repository[];
  runs: RunSummary[];
  problems: string[];
}

export function applyPanelLoad(
  previous: PanelSnapshot,
  load: {
    state: Settled<ControlState>;
    agents: Settled<{ agents: Agent[] }>;
    rules: Settled<{ rules: Rule[] }>;
    repositories: Settled<{ repositories: Repository[]; desired_error?: string }>;
    runs: Settled<{ runs: RunSummary[] }>;
  },
): PanelSnapshot {
  const problems: string[] = [];
  const next: PanelSnapshot = {
    state: previous.state,
    agents: previous.agents,
    rules: previous.rules,
    repositories: previous.repositories,
    runs: previous.runs,
    problems,
  };
  if (load.state.ok && load.state.value) next.state = load.state.value;
  else problems.push(load.state.error || "State is unavailable");
  if (load.agents.ok && load.agents.value) next.agents = load.agents.value.agents;
  else problems.push(load.agents.error || "Agents are unavailable");
  if (load.rules.ok && load.rules.value) next.rules = load.rules.value.rules;
  else problems.push(load.rules.error || "Rules are unavailable");
  if (load.repositories.ok && load.repositories.value) next.repositories = load.repositories.value.repositories;
  else problems.push(load.repositories.error || "Repositories are unavailable");
  if (load.runs.ok && load.runs.value) next.runs = load.runs.value.runs;
  else problems.push(load.runs.error || "Runs are unavailable");
  return next;
}

export function repositoryLabel(repository: Pick<Repository, "org" | "name">): string {
  return `${repository.org}/${repository.name}`;
}

export function repositoryPolicy(repository: Pick<Repository, "lifecycle">): string {
  return `${repository.lifecycle.existing}, remove ${repository.lifecycle.remove}`;
}

export function repositorySyncNotice(status: number, body: string): string {
  if (status !== 200) return body.trim() || `Sync returned ${status}`;
  let parsed: {
    repository_plan?: { remote_mutation?: string; applied?: unknown; observation?: string };
    grant_plan?: {
      credential_active?: boolean;
      key_material?: string;
      remote_registration?: string;
      material?: Array<{ remote_status?: string; remote_key_id?: string }>;
    };
  };
  try {
    parsed = JSON.parse(body) as {
      repository_plan?: { remote_mutation?: string; applied?: unknown; observation?: string };
      grant_plan?: {
        credential_active?: boolean;
        key_material?: string;
        remote_registration?: string;
        material?: Array<{ remote_status?: string; remote_key_id?: string }>;
      };
    };
  } catch {
    return "Sync returned a response that is not JSON. The provider result was not confirmed.";
  }
  const applied = parsed.repository_plan?.applied;
  if (
    parsed.repository_plan?.remote_mutation === "applied" &&
    Array.isArray(applied) &&
    applied.length > 0 &&
    applied.every((item) => typeof item === "string" && /^[a-z_]+:[A-Za-z0-9][A-Za-z0-9._-]*$/.test(item))
  ) {
    return `Synced repositories. Applied GitHub changes were limited to ${applied.join(", ")}.`;
  }
  if (parsed.repository_plan?.remote_mutation === "none" && Array.isArray(applied) && applied.length === 0) {
    const base = parsed.repository_plan.observation === "observed"
      ? "Synced agents, rules, and repositories. GitHub was read. Repository drift was planned and was not applied."
      : "Synced agents, rules, and repositories. Repository plans were not applied to a provider.";
    const grant = parsed.grant_plan;
    if (!grant) return base;
    if (grant.credential_active) return "Sync completed, but a GitHub credential was reported active.";
    if (grant.key_material !== "pending" && grant.key_material !== "none") {
      return "Sync completed, but GitHub key material was not left pending.";
    }
    if (grant.remote_registration !== "unsupported" && grant.remote_registration !== "none") {
      return "Sync completed, but GitHub remote registration was not left unsupported.";
    }
    if (Array.isArray(grant.material) && grant.material.length > 0) {
      const registered = grant.material.some(
        (item) => item?.remote_status === "ready" && typeof item.remote_key_id === "string" && item.remote_key_id.length > 0,
      );
      if (registered) {
        return `${base} The SSH deploy key is registered. A dedicated run can receive its per-run SSH socket. Repository settings, rulesets, Actions, webhooks, and App tokens were not applied.`;
      }
      return `${base} Root holds local SSH material. The deploy key is not ready.`;
    }
    return `${base} GitHub credentials were not activated.`;
  }
  return "Sync completed, but the repository plan did not confirm that the provider was left untouched.";
}

export function githubGrantSummary(agent: Pick<Agent, "github">): string {
  const grant = agent.github;
  if (!grant) return "";
  const permissions = Object.entries(grant.permissions ?? {})
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([name, level]) => `${name} ${level}`)
    .join(", ");
  let credential = "unrecognized credential";
  if (grant.credential === "none") credential = "no credential";
  if (grant.credential === "pending") credential = "credential pending";
  const detail = permissions ? `, ${permissions}` : "";
  return `GitHub ${grant.repository}, git ${grant.git}, ${credential}${detail}. Not activated.`;
}

export function repositoryObservationSummary(repository: Pick<Repository, "provider" | "observation" | "observed" | "drift">): string {
  if (repository.observation === "observed" && repository.observed) {
    const drifting = (repository.drift ?? []).filter((item) => item.status === "drift").length;
    const fields = drifting === 1 ? "field" : "fields";
    return `Last observation of active ${repository.observed.org}/${repository.observed.name} (${repository.observed.repository_id}). ${drifting} drifting ${fields}.`;
  }
  return `Not observed. Genesis did not call ${repository.provider}.`;
}

export function driftLine(item: { field: string; status: string; desired?: string; observed?: string; reason?: string }): string {
  const change = item.desired || item.observed ? `${item.desired || "—"} → ${item.observed || "—"}` : "";
  return [item.field, item.status, change, item.reason].filter(Boolean).join(" · ");
}

export function secretNames(repository: Pick<Repository, "secrets">): string[] {
  const names = [...(repository.secrets?.repository ?? [])];
  for (const environment of repository.secrets?.environments ?? []) {
    for (const name of environment.secrets ?? []) names.push(`${environment.name}:${name}`);
  }
  return names;
}

export function shortDigest(digest: string | undefined): string {
  if (!digest) return "—";
  const hex = digest.startsWith("sha256:") ? digest.slice("sha256:".length) : digest;
  return hex.slice(0, 12);
}

export function driftLabel(drift: string | undefined): string {
  switch (drift) {
    case "in_sync":
      return "Active";
    case "draft":
      return "Draft";
    case "listener_unavailable":
      return "Listener down";
    case "desired_invalid":
      return "Config invalid";
    case "generation_too_large":
      return "Generation too large";
    default:
      return "Unknown";
  }
}

export function presenceLabel(presence: string): string {
  switch (presence) {
    case "active":
      return "active";
    case "draft":
      return "draft";
    case "active_only":
      return "cached";
    default:
      return "unknown";
  }
}

export function mergeEvents(current: LifecycleEvent[], incoming: LifecycleEvent[]): LifecycleEvent[] {
  const bySeq = new Map<string, LifecycleEvent>();
  for (const event of current) bySeq.set(event.sequence, event);
  for (const event of incoming) {
    if (event.sequence) bySeq.set(event.sequence, event);
  }
  return [...bySeq.values()].sort((left, right) => Number(left.sequence) - Number(right.sequence));
}

export function mergeRuns(current: RunSummary[], incoming: RunSummary): RunSummary[] {
  const next = current.filter((run) => run.run_id !== incoming.run_id);
  next.push(incoming);
  next.sort((left, right) => {
    const leftAt = left.accepted_at ?? "";
    const rightAt = right.accepted_at ?? "";
    if (leftAt !== rightAt) return rightAt < leftAt ? -1 : 1;
    return right.run_id < left.run_id ? -1 : 1;
  });
  return next;
}

export function maxCursor(events: LifecycleEvent[]): string {
  let max = 0;
  for (const event of events) {
    const value = Number(event.sequence);
    if (Number.isFinite(value) && value > max) max = value;
  }
  return String(max);
}

export interface ChatLine {
  kind: "result" | "error" | "turn" | "tool" | "meta";
  text: string;
}

export function eventLine(event: LifecycleEvent): ChatLine {
  const data = event.data ?? {};
  switch (event.type) {
    case "dev.genesis.run.result":
      return { kind: "result", text: stringField(data, "final_response") || "(empty response)" };
    case "dev.genesis.run.error":
      return { kind: "error", text: stringField(data, "message") || "error" };
    case "dev.genesis.run.turn":
      return { kind: "turn", text: `Turn ${stringField(data, "phase") || "update"}` };
    case "dev.genesis.run.tool": {
      const name = stringField(data, "name");
      const phase = stringField(data, "phase") || "update";
      return { kind: "tool", text: name ? `Tool ${phase} ${name}` : `Tool ${phase}` };
    }
    case "dev.genesis.run.accepted":
      return { kind: "meta", text: `Accepted ${stringField(data, "event_type") || "event"}` };
    case "dev.genesis.run.start":
      return { kind: "meta", text: `Process ${stringField(data, "pid") || "started"}` };
    case "dev.genesis.run.session.created":
      return { kind: "meta", text: `Session ${stringField(data, "session_id") || event.sessionid || ""}`.trim() };
    case "dev.genesis.run.end":
      return { kind: "meta", text: `Ended ${stringField(data, "state") || "unknown"}` };
    default:
      return { kind: "meta", text: event.type };
  }
}

export function buildMessage(input: {
  message: string;
  rule?: string;
  type?: string;
  source?: string;
  subject?: string;
}): Record<string, string> {
  const body: Record<string, string> = { message: input.message };
  if (input.rule) body.rule = input.rule;
  if (input.type) body.type = input.type;
  if (input.source) body.source = input.source;
  if (input.subject) body.subject = input.subject;
  return body;
}

function stringField(data: Record<string, unknown>, key: string): string {
  const value = data[key];
  return typeof value === "string" ? value : "";
}
