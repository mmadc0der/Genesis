# Control panel

`genesis control` is a separate process from `genesis listen` / `genesis launch`.
It serves the operator UI and a small HTTP API. It does not match rules, start
runs, or own the journal. The listener still does.

```
browser  -- REST + WebSocket -->  genesis control  -- POST /events, POST /sync -->  listener
                                      |  reads YAML                         ^
                                      |  reads events.jsonl                 |
                                      v                                     |
                                 genesis-config (read/write)          genesis-data (read-only)
```

The sync bearer stays in the control process. Browser requests never send or
receive it. An empty token leaves `/sync` disabled; the panel does not invent
one.

## Run it

Start the listener first, then the panel. Both default to loopback.

```sh
export GENESIS_SYNC_TOKEN='replace-with-a-local-sync-token'
uv run --locked ./bin/genesis launch \
  -listen 127.0.0.1:8787 -agents agents.d -rules rules.d -repos repos.d -data genesis-data

./bin/genesis control \
  -listen 127.0.0.1:8790 \
  -listener http://127.0.0.1:8787 \
  -agents agents.d -rules rules.d -repos repos.d -providers providers.d -data genesis-data \
  -web web/dist
```

Build the UI with `npm ci --prefix web && npm run build --prefix web`.
`npm run dev --prefix web` serves Vite on `127.0.0.1:4179` and proxies `/api`
to the control process. The shipped UI is the files the Go process serves.

`-data` must already exist. Control opens it read-only: it does not create
run directories, chmod them, or truncate journals. A torn last JSONL line is
skipped and left on disk for the listener.

## Listener reads

These are the only listener routes added for the panel. They do not reload
YAML or swap the cache.

| Method | Path | Body |
|---|---|---|
| `GET` | `/health` | `{"ok":true}` |
| `GET` | `/generation` | active agents, rules, repositories, digest, `repositories_active`, `sync_configured`, `syncing` |

`sync_configured` is a boolean. The token is not in the payload.

## Control API

Durable reads and commands are REST. Live updates are `GET /api/live`
WebSocket text frames. Replay is a file read. If the socket drops, reconnect
and pass `after` set to the last `sequence` you applied; the journal is
authoritative and the socket is not.

| Method | Path | Behavior |
|---|---|---|
| `GET` | `/api/health` | control process is up |
| `GET` | `/api/state` | desired files vs active generation (`in_sync`, `draft`, `listener_unavailable`, `desired_invalid`, `generation_too_large`) |
| `GET` | `/api/agents` | agents from disk union the active cache, with `presence` |
| `GET` | `/api/agents/{id}` | one agent |
| `GET` | `/api/rules` | rules, same presence rules |
| `GET` | `/api/rules/{file}` | one rule |
| `GET` | `/api/repositories` | repositories from disk union the active cache, with `presence` and `repositories_active` |
| `GET` | `/api/repositories/{id}` | one repository |
| `GET` | `/api/runs?limit=` | recent run summaries from `events.jsonl` |
| `GET` | `/api/runs/{id}` | summary, event count, redacted `result.json`, stderr tail |
| `GET` | `/api/runs/{id}/events?after=&limit=` | journal lines with `sequence` greater than `after` |
| `POST` | `/api/events` | proxy a CloudEvent to the listener; no bearer added |
| `POST` | `/api/messages` | build a CloudEvent from `{message, rule?, type?, source?, subject?}` and proxy it |
| `POST` | `/api/sync` | proxy to listener `POST /sync` and attach the bearer there |

`presence` is `active` (file matches the cache), `draft` (file differs or is
new), `active_only` (cached, file gone), or `unknown` (listener not readable).
Desired state remains the YAML files. The panel does not edit them. Agent
records include optional `user` and `setup` from those files. Invalid
desired YAML does not hide the active cache or the journals: `/api/state`
still returns the active generation when the listener can, `/api/agents`, `/api/rules`, and `/api/repositories` return that cache with
`desired_error` and HTTP 200, and `/api/runs` is read from disk either way.
The UI keeps state, repositories, and runs when an agent, rule, or repository
request fails. Repository records contain secret names only. When the
observation journal digest matches the active repository list, each record
also includes `observation`, the redacted `observed` GitHub view, and
`drift` of that active declaration against the last read. A stale or
missing journal omits those fields. The panel shows the observed id and
the drift list. A repos sync may apply adopted repository settings,
Actions, the declared ruleset, and missing bootstrap files.
`remote_mutation` is `applied` only for intents that sync wrote.
Provider identity files are not served. Agent grants, when
present, show repository, git access, allowlisted permissions, and whether
a credential is pending or unnecessary. They do not show provider identity
records, secret references, or SSH private keys. Run stderr and result
bodies are stripped of OpenSSH private-key blocks before they are served.
A ready deploy key is reported on sync `material` and can be delivered as
a per-run SSH socket. A changed or refused grant is not left deliverable.
The panel does not show App ids, secret references,
private keys, or a run's `GITHUB_TOKEN`. Sync `credential_active` stays
false. That flag may be true only while root has delivered the token into
the run process, and journals redact installation-token text before it is
served. See
[providers.md](providers.md).
`sync_configured` and `syncing` are listener facts on `active` and
`listener`; a file snapshot omits them.

`/api/messages` copies every string attribute in the named rule's `match`,
then applies non-empty type, source, and subject overrides. The message
becomes `data.message`. `specversion` must be absent or `1.0`. A `data` match
key, or an empty `id`, `type`, or `source`, is rejected before the listener
sees the event. Other top-level match keys, including a caller-chosen `id`,
are copied so the active rule can match them. The listener still matches only
the active generation, so a draft rule does not match until sync.

The committed `designer.yaml` rule is the dedicated user-message match
(`dev.genesis.user.message` / `urn:genesis:control` / `subject: designer`).
Select it in the panel. The designer agent has no `user`; Details shows
Shared listener UID. After it writes YAML, click Sync. See
[designer.md](designer.md).

`POST /api/sync` always requires `Content-Type: application/json` and one JSON
object (`{}` selects agents, rules, and repos). A missing media type is 415.
Arrays, null, strings, empty bodies, unknown fields, trailing values, and
unknown scope names are 400 and are not forwarded. `repos` is a scope name.
See [repositories.md](repositories.md).

`GET /generation` is read up to 32 MiB. A larger body is
`generation_too_large` with the listener still marked reachable. A connection
failure stays `listener_unavailable`.

Every control request, including static files and the WebSocket upgrade, must
use a loopback `Host` (`127.0.0.1`, `localhost`, or `::1`, with or without a
port). When `Origin` is present its host must be loopback too. Anything else
is 403. Curl with no `Origin` is allowed when `Host` is loopback.

### WebSocket

Client frames:

```json
{"op":"subscribe","topic":"run","run_id":"gen_…","after":"4"}
{"op":"subscribe","topic":"runs"}
{"op":"subscribe","topic":"state"}
{"op":"unsubscribe","topic":"run","run_id":"gen_…"}
{"op":"ping"}
```

Server frames are `event` (one journal line), `run` (a summary whose state or
sequence changed), `state` (digests and drift; refetch REST for the full
document), `pong`, and `error`. Event frames exist only because the process
re-read `events.jsonl`. They are not a second bus.

The upgrade also requires a loopback `Host`, and a present `Origin` must be
loopback (`127.0.0.1`, `localhost`, `[::1]`). There is
no other authentication. Do not publish the control port beyond localhost.
Inside Compose the process listens on `0.0.0.0` so Docker can forward it, and
the published binding is `127.0.0.1:8790`.

## What this does not do

- No live token stream. The pinned SDK does not emit `assistant/chunk` on the
  runner pipe. Chat text is the settled `result` line.
- No database, SSE channel, or external broker.
- No YAML form editor, charts, or component library.
- No cancel, retry, queue, or run timeout.
- No write to `genesis-data`. Listener recovery still appends `interrupted`
  and `end` for dead in-flight journals.
- Control in another PID or container namespace does not treat a recorded pid
  as alive or dead. A journal without `end` is `open`.
