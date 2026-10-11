# Agents and rules

One YAML file per agent and per rule. The listener caches them at start;
later filesystem edits are inactive until an authorized `POST /sync`. See
[http.md](http.md) and [orchestrator.md](orchestrator.md).

## Agents

One agent lives in each `.yaml` or `.yml` file. The filename stem is the agent
ID (`workspace-janitor.yaml` → `workspace-janitor`):

```yaml
instructions: |
  You are the workspace janitor for this lab.
  Read the CloudEvent, do only the work it asks, write results in the workspace, then stop.
user: workspace-janitor
cwd: /home/workspace-janitor/workspace
home: /home/workspace-janitor
setup:
  workspace: private
env:
  PATH: /usr/local/bin:/usr/bin:/bin
  LANG: C.UTF-8
```

`instructions`, `cwd`, and `home` are required. `cwd` and `home` must be
absolute and distinct. `user` is optional. When set, it is a dedicated Linux
account name (lowercase, not `root`/`genesis`/other reserved names). Two
agents cannot share an OS user. `home` must be `/home/<user>`. `cwd` must be
inside that home, or exactly `/tmp` or `/var/tmp`. `setup` is allowed only with `user` and is a
closed contract: `workspace` (`private`, `shared-read`, `shared-write`) and
optional `groups`. Genesis creates a missing group. The group `shared`
ensures `/shared` (`root:shared`, mode `2770`, setgid) so members can place
files there. There is no command, script, package, or shell
field; the account always gets `/bin/bash`. `env` is the complete non-secret
environment given to that agent's Harness runtime; omitted, it is empty. Genesis then adds named
secrets from its own startup environment (default `DEEPSEEK_API_KEY` when the
value is non-empty), sets `HOME` from `home`, and sets `DSH_SYSTEM_PROMPT`
from `instructions` for the pinned `sdk-minimal` persona hook. It puts the
`genesis` binary's directory on `PATH` (default
`/usr/local/bin:/usr/bin:/bin` when `PATH` is omitted) and sets
`GENESIS_EVENTS_URL` to that listener's `/events` URL. It does not copy
`GENESIS_SYNC_TOKEN`, `GITHUB_APP_WEBHOOK_SECRET`, or the root secret store
into the agent. Declaring
`HOME`, `DSH_SYSTEM_PROMPT`, or any listed secret in `env` is rejected.
Dedicated agents also reserve `USER`, `LOGNAME`, and `SHELL`. Secret values
never appear in YAML. Duplicate IDs, duplicate secret names, duplicate OS
users, unknown fields, and invalid filename stems are rejected. `max_parallel`
is optional and defaults to 1. When set, it must be an integer of at least 1.
`reasoning_effort` is optional: `off`, `low`, `high`, or `max`. Omitted, the
harness keeps its default, `high`.

`cwd` is the SDK workspace and `home` is the child process Unix `HOME`. When
`user` is set, `genesis launch` as root and `POST /sync` of agents reconcile
a dedicated OS account (Bash shell, home, ownership, and workspace access)
and the runner executes as that account. Without `user`, cwd/home remain a
workspace/environment split under the listener UID and are not a security
sandbox. YAML still cannot name packages or arbitrary root commands.

Several rules may point at the same agent. `max_parallel` is how many runs
of that one agent may execute at once. The default is 1: a second match for
the same agent is still accepted (`202` and a new run id) and waits until
one of that agent's running runs finishes. Continuations of the agent count
toward the same cap. A different agent is not in that queue. Two agents
start together whenever each is under its own cap, including when both are
at the default of 1. Raising `max_parallel` allows that many runs of the
same agent at once; they still share `cwd` and `home`. Each waiting run keeps
`queue.json` in its run directory, without secret values. A restart loads
those files and starts every agent that is under its own cap. A run that
had already started and has no live process is still closed as interrupted.
`max_parallel` is not a token budget.

The committed `designer` agent omits `user` on purpose so it can keep
`cwd: /var/lib/genesis/config` (the writable Compose config root). It runs as
the shared listener UID. Its standing instructions describe agents, rules,
event matching, `user`/`home`/`cwd`, automatic dedicated-user reconcile,
safe YAML edits, active vs desired generations, and that an operator must
Sync after it writes files. It may create or update agent and rule YAML. It
must not reconcile OS users, install packages, or perform arbitrary root
actions. Dedicated `cwd` cannot be that config path; do not add `user:` to
the designer. Dedicated workers it writes must declare `user`,
`home: /home/<user>`, `cwd: /home/<user>/workspace`, and
`setup.workspace: private`; Genesis creates that account on launch or
agents `/sync`.

A minimal shared-UID file, one CloudEvent, and the journal names are in
[examples/one-run.md](examples/one-run.md).

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
CloudEvent attributes. `type`, `source`, and every attribute other than
`subject` stay exact: a `*` there is a literal character, not a wildcard.
`agent` is a required agent ID. Every matching rule runs, in lexical filename
order. The listener caches agents and rules at start; later filesystem edits
are inactive until an authorized `POST /sync`. A missing agent reference or
leftover `run` block fails closed at load or sync time.

`subject` may use a glob. A segment that is exactly `*` matches one non-empty
path segment and does not cross `/`. A pattern with no `*` is exact, including
subjects that contain slashes. `**`, `?`, and character classes are not
wildcards.

```yaml
match:
  type: dev.genesis.run
  subject: worker/*
agent: workspace-janitor
```

That matches subject `worker/run-1` and does not match `worker/run-1/extra` or
`worker`. `*/*` matches exactly two segments, such as `oracle/1`.

`rules.d/designer.yaml` is the dedicated user-message rule. `POST
/api/messages` copies its match (`type: dev.genesis.user.message`, `source:
urn:genesis:control`, `subject: designer`) and puts the message text in
`data.message`.

The rule name is its filename. Rules have no `cwd`, environment, arguments,
prompt, model, executable, or other run configuration.
