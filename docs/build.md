# Build

Go 1.23+, Python 3.10+, Node.js 22+, and
[`uv`](https://docs.astral.sh/uv/getting-started/installation/) are required.

```sh
uv sync --locked
mkdir -p bin
go build -o bin/genesis ./cmd/genesis
```

`uv run` places the managed `.venv` first in `PATH`. Equivalently, activate it
with `. .venv/bin/activate` before running `./bin/genesis`. Genesis resolves
`python3` once at startup and embeds its one-shot Python runner in the Go
binary.

Node is required only to build the UI:

```sh
npm ci --prefix web
npm run build --prefix web
```

How to start the listener and the control process is in
[quickstart.md](quickstart.md).

## Credential-free verification

The ordinary checks use fake runners and a mocked SDK; they need no API key or
network call:

```sh
uv lock --check
uv sync --locked
test -z "$(gofmt -l cmd/genesis/*.go)"
uv run --locked ruff format --check cmd/genesis/runner.py cmd/genesis/test_runner.py
uv run --locked ruff check cmd/genesis/runner.py cmd/genesis/test_runner.py
go vet ./...
go test ./...
go test -race ./...
uv run --locked python -m unittest discover -s cmd/genesis -p 'test_*.py'
PYTHONPYCACHEPREFIX=/tmp/genesis-pycache \
  uv run --locked python -m py_compile \
    cmd/genesis/runner.py cmd/genesis/test_runner.py
go build -o /tmp/genesis ./cmd/genesis
npm ci --prefix web
npm test --prefix web
npm run check --prefix web
npm run build --prefix web
```

## Bare metal

The commands at the top of this page produce `bin/genesis` and `web/dist`.
Run them from the repository root. `uv sync --locked` creates `.venv`; that
environment must be on `PATH` when `genesis` starts, because the binary
resolves `python3` once and uses it for the DeepSeek Harness runner.

A real session needs `DEEPSEEK_API_KEY` in the environment of the `genesis`
process. It is not a flag and it does not belong in a file under the
repository. `GENESIS_SYNC_TOKEN` is the bearer for `POST /sync`. Leave it
unset to keep sync disabled.

### Minimal set

Enough to launch, accept events, and call the model. No GitHub App, no
deploy keys, no webhook.

| What | Where |
| --- | --- |
| Agents and rules | `agents.d/` and `rules.d/` in the checkout. These are the files `launch` loads. |
| Journals | `-data genesis-data`. `launch` creates this directory. |
| Model key | `DEEPSEEK_API_KEY` in the environment of `launch` and of any later listener you start yourself. |
| Sync bearer | `GENESIS_SYNC_TOKEN` in that same environment, and again in the `control` process if you want the UI to sync. |

Do not put the model key in `agents.d`, `rules.d`, or `providers.d`. A missing
`repos.d` or `providers.d` leaves that layer inactive. Non-root `launch` does
not open `-credentials` or `-secrets`.

```sh
export DEEPSEEK_API_KEY='replace-with-a-real-key'
export GENESIS_SYNC_TOKEN='replace-with-a-long-token'
./bin/genesis launch \
  -listen 127.0.0.1:8787 \
  -agents agents.d -rules rules.d \
  -data genesis-data
./bin/genesis control \
  -listen 127.0.0.1:8790 \
  -listener http://127.0.0.1:8787 \
  -agents agents.d -rules rules.d \
  -data genesis-data \
  -web web/dist
```

Flags are in [cli.md](cli.md). The first event is in
[quickstart.md](quickstart.md).

### Extended set

Add this when agents should use GitHub: repository declarations, provider
identities, deploy keys, or webhook verification. Root `launch` is required.
It needs `-listener-user`. The secret directory and the provider directory
must be outside `agents.d`, `rules.d`, `repos.d`, and the data directory.

Create the directories yourself. Genesis does not invent the App files.
`-credentials` (default `genesis-credentials`) is where root **stores** the
Ed25519 deploy keys it generates. Do not copy the App PEM or the model key
into it.

```sh
mkdir -p genesis-secrets providers.d repos.d
chmod 0700 genesis-secrets
```

| File | Contents |
| --- | --- |
| `genesis-secrets/GITHUB_APP_RECONCILER_PEM` | Reconciler GitHub App private key, PEM. Mode `0600`. |
| `genesis-secrets/GITHUB_APP_ID` | Decimal App id, no trailing newline. Mode `0600`. Not a field in provider YAML. |
| `genesis-secrets/GITHUB_APP_WEBHOOK_SECRET` | Webhook HMAC secret. Mode `0600`. Required only for `POST /webhooks/github`. |
| `genesis-secrets/<NAME>` | One file per other identity secret named in `providers.d`, for example `GITHUB_APP_PROGRAMMER_PEM`. The name matches `^[A-Z][A-Z0-9_]{0,63}$`. Mode `0600`. The file body is the PEM. Provider YAML stores the name only. |
| `providers.d/*.yaml` | Organization, identity names, and secret **names**. No PEM, no token, no App id. See [providers.md](providers.md). |
| `repos.d/*.yaml` | Desired GitHub repositories. No secret values. See [repositories.md](repositories.md). |

`GITHUB_APP_RECONCILER_PEM`, `GITHUB_APP_ID`, and
`GITHUB_APP_WEBHOOK_SECRET` may be absent. GitHub calls that need a missing
file fail closed. A symlink or a non-file at one of those names is refused.

```sh
sudo -E ./bin/genesis launch \
  -listen 127.0.0.1:8787 \
  -agents agents.d -rules rules.d -repos repos.d \
  -providers providers.d \
  -data genesis-data \
  -credentials genesis-credentials \
  -secrets genesis-secrets \
  -listener-user genesis
```

`sudo -E` keeps `DEEPSEEK_API_KEY` and `GENESIS_SYNC_TOKEN`. The listener
user must already exist. Dedicated agent users are created from `agents.d`
when they declare `user`. Webhook verification is in
[webhooks.md](webhooks.md).

## Compose

`docker compose up --build` builds `genesis:v2` from the Dockerfile (Go
binary, the locked Python environment, and the UI) and starts `genesis` and
`control`. The image entrypoint copies missing default agents and rules into
the `genesis-v2-config` volume. It does not copy secrets or providers.

From a Linux shell in the repository. On Docker Desktop, clone inside WSL,
not under `/mnt/c`.

### Minimal set

The model key and the sync token come from the shell or from `.env` in the
repository root. Compose interpolates them. `.env` is gitignored. The
default sync token, if you leave the variable unset, is `compose-sync-token`.

| What | Where |
| --- | --- |
| `DEEPSEEK_API_KEY` | `.env` or the environment of `docker compose`. Passed only into the `genesis` service. |
| `GENESIS_SYNC_TOKEN` | `.env` or that same environment. Passed into both `genesis` and `control`. |
| Agents, rules, repos | Named volume `genesis-v2-config`, mounted at `/var/lib/genesis/config`. Seeded from the image on first start. Edit inside the volume, then sync. |
| Journals | Named volume `genesis-v2-data` at `/var/lib/genesis/data`. |
| Empty provider and secret mounts | `runtime/providers` and `runtime/secrets` on the host. Compose bind-mounts them. Create the directories before `up`; they may be empty. `runtime/` is gitignored. |

```sh
mkdir -p runtime/providers runtime/secrets
umask 077
cat > .env <<'EOF'
DEEPSEEK_API_KEY=replace-with-a-real-key
GENESIS_SYNC_TOKEN=replace-with-a-long-token
EOF
docker compose up --build
```

The listener is not published. Control is `http://127.0.0.1:8792/`. The
browser does not send the sync token; the control process adds it.

### Extended set

Same `.env` as above. Place GitHub material on the host under `runtime/`,
which is the bind mount, not inside the config volume.

| Host path | In the `genesis` container |
| --- | --- |
| `runtime/secrets/` mode `0700` | `/var/lib/genesis/secrets` (listener only; not mounted on `control`) |
| `runtime/secrets/GITHUB_APP_RECONCILER_PEM` | same filename, mode `0600` |
| `runtime/secrets/GITHUB_APP_ID` | decimal App id, no trailing newline, mode `0600` |
| `runtime/secrets/GITHUB_APP_WEBHOOK_SECRET` | mode `0600` |
| `runtime/secrets/<NAME>` | other identity PEMs named by provider files |
| `runtime/providers/*.yaml` | `/etc/genesis/providers.d` (listener and control, read-only for control) |

Startup tightens provider files to `0640` and secret files to `0600` when
`launch` is root. The entrypoint does not create the secret files. The
`genesis-v2-credentials` volume is `/var/lib/genesis/credentials`, where root
stores generated deploy keys. Do not copy PEMs or `.env` into that volume.

`docker compose down` keeps the named volumes and leaves `runtime/` on the
host. `docker compose down -v` deletes `genesis-v2-config`,
`genesis-v2-data`, and `genesis-v2-credentials`. It does not delete
`runtime/` or `.env`.

Repository declarations inside the config volume, provider rules, and the
webhook route are in [run.md](run.md), [providers.md](providers.md), and
[webhooks.md](webhooks.md).
