# Designer agent and named-volume config

The committed `designer` agent is a shared-UID config editor. It is ordinary
`agents.d` / `rules.d` YAML. It is not a new Genesis subsystem, not a dedicated
OS user, and not an authorized `/sync` client.

## What it is

| Piece | Location | Contract |
|---|---|---|
| Agent | `agents.d/designer.yaml` | No `user` / `setup`. `cwd` is `/var/lib/genesis/config`. `home` is `/home/genesis`. |
| Rule | `rules.d/designer.yaml` | Exact match on `type=dev.genesis.user.message`, `source=urn:genesis:control`, `subject=designer`. |
| Control API | `POST /api/messages` | `{"rule":"designer.yaml","message":"…"}`. Then `POST /api/sync` with `{}` after it writes YAML. The Wire UI has no composer or Sync button yet. |
| Schema | `/usr/share/genesis/schema/repository-declaration.txt` | Loader contract for `repos.d`. World-readable in the image. Not copied into the config volume. |

The listener UID (`genesis` in Compose, uid `65532`) runs this agent. It has
no `user`, so it is the **shared listener UID**. Output files it creates under
the config volume are owned by `genesis`. That is expected, not a dedicated-user
proof.

The designer may create or update agent, rule, and repository YAML. `repos.d`
records GitHub desired state only. The loader accepts `provider: github` and
no local-project type. Writing the file does not create a local project, an
organization, or call a provider. Before it writes `repos.d`, it reads
`/usr/share/genesis/schema/repository-declaration.txt` and follows that file.
That file is the loader contract. The schema is generated from the loader
structs. `repos.d/example.yaml` and the lab-widget template are examples.
If `/etc/genesis/providers.d` has no `org`, it reports that and stops.
Provider identities live in `/etc/genesis/providers.d`; see
[providers.md](providers.md). The designer has no `github` block. A worker
it writes may name a `repos.d` id in an optional `github` block. Secret
values are not YAML fields. Genesis reconciles OS users from worker YAML.
After it writes files, an operator must **Sync**.
`POST /api/sync` with `{}` reloads agents, rules, and repos together. A rules-only sync
can leave a new agent inactive. A sync that omits repos leaves a new
`repos.d` file inactive. Disk edits stay inactive until then. Matching
active/draft digests only mean the cache equals the files currently on the
volume.

## Dedicated workers it writes

This designer omits `user`. Workers it creates (especially agents that wake
on repository events or issues) declare a dedicated OS user. Genesis's
privileged coordinator automatically
reconciles the account at root `genesis launch` and on agents `/sync`:
`/bin/bash`, home and a private or shared-read workspace mode `0755`, and
shared-write mode `0775` for the declared group. `shared-read` and
`shared-write` are workspace modes, used only when the operator asks.
`setup.groups` is separate: extra groups for an agent that places files
for others, and `reporter` for an agent that publishes. A private worker
that does neither omits `groups`. Genesis
creates a missing group. The group name `shared` also ensures `/shared`,
owned by `root:shared`, mode `2770` and setgid, so members can place files
there for other members to read. Do not list reserved groups such as
`genesis`. `max_parallel` and `reasoning_effort` are siblings of `setup`.
Nested under `setup`, the loader rejects the file. Omit `max_parallel` to
keep `1`. Omit `reasoning_effort` to keep `high`. Values are `off`, `low`,
`high`, or `max`. Write either only when the operator asks. Another agent
can read a `0755` home or workspace and cannot write it. New files are
created world-readable (umask `022`). Python and the harness are in the
image for every account: `/app/.venv`, `/app/.venv/bin/python3`, and
`/app/.venv/bin/dsh`. `uv` is used only while building that venv; the
runtime image does not install it. Code another agent should read belongs
in the worker workspace.

A worker's `instructions` include the listener curl. They do not include
host modes, Python paths, the finish event, or session continuation. From
inside the container the listener is `POST`
`http://127.0.0.1:8787/events` with `Content-Type: application/cloudevents+json`
and no bearer. Port `8790` is the control panel, not that path. `source`
is `urn:genesis:agent:<agent id>`. `202` returns the new run ids. `204`
means nothing matched. Any other status is a listener failure.

`genesis` is on PATH for every agent, including a dedicated OS user. A
worker uses `genesis job` and `genesis schedule` for work that outlives the
turn or repeats on an interval. It does not invent packages, PEMs, or sync
tokens, and it does not curl with `GENESIS_SYNC_TOKEN`. Those subcommands,
and `genesis events`, are in [cli.md](cli.md). Do not write `interval` or
`cron` in rule YAML. The loader rejects those fields.

The finish event and session continuation belong in a reviewer's
instructions. Other workers do not need them. When a session ends, the
system emits `dev.genesis.agent.finished` with `subject` equal to that
agent id. `data.runid` is the exact `gen_` id, `data.agent` is the agent
id, and `data.outcome` is `ok` or `error`. `data.session` is the absolute
path of that run's `session.v3.jsonl`, and `data.definition` is the
absolute path of that agent's definition. Both are omitted when empty.
`data.transcript` is the journal copy, not the session log. The run
journal under `/var/lib/genesis/data/runs` stays private. Session
continuation is type `dev.genesis.session.continue`: `subject` is the
finished `gen_` run id, `data.message` is the next turn, and `source` is
`urn:genesis:agent:oracle` or `urn:genesis:agent:<the agent who owns that
run>`. The cited run must already have ended. `202` contains the new run
id. `204` means the continue was refused. The new run reuses the same DSH
session and the same `DSH_HOME`. A chain of agent continuations stops at hop
9. The operator speaks as `urn:genesis:control`, which may continue any ended
run and is not counted against that limit; the Wire's session chat sends its
follow-ups that way.

Publications are the stories the operator reads on the Wire. An agent files
one with `genesis publish`; the listener accepts it only from an agent whose
`setup.groups` contains `reporter`. The designer adds `reporter` to another
worker only when the operator wants that worker to publish, and then puts
the `genesis publish` line and the meaning of the four kinds in that
worker's instructions. The format, limits, and register are in
[publications.md](publications.md).

The shipped oracle (`agents.d/oracle.yaml`, `rules.d/oracle.yaml`) is that
worker shape: user `oracle`, home `/home/oracle`, cwd
`/home/oracle/workspace`, `setup.workspace: private`, and `setup.groups`
`shared` and `reporter`. It is the shipped reporter. Its rule matches
`dev.genesis.agent.finished` with subject `*`, so it reviews every finished
agent. The designer may change that rule. A review after every finish is
mostly redundant, and skipping those runs is fine. One example is to start
the oracle once, when a cycle ends, instead of on every finish. It reads
`data.session` and `data.definition`, writes `reports/<runid>.md` in its
workspace, files one publication for each report with `genesis publish`,
and continues with the curl when the exit criterion is not met.
Reserved OS user names include `genesis` and `root`.

Required shape:

```yaml
user: <username>
home: /home/<username>
cwd: /home/<username>/workspace
setup:
  workspace: private
```

`max_parallel` and `reasoning_effort` are siblings of `setup`, written only
when asked. `groups` stays inside `setup`, and only for an agent that
places files for others or publishes (`reporter`):

```yaml
max_parallel: 2
reasoning_effort: low
setup:
  groups:
    - shared
```

`/tmp` and `/var/tmp` are legal dedicated cwd values when the operator asked
for shared or ephemeral work.

## Control panel message

`POST /api/messages` with `rule: designer.yaml` builds:

```json
{
  "specversion": "1.0",
  "id": "(minted)",
  "source": "urn:genesis:control",
  "type": "dev.genesis.user.message",
  "subject": "designer",
  "data": {"message": "<operator text>"}
}
```

The compact CloudEvent JSON is the SDK user message. `data.message` is the
text from the request. The listener still matches only the **active**
generation, so a newly seeded designer rule does not run until Sync if the
process was already up with an older cache. A container start that ran the
entrypoint first loads whatever is on the volume, including newly copied
defaults.

`GENESIS_SYNC_TOKEN` is not in the designer environment. The control process
holds the token.

## Where to build and start

Bare-metal and Compose builds, including where the minimal and extended
credential sets go, are in [build.md](build.md). The first listener and
control start is in [quickstart.md](quickstart.md). Flags are in
[cli.md](cli.md). The control API is in [control.md](control.md).

## Named volume

Compose keeps agents, rules, and repos on the config volume at
`/var/lib/genesis/config`. On listener start the entrypoint copies
**missing** default YAML from `/usr/share/genesis/defaults`. Existing files
are left unchanged, so an upgraded image can add `designer.yaml` without
replacing a janitor the operator already edited. A deleted default filename
is copied again. Stopping Compose keeps the volume. Deleting volumes is the
destructive reset, and the next start reseeds the image defaults. Those
commands and the volume names are in [build.md](build.md) and
[run.md](run.md).

A volume from before dedicated `user:` shipped can still hold a shared-UID
janitor. The entrypoint will not overwrite it. The designer can appear
beside that janitor because it was a missing filename. Shared listener UID,
files owned by `genesis`, and matching active and draft digests mean the
cache equals the files on the volume. They do not mean the volume matches
the image defaults, and they are not a dedicated-user pass. How to read
that state is in [troubleshooting.md](troubleshooting.md).
