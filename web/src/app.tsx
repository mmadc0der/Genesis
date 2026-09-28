import { useEffect, useRef, useState } from "preact/hooks";
import { getJSON, postText } from "./api";
import { ActivityLine } from "./activity-line";
import { useTranscriptFollow } from "./transcript-scroll";
import {
  applyPanelLoad,
  buildMessage,
  driftLabel,
  activityView,
  continueActivity,
  maxCursor,
  mergeEvents,
  mergeRuns,
  loadJournal,
  readJournal,
  presenceLabel,
  githubGrantSummary,
  driftLine,
  repositoryLabel,
  repositoryObservationSummary,
  repositoryPolicy,
  repositorySyncNotice,
  secretNames,
  shortDigest,
  type ActivityCursor,
  type PanelSnapshot,
  type Settled,
} from "./model";
import type { Agent, ControlState, EventsPage, LifecycleEvent, LiveFrame, LiveOp, Repository, Rule, RunDetail, RunSummary } from "./types";

function settle<T>(promise: Promise<T>): Promise<Settled<T>> {
  return promise.then(
    (value) => ({ ok: true, value }),
    (reason: unknown) => ({ ok: false, error: reason instanceof Error ? reason.message : "Request failed" }),
  );
}

export function App() {
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [state, setState] = useState<ControlState | null>(null);
  const [agents, setAgents] = useState<Agent[]>([]);
  const [rules, setRules] = useState<Rule[]>([]);
  const [repositories, setRepositories] = useState<Repository[]>([]);
  const [runs, setRuns] = useState<RunSummary[]>([]);
  const [selectedRun, setSelectedRun] = useState("");
  const [selectedAgent, setSelectedAgent] = useState("");
  const [selectedRule, setSelectedRule] = useState("");
  const [selectedRepository, setSelectedRepository] = useState("");
  const [events, setEvents] = useState<LifecycleEvent[]>([]);
  const [journalLoading, setJournalLoading] = useState(false);
  const [cursor, setCursor] = useState("0");
  const [detail, setDetail] = useState<RunDetail | null>(null);
  const [message, setMessage] = useState("");
  const [eventType, setEventType] = useState("");
  const [eventSource, setEventSource] = useState("");
  const [eventSubject, setEventSubject] = useState("");
  const [busy, setBusy] = useState(false);
  const [railOpen, setRailOpen] = useState(false);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [connected, setConnected] = useState(false);
  const [problems, setProblems] = useState<string[]>([]);
  const snapshotRef = useRef<PanelSnapshot>({ state: null, agents: [], rules: [], repositories: [], runs: [], problems: [] });
  const selectedRunRef = useRef("");
  const historyRunRef = useRef("");
  const cursorRef = useRef("0");
  const subsRef = useRef<LiveOp[]>([]);
  const socketRef = useRef<WebSocket | null>(null);
  const transcriptRef = useRef<HTMLDivElement>(null);
  const activityRunRef = useRef(selectedRun);
  const activityRef = useRef<ActivityCursor | null>(null);

  selectedRunRef.current = selectedRun;
  cursorRef.current = cursor;

  async function refreshCatalog() {
    const [stateLoad, agentLoad, ruleLoad, repositoryLoad, runLoad] = await Promise.all([
      settle(getJSON<ControlState>("/api/state")),
      settle(getJSON<{ agents: Agent[] }>("/api/agents")),
      settle(getJSON<{ rules: Rule[] }>("/api/rules")),
      settle(getJSON<{ repositories: Repository[]; desired_error?: string }>("/api/repositories")),
      settle(getJSON<{ runs: RunSummary[] }>("/api/runs")),
    ]);
    const next = applyPanelLoad(snapshotRef.current, {
      state: stateLoad,
      agents: agentLoad,
      rules: ruleLoad,
      repositories: repositoryLoad,
      runs: runLoad,
    });
    snapshotRef.current = next;
    setState(next.state);
    setAgents(next.agents);
    setRules(next.rules);
    setRepositories(next.repositories);
    setRuns(next.runs);
    setProblems(next.problems);
  }

  function sendLive(op: LiveOp) {
    subsRef.current = subsRef.current.filter((item) => !(item.topic === op.topic && item.run_id === op.run_id));
    if (op.op === "subscribe") subsRef.current.push(op);
    const socket = socketRef.current;
    if (socket && socket.readyState === WebSocket.OPEN) {
      socket.send(JSON.stringify(op));
    }
  }

  useEffect(() => {
    let stopped = false;
    let delay = 400;
    let timer = 0;
    const connect = () => {
      const proto = location.protocol === "https:" ? "wss" : "ws";
      const socket = new WebSocket(`${proto}://${location.host}/api/live`);
      socketRef.current = socket;
      socket.onopen = () => {
        if (stopped) return;
        setConnected(true);
        delay = 400;
        for (const op of subsRef.current) socket.send(JSON.stringify(op));
      };
      socket.onmessage = (messageEvent) => {
        let frame: LiveFrame;
        try {
          frame = JSON.parse(String(messageEvent.data)) as LiveFrame;
        } catch {
          return;
        }
        if (frame.op === "event" && frame.event && frame.run_id === selectedRunRef.current && historyRunRef.current === frame.run_id) {
          setEvents((current) => mergeEvents(current, [frame.event!]));
          if (frame.cursor) {
            cursorRef.current = frame.cursor;
            setCursor(frame.cursor);
            const stored = subsRef.current.find((item) => item.topic === "run" && item.run_id === frame.run_id);
            if (stored) stored.after = frame.cursor;
          }
        } else if (frame.op === "run" && frame.run) {
          setRuns((current) => mergeRuns(current, frame.run!));
          const run = frame.run;
          if (run.run_id === selectedRunRef.current && historyRunRef.current === run.run_id && Number(run.last_seq) > Number(cursorRef.current)) {
            const after = cursorRef.current;
            void readJournal(
              (pageAfter, limit) => getJSON<EventsPage>(`/api/runs/${run.run_id}/events?after=${pageAfter}&limit=${limit}`),
              after,
            ).then((journal) => {
              if (historyRunRef.current !== run.run_id) return;
              setEvents((current) => mergeEvents(current, journal.events));
              setCursor(journal.cursor);
              cursorRef.current = journal.cursor;
            }).catch(() => undefined);
          }
        } else if (frame.op === "state") {
          void refreshCatalog();
        }
      };
      socket.onclose = () => {
        setConnected(false);
        if (stopped) return;
        timer = window.setTimeout(connect, delay);
        delay = Math.min(delay * 2, 5000);
      };
    };
    connect();
    sendLive({ op: "subscribe", topic: "state" });
    sendLive({ op: "subscribe", topic: "runs" });
    return () => {
      stopped = true;
      window.clearTimeout(timer);
      socketRef.current?.close();
    };
  }, []);

  useEffect(() => {
    refreshCatalog().finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    if (!selectedRun) {
      historyRunRef.current = "";
      setJournalLoading(false);
      setEvents([]);
      setDetail(null);
      setCursor("0");
      cursorRef.current = "0";
      return;
    }
    const runID = selectedRun;
    let cancelled = false;
    historyRunRef.current = "";
    setJournalLoading(true);
    setEvents([]);
    setCursor("0");
    cursorRef.current = "0";
    const detailPromise = getJSON<RunDetail>(`/api/runs/${runID}`);
    loadJournal(
      (after, limit) => getJSON<EventsPage>(`/api/runs/${runID}/events?after=${after}&limit=${limit}`),
      detailPromise.then((detail) => ({ lastSeq: Number(detail.last_seq) || 0, eventCount: detail.event_count })),
    )
      .then(async (journal) => {
        if (cancelled) return;
        const nextDetail = await detailPromise;
        if (cancelled) return;
        setEvents(journal.events);
        setCursor(journal.cursor);
        cursorRef.current = journal.cursor;
        setDetail(nextDetail);
        historyRunRef.current = runID;
        sendLive({ op: "subscribe", topic: "run", run_id: runID, after: journal.cursor });
      })
      .catch((reason: unknown) => {
        if (!cancelled) setError(reason instanceof Error ? reason.message : "Failed to load run");
      })
      .finally(() => {
        if (!cancelled) setJournalLoading(false);
      });
    return () => {
      cancelled = true;
      historyRunRef.current = "";
      sendLive({ op: "unsubscribe", topic: "run", run_id: runID });
    };
  }, [selectedRun]);

  useTranscriptFollow(transcriptRef, events, selectedRun);

  if (activityRunRef.current !== selectedRun) {
    activityRunRef.current = selectedRun;
    activityRef.current = null;
  }
  const activity = continueActivity(activityRef.current, events);
  activityRef.current = activity;
  const lines = activityView(activity);

  const rule = rules.find((item) => item.name === selectedRule);
  const agent = agents.find((item) => item.id === (selectedAgent || detail?.agent || rule?.agent));
  const repository = repositories.find((item) => item.id === selectedRepository);
  const repositoryLayerActive = Boolean(state?.desired?.repositories_active || state?.active?.repositories_active);
  const visibleRuns = runs.filter((run) => {
    if (selectedRule && run.rule !== selectedRule) return false;
    if (selectedAgent && run.agent !== selectedAgent) return false;
    return true;
  });

  async function onSync() {
    setBusy(true);
    setNotice("");
    try {
      const result = await postText("/api/sync", "application/json", JSON.stringify({ scope: ["agents", "rules", "repos"] }));
      setNotice(repositorySyncNotice(result.status, result.text));
      await refreshCatalog();
    } catch (reason) {
      setNotice(reason instanceof Error ? reason.message : "Sync failed");
    } finally {
      setBusy(false);
    }
  }

  async function onSubmit(event: Event) {
    event.preventDefault();
    const text = message.trim();
    if (!text) return;
    setBusy(true);
    setNotice("");
    try {
      const body = buildMessage({
        message: text,
        rule: selectedRule || undefined,
        type: eventType || undefined,
        source: eventSource || undefined,
        subject: eventSubject || undefined,
      });
      const result = await postText("/api/messages", "application/json", JSON.stringify(body));
      if (result.status === 202) {
        const parsed = JSON.parse(result.text) as { runs?: Array<{ run_id: string }> };
        const count = parsed.runs?.length ?? 0;
        setNotice(count === 1 ? "Accepted 1 run." : `Accepted ${count} runs.`);
        setMessage("");
        if (parsed.runs && parsed.runs[0]) setSelectedRun(parsed.runs[0].run_id);
        await refreshCatalog();
      } else if (result.status === 204) {
        setNotice("No active rule matched. Sync a draft rule, or set type and source to an active match.");
      } else if (result.status === 503) {
        setNotice("Sync is in progress. Send the message again in a moment.");
      } else {
        setNotice(result.text.trim() || `Listener returned ${result.status}`);
      }
    } catch (reason) {
      setNotice(reason instanceof Error ? reason.message : "Send failed");
    } finally {
      setBusy(false);
    }
  }

  function chooseRule(name: string) {
    const next = rules.find((item) => item.name === name);
    setSelectedRule(name);
    setSelectedAgent("");
    setSelectedRepository("");
    setEventType(next?.match.type ?? "");
    setEventSource(next?.match.source ?? "");
    setEventSubject(next?.match.subject ?? "");
    setRailOpen(false);
  }

  return (
    <div class={`shell ${railOpen ? "show-rail" : ""} ${drawerOpen ? "show-drawer" : ""}`}>
      <header class="topbar">
        <button type="button" class="text" onClick={() => setRailOpen((open) => !open)} aria-expanded={railOpen}>
          Config
        </button>
        <strong>Genesis</strong>
        <span class={`pill drift-${state?.drift ?? "unknown"}`}>{driftLabel(state?.drift)}</span>
        <span class="mono digest" title={state?.active?.digest || ""}>
          active {shortDigest(state?.active?.digest)}
        </span>
        <span class="mono digest" title={state?.desired?.digest || ""}>
          draft {shortDigest(state?.desired?.digest)}
        </span>
        <button type="button" onClick={onSync} disabled={busy || !state?.listener.reachable || state.listener.syncing}>
          {state?.listener.syncing ? "Syncing" : "Sync"}
        </button>
        <span class={`live ${connected ? "on" : ""}`}>{connected ? "live" : "reconnecting"}</span>
        <button type="button" class="text" onClick={() => setDrawerOpen((open) => !open)} aria-expanded={drawerOpen}>
          Details
        </button>
      </header>
      {error ? <p class="banner">{error}</p> : null}
      {problems.map((problem) => (
        <p class="banner" key={problem}>{problem}</p>
      ))}
      {state?.desired_error ? <p class="banner">{state.desired_error}</p> : null}
      {state?.drift === "generation_too_large" && state.listener.error ? <p class="banner">{state.listener.error}</p> : null}
      {state && !state.listener.reachable ? (
        <p class="banner">The listener is unreachable. Journals can still be read. Sync and new messages wait until it returns.</p>
      ) : null}
      {state?.listener.reachable && !state.listener.sync_configured ? (
        <p class="banner">Sync is disabled. The panel has no token to send, and it will not invent one.</p>
      ) : null}

      <div class="body">
        <aside class="rail">
          <section>
            <h2>Agents</h2>
            {agents.length === 0 ? (
              <p class="empty">
                {problems.some((item) => item.toLowerCase().includes("agent"))
                  ? "Agents could not be loaded."
                  : "No agents in the config directory."}
              </p>
            ) : null}
            {agents.map((item) => (
              <button
                type="button"
                key={item.id}
                class={item.id === (selectedAgent || agent?.id) ? "row selected" : "row"}
                onClick={() => {
                  setSelectedAgent(item.id);
                  setSelectedRule("");
                  setSelectedRepository("");
                  setRailOpen(false);
                }}
              >
                <span>{item.id}</span>
                <em class={`presence presence-${item.presence}`}>{presenceLabel(item.presence)}</em>
              </button>
            ))}
          </section>
          <section>
            <h2>Rules</h2>
            {rules.length === 0 ? (
              <p class="empty">
                {problems.some((item) => item.toLowerCase().includes("rule"))
                  ? "Rules could not be loaded."
                  : "No rules in the config directory."}
              </p>
            ) : null}
            {rules.map((item) => (
              <button
                type="button"
                key={item.name}
                class={item.name === selectedRule ? "row selected" : "row"}
                onClick={() => chooseRule(item.name)}
              >
                <span>
                  <strong>{item.match.type || item.name}</strong>
                  <small>{item.name} → {item.agent}</small>
                </span>
                <em class={`presence presence-${item.presence}`}>{presenceLabel(item.presence)}</em>
              </button>
            ))}
          </section>
          <section>
            <h2>Repositories</h2>
            {repositories.length === 0 ? (
              <p class="empty">
                {problems.some((item) => item.toLowerCase().includes("repositor"))
                  ? "Repositories could not be loaded."
                  : state?.desired_error
                    ? "Desired repositories could not be validated."
                    : repositoryLayerActive
                      ? "No repositories in repos.d."
                      : "repos.d is absent. Declarations stay inactive until that directory exists."}
              </p>
            ) : null}
            {repositories.map((item) => (
              <button
                type="button"
                key={item.id}
                class={item.id === selectedRepository ? "row selected" : "row"}
                onClick={() => {
                  setSelectedRepository(item.id);
                  setSelectedAgent("");
                  setSelectedRule("");
                  setRailOpen(false);
                  setDrawerOpen(true);
                }}
              >
                <span>
                  <strong>{repositoryLabel(item)}</strong>
                  <small>{item.id} · {repositoryPolicy(item)}</small>
                </span>
                <em class={`presence presence-${item.presence}`}>{presenceLabel(item.presence)}</em>
              </button>
            ))}
          </section>
        </aside>

        <main class="center">
          {loading ? <p class="empty">Loading the control panel…</p> : null}
          {!loading && selectedRun ? (
            <>
              <div class="center-head">
                <button type="button" class="text back" onClick={() => setSelectedRun("")}>
                  Back
                </button>
                <span class="mono">{detail?.agent || selectedRun}</span>
              </div>
              <div class="transcript" ref={transcriptRef}>
                {journalLoading ? <p class="empty">Loading the conversation…</p> : null}
                {!journalLoading && events.length === 0 ? <p class="empty">This run has no journal lines yet.</p> : null}
                {lines.map((line) => (
                  <ActivityLine key={line.key || line.text} line={line} />
                ))}
              </div>
            </>
          ) : null}
          {!loading && !selectedRun ? (
            <>
              <div class="center-head">
                <span>Activity</span>
                {selectedRule || selectedAgent ? (
                  <button
                    type="button"
                    class="text"
                    onClick={() => {
                      setSelectedRule("");
                      setSelectedAgent("");
                      setSelectedRepository("");
                    }}
                  >
                    Clear filter
                  </button>
                ) : null}
              </div>
              <div class="activity">
                {visibleRuns.length === 0 ? (
                  <p class="empty">
                    {problems.some((item) => item.toLowerCase().includes("run"))
                      ? "Runs could not be loaded."
                      : "No runs yet. Choose a rule and send a message. The active generation is what matches."}
                  </p>
                ) : null}
                {visibleRuns.map((run) => (
                  <button type="button" key={run.run_id} class="run" onClick={() => setSelectedRun(run.run_id)}>
                    <span class={`state state-${run.state}`}>{run.state}</span>
                    <span>
                      <strong>{run.agent}</strong>
                      <small>{run.rule} · {run.cause_type || "event"}</small>
                    </span>
                    <span class="mono">{run.run_id.slice(0, 12)}</span>
                  </button>
                ))}
              </div>
            </>
          ) : null}
        </main>

        <aside class="drawer">
          <h2>Details</h2>
          {detail && selectedRun ? (
            <dl>
              <dt>Run</dt>
              <dd class="mono">{detail.run_id}</dd>
              <dt>State</dt>
              <dd>{detail.state}</dd>
              <dt>Agent</dt>
              <dd>{detail.agent}</dd>
              <dt>Rule</dt>
              <dd>{detail.rule}</dd>
              <dt>Session</dt>
              <dd class="mono">{detail.session_id || "—"}</dd>
              <dt>Cause</dt>
              <dd>{detail.cause_type || "—"} {detail.cause_id || ""}</dd>
              <dt>Finish</dt>
              <dd>{detail.finish_reason || "—"}</dd>
              <dt>Error</dt>
              <dd>{detail.error || "—"}</dd>
            </dl>
          ) : (
            <p class="empty">Select a run to see its journal summary.</p>
          )}
          {repository ? (
            <section>
              <h3>{repositoryLabel(repository)}</h3>
              <p>{repositoryObservationSummary(repository)}</p>
              <p class="notice">Catalog entry is {presenceLabel(repository.presence)}. Drift compares the active generation with the last observation.</p>
              <dl>
                <dt>Id</dt>
                <dd class="mono">{repository.id}</dd>
                <dt>Provider</dt>
                <dd>{repository.provider}</dd>
                <dt>Policy</dt>
                <dd>{repositoryPolicy(repository)}</dd>
                <dt>Visibility</dt>
                <dd>{repository.settings.visibility}</dd>
                <dt>Branch</dt>
                <dd class="mono">{repository.settings.default_branch}</dd>
                <dt>Actions</dt>
                <dd>{repository.actions.enabled ? repository.actions.allowed : "disabled"}</dd>
                <dt>Bootstrap</dt>
                <dd class="mono">{repository.bootstrap?.template || "none"}</dd>
                <dt>Secrets</dt>
                <dd>{secretNames(repository).join(", ") || "none"}</dd>
                <dt>Ruleset</dt>
                <dd>{repository.protection?.ruleset.name || "none"}</dd>
                <dt>Identities</dt>
                <dd>
                  {repository.identities?.map((identity) => `${identity.name} (${identity.role})`).join(", ") || "none"}
                </dd>
              </dl>
              {repository.settings.description ? <p>{repository.settings.description}</p> : null}
              {repository.observed ? (
                <>
                  <h3>Observed</h3>
                  <dl>
                    <dt>Repository</dt>
                    <dd class="mono">{repository.observed.org}/{repository.observed.name}</dd>
                    <dt>Repo id</dt>
                    <dd class="mono">{repository.observed.repository_id}</dd>
                    <dt>Node</dt>
                    <dd class="mono">{repository.observed.node_id}</dd>
                    <dt>Visibility</dt>
                    <dd>{repository.observed.visibility}</dd>
                    <dt>Branch</dt>
                    <dd class="mono">{repository.observed.default_branch || "none"}</dd>
                    <dt>Archived</dt>
                    <dd>{repository.observed.archived ? "yes" : "no"}</dd>
                    <dt>Actions</dt>
                    <dd>{repository.observed.actions_status === "observed" ? repository.observed.actions_allowed || "observed" : repository.observed.actions_status}</dd>
                    <dt>Rulesets</dt>
                    <dd>{repository.observed.ruleset_status}</dd>
                  </dl>
                  {repository.observed.description ? <p>{repository.observed.description}</p> : null}
                </>
              ) : null}
              {repository.drift && repository.drift.length > 0 ? (
                <>
                  <h3>Drift</h3>
                  <ul class="drift-list">
                    {repository.drift.map((item) => (
                      <li key={`${item.field}-${item.status}`} class={`drift-${item.status}`}>{driftLine(item)}</li>
                    ))}
                  </ul>
                </>
              ) : null}
            </section>
          ) : null}
          {agent ? (
            <section>
              <h3>{agent.id}</h3>
              <p class="instructions">{agent.instructions}</p>
              <p>{agent.user ? `OS user ${agent.user}` : "Shared listener UID"}</p>
              <p class="mono path">{agent.cwd}</p>
              <p class="mono path">{agent.home}</p>
              <p>Secrets: {agent.secrets.length ? agent.secrets.join(", ") : "none"}</p>
              {agent.github ? <p>{githubGrantSummary(agent)}</p> : null}
            </section>
          ) : null}
          {rule ? (
            <section>
              <h3>{rule.name}</h3>
              <ul>
                {Object.entries(rule.match).map(([key, value]) => (
                  <li key={key}>
                    <span class="mono">{key}</span> {value}
                  </li>
                ))}
              </ul>
            </section>
          ) : null}
          {detail?.stderr_tail ? <pre>{detail.stderr_tail}</pre> : null}
        </aside>
      </div>

      <form class="composer" onSubmit={onSubmit}>
        {notice ? <p class="notice">{notice}</p> : null}
        <div class="fields">
          <label>
            Type
            <input value={eventType} onInput={(event) => setEventType(event.currentTarget.value)} placeholder="from the selected rule" />
          </label>
          <label>
            Source
            <input value={eventSource} onInput={(event) => setEventSource(event.currentTarget.value)} placeholder="urn:genesis:control" />
          </label>
          <label>
            Subject
            <input value={eventSubject} onInput={(event) => setEventSubject(event.currentTarget.value)} />
          </label>
        </div>
        <div class="send-row">
          <textarea
            value={message}
            onInput={(event) => setMessage(event.currentTarget.value)}
            placeholder={selectedRule ? `Message for ${selectedRule}` : "Message. Select a rule so the active match is filled in."}
            rows={3}
          />
          <button type="submit" disabled={busy || !message.trim()}>
            Send
          </button>
        </div>
      </form>
    </div>
  );
}
