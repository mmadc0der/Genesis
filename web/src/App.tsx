import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { categoryById, type Edition } from "./editions";
import { loadSnapshot, loopSpan, shortCause, sumUsage, type RunRow, type Snapshot, type Usage } from "./live";
import { toEdition } from "./publications";
import { Story } from "./Story";
import { usePublications } from "./usePublications";
import { canBack, canForward, createViewStack, goBack, goForward } from "./view-stack";

function formatCount(value: number) {
  return new Intl.NumberFormat("en", { maximumFractionDigits: 0 }).format(value);
}

function toneFor(run: RunRow) {
  if (run.state === "failed") return "red";
  if (run.cause_type === "dev.genesis.session.continue") return "violet";
  if (run.state === "open") return "blue";
  if (run.state === "completed") return "green";
  return "amber";
}

function useSmooth(target: number) {
  const [shown, setShown] = useState(target);
  const shownRef = useRef(target);
  useEffect(() => {
    const from = shownRef.current;
    if (from === target) return;
    const start = performance.now();
    let frame = 0;
    const tick = (now: number) => {
      const t = Math.min(1, (now - start) / 700);
      const eased = 1 - (1 - t) ** 3;
      const next = Math.round(from + (target - from) * eased);
      shownRef.current = next;
      setShown(next);
      if (t < 1) frame = requestAnimationFrame(tick);
    };
    frame = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(frame);
  }, [target]);
  return shown;
}

function formatDuration(ms: number) {
  const minutes = Math.max(0, Math.round(ms / 60000));
  const days = Math.floor(minutes / 1440);
  const hours = Math.floor((minutes % 1440) / 60);
  const rest = minutes % 60;
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${rest}m`;
  return `${rest}m`;
}

function Stat({ label, value, tone }: { label: string; value: number; tone: string }) {
  const shown = useSmooth(value);
  return (
    <div className={`stat tone-${tone}`}>
      <span>{label}</span>
      <strong>{formatCount(shown)}</strong>
    </div>
  );
}

function LiveBoard({ usage, events, up, down }: { usage: Usage; events: number; up: number; down: number }) {
  return (
    <section className="board" aria-label="The weather">
      <h2>The weather</h2>
      <div className="board-grid">
        <Stat label="Tokens" value={usage.total} tone="amber" />
        <Stat label="Events" value={events} tone="violet" />
        <div className="stat span-stat">
          <span>Span</span>
          <p className="up">
            <b>Up</b>
            <em>{formatDuration(up)}</em>
          </p>
          <p className="down">
            <b>Down</b>
            <em>{formatDuration(down)}</em>
          </p>
        </div>
      </div>
    </section>
  );
}

function EditionIndex({ editions }: { editions: Edition[] }) {
  const [tip, setTip] = useState<{ id: string; x: number; y: number } | null>(null);
  const hovered = tip ? editions.find((edition) => edition.id === tip.id) : undefined;

  function showTip(id: string, target: HTMLButtonElement) {
    const box = target.getBoundingClientRect();
    setTip({ id, x: box.left + box.width / 2, y: box.top });
  }

  return (
    <>
      <nav className="edition-index" aria-label="Leads">
        {editions.map((edition) => {
          const category = categoryById(edition.category);
          return (
            <button
              key={edition.id}
              type="button"
              className={`lead-square tone-${category.tone}`}
              aria-label={edition.headline}
              onMouseEnter={(event) => showTip(edition.id, event.currentTarget)}
              onMouseLeave={() => setTip(null)}
              onFocus={(event) => showTip(edition.id, event.currentTarget)}
              onBlur={() => setTip(null)}
              onClick={() => document.getElementById(`lead-${edition.id}`)?.scrollIntoView({ behavior: "smooth", block: "start" })}
            />
          );
        })}
      </nav>
      {hovered && tip ? (
        <div className="lead-tip" style={{ left: tip.x, top: tip.y }} role="tooltip">
          <strong>{hovered.headline}</strong>
          <time>{hovered.time}</time>
        </div>
      ) : null}
    </>
  );
}

// FeedEnd sits under the last story. When it comes within reach of the
// scrolling column, the next older page is requested. The effect restarts
// after every page, so a short page cannot leave the feed stuck.
function FeedEnd({
  hasOlder,
  loading,
  count,
  onNeed,
}: {
  hasOlder: boolean;
  loading: boolean;
  count: number;
  onNeed: () => void;
}) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const node = ref.current;
    if (!node || !hasOlder || loading) return;
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((entry) => entry.isIntersecting)) onNeed();
      },
      { root: node.closest(".column"), rootMargin: "0px 0px 600px 0px" },
    );
    observer.observe(node);
    return () => observer.disconnect();
  }, [hasOlder, loading, count, onNeed]);
  return (
    <div ref={ref} className="feed-end" aria-live="polite">
      {loading ? "Loading earlier stories" : !hasOlder && count > 0 ? "That is every story." : null}
    </div>
  );
}

function Arrow() {
  return (
    <svg viewBox="0 0 12 12" width="1em" height="1em" aria-hidden="true">
      <path
        d="M4.2 2.2 L8 6 L4.2 9.8"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

function RailSection({ title, children }: { title: string; children: ReactNode }) {
  const [open, setOpen] = useState(true);
  return (
    <section>
      <button
        type="button"
        className={open ? "rail-title open" : "rail-title"}
        aria-expanded={open}
        onClick={() => setOpen((current) => !current)}
      >
        {title}
        <i aria-hidden="true">
          <svg viewBox="0 0 12 12" width="1em" height="1em">
            <path
              d="M4.2 2.2 L8 6 L4.2 9.8"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.4"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        </i>
      </button>
      {open ? children : null}
    </section>
  );
}

const HISTORY_PREVIEW = 8;
const HISTORY_PAGE = 20;

function RunList({ runs }: { runs: RunRow[] }) {
  if (runs.length === 0) return <p className="rail-note">None</p>;
  return (
    <ul>
      {runs.map((run) => (
        <li key={run.run_id}>
          <i className={`mark mark-${toneFor(run)}`} aria-hidden="true" />
          <span>{shortCause(run.cause_type)}</span>
          <em>
            {run.agent} · {run.state}
          </em>
        </li>
      ))}
    </ul>
  );
}

export default function App() {
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [live, setLive] = useState(false);
  const [atTop, setAtTop] = useState(true);
  const [historyExtra, setHistoryExtra] = useState(0);
  const [views, setViews] = useState(() => createViewStack({ id: "wire" }));
  const [driftOpen, setDriftOpen] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const refreshRef = useRef<() => void>(() => {});
  const publications = usePublications();
  const refreshPublications = publications.refresh;

  useEffect(() => {
    let stopped = false;
    const refresh = () => {
      loadSnapshot()
        .then((next) => {
          if (!stopped) setSnapshot(next);
        })
        .catch(() => {
          if (!stopped) setSnapshot(null);
        });
    };
    refreshRef.current = refresh;
    refresh();
    const clock = window.setInterval(() => setNow(Date.now()), 15000);
    const proto = location.protocol === "https:" ? "wss" : "ws";
    const socket = new WebSocket(`${proto}://${location.host}/api/live`);
    socket.onopen = () => {
      setLive(true);
      socket.send(JSON.stringify({ op: "subscribe", topic: "runs" }));
      socket.send(JSON.stringify({ op: "subscribe", topic: "state" }));
    };
    socket.onmessage = () => {
      refresh();
      refreshPublications();
    };
    socket.onclose = () => setLive(false);
    return () => {
      stopped = true;
      window.clearInterval(clock);
      socket.close();
    };
  }, [refreshPublications]);

  const editions = useMemo(() => publications.items.map((item) => toEdition(item, now)), [publications.items, now]);
  const runs = snapshot?.runs ?? [];
  const running = runs.filter((run) => run.state === "open");
  const history = runs.filter((run) => run.state !== "open");
  const historyShown = HISTORY_PREVIEW + historyExtra;
  const historyVisible = history.slice(0, historyShown);
  const historyHasMore = history.length > historyShown;
  const usage = sumUsage(runs);
  const busy = new Set(running.map((run) => run.agent));
  const span = loopSpan(runs, now);
  const loopLive = busy.size > 0;
  const drift = snapshot?.state?.drift;
  const drifted = drift !== undefined && drift !== "in_sync";
  const agents = [...(snapshot?.agents ?? [])].sort((left, right) => left.id.localeCompare(right.id));
  const rules = snapshot?.rules ?? [];

  return (
    <div className="app">
      <header className="mast">
        <div className="nameplate">Genesis</div>
        <div className="edition">The Wire</div>
        <div className="mast-status">
          <button
            type="button"
            className={drifted ? "drift-chip on" : "drift-chip"}
            disabled={drift === undefined}
            aria-pressed={driftOpen}
            onClick={() => setDriftOpen((current) => !current)}
          >
            <i />
            {drift === undefined ? "Drift" : driftOpen ? drift.replaceAll("_", " ") : drifted ? "Drift" : "No drift"}
          </button>
          <button type="button" className={live ? "live" : "live off"} onClick={() => refreshRef.current()}>
            <i />
            {live ? "Live" : "Offline"}
          </button>
          <div className={loopLive ? "rail-status" : "rail-status stopped"}>
            <i />
            {loopLive ? "Live" : "Stopped"}
          </div>
        </div>
      </header>
      <div className="stage">
        <aside className="rail rail-left">
          <RailSection title="Running">
            {snapshot ? <RunList runs={running} /> : <p className="rail-note">Waiting for control</p>}
          </RailSection>
          <RailSection title="History">
            {snapshot ? (
              <>
                <RunList runs={historyVisible} />
                {historyHasMore ? (
                  <button type="button" className="show-more" onClick={() => setHistoryExtra((current) => current + HISTORY_PAGE)}>
                    More
                  </button>
                ) : null}
              </>
            ) : (
              <p className="rail-note">Waiting for control</p>
            )}
          </RailSection>
        </aside>
        <div className="center">
          <div className="view-bar">
            <button
              type="button"
              className="view-arrow back"
              aria-label="Back"
              disabled={!canBack(views)}
              onClick={() => setViews(goBack)}
            >
              <Arrow />
            </button>
            <button
              type="button"
              className="view-arrow"
              aria-label="Forward"
              disabled={!canForward(views)}
              onClick={() => setViews(goForward)}
            >
              <Arrow />
            </button>
          </div>
          <main
            className="column"
            onScroll={(event) => setAtTop(event.currentTarget.scrollTop < 48)}
          >
            <div
              key={`${views.frames[views.index].id}-${views.index}`}
              className={views.frames.length === 1 ? "view" : views.direction < 0 ? "view view-back" : "view view-forward"}
            >
              <div className="sheet">
                <LiveBoard usage={usage} events={runs.length} up={span.up} down={span.down} />
                <EditionIndex editions={editions} />
                {!publications.loaded ? (
                  <p className="empty">{publications.error ? "Waiting for control" : "Loading stories"}</p>
                ) : editions.length === 0 ? (
                  <p className="empty">No publications yet.</p>
                ) : (
                  editions.map((edition) => <Story key={edition.id} edition={edition} />)
                )}
                <FeedEnd
                  hasOlder={publications.hasOlder}
                  loading={publications.loadingOlder}
                  count={editions.length}
                  onNeed={publications.loadOlder}
                />
              </div>
            </div>
          </main>
          <footer className="edge">
            <span>
              <b>{formatCount(usage.total)}</b> tokens
            </span>
            <span>cache {formatCount(usage.cache_hit)}</span>
            <span>miss {formatCount(usage.cache_miss)}</span>
            <span>out {formatCount(usage.output)}</span>
            <span>reason {formatCount(usage.reasoning)}</span>
          </footer>
        </div>
        <aside className="rail rail-right">
          <div className="rail-scroll">
            <RailSection title="Agents">
              {snapshot ? (
                <ul>
                  {agents.map((agent) => {
                    const active = busy.has(agent.id);
                    return (
                      <li key={agent.id}>
                        <i className={active ? "mark mark-blue" : "mark"} aria-hidden="true" />
                        <span>{agent.id}</span>
                        <em>{active ? "running" : "idle"}</em>
                      </li>
                    );
                  })}
                </ul>
              ) : (
                <p className="rail-note">Waiting for control</p>
              )}
            </RailSection>
            <RailSection title="Events">
              {snapshot ? (
                <ul>
                  {rules.map((rule) => (
                    <li key={rule.name}>
                      <i className="mark" aria-hidden="true" />
                      <span>{shortCause(rule.match.type)}</span>
                      <em>{rule.match.subject || rule.agent}</em>
                    </li>
                  ))}
                </ul>
              ) : (
                <p className="rail-note">Waiting for control</p>
              )}
            </RailSection>
            <RailSection title="Schedules">
              <p className="rail-note">Not reported yet</p>
            </RailSection>
            <RailSection title="Jobs">
              <p className="rail-note">Not reported yet</p>
            </RailSection>
          </div>
        </aside>
      </div>
      <button
        className="return"
        type="button"
        hidden={atTop}
        onClick={() => {
          document.querySelector(".column")?.scrollTo({ top: 0, behavior: "smooth" });
        }}
      >
        Latest
      </button>
    </div>
  );
}
