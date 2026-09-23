# Repository declarations

`repos.d/*.yaml` is desired GitHub repository state. Genesis loads it into
the same immutable generation as agents and rules. Stage 4 observes an
existing repository and plans semantic drift. It does not create a
repository, change settings, mint an agent token, or write Actions,
rulesets, secrets, environments, or webhooks. `remote_mutation` stays
`none` and `applied` stays empty.

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

`repos.d/example.yaml` is that declaration for `octo-org/lab-widget`. The
image does not create that repository. Without a reconciler key, a sync
reports observation `unavailable` and does not call GitHub. With a
reconciler configured for `octo-org`, a missing repository fails the sync
and keeps the previous generation.

| Field | Contract |
|---|---|
| `provider` | `github` only. A future driver name, not a URL or token. |
| `org`, `name` | GitHub organization login and repository name. The login is 1–39 letters, digits, or single hyphens, and cannot start or end with a hyphen. The repository name cannot end with `.git`. User-owned repos are out of contract. Two files cannot claim the same org/name, ignoring case. |
| `lifecycle.remove` | `retain` only. `archive` and `delete` are rejected, so this file cannot express a destructive remote change. |
| `lifecycle.existing` | `adopt` or `refuse`. `adopt` asks root to resolve that exact org and name. `refuse` is classified and does not call GitHub. |
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
does not reload agents or rules. The privileged call observes repositories
and does not reconcile host users, so a dedicated-user edit that has not
been synced stays inactive. Invalid YAML
returns `500` `repositories are invalid` and leaves the previous generation
in place.

Host user reconcile is unchanged. A repos scope also asks the root
coordinator to observe before any host or grant mutation. Agents-only and
rules-only sync do not set that flag and do not call GitHub for repository
state. The sync JSON adds:

```json
{
  "repositories": 1,
  "repository_plan": {
    "requested": true,
    "active": true,
    "remote_mutation": "none",
    "observation": "observed",
    "applied": [],
    "intents": [],
    "observed": [],
    "drift": [],
    "unsupported": []
  }
}
```

Intent kinds, in order, are `ensure_repository`, `ensure_actions`,
`ensure_bootstrap`, `ensure_secrets`, `ensure_protection`,
`ensure_identities`, and `retain_on_remove`. Missing optional blocks omit
their intents.

## Stage 4: read-only observation

Observation reuses the root-only reconciler App. The listener sends the
canonical repository digest. Root re-reads `repos.d` at the start and after
the GETs. The listener re-reads again before it writes the journal or swaps
the generation. A mismatch returns `500` `repository desired state changed
during observation` and keeps the previous generation.

For each `existing: adopt` repository, root selects the one reconciler
identity whose organization matches the declaration exactly in role, and
mints an installation token with only `administration: read` and
`metadata: read`. The only non-GET call is that existing token mint. Root
then:

1. `GET /repos/{org}/{name}` and require the owner login, name, and
   `full_name` to match the declaration exactly, including case.
2. Remint the token by the numeric repository id and `GET` the repository
   again. The id and `node_id` must match the first read.
3. `GET` Actions permissions, and selected-actions when the allowed policy
   is `selected`.
4. `GET` repository rulesets, following at most 10 pages, then each ruleset.
5. When `default_branch` is non-empty, `GET /repos/{org}/{name}/rules/branches/{branch}`.

The first successful read persists `repository_id` and `node_id` on the
genesis file id. A later read that returns a different id or node id fails
closed. Changing the declared org or name for a bound id fails closed
before HTTP. Removing the YAML file keeps the binding. The same GitHub id
cannot bind two genesis ids.

Readable fields are visibility, description, default branch, issues, wiki,
projects, squash, merge commit, rebase, delete-branch-on-merge, and archive
state. Actions enablement and allowed policy are read when those endpoints
answer. Ruleset and branch evaluation are read when those endpoints answer.
A `404` or `403` on Actions, the ruleset list, or branch rules is
`unobservable` for that field. A missing ruleset detail after a successful
list is malformed.

Drift compares the active declaration with that read. Differences in the
readable settings are `drift`. These fields are classified and not applied:

| Field | Status | Why |
|---|---|---|
| `bootstrap.template` | `unobservable` | not a repository field |
| `secrets`, `secrets.environments` | `unobservable` | secrets and environments APIs are not called |
| `identities` | `unsupported` | not a repository setting; no collaborator or agent-token change |
| `lifecycle.remove` | `unsupported` | `retain` does not archive or delete |
| `lifecycle.existing` when `refuse` | `unsupported` | no GitHub call |
| Actions or rules when the endpoint is unavailable | `unobservable` | optional on this permission set |
| extra repository rulesets | `unsupported` | observed and left in place |
| Actions SHA pinning, GitHub-owned or verified allowances | `unobservable` | the declaration cannot express them |
| extra review requirements | `unobservable` | the declaration cannot express them |

When the reconciler is missing, observation is `unavailable`, no HTTP call
is made, and every intent stays unsupported with `repository apply is not
implemented; Genesis did not call the provider or mutate the remote`. If
any `adopt` repository has no usable reconciler, the whole observation is
`unavailable`. A configured root with only `refuse` repositories is
`observed` and makes no HTTP call. When observation is `observed`,
`ensure_repository` is not listed as unsupported. `ensure_actions` and
`ensure_protection` are unsupported only when those endpoints were
unobservable. Bootstrap, secrets, identities, and retain stay unsupported.

Failure does not swap the listener generation and does not replace the
journal. Closed failures are: repository `404`, owner/name/id mismatch,
transfer or rename, pagination past 10 pages or a truncated page, an
installation token whose permissions are not exactly the requested read
set, rate limit, a malformed body, a secret-shaped description, or a
desired-state change during the read.

The listener writes
`{data}/observations/repositories.json` only after a successful
observation whose digest still matches. The file is mode `0600`, the
directory is mode `0700`, and the write is an atomic rename. The body is
version `1` and holds bindings, observed fields, and the digest. It does
not hold a PEM, App JWT, installation token, secret value, or programmer
key. A corrupt or unknown journal fails closed. Control omits the observed
view when the journal digest does not match the active repository list.

`GET /generation` includes `repositories` and `repositories_active`.
Control reads the same files and serves `GET /api/repositories` and
`GET /api/repositories/{id}` with presence, and with `observation`,
`observed`, and `drift` when the journal matches the active generation.
Invalid desired YAML still returns the active cache and `desired_error`.
Drift is active versus last observation, not draft YAML versus GitHub.

## Next mutation stage

Stage 5, `ensure_repository_settings`, is not implemented. It would
`PATCH /repos/{org}/{repo}` only for an already-adopted repository whose
persisted id and node id still match. The patch body would be limited to
`visibility`, `description`, `has_issues`, `has_wiki`, `has_projects`,
`allow_squash_merge`, `allow_merge_commit`, `allow_rebase_merge`, and
`delete_branch_on_merge`. It would still not create a repository, change
the default branch, archive or unarchive, write Actions or rulesets, write
secrets, environments, or webhooks, change deploy keys, or mint an agent
token.

Provider identity files and agent-to-repository grants are specified in
[providers.md](providers.md). Root launch can register an SSH deploy key
for a git read or write grant. That path does not read repository settings
into this journal, does not apply `default_branch`, and `credential_active`
stays false.
