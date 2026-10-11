# Quickstart

The fastest start is Compose. It builds the image, starts the listener and
the control process, and seeds the default agents and rules. Credential
layout and the bare-metal build are in [build.md](build.md).

Set the model key, generate a sync token, and launch:

```sh
export DEEPSEEK_API_KEY='replace-with-a-real-key'
export GENESIS_SYNC_TOKEN="$(openssl rand -hex 32)"
mkdir -p runtime/providers runtime/secrets
docker compose up --build
```

`runtime/providers` and `runtime/secrets` may be empty. Compose bind-mounts
them, so the directories have to exist. The listener is not published.
Open `http://127.0.0.1:8792/`.

## Without Docker

Build `bin/genesis` and `web/dist` as in [build.md](build.md). Non-root
`launch` refuses any agent that declares `user`, and the committed
`workspace-janitor` does. Use a shared-UID agent, as in
[examples/one-run.md](examples/one-run.md), or run root `launch` with
`-listener-user` as in the extended bare-metal section of
[build.md](build.md).

```sh
export DEEPSEEK_API_KEY='replace-with-a-real-key'
export GENESIS_SYNC_TOKEN="$(openssl rand -hex 32)"
uv run --locked ./bin/genesis launch \
  -listen 127.0.0.1:8787 \
  -agents agents.d -rules rules.d \
  -data genesis-data
```

`genesis control` is a second process. It serves the Wire on
`127.0.0.1:8790`. Flags are in [cli.md](cli.md). Panel behavior is in
[control.md](control.md).

```sh
./bin/genesis control \
  -listen 127.0.0.1:8790 \
  -listener http://127.0.0.1:8787 \
  -agents agents.d -rules rules.d \
  -data genesis-data \
  -web web/dist
```

## Next

| Topic | Doc |
| --- | --- |
| Build, credentials, Compose volumes | [build.md](build.md) |
| Flags and loop-control subcommands | [cli.md](cli.md) |
| Agent and rule YAML | [agents.md](agents.md) |
| `POST /events`, journals, `POST /sync` | [http.md](http.md) |
| Logs and failure checks | [troubleshooting.md](troubleshooting.md) |
| Cache and `/sync` semantics | [orchestrator.md](orchestrator.md) |
| Compose details | [run.md](run.md) |
