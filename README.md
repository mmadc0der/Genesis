# Genesis

Genesis accepts structured CloudEvents and asynchronously starts one DeepSeek
Harness session for every matching YAML rule. It builds one executable:
`genesis`.

Each session runs through the official Python SDK with the standalone
`sdk-minimal` profile. `pyproject.toml` and the committed `uv.lock` pin the
official `deepseek-harness-sdk` and `deepseek-harness-runtime-bin` packages to
the same `0.1.5rc1` release. The runtime wheel supplies `dsh`; no `dsh`
executable is looked up in `PATH`.

Who runs is a file-backed agent in `agents.d/`. A rule only selects that
identity. `cwd` is the SDK workspace and `home` is the child process Unix
`HOME`. They are a workspace/environment split under the shared Genesis UID,
not a security sandbox. Genesis does not switch OS users.

## Install and run

Go 1.22+, Python 3.10+, and
[`uv`](https://docs.astral.sh/uv/getting-started/installation/) are required.

```sh
uv sync --locked
export DEEPSEEK_API_KEY='replace-with-a-real-key'

mkdir -p bin
go build -o bin/genesis ./cmd/genesis
uv run --locked ./bin/genesis \
  -listen 127.0.0.1:8787 -agents agents.d -rules rules.d
```

`uv run` places the managed `.venv` first in `PATH`. Equivalently, activate it
with `. .venv/bin/activate` before running `./bin/genesis`. Genesis resolves
`python3` once at startup and embeds its one-shot Python runner in the Go
binary.

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
runs, in lexical filename order, and both directories are reloaded and
validated for every request. A missing agent reference or leftover `run`
block fails closed.

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

A match returns `202` immediately with one Genesis run ID per rule:

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
its invocation. Each one-shot runner uses a fresh temporary Harness home,
invokes `provider="deepseek-official"`, model `deepseek-v4-flash`, and
profile `sdk-minimal`, and deletes the home afterward. That throwaway
`dsh_home` is Genesis-owned and is not the agent `home`. The compact JSON
serialization of the complete CloudEvent is the sole session user message.
Instructions are standing identity, not prepended onto that payload. For the
pinned `0.1.5rc1` profile, the runner also supplies a per-run Cordis patch
setting `session-log-deepseek.enabled: false`; canonical DeepSeek API
requests therefore do not upload or append session-trace suffixes.

Genesis emits JSON logs. The completion record contains `genesis_run_id`,
`rule`, `agent`, `deepseek_session_id`, `finish_reason`, `final_response`,
`error_type`, `error`, `diagnostics`, and captured runner `stderr`. Any finish
reason other than `completed` is logged at `ERROR`, even when the SDK returned
no explicit exception. Diagnostics include the relevant `turn/end`, related
error events, and non-session notifications. Exceptions and runner stderr are
retained. Failures remain asynchronous and are not retried.

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
```

This self-contained smoke test also stays credential-free. It substitutes a
fake `python3`, sends an event with `curl`, then prints the invocation document
and structured logs:

```sh
tmp="$(mktemp -d)"
mkdir -p "$tmp/bin" "$tmp/agents" "$tmp/rules" "$tmp/work" "$tmp/home"

cat >"$tmp/bin/python3" <<'SH'
#!/bin/sh
/bin/cat >"$GENESIS_CAPTURE"
printf '%s\n' '{"deepseek_session_id":"fake-session","finish_reason":"completed","final_response":"fake response","error":null,"diagnostics":null}'
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
  -agents "$tmp/agents" -rules "$tmp/rules" \
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
cat "$tmp/invocation.json"
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
  -rules /tmp/genesis-live-rules
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
the completion log.
