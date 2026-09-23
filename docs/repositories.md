# Repository declarations

`repos.d/*.yaml` is desired GitHub repository state. Genesis loads it into
the same immutable generation as agents and rules. This slice does not call
GitHub, mint credentials, generate keys, or change a remote. A repository
sync returns a semantic plan whose `remote_mutation` is `none` and whose
every intent is `unsupported`.

Listener and control both take `-repos` (default `repos.d`). A missing
directory leaves the layer inactive. The generation digest then stays the
agents-and-rules digest, so existing configs keep matching. An empty
`repos.d` is active: the digest includes `"repositories": []`.

## File

One repository per `.yaml` or `.yml` file. The filename stem is the id
(`^[A-Za-z0-9][A-Za-z0-9._-]*$`) and cannot contain `..`. Unknown fields,
duplicate keys, and a second YAML document fail closed. The directory and
the files must be real directories and regular files. Genesis opens them
without following symlinks, so a symlink is an error rather than a missing
directory. Named pipes and other non-regular files are rejected and are not
read. A declaration larger than 1 MiB is rejected. Secret values never
belong in the file. Names are references.

```yaml
provider: github
org: octo-org
name: lab-widget
lifecycle:
  remove: retain
  existing: adopt
settings:
  visibility: private
  description: Lab widget service
  default_branch: main
  features:
    issues: true
    wiki: false
    projects: false
  merge:
    allow_squash: true
    allow_merge_commit: false
    allow_rebase: false
    delete_branch_on_merge: true
bootstrap:
  template: lab-widget
actions:
  enabled: true
  allowed: selected
  selected:
    - actions/checkout@v4
secrets:
  repository:
    - DEEPSEEK_API_KEY
  environments:
    - name: ci
      secrets:
        - CI_BOT_TOKEN
protection:
  ruleset:
    name: protect-main
    required_approving_reviews: 1
    dismiss_stale_reviews: true
    required_checks:
      - ci
    strict_checks: true
identities:
  - name: programmer
    role: programmer
  - name: reviewer
    role: reviewer
```

`repos.d/example.yaml` is that declaration. It is not applied.

| Field | Contract |
|---|---|
| `provider` | `github` only. A future driver name, not a URL or token. |
| `org`, `name` | GitHub organization login and repository name. The login is 1–39 letters, digits, or single hyphens, and cannot start or end with a hyphen. The repository name cannot end with `.git`. User-owned repos are out of contract. Two files cannot claim the same org/name, ignoring case. |
| `lifecycle.remove` | `retain` only. `archive` and `delete` are rejected, so this file cannot express a destructive remote change. |
| `lifecycle.existing` | `adopt` or `refuse`. Later apply may claim an existing org/name, or must fail closed on collision. This slice only records the choice. |
| `settings` | Visibility (`public` or `private`), optional description, default branch, and explicit feature and merge booleans. At least one merge method must be allowed. The default branch is a single git path segment: no slash, no `..`, no trailing dot, not `HEAD`, and not a name ending in `.lock`. |
| `bootstrap.template` | Optional one-time template id (`lab-widget`). It is not a path, URL, or file body. Genesis does not read a template directory here. |
| `actions` | `enabled` plus `allowed`: `all`, `local_only`, or `selected`. `selected` requires action patterns: `owner/name`, optional subpaths, optional `@ref`, and `*` wildcards. Patterns forbid `..`, backslashes, spaces, and URLs. |
| `secrets` | Optional repository secret names and named environments. Names match `^[A-Z][A-Z0-9_]{0,63}$`. There is no value field. |
| `protection.ruleset` | Optional ruleset intent: name, 0–6 approving reviews, dismiss-stale, check contexts, and strict checks. A check context is one trimmed line of at most 255 characters. It may contain letters, digits, spaces, and `/_. :-(),+`. It cannot contain `..`, a leading `/`, a backslash, or a URL. Strict checks require at least one context. |
| `identities` | Optional named runtime roles: `manager`, `programmer`, `reviewer`, `devops`. Names are unique. Roles are unique. `reconciler` and `designer` are not runtime identities. |

Template names, environment names, ruleset names, and identity names are
lowercase kebab identifiers. They cannot contain `/`, `\`, `..`, or a scheme.
The filename id is the wider agent-id pattern above, except that `..` is
rejected there too.
Descriptions are a single trimmed line of at most 350 characters.
PEM markers and GitHub token prefixes are rejected in every string.

Non-YAML files in the directory are ignored. A directory or symlink whose
name ends in `.yaml` fails the load.

## Generation, sync, and plan

`POST /sync` accepts `repos` beside `agents` and `rules`. An omitted scope
reloads all three. A missing `repos.d` is a successful inactive reload. A
symlink at that path is not missing: the load fails and the previous
generation stays in place. `["repos"]` rereads only repository files. It
does not reload agents or rules and does not send a host plan, so a
dedicated-user edit that has not been synced stays inactive. Invalid YAML
returns `500` `repositories are invalid` and leaves the previous generation
in place.

The repository plan is not sent to the privileged OS coordinator. Host
user reconcile is unchanged. The sync JSON adds:

```json
{
  "repositories": 1,
  "repository_plan": {
    "requested": true,
    "active": true,
    "remote_mutation": "none",
    "applied": [],
    "intents": [],
    "unsupported": []
  }
}
```

Intent kinds, in order, are `ensure_repository`, `ensure_actions`,
`ensure_bootstrap`, `ensure_secrets`, `ensure_protection`,
`ensure_identities`, and `retain_on_remove`. Missing optional blocks omit
their intents. Each emitted intent is also an `unsupported` entry:
`repository apply is not implemented; Genesis did not call the provider or
mutate the remote`.

`GET /generation` includes `repositories` and `repositories_active`.
Control reads the same files and serves `GET /api/repositories` and
`GET /api/repositories/{id}` with the usual presence values. Invalid
desired YAML still returns the active cache and `desired_error`.

## Next slice

Repository apply is still not implemented. Provider identity files and
agent-to-repository grants are specified in [providers.md](providers.md).
That stage plans local grant and credential intents and leaves key material
pending. Stage 2B is the seam that may mint an App JWT or installation
token and register a remote key. It must not run as part of repository
apply, and `credential_active` stays false until a child actually receives
a usable credential.
