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
import { Markdown } from "./Markdown";

const POLL_MS = 2000;
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
  | { id: number; kind: "thought"; text: string }
  | { id: number; kind: "tool"; name: string; args: string }
  | { id: number; kind: "text"; text: string };

let liveSegID = 0;

function nextLiveSeg(seg: LiveSeg): LiveSeg[] {
  return [{ ...seg, id: ++liveSegID }];
}

// continueLive keeps appending to the latest segment when the same kind is
// still streaming. A different kind settles the previous segment so only the
// block that is still receiving tokens stays the collapsed live tail.
function continueLive(prev: LiveSeg[], kind: LiveSeg["kind"], update: (last: LiveSeg) => LiveSeg, create: () => LiveSeg): LiveSeg[] {
  const last = prev[prev.length - 1];
  if (last && last.kind === kind) return [...prev.slice(0, -1), update(last)];
  return [...prev, { ...create(), id: ++liveSegID }];
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
            seeded.push(...nextLiveSeg({ id: 0, kind: "tool", name: turn.tool_name || "tool", args: turn.tool_args || "" }));
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
    const timer = window.setInterval(tick, POLL_MS);

    // Subscribe to live run events for token chunks
    const proto = location.protocol === "https:" ? "wss" : "ws";
    const socket = new WebSocket(`${proto}://${location.host}/api/live`);
    socket.onopen = () => {
      socket.send(JSON.stringify({ op: "subscribe", topic: "run", run_id: runId, after: "now" }));
    };
    socket.onmessage = (event) => {
      try {
        const frame = JSON.parse(event.data);
        if (frame.op === "event" && frame.event) {
          const ev = frame.event;
          if (ev.type === "dev.genesis.run.chunk") {
            const body = ev.data || {};
            const raw = body.raw?.payload?.chunk || {};
            const chunkType = body.chunk_type || raw.type;
            if (chunkType === "reasoning-delta" && raw.text) {
              streamingActive.current = true;
              setLiveSegs((curr) =>
                continueLive(
                  curr,
                  "thought",
                  (last) => (last.kind === "thought" ? { ...last, text: last.text + raw.text } : last),
                  () => ({ id: 0, kind: "thought", text: raw.text }),
                ),
              );
            } else if (chunkType === "text-delta" && raw.text) {
              streamingActive.current = true;
              setLiveSegs((curr) =>
                continueLive(
                  curr,
                  "text",
                  (last) => (last.kind === "text" ? { ...last, text: last.text + raw.text } : last),
                  () => ({ id: 0, kind: "text", text: raw.text }),
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
                      ? { ...last, name: raw.name || last.name, args: last.args + (raw.argumentsDelta || "") }
                      : last,
                  () => ({ id: 0, kind: "tool", name: raw.name || "tool", args: raw.argumentsDelta || "" }),
                ),
              );
            }
          } else if (
            ev.type === "dev.genesis.run.assistant" ||
            ev.type === "dev.genesis.run.turn" ||
            ev.type === "dev.genesis.run.tool" ||
            ev.type === "dev.genesis.run.end"
          ) {
            // Completed turn arrived; reset in-flight stream delta and refresh session
            streamingActive.current = false;
            setLiveSegs([]);
            refresh();
          }
        }
      } catch {
        // ignore malformed frame
      }
    };

    return () => {
      stopped = true;
      streamingActive.current = false;
      window.clearInterval(timer);
      socket.close();
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

  const tokens = session ? sumUsage(session.runs.map((run) => run.run)).total : 0;
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
                  if (!live) {
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
                  return (
                    <div className="tool live-tool" key={seg.id}>
                      <div className="tool-header-inline">
                        <span className="tool-badge">{seg.name || "tool"}</span>
                        <em className={live ? "tool-status running" : "tool-status"}>
                          {live ? (
                            <>
                              <i className="pulse-dot" /> streaming
                            </>
                          ) : (
                            "started"
                          )}
                        </em>
                      </div>
                      {live ? (
                        <ExpandableLiveBlock>
                          <pre className="live-tool-input">
                            {command}
                            <span className="stream-cursor" />
                          </pre>
                        </ExpandableLiveBlock>
                      ) : (
                        <pre className="live-tool-input">{command}</pre>
                      )}
                    </div>
                  );
                }
                return (
                  <div className="msg msg-agent live-msg" key={seg.id}>
                    <div className="msg-meta">
                      <b>output</b>
                      {live ? <span className="live-typing-indicator">typing...</span> : null}
                    </div>
                    {live ? (
                      <ExpandableLiveBlock>
                        <Markdown content={seg.text} />
                        <span className="stream-cursor" />
                      </ExpandableLiveBlock>
                    ) : (
                      <Markdown content={seg.text} />
                    )}
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
