import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { StatusChip, SyncPanel } from "./Chip";
import { categoryById, type Edition } from "./editions";
import {
  agentDot,
  eventSummary,
  isDrifted,
  ruleDot,
  runDot,
  runningByAgent,
  runningByRule,
  runsByRule,
  tokenBreakdown,
  type Dot,
} from "./hints";
import { loadSnapshot, loopSpan, shortCause, sumUsage, type RunRow, type Snapshot, type Usage } from "./live";
import { driftItems } from "./sync";
import { DotHint, EventHint, Tip, TokenHint } from "./Tip";
import { toEdition } from "./publications";
import { Story } from "./Story";
import { usePublications } from "./usePublications";
import { createViewStack, navigateToFrame, pushView } from "./view-stack";
import { Chat } from "./Chat";
import { EventChat } from "./EventChat";
import { ErrorBoundary } from "./ErrorBoundary";

function formatCount(value: number) {
  return new Intl.NumberFormat("en", { maximumFractionDigits: 0 }).format(value);
}

// StatusDot is the small coloured mark in front of a row. An empty slot (no
// colour) is still a hover target, because "no dot" means something too.
function StatusDot({ dot }: { dot: Dot }) {
  return (
    <Tip className="mark-slot" content={<DotHint dot={dot} />}>
      <i className={dot.tone ? `mark mark-${dot.tone}` : "mark"} aria-label={dot.label} />
    </Tip>
  );
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

function Stat({ label, value, tone, hint }: { label: string; value: number; tone: string; hint: ReactNode }) {
  const shown = useSmooth(value);
  return (
    <Tip as="div" className={`stat hoverable tone-${tone}`} content={hint} focusable>
      <span>{label}</span>
      <strong>{formatCount(shown)}</strong>
    </Tip>
  );
}

function LiveBoard({
  usage,
  events,
  up,
  down,
  eventHint,
}: {
  usage: Usage;
  events: number;
  up: number;
  down: number;
  eventHint: ReactNode;
}) {
  return (
    <section className="board" aria-label="The weather">
      <h2>The weather</h2>
      <div className="board-grid">
        <Stat label="Tokens" value={usage.total} tone="amber" hint={<TokenHint breakdown={tokenBreakdown(usage)} />} />
        <Stat label="Events" value={events} tone="violet" hint={eventHint} />
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

// A view frame whose id starts with this opens the session of that run.
const RUN_VIEW = "run:";
const EVENT_VIEW = "event:";

function getInitialFrameId(): string {
  if (typeof window === "undefined") return "wire";
  const params = new URLSearchParams(window.location.search);
  const runId = params.get("run");
  if (runId) return RUN_VIEW + runId;
  const eventRule = params.get("event");
  if (eventRule) return EVENT_VIEW + eventRule;
  return "wire";
}

const HISTORY_PREVIEW = 8;
const HISTORY_PAGE = 20;

// RunList lists runs. Opening one shows its session as a chat in the middle.
function RunList({ runs, current, onOpen }: { runs: RunRow[]; current: string | null; onOpen: (runId: string) => void }) {
  if (runs.length === 0) return <p className="rail-note">None</p>;
  return (
    <ul>
      {runs.map((run) => (
        <li
          key={run.run_id}
          className={run.run_id === current ? "openable current" : "openable"}
          role="button"
          tabIndex={0}
          aria-label={`Open the ${run.agent} session`}
          onClick={() => onOpen(run.run_id)}
          onKeyDown={(event) => {
            if (event.key === "Enter" || event.key === " ") {
              event.preventDefault();
              onOpen(run.run_id);
            }
          }}
        >
          <StatusDot dot={runDot(run)} />
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
  // connecting is true until the socket has answered once, so the page does
  // not announce "Offline" for the moment it takes to connect.
  const [connecting, setConnecting] = useState(true);
  const [atTop, setAtTop] = useState(true);
  const [historyExtra, setHistoryExtra] = useState(0);
  const [views, setViews] = useState(() => createViewStack({ id: getInitialFrameId() }));
  const frameId = views.frames[views.index].id;
  const chatRunId = frameId.startsWith(RUN_VIEW) ? frameId.slice(RUN_VIEW.length) : null;
  const eventRuleName = frameId.startsWith(EVENT_VIEW) ? frameId.slice(EVENT_VIEW.length) : null;

  useEffect(() => {
    const initialId = getInitialFrameId();
    if (!window.history.state || window.history.state.frameId !== initialId) {
      window.history.replaceState({ frameId: initialId }, "");
    }
  }, []);

  useEffect(() => {
    const handlePopState = (event: PopStateEvent) => {
      const params = new URLSearchParams(window.location.search);
      const runId = params.get("run");
      const eventRule = params.get("event");
      const targetId: string =
        event.state?.frameId ??
        (runId ? RUN_VIEW + runId : eventRule ? EVENT_VIEW + eventRule : "wire");
      setViews((current) => navigateToFrame(current, targetId));
    };

    window.addEventListener("popstate", handlePopState);
    return () => window.removeEventListener("popstate", handlePopState);
  }, []);

  const openRun = useCallback((runId: string) => {
    const targetId = RUN_VIEW + runId;
    setViews((current) => {
      if (current.frames[current.index].id === targetId) return current;
      const nextUrl = new URL(window.location.href);
      nextUrl.searchParams.delete("event");
      nextUrl.searchParams.set("run", runId);
      window.history.pushState({ frameId: targetId }, "", nextUrl.toString());
      return pushView(current, { id: targetId });
    });
  }, []);

  const openEvent = useCallback((ruleName: string) => {
    const targetId = EVENT_VIEW + ruleName;
    setViews((current) => {
      if (current.frames[current.index].id === targetId) return current;
      const nextUrl = new URL(window.location.href);
      nextUrl.searchParams.delete("run");
      nextUrl.searchParams.set("event", ruleName);
      window.history.pushState({ frameId: targetId }, "", nextUrl.toString());
      return pushView(current, { id: targetId });
    });
  }, []);

  const openWire = useCallback(() => {
    setViews((current) => {
      if (current.frames[current.index].id === "wire") return current;
      const nextUrl = new URL(window.location.href);
      nextUrl.searchParams.delete("run");
      nextUrl.searchParams.delete("event");
      window.history.pushState({ frameId: "wire" }, "", nextUrl.toString());
      return pushView(current, { id: "wire" });
    });
  }, []);
  const [syncOpen, setSyncOpen] = useState(false);
  const closeSync = useCallback(() => setSyncOpen(false), []);
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
      setConnecting(false);
      socket.send(JSON.stringify({ op: "subscribe", topic: "runs" }));
      socket.send(JSON.stringify({ op: "subscribe", topic: "state" }));
    };
    socket.onmessage = () => {
      refresh();
      refreshPublications();
    };
    socket.onclose = () => {
      setLive(false);
      setConnecting(false);
    };
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
  const linkWord = live ? "Online" : connecting ? "Connecting" : "Offline";
  const invalid = drift === "desired_invalid" ? snapshot?.state?.desired_error || "the files do not parse" : undefined;
  const agentRuns = runningByAgent(runs);
  const ruleRunning = runningByRule(runs);
  const ruleTotals = runsByRule(runs);
  const summary = eventSummary(runs, rules);
  const driftedAgents = agents.filter((agent) => isDrifted(agent.presence)).length;
  const driftedRules = rules.filter((rule) => isDrifted(rule.presence)).length;

  return (
    <div className="app">
      <header className="mast">
        <button type="button" className="nameplate-link" onClick={openWire} title="Genesis wire">
          Genesis
        </button>
        <span className="edition">The Wire</span>
        <div className="mast-status">
          <div className="chip-pop">
          <Tip
            off={syncOpen}
            content={
              <>
                <strong className={drifted ? "tip-title tone-amber" : "tip-title"}>{drifted ? "Drift" : "No drift"}</strong>
                <span className="tip-text">
                  {drift === undefined
                    ? "Control has not reported the sync state."
                    : invalid
                      ? `Control cannot load the files on disk: ${invalid}. Nothing can sync until that is fixed.`
                      : drifted
                        ? `The files on disk differ from what the listener runs: ${driftedAgents} ${driftedAgents === 1 ? "agent" : "agents"} and ${driftedRules} ${driftedRules === 1 ? "rule" : "rules"} marked yellow. Click to sync them.`
                      : "The files on disk are what the listener runs."}
                </span>
              </>
            }
          >
            {drifted ? (
              <StatusChip tone="amber" label="Drift" expanded={syncOpen} onClick={() => setSyncOpen((open) => !open)} />
            ) : (
              <StatusChip tone={drift === undefined ? "faint" : "green"} label={drift === undefined ? "Drift" : "No drift"} />
            )}
          </Tip>
          {syncOpen ? (
            <SyncPanel
              items={driftItems(agents, rules)}
              invalid={invalid}
              onSynced={() => refreshRef.current()}
              onClose={closeSync}
            />
          ) : null}
          </div>
          <Tip
            content={
              <>
                <strong className={live ? "tip-title tone-green" : "tip-title"}>{linkWord}</strong>
                <span className="tip-text">
                  {live
                    ? "This page is connected to control and updates as runs change."
                    : connecting
                      ? "Opening the connection to control."
                      : "This page lost its connection to control. Click to refresh."}
                </span>
              </>
            }
          >
            <StatusChip
              tone={live ? "green" : "faint"}
              label={linkWord}
              onClick={live || connecting ? undefined : () => refreshRef.current()}
            />
          </Tip>
          <Tip
            content={
              <>
                <strong className={loopLive ? "tip-title tone-green" : "tip-title"}>{loopLive ? "Live" : "Stopped"}</strong>
                <span className="tip-text">
                  {loopLive
                    ? `The loop is working: ${[...busy].sort().join(", ")} running now.`
                    : "No agent has a run in progress."}
                </span>
              </>
            }
          >
            <StatusChip tone={loopLive ? "green" : "faint"} label={loopLive ? "Live" : "Stopped"} pulse={loopLive} />
          </Tip>
        </div>
      </header>
      <div className="stage">
        <aside className="rail rail-left">
          <RailSection title="Running">
            {snapshot ? <RunList runs={running} current={chatRunId} onOpen={openRun} /> : <p className="rail-note">Waiting for control</p>}
          </RailSection>
          <RailSection title="History">
            {snapshot ? (
              <>
                <RunList runs={historyVisible} current={chatRunId} onOpen={openRun} />
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
          <main
            className="column"
            hidden={chatRunId !== null || eventRuleName !== null}
            onScroll={(event) => setAtTop(event.currentTarget.scrollTop < 48)}
          >
            <div
              key={`${views.frames[views.index].id}-${views.index}`}
              className={views.frames.length === 1 ? "view" : views.direction < 0 ? "view view-back" : "view view-forward"}
            >
              <div className="sheet">
                <LiveBoard
                  usage={usage}
                  events={summary.triggers}
                  up={span.up}
                  down={span.down}
                  eventHint={<EventHint summary={summary} />}
                />
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
          {chatRunId ? (
            <div
              key={`${frameId}-${views.index}`}
              className={views.direction < 0 ? "view view-back view-chat" : "view view-forward view-chat"}
            >
              <ErrorBoundary onReset={openWire}>
                <Chat runId={chatRunId} onBack={openWire} />
              </ErrorBoundary>
            </div>
          ) : null}
          {eventRuleName ? (
            <div
              key={`${frameId}-${views.index}`}
              className={views.direction < 0 ? "view view-back view-chat" : "view view-forward view-chat"}
            >
              <ErrorBoundary onReset={openWire}>
                {(() => {
                  const selectedRule = rules.find((r) => r.name === eventRuleName);
                  const selectedAgent = selectedRule ? agents.find((a) => a.id === selectedRule.agent) : undefined;
                  return selectedRule ? (
                    <EventChat
                      rule={selectedRule}
                      agent={selectedAgent}
                      onBack={openWire}
                      onRunStarted={(newRunId) => {
                        refreshRef.current();
                        openRun(newRunId);
                      }}
                    />
                  ) : (
                    <p className="empty">Rule {eventRuleName} not found.</p>
                  );
                })()}
              </ErrorBoundary>
            </div>
          ) : null}
          <Tip as="div" className="edge" content={<TokenHint breakdown={tokenBreakdown(usage)} />}>
            <span>
              <b>{formatCount(usage.total)}</b> tokens
            </span>
            <span>cache {formatCount(usage.cache_hit)}</span>
            <span>miss {formatCount(usage.cache_miss)}</span>
            <span>out {formatCount(usage.output)}</span>
            <span>reason {formatCount(usage.reasoning)}</span>
          </Tip>
        </div>
        <aside className="rail rail-right">
          <div className="rail-scroll">
            <RailSection title="Agents">
              {snapshot ? (
                <ul>
                  {agents.map((agent) => {
                    const active = busy.has(agent.id);
                    const drifting = isDrifted(agent.presence);
                    return (
                      <li key={agent.id}>
                        <StatusDot dot={agentDot(agent.presence, agentRuns.get(agent.id) ?? 0, invalid)} />
                        <span>{agent.id}</span>
                        <em>{active ? "running" : drifting ? "not synced" : "idle"}</em>
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
                    <li
                      key={rule.name}
                      className={eventRuleName === rule.name ? "openable current" : "openable"}
                      role="button"
                      tabIndex={0}
                      aria-label={`Open event ${rule.name}`}
                      onClick={() => openEvent(rule.name)}
                      onKeyDown={(event) => {
                        if (event.key === "Enter" || event.key === " ") {
                          event.preventDefault();
                          openEvent(rule.name);
                        }
                      }}
                    >
                      <StatusDot dot={ruleDot(rule.presence, ruleRunning.get(rule.name) ?? 0, ruleTotals.get(rule.name) ?? 0, invalid)} />
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
        hidden={atTop || chatRunId !== null || eventRuleName !== null}
        onClick={() => {
          document.querySelector(".column")?.scrollTo({ top: 0, behavior: "smooth" });
        }}
      >
        Latest
      </button>
    </div>
  );
}
