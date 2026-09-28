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
  if (incoming.length === 0) return current;
  const tail = current.length === 0 ? 0 : Number(current[current.length - 1]?.sequence);
  let append = current.length > 0 && Number.isFinite(tail);
  if (append) {
    for (const event of incoming) {
      const seq = Number(event.sequence);
      if (!event.sequence || !Number.isFinite(seq) || seq <= tail) {
        append = false;
        break;
      }
    }
  }
  if (append) {
    if (incoming.length === 1) return current.concat(incoming);
    return current.concat([...incoming].sort((left, right) => Number(left.sequence) - Number(right.sequence)));
  }
  const bySeq = new Map<string, LifecycleEvent>();
  for (const event of current) bySeq.set(event.sequence, event);
  for (const event of incoming) {
    if (event.sequence) bySeq.set(event.sequence, event);
  }
  return [...bySeq.values()].sort((left, right) => Number(left.sequence) - Number(right.sequence));
}

export interface JournalPage {
  events: LifecycleEvent[];
  cursor: string;
  has_more: boolean;
}

/** Page the journal until it is caught up, so a replay is history rather than a fresh generation. */
export async function readJournal(
  fetchPage: (after: string) => Promise<JournalPage>,
  start = "0",
): Promise<{ events: LifecycleEvent[]; cursor: string }> {
  let events: LifecycleEvent[] = [];
  let after = start || "0";
  let cursor = after;
  for (let page = 0; page < 1000; page++) {
    const next = await fetchPage(after);
    if (next.events.length > 0) events = mergeEvents(events, next.events);
    if (next.cursor) cursor = next.cursor;
    if (!next.has_more || next.events.length === 0 || !next.cursor || next.cursor === after) {
      return { events, cursor };
    }
    after = next.cursor;
  }
  return { events, cursor };
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
  kind: "result" | "error" | "turn" | "tool" | "meta" | "assistant" | "retry";
  text: string;
  key?: string;
  time?: string;
  /** Tool-call row. Collapsed preview compacts arguments; `text` stays the mapped journal line. */
  toolCall?: boolean;
}

/** Collapsed activity rows longer than this end with an ellipsis. */
export const ACTIVITY_PREVIEW_LIMIT = 120;

export function activityPreview(line: Pick<ChatLine, "text" | "toolCall">): string {
  const source = line.toolCall ? compactToolCall(line.text) : line.text;
  return singleLine(source, ACTIVITY_PREVIEW_LIMIT);
}

function compactToolCall(text: string): string {
  const minified = tryMinify(text);
  if (minified !== null) return minified;
  const splitAt = text.indexOf("\n");
  if (splitAt === -1) return text;
  const name = text.slice(0, splitAt).trim();
  const args = text.slice(splitAt + 1);
  const compactArgs = tryMinify(args) ?? args;
  if (!name) return compactArgs;
  if (!compactArgs.trim()) return name;
  return `${name} ${compactArgs}`;
}

function tryMinify(raw: string): string | null {
  const trimmed = raw.trim();
  if (!trimmed.startsWith("{") && !trimmed.startsWith("[")) return null;
  try {
    const parsed = JSON.parse(trimmed) as unknown;
    if (parsed !== null && typeof parsed === "object") return JSON.stringify(parsed);
  } catch {
    return null;
  }
  return null;
}

function singleLine(text: string, limit: number): string {
  const flat = text.replace(/\s+/g, " ").trim();
  const chars = Array.from(flat);
  if (chars.length <= limit) return flat;
  return `${chars.slice(0, limit - 1).join("").trimEnd()}…`;
}

const PANEL_TEXT_CAP_BYTES = 16 * 1024;
const textEncoder = new TextEncoder();

export function eventLine(event: LifecycleEvent): ChatLine {
  const data = event.data ?? {};
  switch (event.type) {
    case "dev.genesis.run.result":
      return { kind: "result", text: stringField(data, "final_response") || "(empty response)" };
    case "dev.genesis.run.error":
      return { kind: "error", text: stringField(data, "message") || "error" };
    case "dev.genesis.run.turn":
      return turnLine(data);
    case "dev.genesis.run.tool":
      return toolLine(data);
    case "dev.genesis.run.assistant":
      return { kind: "assistant", text: assistantText(data) };
    case "dev.genesis.run.retry":
      return { kind: "retry", text: retryText(data) };
    case "dev.genesis.run.chunk":
      return { kind: "assistant", text: chunkText(data) };
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

interface OpenAssistant {
  line: ChatLine;
  reasoning: string;
  body: string;
  reasoningBytes: number;
  bodyBytes: number;
  settled: boolean;
  frozen: boolean;
}

interface OpenTool {
  line: ChatLine;
  name: string;
  args: string;
  argsBytes: number;
  settled: boolean;
  frozen: boolean;
}

export interface ActivityCursor {
  lines: ChatLine[];
  assistants: Map<string, OpenAssistant>;
  tools: Map<string, OpenTool>;
  latestAttempt: string;
  source: LifecycleEvent[];
}

export function activityLines(events: LifecycleEvent[]): ChatLine[] {
  return activityView(continueActivity(null, events));
}

export function activityView(cursor: ActivityCursor): ChatLine[] {
  return cursor.lines.filter((line) => line.text.trim() !== "");
}

export function continueActivity(previous: ActivityCursor | null, events: LifecycleEvent[]): ActivityCursor {
  if (previous && isEventPrefix(previous.source, events)) {
    if (previous.source.length === events.length) return previous;
    for (let index = previous.source.length; index < events.length; index++) absorbActivity(previous, events[index]);
    previous.source = events;
    return previous;
  }
  const cursor = emptyActivity();
  for (const event of events) absorbActivity(cursor, event);
  cursor.source = events;
  return cursor;
}

function emptyActivity(): ActivityCursor {
  return { lines: [], assistants: new Map(), tools: new Map(), latestAttempt: "", source: [] };
}

function isEventPrefix(previous: LifecycleEvent[], next: LifecycleEvent[]): boolean {
  if (previous.length > next.length) return false;
  for (let index = 0; index < previous.length; index++) {
    const prior = previous[index];
    const incoming = next[index];
    if (prior === incoming) continue;
    if (prior.sequence && prior.sequence === incoming.sequence) continue;
    return false;
  }
  return true;
}

function absorbActivity(cursor: ActivityCursor, event: LifecycleEvent): void {
  const data = event.data ?? {};
  if (event.type === "dev.genesis.run.chunk") {
    applyChunk(event, data, cursor.lines, cursor.assistants, cursor.tools, (attemptId) => {
      cursor.latestAttempt = attemptId;
    });
    return;
  }
  if (event.type === "dev.genesis.run.assistant") {
    const text = assistantText(data);
    const open = cursor.latestAttempt ? cursor.assistants.get(cursor.latestAttempt) : undefined;
    if (open && !open.settled && text) {
      open.line.text = text;
      open.line.time = event.time || open.line.time;
      open.settled = true;
      return;
    }
    if (text) cursor.lines.push({ ...eventLine(event), key: event.sequence, time: event.time, text });
    return;
  }
  if (event.type === "dev.genesis.run.tool" && stringField(data, "phase") === "call") {
    const name = toolName(data);
    const args = toolArguments(data);
    const open = findOpenTool(cursor.tools, name, args);
    if (open) {
      open.name = name || open.name;
      open.args = args || open.args;
      open.argsBytes = utf8ByteLength(open.args);
      open.line.text = capPanelText([open.name, open.args].filter(Boolean).join("\n"));
      open.line.time = event.time || open.line.time;
      open.settled = true;
      return;
    }
  }
  const line = eventLine(event);
  if (!line.text) return;
  cursor.lines.push({ ...line, key: event.sequence, time: event.time });
}

function applyChunk(
  event: LifecycleEvent,
  data: Record<string, unknown>,
  lines: ChatLine[],
  assistants: Map<string, OpenAssistant>,
  tools: Map<string, OpenTool>,
  rememberAttempt: (attemptId: string) => void,
): void {
  const frame = chunkFrame(data);
  if (!frame || frame.type !== "chunk") return;
  const attemptId = typeof frame.attemptId === "string" ? frame.attemptId : "";
  if (attemptId) rememberAttempt(attemptId);
  const chunk = asRecord(frame.chunk);
  if (!chunk) return;
  if (chunk.type === "text-delta" || chunk.type === "reasoning-delta") {
    const extra = typeof chunk.text === "string" ? chunk.text : "";
    if (!extra) return;
    let open = assistants.get(attemptId);
    if (open?.settled) return;
    if (!open) {
      open = {
        line: { key: `live-${attemptId || event.sequence}`, kind: "assistant", text: "", time: event.time },
        reasoning: "",
        body: "",
        reasoningBytes: 0,
        bodyBytes: 0,
        settled: false,
        frozen: false,
      };
      assistants.set(attemptId, open);
      lines.push(open.line);
    }
    if (open.frozen) return;
    const reasoning = chunk.type === "reasoning-delta";
    const appended = appendCapped(reasoning ? open.reasoning : open.body, reasoning ? open.reasoningBytes : open.bodyBytes, extra);
    if (reasoning) {
      open.reasoning = appended.text;
      open.reasoningBytes = appended.bytes;
    } else {
      open.body = appended.text;
      open.bodyBytes = appended.bytes;
    }
    open.frozen = appended.frozen;
    paintCapped(open.line, open.reasoning, open.reasoningBytes, open.body, open.bodyBytes, "\n\n");
    return;
  }
  if (chunk.type === "tool-call-delta") {
    const id = typeof chunk.id === "string" && chunk.id ? chunk.id : attemptId || event.sequence;
    const key = `${attemptId}:${id}`;
    let open = tools.get(key);
    if (open?.settled) return;
    if (!open) {
      open = {
        line: { key: `tool-${key}`, kind: "tool", text: "", time: event.time, toolCall: true },
        name: "",
        args: "",
        argsBytes: 0,
        settled: false,
        frozen: false,
      };
      tools.set(key, open);
      lines.push(open.line);
    }
    if (typeof chunk.name === "string" && chunk.name) open.name = chunk.name;
    if (open.frozen) {
      paintCapped(open.line, open.name, utf8ByteLength(open.name), open.args, open.argsBytes, "\n");
      return;
    }
    const delta = typeof chunk.argumentsDelta === "string" ? chunk.argumentsDelta : "";
    const appended = appendCapped(open.args, open.argsBytes, delta);
    open.args = appended.text;
    open.argsBytes = appended.bytes;
    open.frozen = appended.frozen;
    paintCapped(open.line, open.name, utf8ByteLength(open.name), open.args, open.argsBytes, "\n");
  }
}

function findOpenTool(tools: Map<string, OpenTool>, name: string, args: string): OpenTool | undefined {
  for (const open of tools.values()) {
    if (open.settled) continue;
    if (name && open.name === name) return open;
    if (open.args && args.startsWith(open.args)) return open;
  }
  return undefined;
}

function turnLine(data: Record<string, unknown>): ChatLine {
  const failure = turnFailure(data);
  if (!failure) return { kind: "turn", text: `Turn ${stringField(data, "phase") || "update"}` };
  const parts = [failure.message, failure.code, failure.status].filter(Boolean);
  return { kind: "error", text: parts.join("\n") || "Turn end" };
}

function toolLine(data: Record<string, unknown>): ChatLine {
  const name = toolName(data);
  const phase = stringField(data, "phase") || "update";
  if (phase === "call") {
    const args = toolArguments(data);
    const text = [name, args].filter(Boolean).join("\n");
    return { kind: "tool", text: args ? capPanelText(text) : text || "Tool call", toolCall: true };
  }
  if (phase === "result") {
    const result = toolResultText(data);
    const text = [name, result].filter(Boolean).join("\n");
    return { kind: "tool", text: result ? capPanelText(text) : text || "Tool result" };
  }
  return { kind: "tool", text: name ? `Tool ${phase} ${name}` : `Tool ${phase}` };
}

function assistantText(data: Record<string, unknown>): string {
  const pieces = assistantPieces(data);
  return capPanelText([pieces.reasoning, pieces.text].filter(Boolean).join("\n\n"));
}

function retryText(data: Record<string, unknown>): string {
  const body = sessionData(data);
  const failure = asRecord(body?.failure);
  const retry = body?.retry ?? data.retry;
  const count = typeof retry === "number" && Number.isFinite(retry) ? String(retry) : "";
  const code = typeof failure?.code === "string" ? failure.code : stringField(data, "code");
  const message = typeof failure?.message === "string" ? failure.message : stringField(data, "message");
  return ["retry", count, code, message].filter(Boolean).join(" ");
}

function chunkText(data: Record<string, unknown>): string {
  const frame = chunkFrame(data);
  const chunk = asRecord(frame?.chunk);
  if (!chunk) return "";
  if (chunk.type === "text-delta" || chunk.type === "reasoning-delta") {
    return typeof chunk.text === "string" ? capPanelText(chunk.text) : "";
  }
  if (chunk.type === "tool-call-delta") {
    const name = typeof chunk.name === "string" ? chunk.name : "";
    const args = typeof chunk.argumentsDelta === "string" ? chunk.argumentsDelta : "";
    return capPanelText([name, args].filter(Boolean).join("\n"));
  }
  return "";
}

function turnFailure(data: Record<string, unknown>): { message: string; code: string; status: string } | null {
  const reason = asRecord(sessionData(data)?.reason);
  if (reason?.kind === "error") {
    const error = asRecord(reason.error);
    if (error) {
      return {
        message: typeof error.message === "string" ? error.message : stringField(data, "error_message"),
        code: typeof error.code === "string" ? error.code : stringField(data, "error_code"),
        status: statusText(error.status ?? data.error_status),
      };
    }
  }
  if (data.turn_failure === true || stringField(data, "error_message") || stringField(data, "error_code")) {
    return {
      message: stringField(data, "error_message"),
      code: stringField(data, "error_code"),
      status: statusText(data.error_status),
    };
  }
  return null;
}

function assistantPieces(data: Record<string, unknown>): { reasoning: string; text: string } {
  const message = asRecord(sessionData(data)?.message);
  const content = Array.isArray(message?.content) ? message.content : [];
  const reasoning: string[] = [];
  const text: string[] = [];
  for (const block of content) {
    const record = asRecord(block);
    if (!record || typeof record.text !== "string") continue;
    if (record.type === "reasoning") reasoning.push(record.text);
    if (record.type === "text") text.push(record.text);
  }
  return { reasoning: reasoning.join(""), text: text.join("") };
}

function toolName(data: Record<string, unknown>): string {
  return stringField(data, "name") || stringField(sessionData(data) ?? {}, "name");
}

function toolArguments(data: Record<string, unknown>): string {
  const args = sessionData(data)?.arguments ?? data.arguments;
  if (typeof args === "string") return args;
  if (args && typeof args === "object") return JSON.stringify(args);
  return "";
}

function toolResultText(data: Record<string, unknown>): string {
  const parts: string[] = [];
  collectVisibleText(asRecord(sessionData(data)?.message)?.content, parts, 0);
  return parts.join("");
}

function collectVisibleText(value: unknown, parts: string[], depth: number): void {
  if (depth > 6 || value == null) return;
  if (typeof value === "string") {
    parts.push(value);
    return;
  }
  if (Array.isArray(value)) {
    for (const item of value) collectVisibleText(item, parts, depth + 1);
    return;
  }
  const record = asRecord(value);
  if (!record) return;
  if ((record.type === "text" || record.type === "reasoning") && typeof record.text === "string") {
    parts.push(record.text);
    return;
  }
  if (record.type === "tool-result" || "content" in record) collectVisibleText(record.content, parts, depth + 1);
}

function sessionData(data: Record<string, unknown>): Record<string, unknown> | null {
  const raw = asRecord(data.raw);
  const payload = asRecord(raw?.payload);
  return asRecord(asRecord(payload?.event)?.data);
}

function chunkFrame(data: Record<string, unknown>): Record<string, unknown> | null {
  const raw = asRecord(data.raw);
  const payload = asRecord(raw?.payload);
  if (payload && typeof payload.type === "string") return payload;
  return null;
}

function asRecord(value: unknown): Record<string, unknown> | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
  return value as Record<string, unknown>;
}

function statusText(value: unknown): string {
  if (typeof value === "number" && Number.isFinite(value)) return String(value);
  if (typeof value === "string" && value) return value;
  return "";
}

export function capPanelText(text: string): string {
  const encoded = textEncoder.encode(text);
  if (encoded.length <= PANEL_TEXT_CAP_BYTES) return text;
  let decoded = new TextDecoder("utf-8", { fatal: false }).decode(encoded.slice(0, PANEL_TEXT_CAP_BYTES));
  decoded = decoded.replace(/\uFFFD$/, "");
  return `${decoded}\n… truncated`;
}

function utf8ByteLength(text: string): number {
  return text ? textEncoder.encode(text).length : 0;
}

function paintCapped(line: ChatLine, left: string, leftBytes: number, right: string, rightBytes: number, separator: string): void {
  const parts = [left, right].filter(Boolean);
  let bytes = 0;
  if (left && right) bytes = leftBytes + rightBytes + utf8ByteLength(separator);
  else if (left) bytes = leftBytes;
  else bytes = rightBytes;
  const text = parts.join(separator);
  line.text = bytes <= PANEL_TEXT_CAP_BYTES ? text : capPanelText(text);
}

function appendCapped(current: string, currentBytes: number, extra: string): { text: string; bytes: number; frozen: boolean } {
  if (!extra) return { text: current, bytes: currentBytes, frozen: false };
  if (current.endsWith("\n… truncated") || currentBytes >= PANEL_TEXT_CAP_BYTES) {
    return { text: current, bytes: currentBytes, frozen: true };
  }
  const extraBytes = utf8ByteLength(extra);
  if (currentBytes + extraBytes <= PANEL_TEXT_CAP_BYTES) {
    return { text: current + extra, bytes: currentBytes + extraBytes, frozen: false };
  }
  const text = capPanelText(current + extra);
  return { text, bytes: utf8ByteLength(text), frozen: true };
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
