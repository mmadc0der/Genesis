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

`genesis` is on PATH for every agent, including a dedicated OS user. Agents
run `genesis job` and `genesis schedule`. They do not invent packages, PEMs,
or sync tokens, and they do not curl with `GENESIS_SYNC_TOKEN`. Both
commands post one CloudEvent on the listener accept path and do not send a
bearer.

Use `genesis job` when work must keep running after the turn.
`genesis job start --emit '{CloudEvent}' -- <command>` starts that command.
When the command exits, Genesis posts that CloudEvent so a rule can match
it and another run can resume. Genesis stamps the event id and source on
every job and schedule post and rejects type `dev.genesis.agent.finished`,
type `dev.genesis.session.continue`, and type
`dev.genesis.publication.submitted`. Neural-net training is the example:
wake when training ends. `genesis job list`, `genesis job logs <id>`, and
`genesis job stop <id>` show and stop it.

Use `genesis schedule` when the same event should fire on a duration.
`genesis schedule --each=15m --emit '{CloudEvent}'` repeats that one event.
`--each` is a duration of at least 1m and at most 168h. The first fire is
one interval after the schedule is created. `genesis schedule list` and
`genesis schedule cancel <id>` manage it. This is not a `rules.d` interval.
Do not write `interval` or `cron` in rule YAML. The loader rejects those
fields. A `rules.d` timer, when one exists, is a listener clock on the rule
file. The CLI is the ticker agents start, and it fires by posting to
`/events`.

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

## Named volume upgrades

Compose mounts named volume `genesis-config` at `/var/lib/genesis/config`.
The image keeps shippable YAML in `/usr/share/genesis/defaults`.

On every listener start the entrypoint copies **missing** default YAML files
into the volume. Existing files are never replaced, including dest entries
that are dangling or live symlinks. Only regular `*.yaml` / `*.yml` files
whose stems match agent/rule IDs are copied; source and dest directories
must not be symlinks. Operator-created files stay. A deleted default
filename is treated as missing and is copied again. Volume `chown`/`chmod`
applies only to real files and directories and does not follow symlinks.

That is how an upgraded image can deliver `designer.yaml` onto a volume that
already has a janitor and example rule, without silently clobbering desired
config.

`docker compose down` keeps the volume. `docker compose down -v` deletes it.
The next `up` reseeds every default file into a new volume. That reset is
destructive.

## Interpreting a shared-UID “success”

This signature is **not** a passing dedicated-user test:

- `getent passwd` inside the listener container lists `genesis` and does not
  list `workspace-janitor`
- files the agent wrote are owned by `genesis`
- the agent has no `user` (shared listener UID)
- active and draft digests match

It is the expected picture of **persisted pre-upgrade named-volume config**.
A volume created before dedicated `user:` shipped still holds the old
shared-UID janitor YAML. The entrypoint leaves that file alone. Launch
therefore never reconciles `workspace-janitor`. The listener loads those
files at start, so the cache digest equals the draft digest. Matching
digests prove disk and memory agree; they do not prove the volume matches
image defaults.

The designer can still appear on that volume after an upgrade, because it
was a missing filename. Seeing `designer` with Shared listener UID next to a
janitor that also lacks `user:` is the upgrade-seed path working as
designed.

## Non-destructive inspect and update

From a WSL clone of the repo, with Compose already up. This does not delete
volumes.

```sh
cd ~/src/genesis

echo '--- volume janitor (desired) ---'
docker compose exec -u genesis genesis sed -n '1,20p' \
  /var/lib/genesis/config/agents.d/workspace-janitor.yaml

echo '--- image default janitor ---'
docker compose exec genesis sed -n '1,20p' \
  /usr/share/genesis/defaults/agents.d/workspace-janitor.yaml

echo '--- volume vs defaults filenames ---'
docker compose exec -u genesis genesis ls -l \
  /var/lib/genesis/config/agents.d /var/lib/genesis/config/rules.d
docker compose exec genesis ls -l \
  /usr/share/genesis/defaults/agents.d /usr/share/genesis/defaults/rules.d

echo '--- OS users ---'
docker compose exec genesis getent passwd genesis workspace-janitor || true

echo '--- panel agents and digests ---'
curl -s http://127.0.0.1:8790/api/agents
curl -s http://127.0.0.1:8790/api/state | python3 -c \
  'import json,sys; s=json.load(sys.stdin); print(s.get("drift"), s.get("active",{}).get("digest"), s.get("desired",{}).get("digest"))'
```

If the volume janitor has no `user:` line and the image default does, copy
only that file if you **intend** to take the shipped dedicated-user janitor.
That overwrites the volume file; it is an operator choice, not the
entrypoint.

```sh
docker compose exec -u genesis genesis cp \
  /usr/share/genesis/defaults/agents.d/workspace-janitor.yaml \
  /var/lib/genesis/config/agents.d/workspace-janitor.yaml
curl -i http://127.0.0.1:8790/api/sync \
  -H 'Content-Type: application/json' \
  --data '{"scope":["agents","rules"]}'
docker compose exec genesis getent passwd workspace-janitor
```

To add only the designer without touching janitor, confirm the files exist
(the entrypoint should have copied them) and Sync:

```sh
docker compose exec -u genesis genesis test -f \
  /var/lib/genesis/config/agents.d/designer.yaml
docker compose exec -u genesis genesis test -f \
  /var/lib/genesis/config/rules.d/designer.yaml
curl -i http://127.0.0.1:8790/api/sync \
  -H 'Content-Type: application/json' \
  --data '{"scope":["agents","rules"]}'
```

Then `POST /api/messages` with `{"rule":"designer.yaml","message":"…"}` and
wait for the run. Edits remain drafts until Sync.

## Destructive clean-volume smoke

This deletes **both** named volumes, including run journals and any operator
YAML. Use it only when you want image defaults and empty `genesis-data`.

```sh
cd ~/src/genesis
export DEEPSEEK_API_KEY='replace-with-a-real-key'
export GENESIS_SYNC_TOKEN='compose-sync-token'
sh scripts/wsl-docker-smoke.sh --reset-volumes
```

Equivalent Compose steps:

```sh
docker compose down -v
docker compose up --build -d
# wait until http://127.0.0.1:8790/api/health succeeds
docker compose exec genesis getent passwd workspace-janitor
docker compose exec genesis getent passwd genesis
curl -s http://127.0.0.1:8790/api/agents
```

After a clean volume the janitor YAML includes `user: workspace-janitor`,
launch creates that OS user, and `/api/agents` reports `user:
workspace-janitor` for that agent. The designer still omits `user` and still
runs as the shared listener UID.

`sh scripts/wsl-docker-smoke.sh` without `--reset-volumes` keeps the named
volumes. If the volume janitor lacks `user:`, that script fails closed and
prints this persisted-volume diagnosis instead of treating shared-UID output
as a dedicated-user pass.
