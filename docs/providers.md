# Provider identities and repository grants

Stage 2A records who could act on a declared repository. Loading providers
does not call GitHub, mint an App JWT or installation token, or register a
key remotely. Stage 2B, below, may store an SSH identity on the root
coordinator. That identity is not a live GitHub credential.

`providers.d` is a root-owned directory outside the designer-writable config
volume. Listener, launch, and control take `-providers` (default
`providers.d`). A missing directory leaves the layer inactive and leaves the
agents-and-rules digest unchanged. An empty directory is active.

Docker keeps an empty directory at `/etc/genesis/providers.d` in the image,
owned `root:genesis` and mode `0750`. The example file is not copied there.
The entrypoint does not copy providers into `/var/lib/genesis/config`. On
start it tightens an existing providers directory in place: directories
`0750`, regular files `0640`, owner `root:genesis` when launched as root,
without following symlinks. Dedicated agent users are not in the `genesis`
group, so they cannot read secret references. The listener and control
process run as `genesis` (uid/gid `65532`) and can. The shared-UID designer
is that same user, so it can read the directory; it cannot write it.

The designer agent cannot declare a GitHub capability, and a providers path
inside that volume, or inside `agents.d` / `rules.d` / `repos.d`, fails
closed. A path that only looks outside those trees, but resolves through a
parent symlink into one of them, fails closed too.

## Provider file

One provider per `.yaml` or `.yml` file. The filename stem is the id. Unknown
fields, duplicate keys, a second document, symlinks, and non-regular files
fail closed. There is no value field.

```yaml
provider: github
org: octo-org
identities:
  - name: reconciler
    role: reconciler
    credential: app
    secret: GITHUB_APP_RECONCILER_PEM
    app_id: "100001"
    installation_id: "100002"
  - name: programmer
    role: programmer
    credential: app
    secret: GITHUB_APP_PROGRAMMER_PEM
  - name: reviewer
    role: reviewer
    credential: app
    secret: GITHUB_APP_REVIEWER_PEM
  - name: public
    role: reader
    credential: none
```

| Field | Contract |
|---|---|
| `provider` | `github` only. |
| `org` | GitHub organization login. One organization per file. Two files cannot claim the same organization, ignoring case. |
| `identities[].name` | Lowercase kebab id, unique across provider files. |
| `identities[].role` | `reconciler`, `manager`, `programmer`, `reviewer`, `devops`, or `reader`. Unique within the file. `reconciler` is not an agent role. |
| `identities[].credential` | `app` or `none`. `none` is allowed only for `reader`. |
| `identities[].secret` | Required for `app`, forbidden for `none`. A name matching `^[A-Z][A-Z0-9_]{0,63}$`. Two identities cannot share a reference. |
| `identities[].app_id` | Required on `reconciler`, forbidden on every other role. A decimal GitHub App id, not a secret and not a path. |
| `identities[].installation_id` | Required on `reconciler`, forbidden on every other role. A decimal installation id. |

PEM markers and GitHub token prefixes are rejected in every string. The
reference is not a path, URL, or file body. Genesis does not read it.

`providers.d/example.yaml` matches `repos.d/example.yaml`'s organization. It
is not applied. The image does not install it into `/etc/genesis/providers.d`.

## Agent capability

`github` is optional. Existing agents that omit it keep loading. When it is
set, the agent must name a `repos.d` repository and a provider identity:

```yaml
github:
  repository: example
  identity: programmer
  git: write
  permissions:
    contents: write
    workflows: write
    pull_requests: write
    issues: write
    actions: read
    checks: read
    metadata: read
```

`git` is `none`, `read`, or `write`. `permissions` is a non-empty allowlist
of `read` or `write` levels. The level cannot exceed the identity role:

| Role | Ceiling | Git |
|---|---|---|
| programmer | `contents:write`, `workflows:write`, `pull_requests:write`, `issues:write`, `actions:read`, `checks:read`, `metadata:read` | `write` requires `contents:write` |
| reviewer | `pull_requests:write`, `contents:read`, `actions:read`, `checks:read`, `metadata:read` | not `write` |
| manager | `pull_requests:write`, `issues:write`, `contents:write`, `metadata:read`, `actions:read` | `contents:write` may use `git: none` |
| devops | `actions:write`, `contents:read`, `pull_requests:read`, `metadata:read` | not `write` |
| reader | `contents:read`, `metadata:read` | `read` or `none`, and only with `credential: none` |

`administration`, `secrets`, and `environments` are rejected for every
runtime agent. A repository must declare the same role (`reader` is the
public-read exception). The repository organization must match the provider.

These combinations fail closed:

- an agent references the reconciler identity
- the designer, or any agent whose `cwd` is `/var/lib/genesis/config`, declares `github`
- reviewer access and author access (`git: write`, `contents: write`, or `workflows: write`) share one identity on the same repository
- a review-shaped grant does not use the reviewer identity
- `credential: none` is used on a private repository, with git write, or with any write permission
- the capability is present while `providers.d` or `repos.d` is inactive

Public read is the case that can require no credential: reader identity,
`credential: none`, public visibility, git `read` or `none`, and read-only
permissions. That still does not activate a credential.

## Plan seam

An agents sync builds three privileged intents per grant, in agent id order,
after that agent's host intents:

| Kind | Meaning | Result in this stage |
|---|---|---|
| `ensure_repository_grant` | Local semantic grant: agent, repository, git access, allowlisted permissions | applied by the root coordinator as a local record. Not sent to GitHub |
| `ensure_credential` | Credential material for that grant | root stores one Ed25519 identity for a non-public git read/write grant, or records `none` for public read. No App JWT or installation token is minted |
| `ensure_remote_registration` | Remote deploy-key registration | root launch registers the public key when the reconciler app is configured. The listener plan itself does not call GitHub |

The privileged plan sent across the coordinator socket includes the identity
name so a later stage can bind it. HTTP, the control UI, and logs do not.
`grant_plan` on `POST /sync` repeats the intents without identity names or
secret references:

```json
{
  "grant_plan": {
    "requested": true,
    "credential_active": false,
    "key_material": "pending",
    "remote_registration": "unsupported",
    "intents": [],
    "unsupported": []
  }
}
```

`credential_active` stays false. Sync does not deliver an App token,
installation token, or `GH_TOKEN`. `key_material` on this HTTP plan stays
`pending` or `none`, and `remote_registration` stays `unsupported` or
`none` because those summary fields are the listener's classification.
When launch is root, the coordinator may also return `material`: stable
grant id, fingerprint, generation, and remote key id/status. That block
has no private key, public key body, provider identity, App id, or secret
reference. `remote_status: ready` means the deploy key is registered and a
later dedicated run can receive `SSH_AUTH_SOCK`. Rules-only sync does not
emit grant intents.

Control serves the agent grant as repository, git access, permissions, and
`credential` `pending` or `none`. It does not serve provider files. There is
no provider route. Secret references and provider identity records are not
in `/generation`, `/api/agents`, `/api/state`, or sync logs.

## Stage 2B: root-held SSH material

Stage 2B consumes the three grant intents in the root coordinator only.
The listener still builds the inactive `grant_plan` above. It never sees
private keys. A non-root `launch` cannot open the store, so its plan stays
unsupported and no key is written.

`genesis launch` as root takes `-credentials` (default `genesis-credentials`,
`/var/lib/genesis/credentials` in the image). The directory is owned by
root. Its parent must be owned by root or the coordinator, and must not be
group- or world-writable unless that parent is sticky, so another user
cannot rename the store onto a symlink. The image keeps `/var/lib/genesis`
as `root:root` mode `0755`. When launch is root the store root is mode
`0711`: other users can traverse it and cannot list or create entries.
`grants/` and `locks/` stay mode `0700`. The entrypoint first locks the
tree to `0700`; root launch then sets the store root to `0711` so a
dedicated user can reach `sockets/` without reading a private key. The
store must not be a symlink, and it must not overlap `agents.d`,
`rules.d`, `repos.d`, `providers.d`, the data directory, the designer
config root, or `/home`. Compose mounts it on the root listener service
only. Control does not receive the mount. Key and state files are opened
from a directory file descriptor with `O_NOFOLLOW`, so replacing the store
path after open does not redirect a write.

For a grant whose credential is not public-read and whose git access is
`read` or `write`, root generates one Ed25519 key per
`(agent, repository, identity, git access, permissions)`. The private key
is an OpenSSH file, mode `0600`, created with `O_EXCL` under the grant
directory. Public-read grants and `git: none` grants do not get a key.
`git: none` stays pending. It does not get an SSH key, an App JWT, or an
installation token. The reconciler private key is read only while
registering a git read or write deploy key.

Observed state is `state.json` in that directory: stable grant id,
fingerprint, generation `1`, remote key id, and remote status. Generation
does not increment. A changed access tuple is a new grant id. The previous
key stays on disk. Removing the YAML does not delete or rotate material;
the sync result lists `retained_material`. A world-readable key, a symlink,
or a fingerprint mismatch fails closed.

An unconfigured registrar returns `unsupported` and does not call GitHub.
Root launch with `-secrets` uses the production registrar. `credential_active`
stays false either way. Root does not create `SSH_AUTH_SOCK` until the
registrar reports `ready`.

When a dedicated agent's current grant is ready, root serves one sealed
ssh-agent on a per-run socket. The socket holds only that grant key, is
mode `0600`, and is owned by the agent uid. Every directory from the store
root to that socket is traversable and not listable. The child environment
gains `SSH_AUTH_SOCK` and not the key path, `SSH_AGENT_PID`, or `GIT_SSH`.
Stop clears the in-memory key and removes the socket on exit, spawn
failure, and cancel. A uid that already holds a socket for a different
grant does not receive a second one. Shared-UID processes never receive
it. The image installs `openssh-client` so a ready grant can be used by
`git` over SSH. It does not install an SSH server. `GIT_SSH` and
`SSH_AGENT_PID` are still not set.

## Stage 3: deploy-key registration

Root `launch` takes `-secrets` (default `genesis-secrets`,
`/var/lib/genesis/secrets` in the image). The directory is mode `0700`,
owned by root, and is not a symlink. It must not overlap agents, rules,
repos, providers, data, the designer config root, `/home`, or the
credential store. Compose mounts it on the root service only. The listener
process, control, and agents do not receive the flag or the mount.

Each file is a regular file whose name is the provider secret reference.
Genesis opens it from a pinned directory descriptor with `O_NOFOLLOW`.
Symlinks, extra names, hard links, and group- or world-accessible files
fail closed. The reconciler file is the GitHub App RSA private key.
Programmer and reviewer references are not read in this stage. Public-read
and `git: none` grants make no GitHub calls.

For a non-public git `read` or `write` grant, the root coordinator mints a
short-lived App JWT (`iat` 60 seconds in the past, `exp` eight minutes
ahead, `iss` the app id) and repository-scoped installation tokens. It
discovers `GET /repos/{org}/{name}`, then lists deploy keys with a token
scoped to that repository id and only `administration: write` plus
`metadata: read`. The declared agent, identity, git access, and
permissions must still match the provider organization. A cross-org
identity cannot register a key. A key whose
title, SHA256 fingerprint, and `read_only` flag already match is reused.
Otherwise the public key is posted. `git: write` sends `read_only: false`.
`git: read` sends `read_only: true`. The title is `genesis-` plus the
grant id. The numeric key id and `remote_status: ready` are stored on the
grant. Generation stays 1.

A title that belongs to a different key, or a fingerprint that belongs to
a different title, fails closed. Another deploy key on the repository is
left in place, including a key that is not Ed25519, unless it reuses this
grant's title or recorded key id. Genesis does not delete or rotate keys.
Auth `401` mints a new JWT and token once. Rate limits and `5xx` retry
only inside a short bound; a long `Retry-After` fails closed so sync is
not held open. A grant that was `ready` and no longer matches is stored
as `refused`. A changed grant id, a removed grant, or that refusal drops
the held per-run socket immediately, including a socket already given to
a running process. The listener cannot mark a grant ready over IPC. A
create that is not confirmed stays unready; the next sync lists and adopts
an exact match. An installation token must be repository-scoped, name the
declared owner and repository, expire more than 30 seconds out, and carry
only the permissions that were requested.

No private key, App JWT, installation token, or secret reference is
written to logs, IPC errors, or the sync body. Tests inject the HTTP
transport and clock and use a local fake GitHub API. No real credential
is required.

## Stage 4: read-only repository observation

The same root reconciler App observes an `existing: adopt` repository
during a repos sync. That token is exactly `administration: read` plus
`metadata: read`. Repository GETs are the only observation calls. The
installation-token mint remains the existing `POST
/app/installations/{id}/access_tokens`. Observation does not post a deploy
key, patch settings, or mint an agent token. The non-secret journal and
the drift plan are specified in [repositories.md](repositories.md).

The same App's repos sync now creates a missing adopted repository and
applies settings, Actions, the declared ruleset, and missing bootstrap
files. Bootstrap uses a second installation token, exactly
`contents: write` and `metadata: read`, so the App also needs Contents
write. Secrets, environments, webhooks, per-run App tokens, deletion, and
rotation stay unsupported. The contract is
[repositories.md](repositories.md). CI uses a local fake GitHub API. The
manual probes in
[wsl-readonly-observation.md](wsl-readonly-observation.md) and
[ssh-client-verification.md](ssh-client-verification.md) were written when
a repos sync only read. A current repos sync writes the adopted shape, so
do not rerun them expecting GitHub to stay unchanged.
