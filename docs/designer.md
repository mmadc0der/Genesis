# Designer agent and named-volume config

The committed `designer` agent is a shared-UID config editor. It is ordinary
`agents.d` / `rules.d` YAML. It is not a new Genesis subsystem, not a dedicated
OS user, and not an authorized `/sync` client.

## What it is

| Piece | Location | Contract |
|---|---|---|
| Agent | `agents.d/designer.yaml` | No `user` / `setup`. `cwd` is `/var/lib/genesis/config`. `home` is `/home/genesis`. |
| Rule | `rules.d/designer.yaml` | Exact match on `type=dev.genesis.user.message`, `source=urn:genesis:control`, `subject=designer`. |
| Panel | Control composer | Select `designer.yaml` (shown as `dev.genesis.user.message`). Type a message. Send. Then Sync after it writes YAML. |
| Schema | `/usr/share/genesis/schema/repository-declaration.txt` | Loader contract for `repos.d`. World-readable in the image. Not copied into the config volume. |

The listener UID (`genesis` in Compose, uid `65532`) runs this agent. The
panel Details row is **Shared listener UID**. Output files it creates under
the config volume are owned by `genesis`. That is expected, not a dedicated-user
proof.

The designer may create or update agent, rule, and repository YAML. Before
it writes `repos.d`, it reads `/usr/share/genesis/schema/repository-declaration.txt`
and follows that file instead of inspecting the genesis binary. The schema
is generated from the loader structs. `repos.d/example.yaml` and the
lab-widget template are examples, not that contract. Organization discovery
is `/etc/genesis/providers.d/*.yaml` (`org`); there is no `site.yaml`.
Provider files, secret values, and root-only policy are not installed next
to the schema. It must not add a `github` block to itself, and it must not
create `providers.d` inside the config volume. Provider identities live
outside that volume; see [providers.md](providers.md). It must not run
privileged host setup, `useradd`, package installs, or arbitrary root
actions. YAML still cannot name packages or commands. After it writes files,
an operator must **Sync** (`POST /sync` with both `agents` and `rules`).
Disk edits stay inactive until then. Matching active/draft digests only
mean the cache equals the files currently on the volume.

## Dedicated workers it writes

This designer omits `user`. Workers it creates (especially agents that wake
on repository events or issues) must not copy that identity. They declare a
dedicated OS user. Genesis's privileged coordinator automatically
reconciles the account at root `genesis launch` and on agents `/sync`:
`/bin/bash`, `/home/<user>` mode `0700`, and the workspace directory. The
designer does not `useradd`.

Required shape:

```yaml
user: <username>
home: /home/<username>
cwd: /home/<username>/workspace
setup:
  workspace: private
```

Do not omit `user`. Do not set `home: /home/genesis`. Do not point `cwd` at
`/tmp` or `/var/tmp` unless the operator asked for shared or ephemeral work.

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
text from the composer. The listener still matches only the **active**
generation, so a newly seeded designer rule does not run until Sync if the
process was already up with an older cache. A container start that ran the
entrypoint first loads whatever is on the volume, including newly copied
defaults.

Do not put `GENESIS_SYNC_TOKEN` in designer `secrets`. The control process
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
- the control panel Details row says **Shared listener UID**
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

Then in the panel select `dev.genesis.user.message` / `designer.yaml`, send a
message, and wait for the run. Edits remain drafts until Sync.

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
launch creates that OS user, and the panel shows `OS user workspace-janitor`
for that agent. The designer still omits `user` and still shows Shared
listener UID.

`sh scripts/wsl-docker-smoke.sh` without `--reset-volumes` keeps the named
volumes. If the volume janitor lacks `user:`, that script fails closed and
prints this persisted-volume diagnosis instead of treating shared-UID output
as a dedicated-user pass.
