import { applyUsagePush } from "./usageLive";

// One /api/live socket for the whole panel. The wire listens for run-list and
// state frames. An open session adds a run subscription on that same socket.

export type LivePublication = {
  seq: number;
  id: string;
  time: string;
  kind: string;
  headline: string;
  lede: string;
  body?: string;
  from: string;
  to?: string;
  run?: string;
  refs?: string[];
  supersedes?: string;
  superseded_by?: string;
};

export type LivePublicationPage = {
  publications: LivePublication[];
  next_before?: number;
  last_seq: number;
};

type LiveFrame = {
  op?: string;
  run_id?: string;
  run?: { run_id: string };
  event?: { type?: string; data?: unknown };
  publications?: LivePublicationPage;
  usage?: {
    cache_hit: number;
    cache_miss: number;
    output: number;
    reasoning: number;
    total: number;
    snapshot?: boolean;
    runs?: Record<string, { cache_hit: number; cache_miss: number; output: number; reasoning: number; total: number }>;
  };
  snapshot?: boolean;
  restart?: boolean;
};

type Listener = (frame: LiveFrame) => void;

const listeners = new Set<Listener>();
let socket: WebSocket | null = null;
let reconnect = 0;
let stopped = true;
let users = 0;
let watchedRun: string | null = null;

function send(body: object) {
  if (socket && socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify(body));
}

function subscribeOpen() {
  send({ op: "subscribe", topic: "runs" });
  send({ op: "subscribe", topic: "state" });
  send({ op: "subscribe", topic: "publications" });
  if (watchedRun) send({ op: "subscribe", topic: "run", run_id: watchedRun, after: "now" });
}

function connect() {
  if (stopped || users === 0) return;
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const next = new WebSocket(`${proto}://${location.host}/api/live`);
  socket = next;
  next.onopen = () => {
    if (socket === next) subscribeOpen();
  };
  next.onmessage = (event) => {
    let frame: LiveFrame;
    try {
      frame = JSON.parse(String(event.data));
    } catch {
      return;
    }
    if (frame.op === "usage" && frame.usage) applyUsagePush(frame.usage);
    listeners.forEach((listener) => listener(frame));
  };
  next.onclose = () => {
    if (socket === next) socket = null;
    if (!stopped && users > 0) reconnect = window.setTimeout(connect, 1000);
  };
}

export function retainLive() {
  users += 1;
  if (users === 1) {
    stopped = false;
    connect();
  }
  return () => {
    users -= 1;
    if (users > 0) return;
    stopped = true;
    window.clearTimeout(reconnect);
    socket?.close();
    socket = null;
  };
}

export function onLive(listener: Listener) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function watchRun(runId: string | null) {
  if (watchedRun && watchedRun !== runId) {
    send({ op: "unsubscribe", topic: "run", run_id: watchedRun });
  }
  watchedRun = runId;
  if (runId) send({ op: "subscribe", topic: "run", run_id: runId, after: "now" });
}
