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
| `ensure_remote_registration` | Remote key or App registration | unsupported until stage 3. The production driver does not call GitHub |

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

`credential_active` is always false. `key_material` on this HTTP plan stays
`pending` or `none`, and `remote_registration` stays `unsupported` or
`none`. Those fields describe the live grant, which stage 3 has not
registered. When launch is root, the coordinator may also return
`material`: stable grant id, fingerprint, generation, and remote key
id/status. That block has no private key, public key body, provider
identity, or secret reference. Rules-only sync does not emit grant intents.

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
`git: none` stays pending because an installation token is stage 3 work.
The App secret reference is not resolved.

Observed state is `state.json` in that directory: stable grant id,
fingerprint, generation `1`, remote key id, and remote status. Generation
does not increment. A changed access tuple is a new grant id. The previous
key stays on disk. Removing the YAML does not delete or rotate material;
the sync result lists `retained_material`. A world-readable key, a symlink,
or a fingerprint mismatch fails closed.

The production registrar is `github` and always returns `unsupported`. It
does not call GitHub. Tests inject a fake registrar. `credential_active`
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
it. The image does not install OpenSSH; the agent protocol is in-process.

Live GitHub registration remains pending until stage 3. This stage does
not mint an App JWT, an installation token, or `GH_TOKEN`, and it does not
mutate a remote. Repository apply is still a separate seam.
