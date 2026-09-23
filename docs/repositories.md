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
(`^[A-Za-z0-9][A-Za-z0-9._-]*$`). Unknown fields fail closed. The directory
and the files must be real directories and regular files; symlinks are
rejected. Secret values never belong in the file. Names are references.

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
| `org`, `name` | GitHub organization login and repository name. User-owned repos are out of contract. Two files cannot claim the same org/name, ignoring case. |
| `lifecycle.remove` | `retain` only. `archive` and `delete` are rejected, so this file cannot express a destructive remote change. |
| `lifecycle.existing` | `adopt` or `refuse`. Later apply may claim an existing org/name, or must fail closed on collision. This slice only records the choice. |
| `settings` | Visibility (`public` or `private`), optional description, default branch, and explicit feature and merge booleans. At least one merge method must be allowed. |
| `bootstrap.template` | Optional one-time template id (`lab-widget`). It is not a path, URL, or file body. Genesis does not read a template directory here. |
| `actions` | `enabled` plus `allowed`: `all`, `local_only`, or `selected`. `selected` requires action patterns and forbids `..` and URLs. |
| `secrets` | Optional repository secret names and named environments. Names match `^[A-Z][A-Z0-9_]{0,63}$`. There is no value field. |
| `protection.ruleset` | Optional ruleset intent: name, 0–6 approving reviews, dismiss-stale, check contexts, and strict checks. Strict checks require at least one context. |
| `identities` | Optional named runtime roles: `manager`, `programmer`, `reviewer`, `devops`. Names are unique. Roles are unique. `reconciler` and `designer` are not runtime identities. |

Ids, template names, environment names, and ruleset names are lowercase
kebab identifiers. They cannot contain `/`, `\`, `..`, or a scheme.
Descriptions are a single trimmed line of at most 350 characters.
PEM markers and GitHub token prefixes are rejected in every string.

Non-YAML files in the directory are ignored. A directory or symlink whose
name ends in `.yaml` fails the load.

## Generation, sync, and plan

`POST /sync` accepts `repos` beside `agents` and `rules`. An omitted scope
reloads all three. A missing `repos.d` is a successful inactive reload.
Invalid YAML returns `500` `repositories are invalid` and leaves the
previous generation in place.

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

Add a fake provider driver and an observed-status record that accepts this
plan and returns per-intent results without a network call, credential, or
remote id. Live installation-token resolve and create stays behind that
seam.
