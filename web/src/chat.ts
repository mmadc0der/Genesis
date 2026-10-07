import { shortCause, type RunRow } from "./live";

// ChatSession is what control condenses from the run journals of one session:
// every run that shares the DSH session, oldest first. See docs/control.md.

export interface ChatItem {
  seq: string;
  at: string;
  role: "assistant" | "tool" | "system" | "user";
  kind: "text" | "reasoning" | "call" | "result" | "error";
  name?: string;
  call_id?: string;
  text?: string;
  failed?: boolean;
  truncated?: boolean;
}

export interface ChatTrigger {
  type?: string;
  source?: string;
  message?: string;
}

export interface ChatRun {
  run: RunRow;
  trigger: ChatTrigger;
  items: ChatItem[];
  retries?: number;
}

export interface ChatSession {
  run_id: string;
  session_id?: string;
  agent: string;
  runs: ChatRun[];
  continue_run?: string;
  continue_blocked?: string;
}

export type Entry =
  | { kind: "user"; id: string; at: string; who: string; mine: boolean; text: string; note: string }
  | { kind: "agent"; id: string; at: string; text: string; truncated: boolean }
  | { kind: "thought"; id: string; at: string; text: string }
  | {
      kind: "tool";
      id: string;
      at: string;
      name: string;
      summary: string;
      args: string;
      result?: string;
      failed: boolean;
      truncated: boolean;
    }
  | { kind: "error"; id: string; at: string; text: string }
  | { kind: "status"; id: string; at: string; text: string; failed: boolean };

const OPERATOR = "urn:genesis:control";
const AGENT_PREFIX = "urn:genesis:agent:";

// sourceLabel names who sent the event that started a run.
export function sourceLabel(source: string | undefined) {
  if (!source) return "event";
  if (source === OPERATOR) return "You";
  if (source.startsWith(AGENT_PREFIX)) return source.slice(AGENT_PREFIX.length);
  return source.replace(/^urn:genesis:/, "");
}

function parseArguments(args: string | undefined): Record<string, unknown> | null {
  if (!args) return null;
  try {
    const value: unknown = JSON.parse(args);
    return value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : null;
  } catch {
    return null;
  }
}

// toolSummary is the one line shown for a tool call. A shell call is its
// command; any other call is its arguments squeezed onto a line.
export function toolSummary(args: string | undefined) {
  const parsed = parseArguments(args);
  const command = parsed && typeof parsed.command === "string" ? parsed.command : null;
  const text = (command ?? args ?? "").replace(/\s+/g, " ").trim();
  return text.length > 140 ? `${text.slice(0, 139)}…` : text;
}

// toolArguments is the call as the reader opens it: a command as written, other
// arguments pretty printed.
export function toolArguments(args: string | undefined) {
  const parsed = parseArguments(args);
  if (parsed && typeof parsed.command === "string" && Object.keys(parsed).length === 1) return parsed.command;
  if (parsed) return JSON.stringify(parsed, null, 2);
  return args ?? "";
}

function formatTokens(total: number) {
  return new Intl.NumberFormat("en", { maximumFractionDigits: 0 }).format(total);
}

function statusText(run: ChatRun) {
  const parts: string[] = [run.run.state === "completed" ? "Run completed" : `Run ${run.run.state}`];
  const total = run.run.usage?.total ?? 0;
  if (total > 0) parts.push(`${formatTokens(total)} tokens`);
  if (run.retries) parts.push(`${run.retries} ${run.retries === 1 ? "retry" : "retries"}`);
  return parts.join(" · ");
}

// buildThread turns a condensed session into the lines a chat shows: what
// started each run, what the agent said and thought, each tool call joined
// with its result, and how the run ended.
export function buildThread(session: ChatSession): Entry[] {
  const entries: Entry[] = [];
  for (const run of session.runs ?? []) {
    const runId = run.run?.run_id ?? "";
    const message = run.trigger?.message?.trim() ?? "";
    entries.push({
      kind: "user",
      id: `${runId}:start`,
      at: run.run?.accepted_at ?? "",
      who: sourceLabel(run.trigger?.source),
      mine: run.trigger?.source === OPERATOR,
      text: message,
      note: message ? "" : shortCause(run.trigger?.type),
    });
    const calls = new Map<string, Extract<Entry, { kind: "tool" }>>();
    (run.items ?? []).forEach((item, index) => {
      const id = `${runId}:${item.seq}:${index}`;
      if (item.kind === "text") {
        entries.push({ kind: "agent", id, at: item.at, text: item.text ?? "", truncated: Boolean(item.truncated) });
      } else if (item.kind === "reasoning") {
        entries.push({ kind: "thought", id, at: item.at, text: item.text ?? "" });
      } else if (item.kind === "call") {
        const entry: Extract<Entry, { kind: "tool" }> = {
          kind: "tool",
          id,
          at: item.at,
          name: item.name ?? "tool",
          summary: toolSummary(item.text),
          args: toolArguments(item.text),
          failed: false,
          truncated: Boolean(item.truncated),
        };
        if (item.call_id) calls.set(item.call_id, entry);
        entries.push(entry);
      } else if (item.kind === "result") {
        const call = item.call_id ? calls.get(item.call_id) : undefined;
        if (call) {
          call.result = item.text ?? "";
          call.failed = Boolean(item.failed);
          call.truncated = call.truncated || Boolean(item.truncated);
        } else {
          entries.push({
            kind: "tool",
            id,
            at: item.at,
            name: "tool",
            summary: "",
            args: "",
            result: item.text ?? "",
            failed: Boolean(item.failed),
            truncated: Boolean(item.truncated),
          });
        }
      } else if (item.kind === "error") {
        entries.push({ kind: "error", id, at: item.at, text: item.text ?? item.name ?? "error" });
      }
    });
    if (run.run?.state && run.run.state !== "open") {
      entries.push({
        kind: "status",
        id: `${runId}:end`,
        at: run.run.ended_at ?? "",
        text: statusText(run),
        failed: run.run.state !== "completed",
      });
    }
  }
  return entries;
}

// isWorking is true while any run of the session is still open.
export function isWorking(session: ChatSession) {
  return (session.runs ?? []).some((run) => run.run?.state === "open");
}

export type Block = { kind: "text"; text: string } | { kind: "code"; text: string };

// textBlocks splits a message on ``` fences so code is set apart. An unclosed
// fence runs to the end, which is what a half-written reply looks like.
export function textBlocks(text: string): Block[] {
  const blocks: Block[] = [];
  const lines = text.split("\n");
  let current: string[] = [];
  let inCode = false;
  const flush = () => {
    const joined = current.join("\n").replace(/^\n+|\n+$/g, "");
    if (joined !== "") blocks.push({ kind: inCode ? "code" : "text", text: joined });
    current = [];
  };
  for (const line of lines) {
    if (line.trimStart().startsWith("```")) {
      flush();
      inCode = !inCode;
      continue;
    }
    current.push(line);
  }
  flush();
  return blocks;
}

export type Inline = { kind: "text" | "code" | "bold"; text: string };

// inlineParts finds `code` and **bold** inside one block of text.
export function inlineParts(text: string): Inline[] {
  const parts: Inline[] = [];
  const pattern = /`([^`\n]+)`|\*\*([^*\n]+)\*\*/g;
  let last = 0;
  for (const match of text.matchAll(pattern)) {
    const at = match.index ?? 0;
    if (at > last) parts.push({ kind: "text", text: text.slice(last, at) });
    if (match[1] !== undefined) parts.push({ kind: "code", text: match[1] });
    else parts.push({ kind: "bold", text: match[2] });
    last = at + match[0].length;
  }
  if (last < text.length) parts.push({ kind: "text", text: text.slice(last) });
  return parts;
}

async function readError(response: Response) {
  const text = (await response.text().catch(() => "")).trim();
  return text ? text.slice(0, 300) : `Control answered ${response.status}.`;
}

export async function loadChat(runId: string): Promise<ChatSession> {
  const response = await fetch(`/api/runs/${encodeURIComponent(runId)}/chat`);
  if (!response.ok) throw new Error(await readError(response));
  return (await response.json()) as ChatSession;
}

export interface FollowUpResult {
  ok: boolean;
  message: string;
  runId?: string;
}

// sendFollowUp continues the session: control posts a session.continue event
// and the listener starts a new run on the same DSH session.
export async function sendFollowUp(runId: string, message: string): Promise<FollowUpResult> {
  let response: Response;
  try {
    response = await fetch(`/api/runs/${encodeURIComponent(runId)}/continue`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ message }),
    });
  } catch {
    return { ok: false, message: "Control did not answer." };
  }
  if (!response.ok) return { ok: false, message: await readError(response) };
  const body = (await response.json().catch(() => ({}))) as { run_id?: string };
  return { ok: true, message: "Sent.", runId: body.run_id };
}
