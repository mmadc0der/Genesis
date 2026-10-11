# Run

First start: [quickstart.md](quickstart.md).

| Topic | Doc |
| --- | --- |
| Flags and `job` / `schedule` | [cli.md](cli.md) |
| Agent and rule YAML | [agents.md](agents.md) |
| `POST /events`, journals, `POST /sync` | [http.md](http.md) |
| Logs and live check | [troubleshooting.md](troubleshooting.md) |
| One shared-UID YAML and CloudEvent | [examples/one-run.md](examples/one-run.md) |
| Cache, `/sync`, launch failure semantics | [orchestrator.md](orchestrator.md) |
| Designer agent | [designer.md](designer.md) |
| Control API and Wire | [control.md](control.md) |
| GitHub App ingress | [webhooks.md](webhooks.md) |
| Repository declarations | [repositories.md](repositories.md) |
| Provider identities and grants | [providers.md](providers.md) |
| Publications | [publications.md](publications.md) |
## Repositories

`repos.d` is optional desired GitHub state. A missing directory leaves the
layer inactive and does not change the agent/rule digest. The committed
`repos.d/example.yaml` is a declaration for `octo-org/lab-widget`. Without
a reconciler key, observation is `unavailable` and GitHub is not called.
With that key, `existing: adopt` creates the repository when GitHub
reports that it does not exist, binds its id, and applies visibility,
description, features, merge settings, Actions, the declared ruleset, and
missing bootstrap files. `existing: refuse` makes no HTTP call. `remote_mutation` is
`applied` only for intents this sync wrote. Secrets, identities, and
webhook configuration stay unsupported. A dedicated run can still mint its
own short-lived installation token and check out the bound repository.
The App needs Administration write and Contents write; bootstrap uses a
separate contents token. The running designer reads the loader contract
from `/usr/share/genesis/schema/repository-declaration.txt`, generated from
the same structs that load `repos.d`. `repos.d/example.yaml` is an example,
not that contract. Operator-facing rules are in
[repositories.md](repositories.md).

## Providers

`providers.d` is optional root-owned GitHub identity state. A missing
directory leaves the layer inactive and does not change the agent/rule
digest. Files name one GitHub organization, named identities, and secret
references. They never contain secret values. The directory must sit
outside the designer-writable config volume. The image keeps an empty
`/etc/genesis/providers.d` (`root:genesis`, mode `0750`) and does not copy
it or `providers.d/example.yaml` into `genesis-config`. Compose bind-mounts
`./runtime/providers` there for the listener and control services, and
`./runtime/secrets` onto `/var/lib/genesis/secrets` for the root listener
only. `runtime/` is gitignored. On start, root tightens provider files to
`0640` and secret files to `0600`. The entrypoint does not create
`/var/lib/genesis/secrets` or the files inside it. When that directory
exists, startup locks it. `GITHUB_APP_RECONCILER_PEM`, `GITHUB_APP_ID`,
and `GITHUB_APP_WEBHOOK_SECRET` may be absent. A symlink or other non-file
at one of those names exits. GitHub calls fail closed until the files they
need are present. Dedicated agent users cannot read
the provider directory.

An agent may declare an optional `github` capability: a `repos.d`
repository, a non-reconciler identity, git access `none`/`read`/`write`,
and allowlisted API permissions. The designer agent cannot. Reconciler
identities, administration, secrets, and unsafe role combinations fail
closed. Public read can use a reader identity with `credential: none`.

Sync returns a `grant_plan` whose `credential_active` is false. Root
`launch` stores an Ed25519 identity per non-public git grant under
`-credentials` and, when `-secrets` holds the reconciler App key, registers
that public key as a repository deploy key. The listener summary stays
`pending` or `none`; the root result is `material.remote_status`. No socket
is delivered until that status is `ready`, and a changed, removed, or refused
grant drops the socket immediately. Public-read and `git: none`
grants do not receive a token. A dedicated run whose grant requires an
API credential receives one installation token as `GITHUB_TOKEN` for that
process only. Root redacts it from logs, journals, events, the control
API, `dsh_home`, and errors, and removes it when the run ends.
`credential_active` is true only while that token is in the run process;
sync and the panel keep it false. If git access is read or write and the
deploy-key socket is ready, root clones into an empty `cwd` or fetches
when `cwd` is already that repository, using `SSH_AUTH_SOCK` and never the
token in the remote URL. Any other `cwd` contents, a failed mint, or a
failed checkout does not start the session. Private keys, App JWTs, and
installation tokens stay out of logs and APIs. See
[providers.md](providers.md).

The policy header looks like this. The complete file, including settings,
bootstrap, Actions, secret names, ruleset intent, and runtime identities, is
`repos.d/example.yaml`.

```yaml
provider: github
org: octo-org
name: lab-widget
lifecycle:
  remove: retain
  existing: adopt
```

Genesis itself can start without a key, which keeps configuration checks and
credential-free tests usable. A matching real SDK run without a key fails
asynchronously and reports the SDK error through the normal structured
completion diagnostics.

## Docker Desktop on Windows with the repo in WSL

This path is for Docker Desktop's Linux engine via a WSL distro. It is not
validated on this project's cloud VMs unless `docker` is installed there.

Agents and rules are **not** bind-mounted from the WSL tree. Compose mounts
the named volume `genesis-config` at `/var/lib/genesis/config` and
`genesis-data` at `/var/lib/genesis/data`. Those volumes are owned by the
image `genesis` user (uid `65532`). On every listener start the entrypoint
copies **missing** default YAML from `/usr/share/genesis/defaults` into the
volume. Existing files are left unchanged, including operator-edited janitor
or rule YAML on a reused volume, and dest symlinks are not written through.
Only regular files with valid agent/rule names are copied. That is how an
image upgrade can deliver `designer.yaml` without overwriting desired config.
The entrypoint also creates `repos.d` when it is missing. This image does not
seed a repository declaration; an empty `repos.d` is an active, empty layer.
A deleted default filename is copied again; file contents are never replaced.
Ownership passes skip non-regular files so a planted volume symlink cannot
redirect root `chown`/`chmod`. The genesis
binary and `/app/.venv` stay root-owned and are not writable by `genesis`.

The listener is not published on the host. The control service is the UI,
published only as `127.0.0.1:8790`. It mounts config read-write and run data
read-only. The listener still writes journals.

1. Install Docker Desktop on Windows and enable WSL 2.
2. Settings → Resources → WSL integration: enable your distro (for example
   Ubuntu).
3. Clone the repository **inside WSL**, for example `~/src/genesis`, so the
   image build context is a Linux filesystem. Do not build from `/mnt/c/...`.
4. From a WSL shell, not PowerShell. Create the gitignored runtime
   directories before Compose starts; the bind mounts require them.

```sh
cd ~/src/genesis
mkdir -p runtime/providers runtime/secrets
docker compose version
export DEEPSEEK_API_KEY='replace-with-a-real-key'
export GENESIS_SYNC_TOKEN='compose-sync-token'
docker compose up --build
```

5. Edit configuration **inside the volume** as `genesis`, then sync:

```sh
docker compose exec -u genesis genesis tee \
  /var/lib/genesis/config/rules.d/lab.yaml >/dev/null <<'YAML'
match:
  type: dev.genesis.lab
agent: workspace-janitor
YAML
curl -i http://127.0.0.1:8790/api/sync \
  -H 'Content-Type: application/json' \
  --data '{"scope":["agents","rules"]}'
```

The browser does not send the sync token. The control process adds it when
it calls the listener. Windows browsers use `http://127.0.0.1:8790/` because
Docker Desktop publishes that port on localhost only.

6. One-shot wrapper: `sh scripts/wsl-docker-smoke.sh`. It keeps named
   volumes, writes and removes a temporary rule as `genesis`, checks that the
   edit stays inactive until `/sync`, and checks that missing designer files
   were seeded without overwriting existing YAML. If the volume janitor lacks
   `user:`, it fails closed with a persisted-volume diagnosis instead of
   treating shared-UID output as a dedicated-user pass.
7. Destructive clean-volume smoke (deletes `genesis-config` and
   `genesis-data`): `sh scripts/wsl-docker-smoke.sh --reset-volumes`.
   Inspect or update a reused volume without deleting it: see
   [designer.md](designer.md).

Compose runs `genesis launch` as root and drops the listener to `genesis`.
Dedicated agent users are created in the container at launch and on agents
`/sync`. The control process runs as uid 65532 (`genesis`). It waits until the
entrypoint has chowned config and data to that user, so it can write config
and read the listener's `0700` journals through the read-only data mount
without starting as root. Supply `GENESIS_SYNC_TOKEN`;
the binary will not generate one. Do not set `network_mode: host` (it does
not mean the same thing on Docker Desktop). Line endings are forced to LF
via `.gitattributes`.

Persistence:

- `docker compose down` stops the container and **keeps** `genesis-config`
  and `genesis-data`. The next `up` reuses the same agents, rules, and run
  journals, then copies any **new** default filenames that are missing.
  Existing file contents are not replaced. `./runtime/providers` and
  `./runtime/secrets` stay on the host either way.
- `docker compose down -v` deletes `genesis-config`, `genesis-data`, and
  `genesis-credentials`. The next `up` reseeds the image defaults into a
  new config volume and starts with empty run storage. That is the explicit
  destructive reset. It does not delete `runtime/`.

If only the `genesis` OS user exists, agent output is owned by `genesis`, the
UI says Shared listener UID, and active/draft digests match, that is reused
pre-upgrade volume YAML, not a successful dedicated-user test. Inspect and
optionally copy a single default file, or use `--reset-volumes`. Do not
weaken persistence to make that look like a pass.
