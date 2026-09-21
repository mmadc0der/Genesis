# Orchestrator, cached generations, and POST /sync

This is the architecture Genesis now implements. It is smaller than a root
reconciler, and it does not pretend that matcher activation and host
provisioning are one transaction.

## Processes

`genesis listen` is the existing unprivileged HTTP listener. Plain `genesis`
with flags is still listen, so previous command lines keep working.

`genesis launch` is the orchestrator. It:

1. Creates a private `SOCK_STREAM` Unix `socketpair`.
2. Starts `genesis listen` as a child, inheriting only the child end as file
   descriptor `3` (`GENESIS_PRIVILEGED_FD=3`).
3. When the parent is root, execs that child as `-listener-user` /
   `GENESIS_LISTENER_USER`. Missing or unknown target users fail closed
   before listen starts. Non-root launch leaves the listener at the current
   uid.
4. Serves typed privileged coordination on the parent end.
5. Forwards `SIGINT` / `SIGTERM` to the child and exits with the child's code.

The pair is never bound to a filesystem path. Nothing else can connect to it.
The listener sets close-on-exec on the inherited descriptor so Python/`dsh`
children do not receive it. Launch does not escalate. A root parent is only
a supervisor that can drop the listener uid; it is not a Linux host
provisioner.

## Cache

The listener loads `agents.d` and `rules.d` once at start and keeps an
immutable in-memory generation (agent map, rule list, content digest).
`POST /events` matches only that snapshot. Editing YAML on disk does nothing
until `POST /sync`. In-flight runs already hold the accept-time agent copy.

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
{"scope": ["agents", "rules"]}
```

Omitted or empty `scope` means both layers. `["rules"]` rereads rules against
the cached agents. `["agents"]` rereads agents and revalidates cached rules.
Unknown scope values are `400`. Scope is which directories to reread. It is
not independent activation of host vs matcher state, and it is not an mtime
"only new files" filter.

Successful sync:

1. Load the requested YAML. On failure: `500` (`agents are invalid` /
   `rules are invalid`), cache unchanged, no IPC.
2. Build a typed privileged plan from the would-be generation.
3. If a coordinator is attached, send `{op:"coordinate", payload: plan}` over
   the inherited socket and wait. IPC failure: `500` `privileged coordination
   failed`, cache unchanged.
4. Swap the in-process generation pointer.
5. Return `200` with digest, counts, and the privileged result.

Standalone listen (no inherited fd) still reloads the cache. The JSON reports
`privileged.attached: false` and classifies the same plan locally.

## Privileged protocol

Length-prefixed JSON (32-bit big-endian size, then one object). Unknown JSON
fields and unknown `op` / intent `kind` values fail closed.

The child sends a plan of intents. The parent answers with
`host_mutation`, `applied`, and `unsupported`. Current schemas have no OS
user, packages, repos, or profiles, so the only generated kinds are:

| kind | meaning | current evaluation |
|---|---|---|
| `ensure_agent_paths` | agent `cwd` / `home` strings | unsupported: no OS user; Genesis does not mkdir/chown as root |
| `provision_declared_env` | declared `env` keys (not values) | unsupported: `env` is a process map, not a package graph |

`applied` is always empty. `host_mutation` is always `"none"`. No `useradd`,
`chown`, `apt`, or toolchain install is attempted. That is deliberate, not a
stub that reports success.

## Failure semantics

These guarantees are in-process only.

| Event | Matcher cache | Host | In-flight runs |
|---|---|---|---|
| YAML load failure | unchanged | unchanged | keep their snapshot |
| IPC error / timeout / parent death during coordinate | unchanged | unchanged (nothing was applied) | keep their snapshot |
| Successful coordinate with unsupported diffs | swapped to the new generation | unchanged | keep their snapshot |
| Crash during swap | one generation or the other; not a mix of agents from G and rules from G' | unchanged | keep their snapshot |
| `503` during sync | previous generation | n/a | keep their snapshot |

Not claimed, and not implemented:

- Atomicity with another process, container, or host.
- Atomicity between YAML files and Unix users, homes, or packages.
- Rollback of host mutations (none are performed).
- A durable apply journal.

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
rules, digest, whether a sync token is configured, and whether a sync is in
progress. It does not return the token. `POST /events` and `POST /sync` are
unchanged.

`genesis control` is a second process. It reads the config directories and
the run journals, proxies CloudEvents and authorized sync to the listener,
and serves the panel. The journal file remains the replay source. See
[control.md](control.md).

## Docker Desktop on Windows / WSL

Use the Linux engine through WSL. See the README for the exact smoke
commands. Compose runs `genesis launch` as root, drops the listener to the
image `genesis` user, and does not publish the listener port. The control
process is the published UI at host `127.0.0.1:8790` and runs as uid
65532. It mounts `genesis-config` read-write and `genesis-data` read-only,
and starts only after the listener entrypoint has given those volumes to
`genesis`. The listener keeps
`genesis-data` read-write so it can append journals. Agents and rules live
on `genesis-config` at `/var/lib/genesis/config`, seeded from
`/usr/share/genesis/defaults` on first volume creation. Run journals live on
`genesis-data` at `/var/lib/genesis/data`. `docker compose down` keeps both
volumes; `docker compose down -v` deletes them so the next `up` reseeds
config defaults and starts with empty run storage. The binary and
`/app/.venv` are not writable by `genesis`. Supply `GENESIS_SYNC_TOKEN` in
compose; the binary will not invent one. The privileged protocol still
reports unsupported host diffs.
