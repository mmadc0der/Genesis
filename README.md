# Genesis

This first slice contains one binary: `genesisd`, a small CloudEvents-to-`dsh`
dispatcher. The name `genesis` is reserved for a future CLI.

## Run

Go 1.22 or newer and a `dsh` executable in `PATH` are required.

```sh
go test ./...
go build -o bin/genesisd ./cmd/genesisd
./bin/genesisd -listen 127.0.0.1:8787 -rules rules.d
```

`genesisd` resolves `dsh` once at startup. Each accepted request rereads all
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
