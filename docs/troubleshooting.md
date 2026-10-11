# Troubleshooting

How to file a report, and which secrets to leave out, is in
[CONTRIBUTING.md](../CONTRIBUTING.md). This page is where a failure shows up
in each part of Genesis. Status codes and journal fields are specified in
[http.md](http.md). Cache and sync failure semantics are in
[orchestrator.md](orchestrator.md).

Secret values are omitted from journals, slog, and `stderr.log`. Do not
paste `DEEPSEEK_API_KEY`, `GENESIS_SYNC_TOKEN`, webhook secrets, or grant
material.

## Where a run is recorded

Listener stdout is JSON `slog`. A completion line includes `genesis_run_id`,
`rule`, `agent`, `run_dir`, `deepseek_session_id`, `finish_reason`,
`final_response`, `error_type`, `error`, `diagnostics`, and captured runner
`stderr`. Any finish reason other than `completed` is `ERROR`.

The run directory is under `-data` (default `genesis-data`, in Compose
`/var/lib/genesis/data`):

```
runs/gen_<hex>/events.jsonl
runs/gen_<hex>/stderr.log
runs/gen_<hex>/result.json
runs/gen_<hex>/session.json
runs/gen_<hex>/queue.json          # only while the run waits on max_parallel
sessions/<root gen_>/dsh_home/     # retained SDK home, shared by continuations
```

`events.jsonl` is the lifecycle of that run. `result.json` is the terminal
runner object. `stderr.log` is the runner process. `session.json` names the
DeepSeek session, `dsh_home`, and `continued_from` when this run is a
continuation. The control process reads these files. It does not keep a
second copy of the run. Compose: `docker compose logs genesis` for the
listener, `docker compose logs control` for the panel.

`GET /health` and `GET /generation` on the listener are read-only. They do
not reload YAML. `/generation` shows the active agents, rules, repositories,
digest, whether a sync token is configured, and whether a sync is in
progress.

## Listener and matcher

`POST /events` returns before the model finishes. `202` means one run
directory already exists per matching rule; the body lists `rule`, `agent`,
and `run_id`. `204` means nothing matched. A bug that looks like "the agent
never started" is usually a `204`: the event `type`, `source`, or `subject`
did not match the active generation. `subject` globs are in
[agents.md](agents.md). Attributes other than `subject` are exact.

`503` with `Retry-After: 1` means a sync is in progress. The previous
generation is still the matcher. `401` `sync is disabled` on `POST /sync`
means the listener was started with an empty token. Invalid YAML on sync
leaves that previous cache in place.

The run id in the `202` body is the directory to open. If that directory
exists and `events.jsonl` stops at `dev.genesis.run.accepted`, the accept
path worked and the runner has not produced a later lifecycle event yet.

## Runner

After accept, the failure is in that run's `result.json`, `stderr.log`, and
the completion slog line, not in the HTTP status. `finish_reason` other
than `completed` is the harness outcome. `error_type`, `error`, and
`diagnostics` are the SDK detail. Diagnostics include the relevant
`turn/end`.

A `turn/end` whose error code is `TRANSPORT` is retried on the same DSH
session and the same `dsh_home`. That journals `dev.genesis.run.restart`.
It is not a `dev.genesis.session.continue`, and the failed run does not
emit `dev.genesis.agent.finished` when the restart is accepted. Three
restarts in a row is the limit. Model-request retries stay inside the
harness retry policy and show up as `llm/retry` in `events.jsonl`.

`DEEPSEEK_API_KEY` is the listener environment at process start. Sync does
not reload it. A missing or rejected key fails the run asynchronously and
shows up as the SDK error on that completion line.

Continuations share `sessions/<root gen_>/dsh_home/`. A new `gen_` directory
with the same `dsh_home` in `session.json` is a continuation or a transport
restart, not a second session. Agent continuations stop at hop 9.

## Control

Control is a second process. It reads the config directories and the
journals, and it proxies events and sync to the listener. The browser does
not send `GENESIS_SYNC_TOKEN`. If the panel is up and runs stay empty, the
listener and the control process are not looking at the same `-data` or the
same agents directory. On Compose the panel is `http://127.0.0.1:8792/`.
A bare-metal `genesis control` defaults to `127.0.0.1:8790` and talks to
`127.0.0.1:8787`. The listener port is not published by Compose.

`GET /api/runs/{id}/events` is the journal. A missing run there is a missing
directory, not a UI filter. Drift and digest on the panel compare the
control process's view of disk with the listener's active generation. They
do not compare disk with the image defaults.

## Jobs and schedules

`genesis job` and `genesis schedule` post to the listener `POST /events`
with no bearer. A job that exits without a new run posted a CloudEvent that
no rule matched (`204`), or the post itself failed. The job state records
`emit_error` when the post fails. Reserved types
`dev.genesis.agent.finished`, `dev.genesis.session.continue`, and
`dev.genesis.publication.submitted` are rejected before that post. Schedule
bounds and the ticker are in [cli.md](cli.md). A rule file with `interval`
or `cron` fails at load; the schedule command is the ticker.

## Sync, users, and the designer

Disk edits do nothing until `POST /sync` swaps the generation. A rules-only
scope can attach a new rule to a cached agent and leave a newly written
agent file inactive. An agents-only scope can leave the matcher on the old
rules. In-flight runs keep the snapshot they started with.

Dedicated-user reconcile runs at root `launch` and on an agents sync. A
reconcile failure leaves the cache unchanged. The host may already have a
partial user or home; there is no rollback. Non-root `launch` refuses to
start if any agent declares `user`.

The designer has no `user` and is not a sync client. Files it writes stay
drafts until an operator syncs. `GENESIS_SYNC_TOKEN` is not in its
environment. If the only OS user is `genesis`, agent output is owned by
`genesis`, the panel says Shared listener UID, and the digests match, the
volume still holds pre-upgrade YAML. The entrypoint does not replace
existing files. That picture is in [designer.md](designer.md). Volume
create and delete are in [build.md](build.md).

## GitHub

Providers, repository declarations, deploy keys, and webhooks fail closed
and in different places.

A missing `providers.d` or `repos.d` leaves that layer inactive.
`repositories_active` on `/generation` is false when `repos.d` is absent.
An empty directory is active. Provider YAML holds secret names, not PEMs.
The PEM, `GITHUB_APP_ID`, and `GITHUB_APP_WEBHOOK_SECRET` live in the root
secrets directory (`genesis-secrets`, or `runtime/secrets` in Compose). A
missing file does not crash startup. The GitHub call that needs it fails.
A symlink or a non-file at one of those names refuses startup. Paths are in
[build.md](build.md).

`POST /webhooks/github` returns `401` `signature rejected` before any rule
runs and before a delivery record is written. Standalone `listen` cannot
verify and returns `503` `webhook verification is unavailable`. Compose
does not publish the listener, and control does not proxy this route. See
[webhooks.md](webhooks.md).

A repos sync both observes and, for `existing: adopt`, writes the declared
shape: settings, Actions, the ruleset, and missing bootstrap files. A
mismatch between the declaration and what GitHub returns, or a desired
state that changes during the read, fails the sync and keeps the previous
generation. The read order and the `unobservable` cases are in
[repositories.md](repositories.md). There is no separate read-only probe.
An older checklist that expected GitHub to stay unchanged is obsolete.

Sync can report a grant plan whose `credential_active` is false. That is
the normal panel view. Root `launch` stores the deploy key under
`-credentials` and registers it when the reconciler key is present. No
socket is delivered until `material.remote_status` is `ready`. A changed,
removed, or refused grant drops the socket immediately. The per-run
`GITHUB_TOKEN` exists only in that process. Checkout failures (mint,
non-empty `cwd`, fetch) do not start the session. Stages are in
[providers.md](providers.md) and [repositories.md](repositories.md).

## Publications

`genesis publish` is accepted only from an agent whose `setup.groups`
contains `reporter`. A publish from any other agent is a contract refusal,
not a lost file. The register and the read API are in
[publications.md](publications.md).
