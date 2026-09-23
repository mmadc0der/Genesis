# Orchestrator, cached generations, and POST /sync

This is the architecture Genesis now implements. It is smaller than a root
reconciler, and it does not pretend that matcher activation and host
provisioning are one transaction.

## Processes

`genesis listen` is the existing unprivileged HTTP listener. Plain `genesis`
with flags is still listen, so previous command lines keep working.

`genesis launch` is the orchestrator. It:

1. Creates a private `SOCK_STREAM` Unix `socketpair`.
2. When the parent is root, reconciles dedicated agent OS users from `agents.d`
   before the listener starts. Failure exits without serving. Non-root launch
   evaluates the same plan locally and refuses dedicated `user` agents.
3. Starts `genesis listen` as a child, inheriting only the child end as file
   descriptor `3` (`GENESIS_PRIVILEGED_FD=3`).
4. When the parent is root, execs that child as `-listener-user` /
   `GENESIS_LISTENER_USER`. Missing or unknown target users fail closed
   before listen starts. Non-root launch leaves the listener at the current
   uid.
5. Serves typed privileged coordination on the parent end, including spawn
   of dedicated-agent Python children.
6. Forwards `SIGINT` / `SIGTERM` to the child, kills leftover spawned
   agents, and exits with the child's code.

The pair is never bound to a filesystem path. Nothing else can connect to it.
The listener sets close-on-exec on the inherited descriptor so Python/`dsh`
children do not receive it. Launch does not escalate. A root parent is a
supervisor that can drop the listener uid, reconcile dedicated agent
accounts, and exec those agents as their OS users.

## Cache

The listener loads `agents.d`, `rules.d`, and, when the directory exists,
`repos.d` once at start and keeps an immutable in-memory generation (agent
map, rule list, repository map, content digest). A missing `repos.d` is
inactive and is omitted from the digest, so agent/rule digests stay
compatible. A symlink is rejected; it is not treated as missing. `POST /events` matches only that snapshot. Editing YAML on disk
does nothing until `POST /sync`. In-flight runs already hold the accept-time
agent copy. Repository declarations do not start runs.

There is no event queue. While a sync is running, `POST /events` returns
`503` with `Retry-After: 1`. A second `POST /sync` returns `409`.

## POST /sync

`POST /sync` is authorized with `Authorization: Bearer <token>`. The token is
`-sync-token` or `GENESIS_SYNC_TOKEN`. An empty token disables `/sync`
(`401` `sync is disabled`). Launch does not generate a token. This is not a
strong capability boundary: agents share the listener UID and can read
`/proc/<pid>/environ`. It only keeps accidental unauthenticated callers off
the control path.

Optional JSON body:

```json
{"scope": ["agents", "rules", "repos"]}
```

Omitted or empty `scope` means all three layers. `["rules"]` rereads rules
against the cached agents. `["agents"]` rereads agents and revalidates cached
rules. `["repos"]` rereads repository declarations and leaves agents and rules
cached. Unknown scope values are `400`. Scope is which directories to
reread. It is not independent activation of host vs matcher state, and it is
not an mtime "only new files" filter. A missing `repos.d` stays inactive.
A symlink `repos.d` fails the load. `["repos"]` does not reload agents or
rules and does not send a host plan.

Successful sync:

1. Load the requested YAML. On failure: `500` (`agents are invalid` /
   `rules are invalid` / `repositories are invalid`), cache unchanged, no IPC.
2. Build a typed privileged plan from the would-be agents, and a separate
   repository plan when `repos` is in scope. The repository plan is not an
   IPC payload. Its `remote_mutation` is `none`; every intent is
   `unsupported`. Genesis does not call a provider.
3. If a coordinator is attached, send `{op:"coordinate", payload: plan}` over
   the inherited socket and wait. IPC failure **or a failed dedicated-user
   reconcile**: `500` `privileged coordination failed`, cache unchanged.
4. Swap the in-process generation pointer.
5. Return `200` with digest, counts, the privileged result, and
   `repository_plan`.

Standalone listen (no inherited fd) still reloads the cache for shared-UID
agents. Dedicated `user` agents fail closed there: the process cannot
reconcile or exec as another uid. The JSON reports `privileged.attached`
and the privileged result.

## Privileged protocol

Length-prefixed JSON (32-bit big-endian size, then one object). Unknown JSON
fields and unknown `op` / intent `kind` values fail closed.

The child sends a plan of intents. The parent answers with
`host_mutation`, `applied`, `unsupported`, and optional `retained`. Unknown
JSON fields and unknown `op` / intent `kind` values fail closed. The parent
never runs an agent-supplied shell snippet.

| kind | meaning | evaluation |
|---|---|---|
| `ensure_agent_user` | dedicated `user`, Bash, home, cwd, allowlisted `setup` | applied by root launch / agents `/sync`; otherwise a hard failure so the generation is not activated |
| `ensure_agent_paths` | shared-UID agent `cwd` / `home` | unsupported: no OS user; Genesis does not mkdir/chown as root |
| `provision_declared_env` | declared `env` keys (not values) | unsupported: `env` is a process map, not a package graph |

Repository intents (`ensure_repository`, `ensure_actions`,
`ensure_bootstrap`, `ensure_secrets`, `ensure_protection`,
`ensure_identities`, `retain_on_remove`) are not host intents. See
[repositories.md](repositories.md). They are returned on the sync response
and are not applied.

`host_mutation` is `"applied"` when at least one dedicated user was
reconciled, otherwise `"none"`. `applied` lists `ensure_agent_user:<id>`.
Rules-only sync sends an empty plan (`agents: false`) and does not list
retained users. Removing dedicated agents from YAML still does not delete
the Unix user or home; leftover names are listed in `retained` on an agents
sync.

Dedicated `home` is always `/home/<user>`. `cwd` is inside that home, or
exactly `/tmp` / `/var/tmp` (sticky, never chowned). Root will not mkdir or
chown paths under `/tmp`, `/opt`, or other unmanaged trees.

Allowlisted setup today: `workspace` (`private` / `shared-read` /
`shared-write`) and existing supplementary `groups` (not `root`/`sudo`/other
reserved groups). The shell is always `/bin/bash`. Deliberately deferred:
packages, file copies, git clone, extra shells, crontab, mounts, capabilities,
sudo, and any `command` / script field.

Dedicated runs are a second privileged op (`spawn` / `wait`) that passes
stdio fds over the same socketpair. The parent execs the embedded Python
runner with `syscall.Credential` for a username it has previously
reconciled. Shared-UID agents still start in the listener process.

## Failure semantics

These guarantees are in-process only.

| Event | Matcher cache | Host | In-flight runs |
|---|---|---|---|
| YAML load failure | unchanged | unchanged | keep their snapshot |
| IPC error / timeout / parent death during coordinate | unchanged | host may be partially mutated; no rollback | keep their snapshot |
| Dedicated-user reconcile failure | unchanged | host may be partially mutated; no rollback | keep their snapshot |
| Successful coordinate with only unsupported diffs | swapped to the new generation | unchanged | keep their snapshot |
| Successful dedicated-user reconcile | swapped after apply | users/homes/workspaces ensured | keep their snapshot |
| Crash during swap | one generation or the other; not a mix of agents from G and rules from G' | already applied | keep their snapshot |
| `503` during sync | previous generation | n/a | keep their snapshot |

Not claimed, and not implemented:

- Atomicity with another process, container, or host.
- Rollback of host mutations.
- A durable apply journal.
- Deletion of Unix users or homes when YAML is removed.

A rules-only sync can activate a new rule against a previously cached agent
while a newly written agent file stays invisible. An agents-only sync can
leave the matcher on old rules. That mismatch is a consequence of optional
scope, not a transaction.

Secrets are still the listen process environment at start. Sync does not
reload `DEEPSEEK_API_KEY`. The runner strips `GENESIS_SYNC_TOKEN` and
`GENESIS_PRIVILEGED_FD` from Python's inherited environment.

## Observe

`GET /health` and `GET /generation` are read-only. They do not reload YAML,
swap the cache, or accept a bearer. `/generation` returns the active agents,
rules, repositories, digest, whether a sync token is configured, and whether
a sync is in progress. `repositories_active` is false when `repos.d` is
absent. Agent objects include optional `user` and `setup`. It does not
return the token or any secret value. `POST /events` is unchanged.

`genesis control` is a second process. It reads the config directories and
the run journals, proxies CloudEvents and authorized sync to the listener,
and serves the panel. The journal file remains the replay source. See
[control.md](control.md).

## Docker Desktop on Windows / WSL

Use the Linux engine through WSL. See the README for the exact smoke
commands. Compose runs `genesis launch` as root, drops the listener to the
image `genesis` user, reconciles dedicated agent users inside the container,
and does not publish the listener port. The control
process is the published UI at host `127.0.0.1:8790` and runs as uid
65532. It mounts `genesis-config` read-write and `genesis-data` read-only,
and starts only after the listener entrypoint has given those volumes to
`genesis`. The listener keeps
`genesis-data` read-write so it can append journals. Agents and rules live
on `genesis-config` at `/var/lib/genesis/config`. The entrypoint copies
**missing** regular YAML files from `/usr/share/genesis/defaults` on every
start and never overwrites existing volume YAML or dest symlinks. Run journals live on
`genesis-data` at `/var/lib/genesis/data`. `docker compose down` keeps both
volumes; `docker compose down -v` deletes them so the next `up` reseeds
config defaults and starts with empty run storage. The binary and
`/app/.venv` are not writable by `genesis`. Supply `GENESIS_SYNC_TOKEN` in
compose; the binary will not invent one. Dedicated agent homes live under
`/home/<user>` in the container writable layer and are recreated on the next
launch if the container is replaced. The designer agent omits `user` and
uses the config volume as cwd; see [designer.md](designer.md).
