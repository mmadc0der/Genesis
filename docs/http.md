# HTTP

The listener accepts CloudEvents and authorized sync. The control process
proxies some of those calls and reads journals. Control routes and the Wire
are in [control.md](control.md). GitHub App ingress is in
[webhooks.md](webhooks.md). Cache and failure semantics for `/sync` are in
[orchestrator.md](orchestrator.md).

`genesis control` reads agent and rule files, reads run journals, and
proxies CloudEvents plus authorized `POST /sync` to the listener. The sync
token is not sent to the browser. `GET /health` and `GET /generation` are
read-only views of the listener. The Wire UI has no message composer or
Sync button yet. To message the shared-UID designer, `POST /api/messages`
with `{"rule":"designer.yaml", "message":"..."}`. After it writes YAML,
`POST /api/sync` with `{}`. See [designer.md](designer.md).

## POST /events and run results

`POST /events` accepts CloudEvents 1.0 in structured JSON format:

```sh
curl -i http://127.0.0.1:8787/events \
  -H 'Content-Type: application/cloudevents+json' \
  --data '{
    "specversion": "1.0",
    "id": "example-1",
    "source": "urn:genesis:example",
    "type": "dev.genesis.run",
    "subject": "hello",
    "data": {"task": "Inspect the workspace and summarize it."}
  }'
```

A match returns `202` immediately with one Genesis run ID per rule. Genesis
creates that run's directory under `-data` (default `genesis-data`) **before**
the `202`:

```json
{
  "runs": [
    {
      "rule": "example.yaml",
      "agent": "workspace-janitor",
      "run_id": "gen_0123456789abcdef0123456789abcdef"
    }
  ]
}
```

No match returns `204`. `POST /webhooks/github` is the GitHub App ingress
described in [webhooks.md](webhooks.md). A verified delivery for a
persisted repository becomes one CloudEvent on this same matcher.

Each accepted match snapshots the resolved agent into
its invocation. Each one-shot runner uses a Genesis-owned `dsh_home` at
`-data/sessions/<root run id>/dsh_home`. The first run of a chain mints that
home. A continuation copies the same path and DeepSeek session id onto a new
`gen_` run and does not create a second home. The runner invokes
`provider="deepseek-official"`, model `deepseek-flash`, and profile
`sdk-minimal`, and **retains** that home (including DeepSeek session JSONL)
after the child exits. `dsh_home` is not the agent `home`. The compact JSON
serialization of the complete CloudEvent is the sole session user message,
except a continuation, which passes `data.message` as the follow-up text. Instructions are standing identity, not
prepended onto that payload. For the pinned `0.1.5rc1` profile, the runner also
supplies a per-run Cordis patch setting `session-log-deepseek.enabled: false`;
canonical DeepSeek API requests therefore do not upload or append session-trace
suffixes. The same patch replaces the `llm-deepseek` row
(`@deepseek-ai/dsh-llm-deepseek`) because a Cordis id patch replaces that
row's whole config. It restates `apiKeyEnv: DEEPSEEK_API_KEY`, the
`DSH_CONTEXT_WINDOW` context-window expression (otherwise 1,000,000), and
`streamIdleTimeoutMs: 172800000`, and sets the provider `retryPolicy` to
normal mode with `maxRetries: 10` and `backoff.maxDelayMs: 60000`
(initial delay 500 ms, jitter 0.1). Omitting `retryPolicy` on this pin
resolves `maxRetries: 5` and `maxDelayMs: 10000` with the same initial
delay and jitter. Eligible codes stay omitted, so the harness defaults
remain, including `TRANSPORT`. When that budget is exhausted the turn
still fails, and `llm/retry` events are still appended.

The embedded Python runner writes flushed NDJSON on stdout: `session.created`
as soon as `start_session()` returns, then raw SDK `on_notification` frames,
then exactly one `result`. Go stream-reads that pipe while the child runs,
maps root-session trajectory notifications into Genesis lifecycle CloudEvents,
and appends them to `events.jsonl` before in-process fan-out. Those records
are observations, not ingress: do not POST them back to `/events`.

```
<data>/sessions/<root gen_>/dsh_home/   # retained SDK home / session JSONL
<data>/runs/gen_<hex>/
  events.jsonl    # CloudEvents 1.0 lifecycle records
  stderr.log
  result.json     # terminal runner object, redacted
  session.json    # session id, dsh_home, correlation id, continued_from
```

Lifecycle types are `dev.genesis.run.accepted`, `start`, `session.created`,
`turn`, `tool`, `assistant`, `retry`, `restart`, `chunk`, `result`, `error`, and `end`.
Each event has a unique `evt_` id, a per-run `sequence`, `time`, run/agent/rule
identity, optional `sessionid`, and causation (`causeid` / `causesource` /
`causetype`) to the incoming CloudEvent. Turn, tool, assistant, retry, and
chunk payloads keep a `raw` SDK notification. The same journal redactor runs
before the line is appended. `assistant/message`, `assistant/attempt`, and
`llm/retry` are journaled with turn and tool rows. `on_chunk` is journaled
when the runtime forwards it. The control panel reads this file through
`GET /api/runs/{id}/events` and the `/api/live` WebSocket; there is no second
transport. A non-completed finish publishes the wrapper
`DeepSeek run finished with finish_reason=…` error only when the journal has
no `turn/end` failure object (`reason.error`).

The per-run Cordis patch still disables `session-log-deepseek` and sets
the DeepSeek retry budget described above. It also
inserts `cmd/genesis/assistant_stream_plugin.mjs` by absolute path. On
`deepseek-harness` `0.1.5rc1` that local file loads inside `dsh`. The plugin
subscribes to Cordis `agent/assistant-stream` and writes JSON-RPC `on_chunk`
on the same stdout as `session.event`. The frame is unchanged, plus
`sessionId` copied from `agent.session.id` when the frame does not already
have one, which is what the published Python client requires before it will
deliver the notification. The pinned SDK does not emit `assistant/chunk`.
`text-delta`, `reasoning-delta`, and `tool-call-delta` travel as `on_chunk`.
`usage` and `finish` frames are stored and are not painted as tokens.

Genesis emits JSON logs. The completion record contains `genesis_run_id`,
`rule`, `agent`, `run_dir`, `deepseek_session_id`, `finish_reason`,
`final_response`, `error_type`, `error`, `diagnostics`, and captured runner
`stderr`. Any finish reason other than `completed` is logged at `ERROR`, even
when the SDK returned no explicit exception. Diagnostics include the relevant
`turn/end`, related error events, and non-session notifications. Exceptions
and runner stderr are retained. A failed run stays asynchronous and is not
started again, except a `turn/end` failure whose error code is `TRANSPORT`.
That failure journals `dev.genesis.run.restart` and starts another run on the
same DSH session and `dsh_home`. The new run is not a
`dev.genesis.session.continue` event and does not append a user message; the
resume plugin drops the SDK prompt. If the persisted session already ends on
`turn/end`, it acks `session/prompt` immediately so the Python SDK's
`session.run` can wait for `session.status` idle; otherwise it wakes the
in-flight turn and acks the prompt when `turn/end` is appended. Three such
restarts in a row are the limit. The failed run does not emit
`dev.genesis.agent.finished` when a restart is accepted. Model-request retries
stay inside the harness policy above.
Secret values never appear in journals, slog, or `stderr.log`.

## POST /sync

`POST /sync` reloads the requested YAML layers into the listener cache and
asks the inherited privileged coordinator (when `genesis launch` attached
one) to evaluate a typed host plan. It does not scan the filesystem on
`POST /events`.

```sh
curl -i http://127.0.0.1:8787/sync \
  -H "Authorization: Bearer $GENESIS_SYNC_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"scope":["agents","rules","repos"]}'
```

`scope` is optional. Omitted, it rereads agents, rules, and repositories.
`["rules"]`, `["agents"]`, or `["repos"]` rereads only that directory.
That is not host/matcher atomicity: a rules-only sync can activate new rules
against previously cached agents while a newly written agent file stays
invisible. Invalid YAML or a broken privileged socket leaves the previous
cache in place. A repository sync validates `repos.d` and records a plan; it
does not create, adopt, or edit a GitHub repository. Invalid repository YAML
returns `repositories are invalid` and keeps the previous cache.

Dedicated `user` accounts are reconciled only on a full or
agents sync, and only when `genesis launch` is root. If that recon fails, the
matcher cache is not swapped. Removing an agent file does not `userdel` or
delete homes. Shared-UID agents still report `unsupported` cwd/home and env
diffs; Genesis still does not install packages.

While sync runs, `POST /events` returns `503` with `Retry-After: 1`. A second
sync returns `409`. In-flight runs keep the snapshot they were accepted with.

Bearer auth is a local operator token, not a sandbox. Dedicated agents run
as their reconciled OS user. Shared-UID agents still share the listener UID.
Leaving the token empty disables `/sync` (`401` `sync is disabled`); Genesis does not invent one.

The control process exposes the same reload as `POST /api/sync` and adds the
bearer itself. See [control.md](control.md).
