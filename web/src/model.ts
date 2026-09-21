import type { LifecycleEvent, RunSummary } from "./types";

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
