import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import type { Dot, TokenBreakdown, EventSummary, Tone } from "./hints";

interface Anchor {
  x: number;
  top: number;
  bottom: number;
}

interface Placement {
  left: number;
  top: number;
}

const MARGIN = 8;

// Tip shows `content` in a small panel while the pointer or keyboard focus is
// on its children. The panel is drawn in document.body at a fixed position,
// so no scrolling rail can clip it, and it stays inside the window.
export function Tip({
  content,
  children,
  className,
  as: Tag = "span",
  focusable = false,
  off = false,
}: {
  content: ReactNode;
  children: ReactNode;
  className?: string;
  as?: "span" | "div";
  focusable?: boolean;
  // off hides the panel while something else, such as a menu, is open.
  off?: boolean;
}) {
  const [anchor, setAnchor] = useState<Anchor | null>(null);
  const [placement, setPlacement] = useState<Placement | null>(null);
  const bubble = useRef<HTMLDivElement>(null);

  useLayoutEffect(() => {
    if (!anchor || !bubble.current) {
      setPlacement(null);
      return;
    }
    const box = bubble.current.getBoundingClientRect();
    const left = Math.max(MARGIN, Math.min(anchor.x - box.width / 2, window.innerWidth - box.width - MARGIN));
    let top = anchor.top - box.height - MARGIN;
    if (top < MARGIN) top = anchor.bottom + MARGIN;
    setPlacement({ left, top });
  }, [anchor]);

  useEffect(() => {
    if (!anchor) return;
    const hide = () => setAnchor(null);
    window.addEventListener("scroll", hide, true);
    window.addEventListener("resize", hide);
    return () => {
      window.removeEventListener("scroll", hide, true);
      window.removeEventListener("resize", hide);
    };
  }, [anchor]);

  function show(target: HTMLElement) {
    const box = target.getBoundingClientRect();
    setAnchor({ x: box.left + box.width / 2, top: box.top, bottom: box.bottom });
  }

  return (
    <Tag
      className={className}
      tabIndex={focusable ? 0 : undefined}
      onMouseEnter={(event) => show(event.currentTarget)}
      onMouseLeave={() => setAnchor(null)}
      onFocus={(event) => show(event.currentTarget)}
      onBlur={() => setAnchor(null)}
    >
      {children}
      {anchor && !off
        ? createPortal(
            <div
              ref={bubble}
              className="tip"
              role="tooltip"
              style={{
                left: placement?.left ?? 0,
                top: placement?.top ?? 0,
                visibility: placement ? "visible" : "hidden",
              }}
            >
              {content}
            </div>,
            document.body,
          )
        : null}
    </Tag>
  );
}

function formatCount(value: number) {
  return new Intl.NumberFormat("en", { maximumFractionDigits: 0 }).format(value);
}

function percent(share: number) {
  if (share <= 0) return "0%";
  if (share < 0.01) return "<1%";
  return `${Math.round(share * 100)}%`;
}

export function DotHint({ dot }: { dot: Dot }) {
  return (
    <>
      <strong className={dot.tone ? `tip-title tone-${dot.tone}` : "tip-title"}>{dot.label}</strong>
      <span className="tip-text">{dot.detail}</span>
    </>
  );
}

export function TokenHint({ breakdown }: { breakdown: TokenBreakdown }) {
  return (
    <>
      <strong className="tip-title">Tokens</strong>
      <div className="tip-bar" aria-hidden="true">
        {breakdown.parts.map((part) => (
          <i key={part.key} className={`fill-${part.tone}`} style={{ flexGrow: part.share }} />
        ))}
      </div>
      <dl className="tip-rows">
        {breakdown.parts.map((part) => (
          <div key={part.key} className="tip-row">
            <dt>
              <i className={`swatch fill-${part.tone}`} aria-hidden="true" />
              {part.label}
            </dt>
            <dd className={`tone-${part.tone}`}>
              {formatCount(part.value)} <small>{percent(part.share)}</small>
            </dd>
          </div>
        ))}
        <div className="tip-row total">
          <dt>Total</dt>
          <dd>{formatCount(breakdown.total)}</dd>
        </div>
      </dl>
      <span className="tip-text">
        Reasoning, {formatCount(breakdown.reasoning)}, is already counted inside output.
      </span>
    </>
  );
}

const causeTone: Record<string, Tone> = {
  "dev.genesis.user.message": "blue",
  "dev.genesis.agent.finished": "green",
  "dev.genesis.session.continue": "violet",
};

function causeColor(cause: string): Tone {
  return causeTone[cause] ?? "amber";
}

export function EventHint({ summary }: { summary: EventSummary }) {
  const { rules } = summary;
  const shown = summary.byCause.slice(0, 5);
  const rest = summary.byCause.slice(5).reduce((sum, item) => sum + item.count, 0);
  return (
    <>
      <strong className="tip-title">Events</strong>
      <span className="tip-text">
        {formatCount(summary.triggers)} {summary.triggers === 1 ? "event" : "events"} started agents.
        An event no rule matches is not logged.
      </span>
      <dl className="tip-rows">
        {shown.map((item) => (
          <div key={item.cause} className="tip-row">
            <dt>
              <i className={`swatch fill-${causeColor(item.cause)}`} aria-hidden="true" />
              {item.cause.replace(/^dev\.genesis\./, "")}
            </dt>
            <dd className={`tone-${causeColor(item.cause)}`}>{formatCount(item.count)}</dd>
          </div>
        ))}
        {rest > 0 ? (
          <div className="tip-row">
            <dt>other</dt>
            <dd>{formatCount(rest)}</dd>
          </div>
        ) : null}
      </dl>
      <dl className="tip-rows">
        <div className="tip-row">
          <dt>Runs going</dt>
          <dd className="tone-blue">{formatCount(summary.running)}</dd>
        </div>
        <div className="tip-row">
          <dt>Runs completed</dt>
          <dd className="tone-green">{formatCount(summary.completed)}</dd>
        </div>
        <div className="tip-row">
          <dt>Runs failed</dt>
          <dd className="tone-red">{formatCount(summary.failed)}</dd>
        </div>
        <div className="tip-row">
          <dt>Journal events logged</dt>
          <dd>{formatCount(summary.journalLines)}</dd>
        </div>
        <div className="tip-row">
          <dt>Rules that fired</dt>
          <dd>
            {rules.fired} of {rules.total}
          </dd>
        </div>
        {rules.unsynced > 0 ? (
          <div className="tip-row">
            <dt>Rules not synced</dt>
            <dd className="tone-amber">{rules.unsynced}</dd>
          </div>
        ) : null}
      </dl>
      {summary.capped ? <span className="tip-text">Counts cover the latest runs only.</span> : null}
    </>
  );
}
