import { useCallback, useEffect, useLayoutEffect, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import {
  buildThread,
  isWorking,
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

function Line({ entry, working }: { entry: Entry; working: boolean }) {
  if (entry.kind === "user") {
    return (
      <div className={entry.mine ? "msg msg-me" : "msg msg-event"}>
        <div className="msg-meta">
          <b>{entry.who}</b>
          <time>{clock(entry.at)}</time>
        </div>
        {entry.text ? <Markdown content={entry.text} /> : <p className="msg-note">Started by {entry.note}</p>}
      </div>
    );
  }
  if (entry.kind === "agent") {
    return (
      <div className="msg msg-agent">
        <div className="msg-meta">
          <time>{clock(entry.at)}</time>
        </div>
        <Markdown content={entry.text} />
        {entry.truncated ? <p className="msg-note">Shortened.</p> : null}
      </div>
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
export function Chat({ runId, onBack }: { runId: string; onBack?: () => void }) {
  const [session, setSession] = useState<ChatSession | null>(null);
  const [error, setError] = useState("");
  const [draft, setDraft] = useState("");
  const [sending, setSending] = useState(false);
  const [sendError, setSendError] = useState("");
  const scroller = useRef<HTMLDivElement>(null);
  const stick = useRef(true);

  const refresh = useCallback(() => {
    return loadChat(runId)
      .then((next) => {
        setSession(next);
        setError("");
      })
      .catch((failure: unknown) => setError(failure instanceof Error ? failure.message : "Control did not answer."));
  }, [runId]);

  useEffect(() => {
    setSession(null);
    setError("");
    setSendError("");
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
    return () => {
      stopped = true;
      window.clearInterval(timer);
    };
  }, [refresh]);

  const thread = session ? buildThread(session) : [];
  const working = session ? isWorking(session) : false;

  useLayoutEffect(() => {
    const node = scroller.current;
    if (node && stick.current) node.scrollTop = node.scrollHeight;
  }, [thread.length, working, session?.runs.length]);

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
          {working ? (
            <p className="chat-working" aria-live="polite">
              <i />
              <i />
              <i />
            </p>
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
