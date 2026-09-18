# Genesis

Genesis is a small CloudEvents-to-`dsh` dispatcher. This project builds exactly
one executable: `genesis`.

## Run

Go 1.22 or newer and a `dsh` executable in `PATH` are required.

```sh
mkdir -p bin
go build -o bin/genesis ./cmd/genesis
./bin/genesis -listen 127.0.0.1:8787 -rules rules.d
```

`genesis` resolves `dsh` once at startup. Each accepted request rereads all
`.yaml` and `.yml` files in the rules directory, so the next request sees a
rule edit without a restart.

## Rule

One rule lives in each file. `match` compares top-level string CloudEvent
attributes exactly and case-sensitively; there are no patterns or templates.
The first matching file in lexical filename order runs.

```yaml
match:
  type: dev.genesis.run
  source: urn:genesis:example
  subject: hello
run:
  cwd: /tmp
  args: [run, example]
  env:
    GENESIS_EXAMPLE: "1"
```

`run.cwd` must be absolute. Arguments are passed verbatim, without a shell.
`run.env` is the complete child environment—server variables are not
inherited. Rules cannot choose another executable.

## Send an event

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
    "data": {"note": "data is not interpolated into the command"}
  }'
```

A started process returns `202` with the rule filename; no match returns `204`.
The process runs asynchronously. Start failures return `500`; failures after
start are logged and are not retried.

## Verify

Run the complete automated check set from the repository root:

```sh
test -z "$(gofmt -l cmd/genesis/*.go)"
go vet ./...
go test ./...
go test -race ./...
go build -o /tmp/genesis ./cmd/genesis
```

The following self-contained smoke test creates a fake `dsh`, starts `genesis`,
sends a structured CloudEvent with `curl`, and prints what the fake executable
received:

```sh
tmp="$(mktemp -d)"
mkdir -p "$tmp/bin" "$tmp/rules" "$tmp/work"

cat >"$tmp/bin/dsh" <<'SH'
#!/bin/sh
{
  printf 'cwd=%s\n' "$PWD"
  for arg do
    printf 'arg=%s\n' "$arg"
  done
  printf 'MARKER=%s\n' "$MARKER"
} >"$OUT"
SH
chmod +x "$tmp/bin/dsh"

cat >"$tmp/rules/manual.yaml" <<YAML
match:
  type: dev.genesis.manual
  source: urn:genesis:manual
run:
  cwd: "$tmp/work"
  args: [alpha, "two words"]
  env:
    OUT: "$tmp/result"
    MARKER: from-rule
YAML

PATH="$tmp/bin:$PATH" go run ./cmd/genesis \
  -listen 127.0.0.1:18787 -rules "$tmp/rules" \
  >"$tmp/server.log" 2>&1 &
server_pid=$!
trap 'kill "$server_pid" 2>/dev/null || true; rm -rf "$tmp"' EXIT

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
    "id": "manual-1",
    "source": "urn:genesis:manual",
    "type": "dev.genesis.manual"
  }'

for _ in $(seq 1 100); do
  test ! -f "$tmp/result" || break
  sleep 0.05
done
test -f "$tmp/result" || { cat "$tmp/server.log"; exit 1; }
cat "$tmp/result"

kill "$server_pid"
wait "$server_pid" 2>/dev/null || true
rm -rf "$tmp"
trap - EXIT
```

The final output includes the temporary `work` directory, `arg=alpha`,
`arg=two words`, and `MARKER=from-rule`.
