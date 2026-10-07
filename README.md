# Genesis

Genesis accepts structured CloudEvents and asynchronously starts one DeepSeek
Harness session for every matching YAML rule. It builds one executable:
`genesis`.

`genesis` and `genesis listen` are the unprivileged event listener.
`genesis launch` starts that listener as a child and supervises it over a
private inherited Unix socketpair used only for typed privileged
coordination. See [docs/orchestrator.md](docs/orchestrator.md) for the exact
cache, `/sync`, and failure semantics. The committed `workspace-janitor`
example declares a dedicated OS user, so non-root `listen` / `launch` refuse
it. Use Docker Compose for that identity, or drop `user` and `setup` for a
shared-UID local process. The committed `designer` agent deliberately omits
`user` and edits YAML on the writable config root; see
[docs/designer.md](docs/designer.md).

Each session runs through the official Python SDK with the standalone
`sdk-minimal` profile. `pyproject.toml` and the committed `uv.lock` pin the
official `deepseek-harness-sdk` and `deepseek-harness-runtime-bin` packages to
the same `0.1.5rc1` release. The runtime wheel supplies `dsh`; no `dsh`
executable is looked up in `PATH`.

Who runs is a file-backed agent in `agents.d/`. A rule only selects that
identity. `cwd` is the SDK workspace and `home` is the child process Unix
`HOME`. When `user` is set, `genesis launch` as root and `POST /sync` of
agents reconcile a dedicated OS account (Bash shell, home, ownership, and
workspace access) and the runner executes as that account. Without `user`,
cwd/home remain a workspace/environment split under the listener UID and are
not a security sandbox. YAML still cannot name packages or arbitrary root
commands.

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
  -listen 127.0.0.1:8787 -agents agents.d -rules rules.d -repos repos.d -providers providers.d -data genesis-data
```

`genesis -listen ...` still runs the listener in this process. `launch` is
the supervisor: it inherits no extra HTTP port and keeps a private socketpair
for privileged coordination. `-data` (default `genesis-data`) is the per-run
journal root; an invalid path refuses to listen. Root `launch` also takes
`-credentials` (default `genesis-credentials`) for SSH grant material and
`-secrets` (default `genesis-secrets`) for the reconciler App private key.
The App webhook secret is the sibling file `GITHUB_APP_WEBHOOK_SECRET`
(mode `0600`). See [docs/webhooks.md](docs/webhooks.md).
An
empty `-sync-token` / `GENESIS_SYNC_TOKEN` disables `POST /sync` (`401`
`sync is disabled`); set a token to activate filesystem edits. When `launch` runs as root it requires
`-listener-user` / `GENESIS_LISTENER_USER` and execs the listener as that
user. It also reconciles dedicated agent OS users from `agents.d` before the
listener starts. Non-root launch keeps the current user, does not need that
flag, and refuses to start if any agent declares `user`.

`uv run` places the managed `.venv` first in `PATH`. Equivalently, activate it
with `. .venv/bin/activate` before running `./bin/genesis`. Genesis resolves
`python3` once at startup and embeds its one-shot Python runner in the Go
binary.

## Control panel

`genesis control` is a second process. It serves the Wire (a small React,
read-only view of runs, agents, rules, drift, token use, and the
[publications](docs/publications.md) agents in the `reporter` group file) and a REST API
on `127.0.0.1:8790`, reads agent/rule files, reads run journals, and
proxies CloudEvents plus authorized `POST /sync` to the listener. The sync
token is not sent to the browser. Node is required only to build the UI.
See [docs/control.md](docs/control.md).

```sh
npm ci --prefix web
npm run build --prefix web
./bin/genesis control \
  -listen 127.0.0.1:8790 \
  -listener http://127.0.0.1:8787 \
  -agents agents.d -rules rules.d -repos repos.d -providers providers.d -data genesis-data \
  -web web/dist
```

Open `http://127.0.0.1:8790/`. The listener stays on `127.0.0.1:8787`.
`GET /health` and `GET /generation` are read-only views of that process.
The Wire UI has no message composer or Sync button yet. To message the
shared-UID designer, `POST /api/messages` with `{"rule":"designer.yaml",
"message":"..."}`. After it writes YAML, `POST /api/sync` with `{}`. See
[docs/designer.md](docs/designer.md).

## Agents

One agent lives in each `.yaml` or `.yml` file. The filename stem is the agent
ID (`workspace-janitor.yaml` → `workspace-janitor`):

```yaml
instructions: |
  You are the workspace janitor for this lab.
  Read the CloudEvent, do only the work it asks, write results in the workspace, then stop.
user: workspace-janitor
cwd: /home/workspace-janitor/workspace
home: /home/workspace-janitor
setup:
  workspace: private
env:
  PATH: /usr/local/bin:/usr/bin:/bin
  LANG: C.UTF-8
```

`instructions`, `cwd`, and `home` are required. `cwd` and `home` must be
absolute and distinct. `user` is optional. When set, it is a dedicated Linux
account name (lowercase, not `root`/`genesis`/other reserved names). Two
agents cannot share an OS user. `home` must be `/home/<user>`. `cwd` must be
inside that home, or exactly `/tmp` or `/var/tmp`. `setup` is allowed only with `user` and is a
closed contract: `workspace` (`private`, `shared-read`, `shared-write`) and
optional `groups`. Genesis creates a missing group. The group `shared`
ensures `/shared` (`root:shared`, mode `2770`, setgid) so members can place
files there. There is no command, script, package, or shell
field; the account always gets `/bin/bash`. `env` is the complete non-secret
environment given to that agent's Harness runtime; omitted, it is empty. Genesis then adds named
secrets from its own startup environment (default `DEEPSEEK_API_KEY` when the
value is non-empty), sets `HOME` from `home`, and sets `DSH_SYSTEM_PROMPT`
from `instructions` for the pinned `sdk-minimal` persona hook. It puts the
`genesis` binary's directory on `PATH` (default
`/usr/local/bin:/usr/bin:/bin` when `PATH` is omitted) and sets
`GENESIS_EVENTS_URL` to that listener's `/events` URL. It does not copy
`GENESIS_SYNC_TOKEN`, `GITHUB_APP_WEBHOOK_SECRET`, or the root secret store
into the agent. Declaring
`HOME`, `DSH_SYSTEM_PROMPT`, or any listed secret in `env` is rejected.
Dedicated agents also reserve `USER`, `LOGNAME`, and `SHELL`. Secret values
never appear in YAML. Duplicate IDs, duplicate secret names, duplicate OS
users, unknown fields, and invalid filename stems are rejected. `max_parallel`
is optional and defaults to 1. When set, it must be an integer of at least 1.
`reasoning_effort` is optional: `off`, `low`, `high`, or `max`. Omitted, the
harness keeps its default, `high`.

Several rules may point at the same agent. `max_parallel` is how many runs
of that one agent may execute at once. The default is 1: a second match for
the same agent is still accepted (`202` and a new run id) and waits until
one of that agent's running runs finishes. Continuations of the agent count
toward the same cap. A different agent is not in that queue. Two agents
start together whenever each is under its own cap, including when both are
at the default of 1. Raising `max_parallel` allows that many runs of the
same agent at once; they still share `cwd` and `home`. Each waiting run keeps
`queue.json` in its run directory, without secret values. A restart loads
those files and starts every agent that is under its own cap. A run that
had already started and has no live process is still closed as interrupted.
`max_parallel` is not a token budget.

The committed `designer` agent omits `user` on purpose so it can keep
`cwd: /var/lib/genesis/config` (the writable Compose config root). It runs as
the shared listener UID. Its standing instructions describe agents, rules,
event matching, `user`/`home`/`cwd`, automatic dedicated-user reconcile,
safe YAML edits, active vs desired generations, and that an operator must
Sync after it writes files. It may create or update agent and rule YAML. It
must not reconcile OS users, install packages, or perform arbitrary root
actions. Dedicated `cwd` cannot be that config path; do not add `user:` to
the designer. Dedicated workers it writes must declare `user`,
`home: /home/<user>`, `cwd: /home/<user>/workspace`, and
`setup.workspace: private`; Genesis creates that account on launch or
agents `/sync`.

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
CloudEvent attributes. `subject` may use a single-segment `*` glob and other
attributes stay exact. `agent` is a required agent ID. Every matching rule
runs, in lexical filename order. The listener caches agents and rules at
start; later filesystem edits are inactive until an authorized `POST /sync`.
A missing agent reference or leftover `run` block fails closed at load or
sync time.

`rules.d/designer.yaml` is the dedicated user-message rule. `POST
/api/messages` copies its match (`type: dev.genesis.user.message`, `source:
urn:genesis:control`, `subject: designer`) and puts the message text in
`data.message`.

The rule name is its filename. Rules have no `cwd`, environment, arguments,
prompt, model, executable, or other run configuration.

## Jobs and schedules

Agents, including dedicated OS users, can run the `genesis` binary. It is on
their `PATH`. `genesis job` and `genesis schedule` do not receive
`GENESIS_SYNC_TOKEN`, the webhook secret, or the root secret store, and they
do not ask the agent to curl with that token.

`genesis job start --emit '{CloudEvent}' -- <command>` runs the command in
its own session, so it keeps going after the agent turn ends. `genesis job
list`, `genesis job logs <id>`, and `genesis job stop <id>` inspect and stop
it. When the command exits, a supervisor posts that CloudEvent to the
listener `POST /events` path (no bearer). Genesis stamps `id` and `source`
on that post and ignores a caller-supplied `id` or `source`. `source` is
`urn:genesis:agent:<id>` when `GENESIS_AGENT` is set, and otherwise
`urn:genesis:job`. Types `dev.genesis.agent.finished` and
`dev.genesis.session.continue` are rejected. A rule can match the posted
event and start another run — for example, wake when neural-net training
ends. With no `--emit`, the event type is `dev.genesis.job.exited`.

`genesis schedule --each=15m --emit '{CloudEvent}'` stores one schedule and
keeps a small per-user ticker. `--each` is a Go duration of at least `1m`
and at most `168h`, in whole seconds. The first fire is one interval after
creation. Each fire posts that CloudEvent to the same `/events` accept path,
with a new stamped `id` and the same `source` rule (`urn:genesis:schedule`
when `GENESIS_AGENT` is unset). One unreadable file under `schedules/` does
not stop the other schedules. `genesis schedule list` and
`genesis schedule cancel <id>` manage it. Cancel removes the schedule even
if a fire is in progress. This ticker is the CLI agents call. It is not a
`rules.d` `interval` or `cron`
rule; the loader still rejects those fields.

## Repositories

`repos.d` is optional desired GitHub state. A missing directory leaves the
layer inactive and does not change the agent/rule digest. The committed
`repos.d/example.yaml` is a declaration for `octo-org/lab-widget`. Without
a reconciler key, observation is `unavailable` and GitHub is not called.
With that key, `existing: adopt` creates the repository when GitHub
reports that it does not exist, binds its id, and applies visibility,
description, features, merge settings, Actions, the declared ruleset, and
missing bootstrap files. `existing: refuse` makes no HTTP call. `remote_mutation` is
`applied` only for intents this sync wrote. Secrets, identities, and
webhook configuration stay unsupported. A dedicated run can still mint its
own short-lived installation token and check out the bound repository.
The App needs Administration write and Contents write; bootstrap uses a
separate contents token. The running designer reads the loader contract
from `/usr/share/genesis/schema/repository-declaration.txt`, generated from
the same structs that load `repos.d`. `repos.d/example.yaml` is an example,
not that contract. Operator-facing rules are in
[docs/repositories.md](docs/repositories.md).

## Providers

`providers.d` is optional root-owned GitHub identity state. A missing
directory leaves the layer inactive and does not change the agent/rule
digest. Files name one GitHub organization, named identities, and secret
references. They never contain secret values. The directory must sit
outside the designer-writable config volume. The image keeps an empty
`/etc/genesis/providers.d` (`root:genesis`, mode `0750`) and does not copy
it or `providers.d/example.yaml` into `genesis-config`. Compose bind-mounts
`./runtime/providers` there for the listener and control services, and
`./runtime/secrets` onto `/var/lib/genesis/secrets` for the root listener
only. `runtime/` is gitignored. On start, root tightens provider files to
`0640` and secret files to `0600`. The entrypoint does not create
`/var/lib/genesis/secrets` or the files inside it. When that directory
exists, startup locks it. `GITHUB_APP_RECONCILER_PEM`, `GITHUB_APP_ID`,
and `GITHUB_APP_WEBHOOK_SECRET` may be absent. A symlink or other non-file
at one of those names exits. GitHub calls fail closed until the files they
need are present. Dedicated agent users cannot read
the provider directory.

An agent may declare an optional `github` capability: a `repos.d`
repository, a non-reconciler identity, git access `none`/`read`/`write`,
and allowlisted API permissions. The designer agent cannot. Reconciler
identities, administration, secrets, and unsafe role combinations fail
closed. Public read can use a reader identity with `credential: none`.

Sync returns a `grant_plan` whose `credential_active` is false. Root
`launch` stores an Ed25519 identity per non-public git grant under
`-credentials` and, when `-secrets` holds the reconciler App key, registers
that public key as a repository deploy key. The listener summary stays
`pending` or `none`; the root result is `material.remote_status`. No socket
is delivered until that status is `ready`, and a changed, removed, or refused
grant drops the socket immediately. Public-read and `git: none`
grants do not receive a token. A dedicated run whose grant requires an
API credential receives one installation token as `GITHUB_TOKEN` for that
process only. Root redacts it from logs, journals, events, the control
API, `dsh_home`, and errors, and removes it when the run ends.
`credential_active` is true only while that token is in the run process;
sync and the panel keep it false. If git access is read or write and the
deploy-key socket is ready, root clones into an empty `cwd` or fetches
when `cwd` is already that repository, using `SSH_AUTH_SOCK` and never the
token in the remote URL. Any other `cwd` contents, a failed mint, or a
failed checkout does not start the session. Private keys, App JWTs, and
installation tokens stay out of logs and APIs. See
[docs/providers.md](docs/providers.md).

The policy header looks like this. The complete file, including settings,
bootstrap, Actions, secret names, ruleset intent, and runtime identities, is
`repos.d/example.yaml`.

```yaml
provider: github
org: octo-org
name: lab-widget
lifecycle:
  remove: retain
  existing: adopt
```

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

No match returns `204`. `POST /webhooks/github` is the GitHub App ingress
described in [docs/webhooks.md](docs/webhooks.md). A verified delivery for a
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
Leaving the token empty disables `/sync`; Genesis does not invent one.

## Docker Desktop on Windows with the repo in WSL

This path is for Docker Desktop's Linux engine via a WSL distro. It is not
validated on this project's cloud VMs unless `docker` is installed there.

Agents and rules are **not** bind-mounted from the WSL tree. Compose mounts
the named volume `genesis-config` at `/var/lib/genesis/config` and
`genesis-data` at `/var/lib/genesis/data`. Those volumes are owned by the
image `genesis` user (uid `65532`). On every listener start the entrypoint
copies **missing** default YAML from `/usr/share/genesis/defaults` into the
volume. Existing files are left unchanged, including operator-edited janitor
or rule YAML on a reused volume, and dest symlinks are not written through.
Only regular files with valid agent/rule names are copied. That is how an
image upgrade can deliver `designer.yaml` without overwriting desired config.
The entrypoint also creates `repos.d` when it is missing. This image does not
seed a repository declaration; an empty `repos.d` is an active, empty layer.
A deleted default filename is copied again; file contents are never replaced.
Ownership passes skip non-regular files so a planted volume symlink cannot
redirect root `chown`/`chmod`. The genesis
binary and `/app/.venv` stay root-owned and are not writable by `genesis`.

The listener is not published on the host. The control service is the UI,
published only as `127.0.0.1:8790`. It mounts config read-write and run data
read-only. The listener still writes journals.

1. Install Docker Desktop on Windows and enable WSL 2.
2. Settings → Resources → WSL integration: enable your distro (for example
   Ubuntu).
3. Clone the repository **inside WSL**, for example `~/src/genesis`, so the
   image build context is a Linux filesystem. Do not build from `/mnt/c/...`.
4. From a WSL shell, not PowerShell. Create the gitignored runtime
   directories before Compose starts; the bind mounts require them.

```sh
cd ~/src/genesis
mkdir -p runtime/providers runtime/secrets
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

6. One-shot wrapper: `sh scripts/wsl-docker-smoke.sh`. It keeps named
   volumes, writes and removes a temporary rule as `genesis`, checks that the
   edit stays inactive until `/sync`, and checks that missing designer files
   were seeded without overwriting existing YAML. If the volume janitor lacks
   `user:`, it fails closed with a persisted-volume diagnosis instead of
   treating shared-UID output as a dedicated-user pass.
7. Destructive clean-volume smoke (deletes `genesis-config` and
   `genesis-data`): `sh scripts/wsl-docker-smoke.sh --reset-volumes`.
   Inspect or update a reused volume without deleting it: see
   [docs/designer.md](docs/designer.md).

Compose runs `genesis launch` as root and drops the listener to `genesis`.
Dedicated agent users are created in the container at launch and on agents
`/sync`. The control process runs as uid 65532 (`genesis`). It waits until the
entrypoint has chowned config and data to that user, so it can write config
and read the listener's `0700` journals through the read-only data mount
without starting as root. Supply `GENESIS_SYNC_TOKEN`;
the binary will not generate one. Do not set `network_mode: host` (it does
not mean the same thing on Docker Desktop). Line endings are forced to LF
via `.gitattributes`.

Persistence:

- `docker compose down` stops the container and **keeps** `genesis-config`
  and `genesis-data`. The next `up` reuses the same agents, rules, and run
  journals, then copies any **new** default filenames that are missing.
  Existing file contents are not replaced. `./runtime/providers` and
  `./runtime/secrets` stay on the host either way.
- `docker compose down -v` deletes `genesis-config`, `genesis-data`, and
  `genesis-credentials`. The next `up` reseeds the image defaults into a
  new config volume and starts with empty run storage. That is the explicit
  destructive reset. It does not delete `runtime/`.

If only the `genesis` OS user exists, agent output is owned by `genesis`, the
UI says Shared listener UID, and active/draft digests match, that is reused
pre-upgrade volume YAML, not a successful dedicated-user test. Inspect and
optionally copy a single default file, or use `--reset-volumes`. Do not
weaken persistence to make that look like a pass.


