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
  const [fullyOpen, setFullyOpen] = useState(false);
  const scrollRef = useRef<HTMLDivElement>(null);

  useLayoutEffect(() => {
    if (!fullyOpen && scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
    }
  });

  return (
    <div
      className={`live-block-container ${fullyOpen ? "live-block-full" : "live-block-semi"}`}
      onClick={() => setFullyOpen((prev) => !prev)}
      role="button"
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          setFullyOpen((prev) => !prev);
        }
      }}
    >
      <div className="live-scroll-window" ref={scrollRef}>
        {children}
      </div>
      <div className="live-block-hint">
        <span>{fullyOpen ? "▲ collapse to live rows" : "▼ click to fully open"}</span>
      </div>
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
  const [open, setOpen] = useState(defaultOpen);
  return (
    <div
      className={`entry-card ${open ? "entry-open" : "entry-closed"} ${className}`}
      onClick={() => setOpen((prev) => !prev)}
      role="button"
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          setOpen((prev) => !prev);
        }
      }}
    >
      <div className="entry-content">{children}</div>
      <div className="entry-toggle-hint">
        <span>{open ? "▲ collapse" : "▼ click to expand"}</span>
      </div>
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
      <ExpandableEntry defaultOpen={false}>
        <div className="msg msg-agent">
          <div className="msg-meta">
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
export interface StreamDeltas {
  thought: string;
  text: string;
  toolName?: string;
  toolArgs: string;
}

export function Chat({ runId, onBack }: { runId: string; onBack?: () => void }) {
  const [session, setSession] = useState<ChatSession | null>(null);
  const [error, setError] = useState("");
  const [draft, setDraft] = useState("");
  const [sending, setSending] = useState(false);
  const [sendError, setSendError] = useState("");
  const [liveStream, setLiveStream] = useState<StreamDeltas>({ thought: "", text: "", toolArgs: "" });
  const scroller = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  const streamingActive = useRef(false);

  const refresh = useCallback(() => {
    return loadChat(runId)
      .then((next) => {
        setSession(next);
        setError("");
        if (!streamingActive.current && next.active_turn) {
          setLiveStream({
            thought: next.active_turn.thought || "",
            text: next.active_turn.text || "",
            toolName: next.active_turn.tool_name,
            toolArgs: next.active_turn.tool_args || "",
          });
        }
      })
      .catch((failure: unknown) => setError(failure instanceof Error ? failure.message : "Control did not answer."));
  }, [runId]);

  useEffect(() => {
    setSession(null);
    setError("");
    setSendError("");
    setLiveStream({ thought: "", text: "", toolArgs: "" });
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
              setLiveStream((curr) => ({ ...curr, thought: curr.thought + raw.text }));
            } else if (chunkType === "text-delta" && raw.text) {
              streamingActive.current = true;
              setLiveStream((curr) => ({ ...curr, text: curr.text + raw.text }));
            } else if (chunkType === "tool-call-delta") {
              streamingActive.current = true;
              setLiveStream((curr) => ({
                ...curr,
                toolName: raw.name || curr.toolName,
                toolArgs: curr.toolArgs + (raw.argumentsDelta || ""),
              }));
            }
          } else if (
            ev.type === "dev.genesis.run.assistant" ||
            ev.type === "dev.genesis.run.turn" ||
            ev.type === "dev.genesis.run.tool" ||
            ev.type === "dev.genesis.run.end"
          ) {
            // Completed turn arrived; reset in-flight stream delta and refresh session
            streamingActive.current = false;
            setLiveStream({ thought: "", text: "", toolArgs: "" });
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
  }, [thread.length, working, session?.runs.length, liveStream.thought, liveStream.text, liveStream.toolArgs]);

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
  const canSend = Boolean(session?.continue_run) && !sending;

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
          {working && (liveStream.thought || liveStream.text || liveStream.toolArgs) ? (
            <div className="stream-live-container">
              {liveStream.thought ? (
                <details className="think live-block-semi" open>
                  <summary>
                    <span className="think-icon">▸</span>
                    <span className="think-label">Thinking</span>
                    <span className="live-typing-indicator">thinking</span>
                  </summary>
                  <div className="think-body live-scroll-window">
                    <Markdown content={liveStream.thought} />
                    <span className="stream-cursor" />
                  </div>
                </details>
              ) : null}
              {liveStream.toolName || liveStream.toolArgs ? (
                <div className="tool live-tool">
                  <div className="tool-header-inline">
                    <span className="tool-badge">{liveStream.toolName || "tool"}</span>
                    <span className="tool-summary">
                      {liveToolCommand(liveStream.toolArgs) || "preparing command..."}
                      <span className="stream-cursor" />
                    </span>
                    <em className="tool-status running">
                      <i className="pulse-dot" /> streaming
                    </em>
                  </div>
                </div>
              ) : null}
              {liveStream.text ? (
                <div className="msg msg-agent live-msg">
                  <div className="msg-meta">
                    <b>{session?.agent}</b>
                    <span className="live-typing-indicator">typing...</span>
                  </div>
                  <ExpandableLiveBlock>
                    <Markdown content={liveStream.text} />
                    <span className="stream-cursor" />
                  </ExpandableLiveBlock>
                </div>
              ) : null}
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
          <div className="composer-actions">
            <span className={sendError ? "composer-note bad" : "composer-note"}>
              {sendError || (error && session ? "Control is not answering. Showing the last view." : "")}
            </span>
            <button type="submit" disabled={!canSend || draft.trim() === ""}>
              {sending ? "Sending" : "Send"}
            </button>
          </div>
        </div>
      </form>
    </section>
  );
}
