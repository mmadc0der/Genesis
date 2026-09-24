# Repository declarations

`repos.d/*.yaml` is desired GitHub repository state. Genesis loads it into
the same immutable generation as agents and rules. A repos sync with a
configured reconciler reads each `existing: adopt` repository and applies
the declared shape below. `existing: refuse` makes no HTTP call. A failed
apply keeps the previous listener generation. `remote_mutation` is
`applied` only when this sync wrote at least one intent, and `applied`
names only those intents (`kind:id`). Every other known intent stays
unsupported with a concrete reason.

Listener and control both take `-repos` (default `repos.d`). A missing
directory leaves the layer inactive. The generation digest then stays the
agents-and-rules digest, so existing configs keep matching. An empty
`repos.d` is active: the digest includes `"repositories": []`.

The image installs this contract for the designer at
`/usr/share/genesis/schema/repository-declaration.txt`. That text is
generated from the loader structs. Designer instructions tell the agent to
read it instead of inspecting the genesis binary. `repos.d/example.yaml`
and the lab-widget template are examples, not that contract.

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
| `bootstrap.template` | Optional template id (`lab-widget`). It is not a path, URL, or file body. Apply resolves it only to a Genesis-owned template directory shipped in this repository. |
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
does not reload agents or rules. The privileged call applies the adopted repository shape, then re-reads
it. It does not reconcile host users, so a dedicated-user edit that has
not been synced stays inactive. Invalid YAML returns `500`
`repositories are invalid` and leaves the previous generation in place.

Host user reconcile is unchanged. A repos scope asks the root coordinator
to apply and observe before any host or grant mutation. Agents-only and
rules-only sync do not set that flag and do not call GitHub for repository
state. The sync JSON adds:

```json
{
  "repositories": 1,
  "repository_plan": {
    "requested": true,
    "active": true,
    "remote_mutation": "applied",
    "observation": "observed",
    "applied": ["ensure_repository:example"],
    "intents": [],
    "observed": [],
    "drift": [],
    "unsupported": []
  }
}
```

The arrays in that example are abbreviated. A real plan lists every intent
and classifies each one as applied or unsupported. Intent kinds, in order,
are `ensure_repository`, `ensure_actions`,
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
   again. The id, `node_id`, owner, name, and `full_name` must match the
   first read and the declaration. `full_name` must be `owner/name`.
3. `GET` Actions permissions, and selected-actions when the allowed policy
   is `selected`.
4. `GET` repository rulesets, following at most 10 pages, then each ruleset.
5. When `default_branch` is one safe path segment, `GET /repos/{org}/{name}/rules/branches/{branch}`.
   An empty name, or a name containing `/`, `\`, or `..`, is `unobservable`
   and is not requested.

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
| extra repository rulesets | `unsupported` | observed and left in place |
| Actions SHA pinning when enabled or absent | `unobservable` | the declaration cannot express it, and a missing field is not treated as off |
| GitHub-owned or verified action allowances | `unobservable` | the declaration cannot express them |
| extra review requirements or other ruleset rule types | `unobservable` | the declaration cannot express them |
| dismiss-stale when the ruleset omits it | `unobservable` | a missing boolean is not treated as false |
| ruleset or branch endpoint unavailable, including when no ruleset is declared | `unobservable` | optional on this permission set |

When the reconciler is missing, observation is `unavailable`, no HTTP call
is made, and every intent stays unsupported with `repository apply is not
implemented; Genesis did not call the provider or mutate the remote`. If
any `adopt` repository has no usable reconciler, the whole observation is
`unavailable`. A configured root with only `refuse` repositories is
`observed` and makes no HTTP call. When observation is `observed`,
`ensure_repository` is not listed as unsupported. `ensure_actions` and
`ensure_protection` are unsupported only when those endpoints were
unobservable. Bootstrap, secrets, identities, and retain stay unsupported.

Root logs each GitHub response as `github call` (`method`, `path`, `status`)
and each accepted token as `github token scope` (`requested`, `returned`).
Those lines are permission names and API paths. They do not include a JWT,
installation token, or PEM. A query string is omitted. A path or permission
value that looks like a credential is logged as `redacted`.

Failure does not swap the listener generation and does not replace the
journal. Closed failures are: repository `404`, owner/name/id mismatch,
transfer or rename, pagination past 10 pages or a truncated page, an
installation token whose permissions are not exactly the requested read
set, rate limit, a malformed or non-200 body including `304`, a
secret-shaped description, a desired-state change during the read, or a
journal body larger than 1 MiB. A failed journal write leaves both the
previous journal and the previous generation in place. The journal is
written only after the grant plan is confirmed inactive. Observation
requests do not send conditional `ETag` headers.

The listener writes
`{data}/observations/repositories.json` only after a successful
observation whose digest still matches. The file is mode `0600`, the
directory is mode `0700`, and the write is an atomic rename. The body is
version `1` and holds bindings, observed fields, and the digest. It does
not hold a PEM, App JWT, installation token, secret value, or programmer
key. A corrupt or unknown journal fails closed. Control omits the observed
view when the journal digest does not match the active repository list.
A draft whose org or name differs from the active repository does not
inherit that repository's observed id.

`GET /generation` includes `repositories` and `repositories_active`.
Control reads the same files and serves `GET /api/repositories` and
`GET /api/repositories/{id}` with presence, and with `observation`,
`observed`, and `drift` when the journal matches the active generation.
Invalid desired YAML still returns the active cache and `desired_error`.
Drift is active versus last observation, not draft YAML versus GitHub.

## Apply

Apply runs in the same repos sync as observation, using the root reconciler
App, and only for `existing: adopt`. `existing: refuse` is classified and
makes no HTTP call. The read-only observation above still journals the
repository after a successful apply. It is not replaced.

Settings, create, Actions, and rulesets use one installation token requested
as exactly `administration: write` and `metadata: read`. Bootstrap contents
calls use a second token requested as exactly `contents: write` and
`metadata: read`. Any other returned permission set fails closed. The App
installation therefore needs repository **Administration: write** and
**Contents: write** (metadata is included with repository access). Logs,
journals, API responses, and errors still contain no JWT, installation
token, or PEM.

For each adopted repository Genesis first requests an administration token
limited to that repository name. GitHub answers `422` or `404` when the
repository does not exist or is not accessible to the installation. That
response is the missing-repository signal:

- With no persisted binding, Genesis requests a second administration
  token with the same permissions and no repository limit
  (`repository_selection` `all` or `selected`). It uses that token only for
  `POST /orgs/{org}/repos`, with the declared name, visibility, description,
  the matching `private` flag, and `auto_init: true`. Any other permission
  set fails closed before the create. Genesis then re-mints a token limited
  to the new name, re-reads, and binds `repository_id` and `node_id` with
  the same fail-closed identity rules as observation. If the resulting
  default branch name differs from the declaration, the sync fails closed.
  Genesis does not rename, transfer, archive, unarchive, or delete.
- The same `422` or `404` for an id that is already bound does not create a
  replacement and does not request the unlimited token.
- A later repository `GET` of `404` with no persisted binding also creates,
  using the token that was already minted for that name. An existing
  repository keeps its binding. A different id, node id, owner, or name
  fails closed. The default branch is not changed when it already exists.

After the identity check, Genesis reads Actions and rulesets with that same
administration token before it writes:

| Shape | Behavior |
|---|---|
| Settings | `PATCH /repos/{org}/{repo}` for visibility, description, issues, wiki, projects, allow_squash, allow_merge_commit, allow_rebase, and delete_branch_on_merge when any of those differ. The body does not include the default branch, the name, or archived. |
| Actions | `PUT` enabled, allowed, and, when allowed is `selected`, the selected patterns. GitHub's `204 No Content` is success. If SHA pinning is on, SHA pinning was not reported, selected actions are unknown, or the repository has GitHub-owned or verified allowances, Genesis does not overwrite Actions and fails the sync. A failed Actions write, including a patterns `PUT` that follows a successful permissions `PUT`, does not swap the listener generation. |
| Protection | Create or update only the one declared repository ruleset: active enforcement, approving reviews, dismiss-stale, required checks, and strict checks, targeting `refs/heads/{current default branch}`. Extra rulesets are left in place. A ruleset with bypass actors, unmodeled rules, extra review requirements, or an unreported dismiss-stale value is not overwritten. If the ruleset list was unobservable, Genesis does not guess and does not create one. |
| Bootstrap | Commit template files that are absent, on the current default branch, with the contents token. Existing files are never overwritten. The built-in `lab-widget` template ships `.github/workflows/ci.yml`, whose check name is `ci`, matching that template's `protection.required_checks`. A template id that is not shipped, or required checks the template does not provide, fails closed before any HTTP call. |

`ensure_secrets`, `ensure_identities`, and `retain_on_remove` stay
unsupported. This stage does not write secrets, environments, webhooks,
or deploy keys, and it does not clone a workspace. A dedicated run may
mint a short-lived installation token and check out the bound repository
afterward; that is not part of create or apply. See
[providers.md](providers.md).
GitHub App webhook deliveries are accepted on the listener. That ingress
does not write repository webhooks, mint a per-run App token, or clone a
workspace. See [webhooks.md](webhooks.md).
An intent that was not written remains unsupported with a concrete reason,
including "already matches" when no write was required.

`remote_mutation` is `none` when this sync wrote nothing and `applied`
when it wrote at least one intent. `applied` contains only `kind:id`
values for intents actually written. A failed apply, including an unsafe
Actions policy, an identity mismatch, a rename or transfer, a created
default branch that differs from the declaration, or a desired-state
change during the operation, returns an error and keeps the previous
listener generation. The journal is still written only after the grant
plan is confirmed inactive and the digest still matches.

Provider identity files and agent-to-repository grants are specified in
[providers.md](providers.md). Root launch can register an SSH deploy key
for a git read or write grant. That path does not read repository settings
into this journal, does not apply `default_branch`, and `credential_active`
stays false.
