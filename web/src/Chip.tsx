import { useEffect, useRef, useState } from "react";
import { postSync, type DriftItem } from "./sync";

// StatusChip is one word and one dot in the masthead. It is a button only
// when it has an action; otherwise it is plain text with a hover hint.
export function StatusChip({
  tone,
  label,
  pulse = false,
  onClick,
  disabled = false,
  expanded,
}: {
  tone: "green" | "amber" | "faint";
  label: string;
  pulse?: boolean;
  onClick?: () => void;
  disabled?: boolean;
  expanded?: boolean;
}) {
  const className = `chip ${tone}${pulse ? " pulse" : ""}`;
  if (onClick) {
    return (
      <button type="button" className={className} onClick={onClick} disabled={disabled} aria-expanded={expanded}>
        <i />
        {label}
      </button>
    );
  }
  return (
    <div className={className}>
      <i />
      {label}
    </div>
  );
}

// SyncPanel lists what a Sync would change and runs it. It closes on Escape,
// on a click anywhere outside its container, and shortly after a good sync.
export function SyncPanel({
  items,
  invalid,
  onSynced,
  onClose,
}: {
  items: DriftItem[];
  invalid?: string;
  onSynced: () => void;
  onClose: () => void;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [phase, setPhase] = useState<"idle" | "syncing" | "done" | "failed">("idle");
  const [message, setMessage] = useState("");

  useEffect(() => {
    const away = (event: MouseEvent) => {
      const container = ref.current?.parentElement;
      if (container && event.target instanceof Node && !container.contains(event.target)) onClose();
    };
    const escape = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    document.addEventListener("mousedown", away);
    document.addEventListener("keydown", escape);
    return () => {
      document.removeEventListener("mousedown", away);
      document.removeEventListener("keydown", escape);
    };
  }, [onClose]);

  useEffect(() => {
    if (phase !== "done") return;
    const timer = window.setTimeout(onClose, 1400);
    return () => window.clearTimeout(timer);
  }, [phase, onClose]);

  async function run() {
    setPhase("syncing");
    setMessage("");
    const result = await postSync();
    setMessage(result.message);
    setPhase(result.ok ? "done" : "failed");
    if (result.ok) onSynced();
  }

  if (phase === "done") {
    return (
      <div ref={ref} className="sync-panel" role="dialog" aria-label="Sync changes">
        <h3 className="good">Synced</h3>
        <p>The listener now runs the files on disk.</p>
      </div>
    );
  }

  return (
    <div ref={ref} className="sync-panel" role="dialog" aria-label="Sync changes">
      <h3>Not synced</h3>
      {invalid ? (
        <p>Control cannot load the files on disk: {invalid}. Fix them before syncing.</p>
      ) : items.length > 0 ? (
        <>
          <p>Sync makes the listener run these files.</p>
          <ul className="sync-list">
            {items.map((item) => (
              <li key={`${item.kind}-${item.name}`}>
                <span>{item.name}</span>
                <em>
                  {item.kind} · {item.note}
                </em>
              </li>
            ))}
          </ul>
        </>
      ) : (
        <p>The listener differs from the files on disk.</p>
      )}
      <button type="button" className="sync-go" disabled={phase === "syncing" || Boolean(invalid)} onClick={run}>
        {phase === "syncing" ? "Syncing" : "Sync now"}
      </button>
      {phase === "failed" && message ? <p className="sync-note bad">{message}</p> : null}
    </div>
  );
}
