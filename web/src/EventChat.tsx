import { useState, type FormEvent, type KeyboardEvent } from "react";
import type { AgentRow, RuleRow } from "./live";
import { shortCause } from "./live";
import { emitMessage } from "./chat";
import { Markdown } from "./Markdown";

interface EventChatProps {
  rule: RuleRow;
  agent?: AgentRow;
  onBack: () => void;
  onRunStarted: (runId: string) => void;
}

export function EventChat({ rule, agent, onBack, onRunStarted }: EventChatProps) {
  const [draft, setDraft] = useState("");
  const [sending, setSending] = useState(false);
  const [sendError, setSendError] = useState("");

  async function submit(event?: FormEvent) {
    event?.preventDefault();
    const text = draft.trim();
    if (!text || sending) return;
    setSending(true);
    setSendError("");
    const result = await emitMessage(rule.name, text);
    setSending(false);
    if (!result.ok || !result.runId) {
      setSendError(result.message || "Failed to start run.");
      return;
    }
    setDraft("");
    onRunStarted(result.runId);
  }

  function keys(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
      event.preventDefault();
      void submit();
    }
  }

  // Extract a brief summary or first paragraph from agent instructions
  const instructionsSummary = agent?.instructions
    ? agent.instructions.trim().split("\n\n")[0]
    : "";

  return (
    <section className="chat" aria-label="Event Dispatch">
      <header className="chat-head">
        <button
          type="button"
          className="chat-back"
          onClick={onBack}
          title="Back to Wire"
          aria-label="Back to wire"
        >
          ← Wire
        </button>
        <h2>{rule.agent}</h2>
        <span className="chat-state">
          <i />
          fresh session
        </span>
        <span className="chat-meta">
          {shortCause(rule.match.type)}
        </span>
      </header>

      <div className="chat-scroll">
        <div className="chat-thread">
          <div className="agent-intro-card">
            <div className="agent-intro-head">
              <div className="agent-intro-title">
                <b>{rule.agent}</b>
                <span className="rule-badge">{rule.name}</span>
              </div>
              <span className="event-type-badge">{rule.match.type}</span>
            </div>

            <div className="agent-intro-meta">
              {rule.match.subject && (
                <div className="agent-meta-item">
                  <span className="agent-meta-label">Subject</span>
                  <code>{rule.match.subject}</code>
                </div>
              )}
              {agent?.cwd && (
                <div className="agent-meta-item">
                  <span className="agent-meta-label">Workspace</span>
                  <code>{agent.cwd}</code>
                </div>
              )}
              {agent?.reasoning_effort && (
                <div className="agent-meta-item">
                  <span className="agent-meta-label">Reasoning</span>
                  <span>{agent.reasoning_effort}</span>
                </div>
              )}
              <div className="agent-meta-item">
                <span className="agent-meta-label">Status</span>
                <span className="presence-tag">{agent?.presence || rule.presence || "active"}</span>
              </div>
            </div>

            {instructionsSummary ? (
              <div className="agent-intro-instructions">
                <span className="agent-meta-label">Role & Scope</span>
                <Markdown content={instructionsSummary} />
              </div>
            ) : null}
          </div>

          <div className="chat-empty-prompt">
            <p>
              Ready to trigger a new session for <b>{rule.agent}</b>. Enter your prompt below to emit{" "}
              <code>{shortCause(rule.match.type)}</code>.
            </p>
          </div>
        </div>
      </div>

      <form className="composer" onSubmit={submit}>
        <div className="composer-box">
          <textarea
            value={draft}
            rows={Math.min(8, Math.max(2, draft.split("\n").length))}
            disabled={sending}
            placeholder={`Message ${rule.agent} to emit ${shortCause(rule.match.type)}... Enter sends, Shift+Enter adds a line.`}
            aria-label="Event message"
            onChange={(event) => setDraft(event.target.value)}
            onKeyDown={keys}
            autoFocus
          />
          <div className="composer-actions">
            <span className={sendError ? "composer-note bad" : "composer-note"}>
              {sendError}
            </span>
            <button type="submit" disabled={sending || draft.trim() === ""}>
              {sending ? "Emitting..." : "Send"}
            </button>
          </div>
        </div>
      </form>
    </section>
  );
}
