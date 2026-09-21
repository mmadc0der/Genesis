# Genesis

Genesis accepts structured CloudEvents and asynchronously starts one DeepSeek
Harness session for every matching YAML rule. It builds one executable:
`genesis`.

`genesis` and `genesis listen` are the unprivileged event listener.
`genesis launch` starts that listener as a child and supervises it over a
private inherited Unix socketpair used only for typed privileged
coordination. See [docs/orchestrator.md](docs/orchestrator.md) for the exact
cache, `/sync`, and failure semantics.

Each session runs through the official Python SDK with the standalone
`sdk-minimal` profile. `pyproject.toml` and the committed `uv.lock` pin the
official `deepseek-harness-sdk` and `deepseek-harness-runtime-bin` packages to
the same `0.1.5rc1` release. The runtime wheel supplies `dsh`; no `dsh`
executable is looked up in `PATH`.

Who runs is a file-backed agent in `agents.d/`. A rule only selects that
identity. `cwd` is the SDK workspace and `home` is the child process Unix
`HOME`. They are a workspace/environment split under the shared Genesis UID,
not a security sandbox. Genesis does not switch OS users and does not apply
root host diffs; current YAML cannot name OS users or packages.

## Install and run

Go 1.23+, Python 3.10+, Node.js 22+, and
[`uv`](https://docs.astral.sh/uv/getting-started/installation/) are required.

```sh
uv sync --locked
export DEEPSEEK_API_KEY='replace-with-a-real-key'
export GENESIS_SYNC_TOKEN='replace-with-a-local-sync-token'

mkdir -p bin
go build -o bin/genesis ./cmd/genesis
uv run --locked ./bin/genesis launch \
  -listen 127.0.0.1:8787 -agents agents.d -rules rules.d -data genesis-data
```

`genesis -listen ...` still runs the listener in this process. `launch` is
the supervisor: it inherits no extra HTTP port and keeps a private socketpair
for privileged coordination. `-data` (default `genesis-data`) is the per-run
journal root; an invalid path refuses to listen. An empty `-sync-token` /
`GENESIS_SYNC_TOKEN` disables `POST /sync` (`401` `sync is disabled`); set a
token to activate filesystem edits. When `launch` runs as root it requires
`-listener-user` / `GENESIS_LISTENER_USER` and execs the listener as that
user. Non-root launch keeps the current user and does not need that flag.

`uv run` places the managed `.venv` first in `PATH`. Equivalently, activate it
with `. .venv/bin/activate` before running `./bin/genesis`. Genesis resolves
`python3` once at startup and embeds its one-shot Python runner in the Go
binary.

## Control panel

`genesis control` is a second process. It serves a small Preact UI and REST
API on `127.0.0.1:8790`, reads agent/rule files, reads run journals, and
proxies CloudEvents plus authorized `POST /sync` to the listener. The sync
token is not sent to the browser. Node is required only to build the UI.
See [docs/control.md](docs/control.md).

```sh
npm ci --prefix web
npm run build --prefix web
./bin/genesis control \
  -listen 127.0.0.1:8790 \
  -listener http://127.0.0.1:8787 \
  -agents agents.d -rules rules.d -data genesis-data \
  -web web/dist
```

Open `http://127.0.0.1:8790/`. The listener stays on `127.0.0.1:8787`.
`GET /health` and `GET /generation` are read-only views of that process.

## Agents

One agent lives in each `.yaml` or `.yml` file. The filename stem is the agent
ID (`workspace-janitor.yaml` → `workspace-janitor`):

```yaml
instructions: |
  You are the workspace janitor for this lab.
  Read the CloudEvent, do only the work it asks, write results in the workspace, then stop.
cwd: /tmp
home: /tmp/genesis-home
env:
  PATH: /usr/local/bin:/usr/bin:/bin
  LANG: C.UTF-8
```

`instructions`, `cwd`, and `home` are required. `cwd` and `home` must be
absolute and distinct. `env` is the complete non-secret environment given to
that agent's Harness runtime; omitted, it is empty. Genesis then adds named
secrets from its own startup environment (default `DEEPSEEK_API_KEY` when the
value is non-empty), sets `HOME` from `home`, and sets `DSH_SYSTEM_PROMPT`
from `instructions` for the pinned `sdk-minimal` persona hook. Declaring
`HOME`, `DSH_SYSTEM_PROMPT`, or any listed secret in `env` is rejected.
Secret values never appear in YAML. Duplicate IDs, duplicate secret names,
unknown fields, and invalid filename stems are rejected.

Several rules may point at the same agent. Concurrent matches share `cwd` and
`home` and may race; Genesis does not queue them.

## Rules

One rule lives in each `.yaml` or `.yml` file:

```yaml
match:
  type: dev.genesis.run
  source: urn:genesis:example
  subject: hello
agent: workspace-janitor
```

`match` entries are exact, case-sensitive comparisons against top-level string
CloudEvent attributes. `agent` is a required agent ID. Every matching rule
runs, in lexical filename order. The listener caches agents and rules at
start; later filesystem edits are inactive until an authorized `POST /sync`.
A missing agent reference or leftover `run` block fails closed at load or
sync time.

The rule name is its filename. Rules have no `cwd`, environment, arguments,
prompt, model, executable, or other run configuration.

Genesis itself can start without a key, which keeps configuration checks and
credential-free tests usable. A matching real SDK run without a key fails
asynchronously and reports the SDK error through the normal structured
completion diagnostics.

## HTTP and run results

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

No match returns `204`. Each accepted match snapshots the resolved agent into
its invocation. Each one-shot runner uses a Genesis-owned `dsh_home` under
`-data/runs/<run_id>/`, invokes `provider="deepseek-official"`, model
`deepseek-v4-flash`, and profile `sdk-minimal`, and **retains** that home
(including DeepSeek session JSONL) after the child exits. `dsh_home` is not
the agent `home`. The compact JSON serialization of the complete CloudEvent is
the sole session user message. Instructions are standing identity, not
prepended onto that payload. For the pinned `0.1.5rc1` profile, the runner also
supplies a per-run Cordis patch setting `session-log-deepseek.enabled: false`;
canonical DeepSeek API requests therefore do not upload or append session-trace
suffixes.

The embedded Python runner writes flushed NDJSON on stdout: `session.created`
as soon as `start_session()` returns, then raw SDK `on_notification` frames,
then exactly one `result`. Go stream-reads that pipe while the child runs,
maps root-session turn/tool brackets into Genesis lifecycle CloudEvents, and
appends them to `events.jsonl` before in-process fan-out. Those records are
observations, not ingress: do not POST them back to `/events`.

```
<data>/runs/gen_<hex>/
  events.jsonl    # CloudEvents 1.0 lifecycle records
  stderr.log
  result.json     # terminal runner object, redacted
  dsh_home/       # retained SDK home / session JSONL
```

Lifecycle types are `dev.genesis.run.accepted`, `start`, `session.created`,
`turn`, `tool`, `result`, `error`, and `end`. Each event has a unique `evt_`
id, a per-run `sequence`, `time`, run/agent/rule identity, optional
`sessionid`, and causation (`causeid` / `causesource` / `causetype`) to the
incoming CloudEvent. Turn and tool payloads keep a `raw` SDK notification
without making SDK schema the domain vocabulary. There is no live token type:
the pinned SDK does not deliver `assistant/chunk` or adapter `text-delta` on
this pipe.

Genesis emits JSON logs. The completion record contains `genesis_run_id`,
`rule`, `agent`, `run_dir`, `deepseek_session_id`, `finish_reason`,
`final_response`, `error_type`, `error`, `diagnostics`, and captured runner
`stderr`. Any finish reason other than `completed` is logged at `ERROR`, even
when the SDK returned no explicit exception. Diagnostics include the relevant
`turn/end`, related error events, and non-session notifications. Exceptions
and runner stderr are retained. Failures remain asynchronous and are not
retried. Secret values never appear in journals, slog, or `stderr.log`.

## Credential-free verification

The ordinary checks use fake runners and a mocked SDK; they need no API key or
network call:

```sh
uv lock --check
uv sync --locked
test -z "$(gofmt -l cmd/genesis/*.go)"
uv run --locked ruff format --check cmd/genesis/runner.py cmd/genesis/test_runner.py
uv run --locked ruff check cmd/genesis/runner.py cmd/genesis/test_runner.py
go vet ./...
go test ./...
go test -race ./...
uv run --locked python -m unittest discover -s cmd/genesis -p 'test_*.py'
PYTHONPYCACHEPREFIX=/tmp/genesis-pycache \
  uv run --locked python -m py_compile \
    cmd/genesis/runner.py cmd/genesis/test_runner.py
go build -o /tmp/genesis ./cmd/genesis
npm ci --prefix web
npm test --prefix web
npm run check --prefix web
npm run build --prefix web
```

This self-contained smoke test also stays credential-free. It substitutes a
fake `python3`, sends an event with `curl`, then prints the invocation document
and structured logs:

```sh
tmp="$(mktemp -d)"
mkdir -p "$tmp/bin" "$tmp/agents" "$tmp/rules" "$tmp/work" "$tmp/home" "$tmp/data"

cat >"$tmp/bin/python3" <<'SH'
#!/bin/sh
/bin/cat >"$GENESIS_CAPTURE"
printf '%s\n' '{"v":1,"type":"session.created","run_id":"ignored","session_id":"fake-session"}'
printf '%s\n' '{"v":1,"type":"notification","method":"session.event","payload":{"sessionId":"fake-session","event":{"type":"turn/start","seq":1,"data":{}}}}'
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"fake-session","finish_reason":"completed","final_response":"fake response","error":null,"diagnostics":null}'
SH
chmod +x "$tmp/bin/python3"

cat >"$tmp/agents/workspace-janitor.yaml" <<YAML
instructions: |
  You are the workspace janitor for this lab.
cwd: "$tmp/work"
home: "$tmp/home"
env:
  ONLY_DECLARED: "yes"
YAML

cat >"$tmp/rules/smoke.yaml" <<YAML
match:
  type: dev.genesis.smoke
agent: workspace-janitor
YAML

go build -o "$tmp/genesis" ./cmd/genesis
env -u DEEPSEEK_API_KEY \
  GENESIS_CAPTURE="$tmp/invocation.json" PATH="$tmp/bin:$PATH" \
  "$tmp/genesis" -listen 127.0.0.1:18787 \
  -agents "$tmp/agents" -rules "$tmp/rules" -data "$tmp/data" \
  >"$tmp/server.log" 2>&1 &
server_pid=$!
cleanup() {
  kill "$server_pid" 2>/dev/null || true
  wait "$server_pid" 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

ready=
for _ in $(seq 1 100); do
  if curl -s -o /dev/null http://127.0.0.1:18787/; then
    ready=1
    break
  fi
  sleep 0.05
done
test "$ready" = 1 || { cat "$tmp/server.log"; exit 1; }

curl -i http://127.0.0.1:18787/events \
  -H 'Content-Type: application/cloudevents+json' \
  --data '{
    "specversion": "1.0",
    "id": "smoke-1",
    "source": "urn:genesis:smoke",
    "type": "dev.genesis.smoke",
    "data": {"task": "credential-free smoke test"}
  }'

for _ in $(seq 1 100); do
  test ! -f "$tmp/invocation.json" || break
  sleep 0.05
done
for _ in $(seq 1 100); do
  ! grep -q '"msg":"Genesis run finished"' "$tmp/server.log" || break
  sleep 0.05
done
test -f "$tmp/invocation.json" || { cat "$tmp/server.log"; exit 1; }
grep -q '"msg":"Genesis run finished"' "$tmp/server.log" ||
  { cat "$tmp/server.log"; exit 1; }
grep -q '"agent":"workspace-janitor"' "$tmp/invocation.json" ||
  { cat "$tmp/invocation.json"; exit 1; }
grep -q '"HOME"' "$tmp/invocation.json" ||
  { cat "$tmp/invocation.json"; exit 1; }
grep -q '"DSH_SYSTEM_PROMPT"' "$tmp/invocation.json" ||
  { cat "$tmp/invocation.json"; exit 1; }
! grep -q 'DEEPSEEK_API_KEY' "$tmp/invocation.json" ||
  { cat "$tmp/invocation.json"; exit 1; }
test -d "$tmp/data/runs" || { cat "$tmp/server.log"; exit 1; }
run_dir="$(find "$tmp/data/runs" -mindepth 1 -maxdepth 1 -type d | head -n 1)"
test -n "$run_dir" || { ls -la "$tmp/data"; cat "$tmp/server.log"; exit 1; }
grep -q '"type":"dev.genesis.run.accepted"' "$run_dir/events.jsonl" ||
  { cat "$run_dir/events.jsonl"; exit 1; }
grep -q '"type":"dev.genesis.run.end"' "$run_dir/events.jsonl" ||
  { cat "$run_dir/events.jsonl"; exit 1; }
! grep -q 'DEEPSEEK_API_KEY' "$run_dir/events.jsonl" ||
  { cat "$run_dir/events.jsonl"; exit 1; }
cat "$tmp/invocation.json"
cat "$run_dir/events.jsonl"
cat "$tmp/server.log"

cleanup
trap - EXIT
```

## Optional live verification

Run `uv sync --locked`, export `DEEPSEEK_API_KEY`, and create throwaway agent
and rule files without committing the credential:

```sh
read -rsp 'DEEPSEEK_API_KEY: ' DEEPSEEK_API_KEY
printf '\n'
export DEEPSEEK_API_KEY
mkdir -p /tmp/genesis-live-agents /tmp/genesis-live-rules \
  /tmp/genesis-live-work /tmp/genesis-live-home
cat >/tmp/genesis-live-agents/workspace-janitor.yaml <<YAML
instructions: |
  You are the workspace janitor for this lab.
  Read the CloudEvent, do only the work it asks, write results in the workspace, then stop.
cwd: /tmp/genesis-live-work
home: /tmp/genesis-live-home
env:
  LANG: "${LANG:-C.UTF-8}"
  PATH: "$PATH"
YAML
cat >/tmp/genesis-live-rules/live.yaml <<YAML
match:
  type: dev.genesis.live
agent: workspace-janitor
YAML
chmod 600 /tmp/genesis-live-agents/workspace-janitor.yaml \
  /tmp/genesis-live-rules/live.yaml
uv run --locked ./bin/genesis \
  -listen 127.0.0.1:18787 \
  -agents /tmp/genesis-live-agents \
  -rules /tmp/genesis-live-rules \
  -data /tmp/genesis-live-data
```

From another terminal, send:

```sh
curl -i http://127.0.0.1:18787/events \
  -H 'Content-Type: application/cloudevents+json' \
  --data '{
    "specversion": "1.0",
    "id": "live-1",
    "source": "urn:genesis:live",
    "type": "dev.genesis.live",
    "data": {"task": "Create hello.txt containing hello, then report success."}
  }'
```

The request returns before the model finishes; watch the first terminal for
the completion log and `tail -f /tmp/genesis-live-data/runs/gen_*/events.jsonl`.

## POST /sync

`POST /sync` reloads the requested YAML layers into the listener cache and
asks the inherited privileged coordinator (when `genesis launch` attached
one) to evaluate a typed host plan. It does not scan the filesystem on
`POST /events`.

```sh
curl -i http://127.0.0.1:8787/sync \
  -H "Authorization: Bearer $GENESIS_SYNC_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"scope":["agents","rules"]}'
```

`scope` is optional. `["rules"]` or `["agents"]` rereads only that directory.
That is not host/matcher atomicity: a rules-only sync can activate new rules
against previously cached agents while a newly written agent file stays
invisible. Invalid YAML or a broken privileged socket leaves the previous
cache in place. The JSON `privileged` object reports `attached`,
`host_mutation: "none"`, empty `applied`, and explicit `unsupported` diffs
for cwd/home and declared env keys. Genesis does not useradd, chown, or
install packages.

While sync runs, `POST /events` returns `503` with `Retry-After: 1`. A second
sync returns `409`. In-flight runs keep the snapshot they were accepted with.

Bearer auth is a local operator token, not a sandbox. Agents share the
Genesis UID. Leaving the token empty disables `/sync`; Genesis does not
invent one.

## Docker Desktop on Windows with the repo in WSL

This path is for Docker Desktop's Linux engine via a WSL distro. It is not
validated on this project's cloud VMs unless `docker` is installed there.

Agents and rules are **not** bind-mounted from the WSL tree. Compose mounts
the named volume `genesis-config` at `/var/lib/genesis/config` and
`genesis-data` at `/var/lib/genesis/data`. Those volumes are owned by the
image `genesis` user (uid `65532`). The first time `genesis-config` is
created, the listener entrypoint copies default YAML from
`/usr/share/genesis/defaults`. Later `docker compose up` keeps whatever is
already in the volumes. The genesis binary and `/app/.venv` stay root-owned
and are not writable by `genesis`.

The listener is not published on the host. The control service is the UI,
published only as `127.0.0.1:8790`. It mounts config read-write and run data
read-only. The listener still writes journals.

1. Install Docker Desktop on Windows and enable WSL 2.
2. Settings → Resources → WSL integration: enable your distro (for example
   Ubuntu).
3. Clone the repository **inside WSL**, for example `~/src/genesis`, so the
   image build context is a Linux filesystem. Do not build from `/mnt/c/...`.
4. From a WSL shell, not PowerShell:

```sh
cd ~/src/genesis
docker compose version
export DEEPSEEK_API_KEY='replace-with-a-real-key'
export GENESIS_SYNC_TOKEN='compose-sync-token'
docker compose up --build
```

5. Edit configuration **inside the volume** as `genesis`, then sync:

```sh
docker compose exec -u genesis genesis tee \
  /var/lib/genesis/config/rules.d/lab.yaml >/dev/null <<'YAML'
match:
  type: dev.genesis.lab
agent: workspace-janitor
YAML
curl -i http://127.0.0.1:8790/api/sync \
  -H 'Content-Type: application/json' \
  --data '{"scope":["agents","rules"]}'
```

The browser does not send the sync token. The control process adds it when
it calls the listener. Windows browsers use `http://127.0.0.1:8790/` because
Docker Desktop publishes that port on localhost only.

6. One-shot wrapper: `sh scripts/wsl-docker-smoke.sh`. It writes and removes
   a temporary rule inside the container as `genesis` and checks that the
   edit stays inactive until `/sync`.

Compose runs `genesis launch` as root and drops the listener to `genesis`.
The control process runs as root so it can read the listener's `0700` journal
directories through the read-only data mount. Supply `GENESIS_SYNC_TOKEN`;
the binary will not generate one. Do not set `network_mode: host` (it does
not mean the same thing on Docker Desktop). Line endings are forced to LF
via `.gitattributes`.

Persistence:

- `docker compose down` stops the container and **keeps** `genesis-config`
  and `genesis-data`. The next `up` reuses the same agents, rules, and run
  journals.
- `docker compose down -v` deletes both volumes. The next `up` reseeds the
  image defaults into a new config volume and starts with empty run storage.
  That is the reset.


