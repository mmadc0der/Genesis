# Provider identities and repository grants

Stage 2A records who could act on a declared repository. It does not call
GitHub, mint an App JWT or installation token, generate an SSH key, inject a
credential, or register a key remotely.

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
| `ensure_repository_grant` | Local semantic grant: agent, repository, git access, allowlisted permissions | unsupported; not activated |
| `ensure_credential` | Credential material for that grant | `pending` when the identity has a secret reference, `none` for public read. No JWT, installation token, or SSH key is minted |
| `ensure_remote_registration` | Remote key or App registration | unsupported. Genesis does not call GitHub |

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

`credential_active` is always false. `key_material` is `pending` or `none`.
`remote_registration` is `unsupported` when any grant exists, otherwise
`none`. Root apply records the same unsupported results and does not create
a key file, a user, or a token. Rules-only sync does not emit grant intents.

Control serves the agent grant as repository, git access, permissions, and
`credential` `pending` or `none`. It does not serve provider files. There is
no provider route. Secret references and provider identity records are not
in `/generation`, `/api/agents`, `/api/state`, or sync logs.

## Stage 2B seam

Stage 2B is the first stage that may turn these intents into material. It
should consume the same three kinds from the privileged plan:

1. Resolve `ensure_credential` only inside the root coordinator, using the
   secret reference as a lookup key in a root-only store. Do not put the
   reference or the value into agent YAML, the control API, or the child
   environment until a real credential exists.
2. Mint an App JWT and a repo-scoped installation token, or register a
   remote key, only while handling `ensure_credential` and
   `ensure_remote_registration`. Public-read grants stay `credential: none`
   and must not grow a token.
3. Flip `key_material` away from `pending` and `remote_registration` away
   from `unsupported` only after that work succeeds. `credential_active`
   stays false until the child is actually given a usable, expiring
   credential. Stage 2A never sets it true.

Repository apply (`ensure_repository` and the rest of the repository plan)
is still a separate seam. Stage 2B does not need to create the GitHub
repository. It also must not inject `GH_TOKEN` as a side effect of planning.
