import { useCallback, useEffect, useLayoutEffect, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import {
  buildThread,
  isWorking,
  liveToolCommand,
  loadChat,
  sendFollowUp,
  type ChatSession,
  type Entry,
} from "./chat";
import { sumUsage } from "./live";
import { sessionTokens, useUsageLive } from "./usageLive";
import { Markdown } from "./Markdown";
import { onLive, watchRun } from "./liveSocket";

const STICK_PX = 96;

function clock(value: string) {
  const time = Date.parse(value);
  if (!Number.isFinite(time)) return "";
  return new Date(time).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function CopyButton({ text, label = "Copy" }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  const copy = (e: React.MouseEvent) => {
    e.stopPropagation();
    e.preventDefault();
    if (!text) return;
    void navigator.clipboard.writeText(text).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    });
  };
  return (
    <button
      type="button"
      className={`tool-copy ${copied ? "copied" : ""}`}
      onClick={copy}
      title="Copy to clipboard"
      aria-label="Copy to clipboard"
    >
      {copied ? "Copied" : label}
    </button>
  );
}

function RunningTimer({ since }: { since: string }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);

  const startTime = Date.parse(since);
  const diffSec = Number.isFinite(startTime) ? Math.max(0, Math.floor((now - startTime) / 1000)) : 0;
  const mins = Math.floor(diffSec / 60);
  const secs = diffSec % 60;
  const formatted = mins > 0 ? `${mins}m ${secs}s` : `${secs}s`;

  return (
    <span className="live-timer">
      <i className="pulse-dot" /> running ({formatted})
    </span>
  );
}

function ExpandableLiveBlock({ children }: { children: React.ReactNode }) {
  const [open, setOpen] = useState(false);
  const [tall, setTall] = useState(false);
  const scrollRef = useRef<HTMLDivElement>(null);

  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const style = getComputedStyle(el);
    const line = parseFloat(style.lineHeight);
    const row = Number.isFinite(line) && line > 0 ? line : 22;
    const nextTall = el.scrollHeight > row * 4 + (tall ? -row : row);
    if (nextTall !== tall) setTall(nextTall);
    if (!open && el.scrollHeight > Number(el.dataset.seenHeight || 0)) {
      el.scrollTop = el.scrollHeight;
      el.dataset.seenHeight = String(el.scrollHeight);
    }
  });

  const toggle = () => {
    if (tall) setOpen((prev) => !prev);
  };

  return (
    <div
      className={`live-block-container ${tall ? (open ? "live-block-expanded" : "live-block-collapsed") : "live-block-fit"}`}
      onClick={toggle}
      role={tall ? "button" : undefined}
      tabIndex={tall ? 0 : undefined}
      onKeyDown={(e) => {
        if (!tall) return;
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          toggle();
        }
      }}
    >
      <div className="live-scroll-window" ref={scrollRef}>
        {children}
      </div>
      {tall ? (
        <div className="live-block-hint">
          <span>{open ? "▲ collapse" : "▼ click to expand"}</span>
        </div>
      ) : null}
    </div>
  );
}

function ExpandableEntry({
  children,
  defaultOpen = false,
  className = "",
}: {
  children: React.ReactNode;
  defaultOpen?: boolean;
  className?: string;
}) {
  const contentRef = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState(defaultOpen);
  const [tall, setTall] = useState(false);

  useEffect(() => {
    if (defaultOpen) setOpen(true);
  }, [defaultOpen]);

  useLayoutEffect(() => {
    const el = contentRef.current;
    if (!el) return;
    const measure = () => {
      const msg = el.querySelector(".msg");
      const target = msg ?? el;
      const style = getComputedStyle(target);
      const line = parseFloat(style.lineHeight);
      const row = Number.isFinite(line) && line > 0 ? line : 22;
      const meta = target.querySelector(".msg-meta");
      const metaH = meta instanceof HTMLElement ? meta.offsetHeight + 6 : 0;
      const pad = (parseFloat(style.paddingTop) || 0) + (parseFloat(style.paddingBottom) || 0);
      setTall(target.scrollHeight > metaH + pad + row * 4 + 1);
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [children]);

  const toggle = () => {
    if (tall) setOpen((prev) => !prev);
  };

  return (
    <div
      className={`entry-card ${tall ? (open ? "entry-open" : "entry-closed") : "entry-fit"} ${className}`}
      onClick={toggle}
      role={tall ? "button" : undefined}
      tabIndex={tall ? 0 : undefined}
      onKeyDown={(e) => {
        if (!tall) return;
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          toggle();
        }
      }}
    >
      <div className="entry-content" ref={contentRef}>{children}</div>
      {tall ? (
        <div className="entry-toggle-hint">
          <span>{open ? "▲ collapse" : "▼ click to expand"}</span>
        </div>
      ) : null}
    </div>
  );
}

function Line({ entry, working }: { entry: Entry; working: boolean }) {
  if (entry.kind === "user") {
    return (
      <ExpandableEntry defaultOpen={false}>
        <div className={entry.mine ? "msg msg-me" : "msg msg-event"}>
          <div className="msg-meta">
            <b>{entry.who}</b>
            <time>{clock(entry.at)}</time>
          </div>
          {entry.text ? <Markdown content={entry.text} /> : <p className="msg-note">Started by {entry.note}</p>}
        </div>
      </ExpandableEntry>
    );
  }
  if (entry.kind === "agent") {
    return (
      <ExpandableEntry defaultOpen={Boolean(entry.result)}>
        <div className="msg msg-agent">
          <div className="msg-meta">
            <b>{entry.result ? "result" : "output"}</b>
            <time>{clock(entry.at)}</time>
          </div>
          <Markdown content={entry.text} />
          {entry.truncated ? <p className="msg-note">Shortened.</p> : null}
        </div>
      </ExpandableEntry>
    );
  }
  if (entry.kind === "thought") {
    return (
      <details className="think">
        <summary>
          <span className="think-icon">▸</span>
          <span className="think-label">Thinking</span>
          {entry.at ? <time className="think-time">{clock(entry.at)}</time> : null}
        </summary>
        <div className="think-body">
          <Markdown content={entry.text} />
        </div>
      </details>
    );
  }
  if (entry.kind === "tool") {
    const pending = entry.result === undefined;
    const statusClass = pending ? (working ? "running" : "faint") : entry.failed ? "failed" : "done";
    return (
      <details className={entry.failed ? "tool failed" : "tool"}>
        <summary>
          <span className="tool-badge">{entry.name || "tool"}</span>
          <span className="tool-summary" title={entry.summary}>{entry.summary}</span>
          <em className={`tool-status ${statusClass}`}>
            {pending ? (
              working ? (
                <RunningTimer since={entry.at} />
              ) : (
                "no result"
              )
            ) : entry.failed ? (
              "failed"
            ) : (
              "done"
            )}
          </em>
        </summary>
        <div className="tool-body">
          {entry.args ? (
            <div className="tool-card">
              <div className="tool-card-head">
                <span>Input / Command</span>
                <CopyButton text={entry.args} />
              </div>
              <pre className="tool-args">{entry.args}</pre>
            </div>
          ) : null}
          {entry.result !== undefined ? (
            <div className="tool-card">
              <div className="tool-card-head">
                <span>Output</span>
                <CopyButton text={entry.result || ""} />
              </div>
              <pre className="tool-result">{entry.result || "(no output)"}</pre>
            </div>
          ) : null}
          {entry.truncated ? <p className="msg-note">Shortened.</p> : null}
        </div>
      </details>
    );
  }
  if (entry.kind === "error") {
    return <p className="msg-error">{entry.text}</p>;
  }
  return <p className={entry.failed ? "msg-status failed" : "msg-status"}>{entry.text}</p>;
}

// Chat is one session as a conversation: what started each run, what the agent
// said and did, and a box to send it a follow-up. The follow-up continues the
// same DSH session through control, so the agent keeps its context.
type LiveSeg =
  | { id: number; kind: "thought"; text: string; settled?: boolean }
  | { id: number; kind: "tool"; name: string; args: string; output?: string; settled?: boolean; since: string }
  | { id: number; kind: "text"; text: string; settled?: boolean };

type StreamChunk = {
  type?: string;
  text?: string;
  name?: string;
  argumentsDelta?: string;
  outputDelta?: string;
  block?: { type?: string; name?: string; arguments?: string };
};

// settleTools marks bash/tool cards whose token stream has closed. Execution
// can still be running; the card should leave the streaming accent then.
function settleTools(prev: LiveSeg[], patch?: { name?: string; args?: string; output?: string }): LiveSeg[] {
  let index = -1;
  for (let i = prev.length - 1; i >= 0; i--) {
    if (prev[i].kind === "tool") {
      index = i;
      break;
    }
  }
  if (index < 0) return prev;
  const seg = prev[index];
  if (seg.kind !== "tool") return prev;
  const next = prev.slice();
  next[index] = {
    ...seg,
    name: patch?.name || seg.name,
    args: patch?.args ? patch.args : seg.args,
    output: patch?.output !== undefined ? (seg.output || "") + patch.output : seg.output,
    settled: true,
  };
  return next;
}

function toolEventText(data: {
  arguments?: string;
  output?: string;
  raw?: {
    payload?: {
      event?: {
        data?: {
          arguments?: string;
          message?: { content?: { type?: string; text?: string; content?: { type?: string; text?: string }[] }[] };
        };
      };
    };
  };
}) {
  let args = typeof data.arguments === "string" ? data.arguments : "";
  let output = typeof data.output === "string" ? data.output : "";
  if (args === "" || output === "") {
    const eventData = data.raw?.payload?.event?.data;
    if (args === "" && typeof eventData?.arguments === "string") args = eventData.arguments;
    if (output === "") {
      for (const part of eventData?.message?.content ?? []) {
        if (part.type !== "tool-result") continue;
        for (const inner of part.content ?? []) {
          if (inner.type === "text" && inner.text) output += inner.text;
        }
      }
    }
  }
  return { args, output };
}

let liveSegID = 0;

function nextLiveSeg(seg: LiveSeg): LiveSeg[] {
  return [{ ...seg, id: ++liveSegID }];
}

// continueLive keeps appending to the latest segment when the same kind is
// still streaming. A different kind settles the previous segment so only the
// block that is still receiving tokens stays the collapsed live tail.
function settleAll(prev: LiveSeg[]): LiveSeg[] {
  if (prev.every((seg) => seg.settled)) return prev;
  return prev.map((seg) => (seg.settled ? seg : { ...seg, settled: true }));
}

function continueLive(prev: LiveSeg[], kind: LiveSeg["kind"], update: (last: LiveSeg) => LiveSeg, create: () => LiveSeg): LiveSeg[] {
  const last = prev[prev.length - 1];
  if (last && last.kind === kind) return [...prev.slice(0, -1), update(last)];
  return [...settleAll(prev), { ...create(), id: ++liveSegID }];
}

export function Chat({
  runId,
  onBack,
  onContinued,
}: {
  runId: string;
  onBack?: () => void;
  onContinued?: (runId: string) => void;
}) {
  const [session, setSession] = useState<ChatSession | null>(null);
  const [error, setError] = useState("");
  const [draft, setDraft] = useState("");
  const [sending, setSending] = useState(false);
  const [sendError, setSendError] = useState("");
  const [liveSegs, setLiveSegs] = useState<LiveSeg[]>([]);
  const scroller = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  const streamingActive = useRef(false);

  const refresh = useCallback(() => {
    return loadChat(runId)
      .then((next) => {
        setSession(next);
        setError("");
        if (!streamingActive.current && next.active_turn) {
          const turn = next.active_turn;
          const seeded: LiveSeg[] = [];
          if (turn.thought) seeded.push(...nextLiveSeg({ id: 0, kind: "thought", text: turn.thought }));
          if (turn.tool_name || turn.tool_args) {
            seeded.push(...nextLiveSeg({ id: 0, kind: "tool", name: turn.tool_name || "tool", args: turn.tool_args || "", since: new Date().toISOString() }));
          }
          if (turn.text) seeded.push(...nextLiveSeg({ id: 0, kind: "text", text: turn.text }));
          setLiveSegs(seeded);
        }
      })
      .catch((failure: unknown) => setError(failure instanceof Error ? failure.message : "Control did not answer."));
  }, [runId]);

  useEffect(() => {
    setSession(null);
    setError("");
    setSendError("");
    setLiveSegs([]);
    streamingActive.current = false;
    stick.current = true;
    let stopped = false;
    let busy = false;
    const tick = () => {
      if (busy || stopped) return;
      busy = true;
      refresh().finally(() => {
        busy = false;
      });
    };
    tick();
    watchRun(runId);
    const unlisten = onLive((frame) => {
      if (stopped || (frame.run_id && frame.run_id !== runId)) return;
      try {
        if (frame.op === "event" && frame.event) {
          const ev = frame.event;
          if (ev.type === "dev.genesis.run.chunk") {
            const body = (ev.data || {}) as {
              frame?: string;
              chunk_type?: string;
              phase?: string;
              name?: string;
              raw?: { payload?: { chunk?: StreamChunk } };
            };
            const raw: StreamChunk = body.raw?.payload?.chunk || {};
            const text = raw.text ?? "";
            const chunkType = body.chunk_type || raw.type;
            if (chunkType === "reasoning-delta" && text) {
              streamingActive.current = true;
              setLiveSegs((curr) =>
                continueLive(
                  curr,
                  "thought",
                  (last) => (last.kind === "thought" ? { ...last, text: last.text + text } : last),
                  () => ({ id: 0, kind: "thought", text }),
                ),
              );
            } else if (chunkType === "text-delta" && text) {
              streamingActive.current = true;
              setLiveSegs((curr) =>
                continueLive(
                  curr,
                  "text",
                  (last) => (last.kind === "text" ? { ...last, text: last.text + text } : last),
                  () => ({ id: 0, kind: "text", text }),
                ),
              );
            } else if (chunkType === "tool-call-delta") {
              streamingActive.current = true;
              setLiveSegs((curr) =>
                continueLive(
                  curr,
                  "tool",
                  (last) =>
                    last.kind === "tool"
                      ? { ...last, settled: false, name: raw.name || last.name, args: last.args + (raw.argumentsDelta || "") }
                      : last,
                  () => ({ id: 0, kind: "tool", name: raw.name || "tool", args: raw.argumentsDelta || "", since: new Date().toISOString() }),
                ),
              );
            } else if (chunkType === "block-end" && raw.block?.type === "tool-call") {
              // The command stream closed. Do not keep the blue streaming accent
              // until the shell process itself exits.
              streamingActive.current = true;
              setLiveSegs((curr) => settleTools(curr, { name: raw.block?.name, args: raw.block?.arguments }));
            } else if (chunkType === "block-end" && (raw.block?.type === "reasoning" || raw.block?.type === "text")) {
              const kind = raw.block.type === "reasoning" ? "thought" : "text";
              streamingActive.current = true;
              setLiveSegs((curr) => {
                const last = curr[curr.length - 1];
                if (!last || last.kind !== kind || last.settled) return curr;
                return [...curr.slice(0, -1), { ...last, settled: true }];
              });
            } else if ((chunkType === "tool-output-delta" || chunkType === "output-delta") && (text || raw.outputDelta)) {
              streamingActive.current = true;
              const piece = text || raw.outputDelta || "";
              setLiveSegs((curr) => settleTools(curr, { output: piece }));
            } else if (body.frame === "end" || chunkType === "finish") {
              streamingActive.current = true;
              setLiveSegs((curr) => settleAll(curr));
            }
          } else if (ev.type === "dev.genesis.run.tool") {
            const toolBody = (ev.data || {}) as Parameters<typeof toolEventText>[0] & {
              phase?: string;
              name?: string;
              arguments?: string;
              output?: string;
            };
            const parsed = toolEventText(toolBody);
            const args = toolBody.arguments ?? parsed.args;
            const output = toolBody.output ?? parsed.output;
            if (toolBody.phase === "call") {
              streamingActive.current = true;
              setLiveSegs((curr) => settleTools(curr, { name: toolBody.name, args }));
            } else if (toolBody.phase === "result") {
              streamingActive.current = true;
              setLiveSegs((curr) => settleTools(curr, { output }));
            }
          } else if (ev.type === "dev.genesis.run.end") {
            // One refetch when the run settles. Chunks already updated the live tail.
            streamingActive.current = false;
            setLiveSegs([]);
            refresh();
          }
        }
      } catch {
        // ignore malformed frame
      }
    });

    return () => {
      stopped = true;
      streamingActive.current = false;
      unlisten();
      watchRun(null);
    };
  }, [runId, refresh]);

  const thread = session ? buildThread(session) : [];
  const working = session ? isWorking(session) : false;

  useLayoutEffect(() => {
    const node = scroller.current;
    if (node && stick.current) node.scrollTop = node.scrollHeight;
  }, [thread.length, working, session?.runs.length, liveSegs]);

  async function submit(event?: FormEvent) {
    event?.preventDefault();
    const text = draft.trim();
    if (!text || sending || !session?.continue_run) return;
    setSending(true);
    setSendError("");
    const result = await sendFollowUp(session.continue_run, text);
    setSending(false);
    if (!result.ok) {
      setSendError(result.message);
      return;
    }
    setDraft("");
    stick.current = true;
    if (result.runId && result.runId !== runId) {
      onContinued?.(result.runId);
      return;
    }
    await refresh();
  }

  function keys(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
      event.preventDefault();
      void submit();
    }
  }

  const usageLive = useUsageLive();
  const tokens = session
    ? sessionTokens(
        usageLive.runs,
        session.runs.map((run) => run.run.run_id),
        sumUsage(session.runs.map((run) => run.run)).total,
      )
    : 0;
  const blocked = session?.continue_blocked ?? "";
  return (
    <section className="chat" aria-label="Session">
      <header className="chat-head">
        <button
          type="button"
          className="chat-back"
          onClick={onBack ?? (() => window.history.back())}
          title="Back to Wire"
          aria-label="Back to wire"
        >
          ← Wire
        </button>
        <h2>{session?.agent ?? "Session"}</h2>
        {session ? (
          <>
            <span className={working ? "chat-state working" : "chat-state"}>
              <i />
              {working ? "working" : "idle"}
            </span>
            <span className="chat-meta">
              {session.runs.length} {session.runs.length === 1 ? "run" : "runs"}
              {tokens > 0 ? ` · ${new Intl.NumberFormat("en").format(tokens)} tokens` : ""}
            </span>
          </>
        ) : null}
      </header>
      <div
        className="chat-scroll"
        ref={scroller}
        onScroll={(event) => {
          const node = event.currentTarget;
          stick.current = node.scrollHeight - node.scrollTop - node.clientHeight < STICK_PX;
        }}
      >
        <div className="chat-thread">
          {!session && !error ? <p className="empty">Loading session</p> : null}
          {!session && error ? <p className="empty">{error}</p> : null}
          {thread.map((entry) => (
            <Line key={entry.id} entry={entry} working={working} />
          ))}
          {working && liveSegs.length > 0 ? (
            <div className="stream-live-container">
              {liveSegs.map((seg, index) => {
                const live = index === liveSegs.length - 1;
                if (seg.kind === "thought") {
                  const streaming = live && !seg.settled;
                  if (!streaming) {
                    return (
                      <details className="think" key={seg.id}>
                        <summary>
                          <span className="think-icon">▸</span>
                          <span className="think-label">Thinking</span>
                        </summary>
                        <div className="think-body">
                          <Markdown content={seg.text} />
                        </div>
                      </details>
                    );
                  }
                  return (
                    <div className="think" key={seg.id}>
                      <div className="think-summary">
                        <span className="think-icon">▸</span>
                        <span className="think-label">Thinking</span>
                        <span className="live-typing-indicator">thinking</span>
                      </div>
                      <ExpandableLiveBlock>
                        <div className="think-body">
                          <Markdown content={seg.text} />
                          <span className="stream-cursor" />
                        </div>
                      </ExpandableLiveBlock>
                    </div>
                  );
                }
                if (seg.kind === "tool") {
                  const command = liveToolCommand(seg.args) || "preparing command...";
                  const streaming = live && !seg.settled;
                  const executing = !streaming && seg.output === undefined;
                  return (
                    <details className="tool live-tool" key={seg.id} open={streaming || undefined}>
                      <summary>
                        <span className="tool-badge">{seg.name || "tool"}</span>
                        <span className="tool-summary" title={command}>{command}</span>
                        <em className={streaming || executing ? "tool-status running" : "tool-status"}>
                          {streaming ? (
                            <>
                              <i className="pulse-dot" /> streaming
                            </>
                          ) : executing ? (
                            <RunningTimer since={seg.since} />
                          ) : (
                            "done"
                          )}
                        </em>
                      </summary>
                      <div className="tool-body">
                        <div className={streaming ? "tool-card streaming" : "tool-card"}>
                          <div className="tool-card-head">
                            <span>Input / Command</span>
                            <CopyButton text={seg.args || command} />
                          </div>
                          {streaming ? (
                            <ExpandableLiveBlock>
                              <pre className="tool-args">
                                {command}
                                <span className="stream-cursor" />
                              </pre>
                            </ExpandableLiveBlock>
                          ) : (
                            <pre className="tool-args">{command}</pre>
                          )}
                        </div>
                        {streaming ? null : (
                          <div className="tool-card">
                            <div className="tool-card-head">
                              <span>Output</span>
                              <CopyButton text={seg.output || ""} />
                            </div>
                            <pre className="tool-result">{seg.output !== undefined ? seg.output || "(no output)" : ""}</pre>
                          </div>
                        )}
                      </div>
                    </details>
                  );
                }
                const streamingText = live && !seg.settled;
                if (!streamingText) {
                  return (
                    <details className="think" key={seg.id}>
                      <summary>
                        <span className="think-icon">▸</span>
                        <span className="think-label">Output</span>
                      </summary>
                      <div className="think-body">
                        <Markdown content={seg.text} />
                      </div>
                    </details>
                  );
                }
                return (
                  <div className="msg msg-agent live-msg" key={seg.id}>
                    <div className="msg-meta">
                      <b>output</b>
                      <span className="live-typing-indicator">typing...</span>
                    </div>
                    <ExpandableLiveBlock>
                      <Markdown content={seg.text} />
                      <span className="stream-cursor" />
                    </ExpandableLiveBlock>
                  </div>
                );
              })}
            </div>
          ) : null}
        </div>
      </div>
      <form className="composer" onSubmit={submit}>
        <div className="composer-box">
          <textarea
            value={draft}
            rows={Math.min(8, Math.max(2, draft.split("\n").length))}
            disabled={!session || Boolean(blocked) || sending}
            placeholder={blocked || "Send a follow-up. Enter sends, Shift+Enter adds a line."}
            aria-label="Follow-up message"
            onChange={(event) => setDraft(event.target.value)}
            onKeyDown={keys}
          />
        </div>
        {sendError || (error && session) ? (
          <p className={sendError ? "composer-note bad" : "composer-note"}>
            {sendError || "Control is not answering. Showing the last view."}
          </p>
        ) : null}
      </form>
    </section>
  );
}
