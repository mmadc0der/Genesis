# CLI

`genesis` with listen flags, and `genesis listen`, are the unprivileged
listener. `genesis launch` starts that listener as a child and supervises it
over a private inherited Unix socketpair used only for typed privileged
coordination. `genesis control` is a second process. See
[quickstart.md](quickstart.md) for a first start and
[orchestrator.md](orchestrator.md) for cache, `/sync`, and launch failure
semantics.

## Listen and launch

`launch` inherits no extra HTTP port. `-data` (default `genesis-data`) is the
per-run journal root; an invalid path refuses to listen. Root `launch` also
takes `-credentials` (default `genesis-credentials`) for SSH grant material
and `-secrets` (default `genesis-secrets`) for the reconciler App private key.
The App webhook secret is the sibling file `GITHUB_APP_WEBHOOK_SECRET`
(mode `0600`). See [webhooks.md](webhooks.md).
An empty `-sync-token` / `GENESIS_SYNC_TOKEN` disables `POST /sync` (`401`
`sync is disabled`); set a token to activate filesystem edits. When `launch` runs as root it requires
`-listener-user` / `GENESIS_LISTENER_USER` and execs the listener as that
user. It also reconciles dedicated agent OS users from `agents.d` before the
listener starts. Non-root launch keeps the current user, does not need that
flag, and refuses to start if any agent declares `user`.

| Flag | Listen default | Launch default | Env when the flag is omitted |
| --- | --- | --- | --- |
| `-listen` | `127.0.0.1:8787` | `127.0.0.1:8787` | |
| `-agents` | `agents.d` | `agents.d` | |
| `-rules` | `rules.d` | `rules.d` | |
| `-repos` | `repos.d` | `repos.d` | |
| `-providers` | `providers.d` | `providers.d` | |
| `-data` | `genesis-data` | `genesis-data` | |
| `-sync-token` | empty disables `/sync` | empty disables `/sync` | `GENESIS_SYNC_TOKEN` |
| `-credentials` | | `genesis-credentials` | |
| `-secrets` | | `genesis-secrets` | |
| `-listener-user` | | required when launch is root | `GENESIS_LISTENER_USER` |

`-credentials`, `-secrets`, and `-listener-user` are launch flags. A missing
`-repos` or `-providers` directory leaves that layer inactive.

`DEEPSEEK_API_KEY` is not a flag. When it is non-empty at startup, Genesis
can pass it into an agent environment. See [agents.md](agents.md).

## Control

```sh
./bin/genesis control \
  -listen 127.0.0.1:8790 \
  -listener http://127.0.0.1:8787 \
  -agents agents.d -rules rules.d -repos repos.d -providers providers.d -data genesis-data \
  -web web/dist
```

| Flag | Default |
| --- | --- |
| `-listen` | `127.0.0.1:8790` |
| `-listener` | `http://127.0.0.1:8787` |
| `-agents` | `agents.d` |
| `-rules` | `rules.d` |
| `-repos` | `repos.d` |
| `-providers` | `providers.d` |
| `-data` | `genesis-data` |
| `-web` | `web/dist` |
| `-sync-token` | `GENESIS_SYNC_TOKEN` |

`-data` must already exist. A missing `-web` directory serves the API only.
The sync token is sent only to the listener `POST /sync`. What the process
serves is in [control.md](control.md).

## Additional loop control

Common agentic-loop and environment steps are separate `genesis`
subcommands, so an agent does not hand-write the listener curl for each one.
A job keeps a command alive after the turn and posts when it exits. A
schedule posts on an interval. `publish` files one story for the operator.
`events` reads the loop instead of posting into it. All four are on `PATH`
for agents, including dedicated OS users. They do not receive
`GENESIS_SYNC_TOKEN`, the webhook secret, or the root secret store.

Each subsection is one subcommand. Add the next one as another subsection
in the same shape: what it is for, the invocation, then what it posts.

### `job`

Keeps a command running after the agent turn ends, then posts a CloudEvent
so a rule can start the next run. For example, wake when neural-net training
exits.

```sh
genesis job start --emit '{CloudEvent}' -- <command>
genesis job list
genesis job logs <id>
genesis job stop <id>
```

`--id` sets the job id; omitted, one is generated. `start` prints the id.
The command runs in its own session. When it exits, a supervisor posts the
CloudEvent to the listener `POST /events` path with no bearer. Genesis
stamps `id` and `source` and ignores a caller-supplied `id` or `source`.
`source` is `urn:genesis:agent:<id>` when `GENESIS_AGENT` is set, and
otherwise `urn:genesis:job`. With no `--emit`, the event type is
`dev.genesis.job.exited`. Types `dev.genesis.agent.finished`,
`dev.genesis.session.continue`, and `dev.genesis.publication.submitted`
are rejected. `genesis job supervise` is the process `start` leaves
behind, not a separate command.

### `schedule`

Posts one CloudEvent on a fixed interval, without a `rules.d` cron field.
The loader still rejects `interval` and `cron` on a rule.

```sh
genesis schedule --each=15m --emit '{CloudEvent}'
genesis schedule list
genesis schedule cancel <id>
```

`--each` is a Go duration of at least `1m` and at most `168h`, in whole
seconds. Creating a schedule stores it and keeps a small per-user ticker.
The first fire is one interval after creation. Each fire posts that
CloudEvent to the same `/events` accept path, with a new stamped `id` and
the same `source` rule (`urn:genesis:schedule` when `GENESIS_AGENT` is
unset). One unreadable file under `schedules/` does not stop the other
schedules. Cancel removes the schedule even if a fire is in progress.
`genesis schedule run` is that ticker process, not a separate command.

### `publish`

Files one publication for the operator. It is meant to run inside an agent:
`GENESIS_AGENT` must be set, or the command refuses to start. The listener
accepts it only when that agent's `setup.groups` contains `reporter`. Kinds,
limits, and the register are in [publications.md](publications.md).

```sh
genesis publish --kind report --headline '<one line>' --lede '<one or two sentences>'
```

`--kind` is `report`, `progress`, `breakthrough`, or `impasse`. `--headline`
is at most 120 characters. `--lede` is at most 400 characters. Optional:
`--run <gen_ id>`, `--ref <gen_ or pub_ id>` up to 8 times, `--to <agent id>`,
`--supersedes <pub_ id>`, and `--body-file <path>` for markdown of at most
16 KiB. It prints the new `pub_` id. The post is one
`dev.genesis.publication.submitted` CloudEvent. Genesis stamps the id,
source, and time.

### `events`

Reads CloudEvents from the listener, or from a local `-data` directory when
that directory has no listener-owned store. It does not post.

```sh
genesis events
genesis events -n 50
genesis events -f
genesis events --type dev.genesis.agent.finished --agent oracle
genesis events --since 10m --format short
```

The default is the 20 most recent events as JSON lines. `-n` / `--tail`
sets that count (`0` is all of them). `-f` / `--follow` keeps reading.
A positional pattern or `-e` / `--grep` filters. `-i` is case-insensitive.
`--type`, `--agent`, `--run`, and `--since` (a duration such as `10m` or
`1h`, or a timestamp) narrow the set. `--format` is `json` (default),
`short`, or `pretty`. Token chunks are omitted unless `--chunks` is set.
`--url` defaults to `GENESIS_EVENTS_URL` or `http://127.0.0.1:8787/events`.
`--data <dir>` reads journals on disk instead.
