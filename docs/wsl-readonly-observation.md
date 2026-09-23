# Read-only observation probe

Use this on a WSL clone (`~/src/genesis`, not `/mnt/...`) with Docker
Desktop integration. Do not run it from PowerShell or `cmd.exe`. CI does
not perform this check.

This probe uses the existing organization `genesis-id13-tech`, the public
repository `verification`, and the reconciler App already installed only
on that repository with Administration write and Metadata read. It does
not create an App, delete the repository, register a deploy key, or change
settings. The host PEM stays where it already is.

Keep one shell. Do not print, commit, or paste a PEM, App JWT, installation
token, `GH_TOKEN`, or `GITHUB_TOKEN`. The scripts below print only the
evidence lines. If a script exits nonzero, run the cleanup section before
doing anything else.

## 0. Checkout and names

```sh
cd ~/src/genesis
git checkout cursor/repo-observe-drift-9434

export ORG=genesis-id13-tech
export REPO=verification
export MISMATCH_NAME=verification-observe-hold
export PEM_FILE=$HOME/genesis-probe-reconciler.pem
export GENESIS_SYNC_TOKEN=compose-sync-token
unset GITHUB_TOKEN GH_TOKEN GITHUB_APP_PEM GITHUB_PRIVATE_KEY \
  GITHUB_APP_PRIVATE_KEY GITHUB_APP_RECONCILER_PEM DEEPSEEK_API_KEY || true

test "$ORG" = genesis-id13-tech
test "$REPO" = verification
test -f "$PEM_FILE"
test ! -L "$PEM_FILE"
test "$(stat -c %a "$PEM_FILE")" = 600
gh auth status
```

`PEM_FILE` must be the existing App key. Change that one assignment if the
file lives somewhere else. Do not `cat` it. `gh auth status` must show an
owner of `genesis-id13-tech`. That login stays on the host.

## 1. Confirm the repo and the App

Every `gh` command in this file is a GET until the rename section, and that
section restores the original name by repository id.

```sh
gh api "/repos/$ORG/$REPO" --jq '{full_name,id,node_id,visibility,private,archived,default_branch}'
gh api "/repos/$ORG/$REPO/keys" --jq 'length'
gh api "/orgs/$ORG/installations" --jq '.installations[] | {id, app_id, repository_selection, permissions}'
```

Expected: `full_name` is `genesis-id13-tech/verification`, `visibility` is
`public`, `private` is `false`, and `archived` is `false`. Record `id` and
`node_id`. The key length is whatever already exists; this probe must not
change it. Stop if more than one installation is listed, if
`repository_selection` is not `selected`, or if `permissions` are not
exactly `administration: write` and `metadata: read`.

```sh
export APP_ID=
export INSTALLATION_ID=
export REPO_ID=
export NODE_ID=
case $APP_ID in ''|*[!0-9]*|0*) echo "APP_ID must be a positive decimal" >&2; exit 1 ;; esac
case $INSTALLATION_ID in ''|*[!0-9]*|0*) echo "INSTALLATION_ID must be a positive decimal" >&2; exit 1 ;; esac
case $REPO_ID in ''|*[!0-9]*|0*) echo "REPO_ID must be a positive decimal" >&2; exit 1 ;; esac
case $NODE_ID in ''|*[!A-Za-z0-9_+=/-]*) echo "NODE_ID is empty or has whitespace" >&2; exit 1 ;; esac

gh api "/orgs/$ORG/installations" --jq ".installations[] | select(.id == $INSTALLATION_ID and .app_id == $APP_ID) | {id, app_id, repository_selection, permissions}"
gh api "/user/installations/$INSTALLATION_ID/repositories" --jq '{total_count, repository_selection, repositories: [.repositories[] | {full_name, private}]}'
gh api "/repos/$ORG/$MISMATCH_NAME" --jq .id && echo "temporary name already exists" >&2 && exit 1
```

Expected: one installation, `total_count` is `1`, and the only repository
is `genesis-id13-tech/verification` with `private: false`. The last `gh`
must fail with Not Found. A hit means `verification-observe-hold` already
exists; do not continue.

```sh
restore_verification_name() {
  live=$(gh api "/repositories/$REPO_ID" --jq .name)
  if [ "$live" = "$REPO" ]; then
    echo "restored_name $REPO"
    return 0
  fi
  gh api --method PATCH "/repositories/$REPO_ID" -f name="$REPO" --jq '{full_name,id,node_id}'
  echo "restored_name $REPO"
}
```

## 2. Build and run

```sh
docker compose up --build -d
curl -sf -o /dev/null --retry 30 --retry-delay 1 --retry-connrefused \
  http://127.0.0.1:8790/api/health
```

Expected: HTTP 200 from `/api/health`. This does not sync and does not call
GitHub.

## 3. Install the provider and the declaration

The provider file is root-owned. The repository file is the listener user's.
No agent file is changed, and the sync scope below is `repos` only, so a
deploy key is not registered.

```sh
repos_present=$(docker compose exec -T -u genesis genesis sh -c 'find /var/lib/genesis/config/repos.d -mindepth 1 -maxdepth 1 -printf "%f\n"')
providers_present=$(docker compose exec -T -u 0 genesis sh -c 'find /etc/genesis/providers.d -mindepth 1 -maxdepth 1 -printf "%f\n"')
if [ -n "$repos_present" ] || [ -n "$providers_present" ]; then
  echo "unexpected existing declaration or provider file" >&2
  printf '%s\n' "$repos_present" "$providers_present" >&2
  exit 1
fi

docker compose exec -T -u 0 genesis sh -c '
  umask 077
  cat > /var/lib/genesis/secrets/GITHUB_APP_RECONCILER_PEM
  chown root:root /var/lib/genesis/secrets /var/lib/genesis/secrets/GITHUB_APP_RECONCILER_PEM
  chmod 0700 /var/lib/genesis/secrets
  chmod 0600 /var/lib/genesis/secrets/GITHUB_APP_RECONCILER_PEM
  find /var/lib/genesis/secrets -mindepth 1 -maxdepth 1 -printf "%y %m %s\n"
' < "$PEM_FILE"

python3 - <<'PY'
import json, os
from pathlib import Path
org, repo = os.environ["ORG"], os.environ["REPO"]
app, inst = os.environ["APP_ID"], os.environ["INSTALLATION_ID"]

def gh(path):
    return json.loads(os.popen(f"gh api {path}").read())

live = gh(f"/repos/{org}/{repo}")
actions = gh(f"/repos/{org}/{repo}/actions/permissions")
if live["full_name"] != f"{org}/{repo}" or live["visibility"] != "public" or live["archived"]:
    raise SystemExit("repository is not the expected public unarchived repo")
if not live.get("default_branch"):
    raise SystemExit("repository has no default branch; refusing to invent one")
description = live.get("description") or ""
lowered = description.lower()
if "\n" in description or any(marker in lowered for marker in ("-----begin", "ghp_", "github_pat_", "gho_", "ghu_", "ghs_", "ghr_", "x-access-token")):
    raise SystemExit("description is not a single safe line")
for key in ("has_issues", "has_wiki", "has_projects", "allow_squash_merge", "allow_merge_commit", "allow_rebase_merge", "delete_branch_on_merge"):
    if live.get(key) not in (True, False):
        raise SystemExit(key + " is not a boolean")
allowed = actions.get("allowed_actions")
if actions.get("enabled") not in (True, False) or allowed not in ("all", "local_only", "selected"):
    raise SystemExit("actions permissions are not a supported policy")
selected = []
if allowed == "selected":
    selected = gh(f"/repos/{org}/{repo}/actions/permissions/selected-actions").get("patterns_allowed") or []
    if not selected:
        raise SystemExit("selected actions list is empty")

def ybool(value):
    return "true" if value else "false"

lines = [
    "provider: github",
    f"org: {org}",
    f"name: {repo}",
    "lifecycle:",
    "  remove: retain",
    "  existing: adopt",
    "settings:",
    "  visibility: public",
    f"  description: {json.dumps(description)}",
    f"  default_branch: {live['default_branch']}",
    "  features:",
    f"    issues: {ybool(live['has_issues'])}",
    f"    wiki: {ybool(live['has_wiki'])}",
    f"    projects: {ybool(live['has_projects'])}",
    "  merge:",
    f"    allow_squash: {ybool(live['allow_squash_merge'])}",
    f"    allow_merge_commit: {ybool(live['allow_merge_commit'])}",
    f"    allow_rebase: {ybool(live['allow_rebase_merge'])}",
    f"    delete_branch_on_merge: {ybool(live['delete_branch_on_merge'])}",
    "actions:",
    f"  enabled: {ybool(actions['enabled'])}",
    f"  allowed: {allowed}",
]
if selected:
    lines.append("  selected:")
    for pattern in selected:
        lines.append(f"    - {json.dumps(pattern)}")
Path("/tmp/genesis-verification.yaml").write_text("\n".join(lines) + "\n")
Path("/tmp/genesis-probe-provider.yaml").write_text(f"""provider: github
org: {org}
identities:
  - name: reconciler
    role: reconciler
    credential: app
    secret: GITHUB_APP_RECONCILER_PEM
    app_id: "{app}"
    installation_id: "{inst}"
""")
print("declaration_bytes", Path("/tmp/genesis-verification.yaml").stat().st_size)
print("default_branch", live["default_branch"])
print("actions", ybool(actions["enabled"]), allowed, len(selected))
PY

docker compose exec -T -u 0 genesis sh -c 'cat > /etc/genesis/providers.d/verification.yaml && chown root:genesis /etc/genesis/providers.d/verification.yaml && chmod 0640 /etc/genesis/providers.d/verification.yaml' \
  < /tmp/genesis-probe-provider.yaml
docker compose exec -T -u genesis genesis sh -c 'cat > /var/lib/genesis/config/repos.d/verification.yaml' \
  < /tmp/genesis-verification.yaml
rm -f /tmp/genesis-probe-provider.yaml
```

Expected: one secrets line, `f 600`, with the same byte count as
`wc -c < "$PEM_FILE"`. The script exits if `repos.d` or `providers.d`
already contains a file. Do not delete a file you did not create in this
probe. Another declaration would be observed too.

## 4. Repos-only sync

```sh
gh api "/repos/$ORG/$REPO" --jq '{id,node_id,visibility,private,description,default_branch,archived,has_issues,has_wiki,has_projects,allow_squash_merge,allow_merge_commit,allow_rebase_merge,delete_branch_on_merge}' \
  > /tmp/genesis-repo-before.json
gh api "/repos/$ORG/$REPO/keys" --jq 'length' | tee /tmp/genesis-keys-before.txt
SINCE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
sleep 1
curl -sS -D /tmp/genesis-observe.headers -o /tmp/genesis-observe.json \
  http://127.0.0.1:8790/api/sync \
  -H 'Content-Type: application/json' \
  --data '{"scope":["repos"]}'
head -n 1 /tmp/genesis-observe.headers
gh api "/repos/$ORG/$REPO" --jq '{id,node_id,visibility,private,description,default_branch,archived,has_issues,has_wiki,has_projects,allow_squash_merge,allow_merge_commit,allow_rebase_merge,delete_branch_on_merge}' \
  > /tmp/genesis-repo-after.json
gh api "/repos/$ORG/$REPO/keys" --jq 'length' | tee /tmp/genesis-keys-after.txt
cmp /tmp/genesis-repo-before.json /tmp/genesis-repo-after.json && echo repo_unchanged
cmp /tmp/genesis-keys-before.txt /tmp/genesis-keys-after.txt && echo keys_unchanged

python3 - <<'PY'
import json, os
plan = json.load(open("/tmp/genesis-observe.json"))
repo = plan["repository_plan"]
grant = plan["grant_plan"]
assert repo["observation"] == "observed", repo.get("observation")
assert repo["remote_mutation"] == "none"
assert repo["applied"] == []
assert grant["credential_active"] is False
assert grant["remote_registration"] == "none"
assert grant["key_material"] == "none"
observed = repo["observed"][0]
live = json.load(open("/tmp/genesis-repo-after.json"))
assert observed["repository_id"] == live["id"] == int(os.environ["REPO_ID"])
assert observed["node_id"] == live["node_id"] == os.environ["NODE_ID"]
assert observed["visibility"] == "public"
assert observed["default_branch"] == live["default_branch"]
blob = json.dumps(plan)
for marker in ("ghs_", "ghp_", "github_pat_", "BEGIN ", "PRIVATE KEY", "eyJ"):
    if marker in blob:
        raise SystemExit("sync json contains " + marker)
drift = repo.get("drift") or []
settings = [item for item in drift if item["field"].startswith("settings.") and item["status"] == "drift"]
if settings:
    raise SystemExit("settings drift: " + json.dumps(settings))
remove = [item for item in drift if item["field"] == "lifecycle.remove"]
assert len(remove) == 1 and remove[0]["status"] == "unsupported"
print("observation", repo["observation"])
print("repository_remote_mutation", repo["remote_mutation"])
print("repository_id", observed["repository_id"])
print("node_id", observed["node_id"])
print("grant_remote_registration", grant["remote_registration"])
for item in drift:
    print("drift", item["field"], item["status"])
PY

docker compose logs genesis --since "$SINCE" > /tmp/genesis-observe.logs 2>&1
python3 - <<'PY'
import json
posts = gets = scopes = other = 0
for raw in open("/tmp/genesis-observe.logs"):
    start = raw.find("{")
    if start < 0:
        continue
    try:
        entry = json.loads(raw[start:])
    except json.JSONDecodeError:
        continue
    blob = json.dumps(entry)
    for marker in ("ghs_", "ghp_", "github_pat_", "BEGIN ", "PRIVATE KEY", "eyJ"):
        if marker in blob:
            raise SystemExit("log contains " + marker)
    if entry.get("msg") == "github call":
        method, path = entry["method"], entry["path"]
        if method == "GET":
            gets += 1
        elif method == "POST" and path.endswith("/access_tokens"):
            posts += 1
            print("token_post", entry["status"], "access_tokens")
        else:
            other += 1
            print("unexpected_call", method, path, entry.get("status"))
    elif entry.get("msg") == "github token scope":
        scopes += 1
        if entry.get("requested") != "administration=read,metadata=read" or entry.get("returned") != "administration=read,metadata=read":
            raise SystemExit("token scope " + json.dumps({"requested": entry.get("requested"), "returned": entry.get("returned")}))
        print("token_scope", entry["requested"], entry["returned"])
if other or posts < 2 or gets < 2 or scopes < 2:
    raise SystemExit(f"posts={posts} gets={gets} scopes={scopes} other={other}")
print("github_posts", posts)
print("github_gets", gets)
print("github_scopes", scopes)
PY

docker compose exec -T -u genesis genesis python3 -c '
import json, pathlib
path = pathlib.Path("/var/lib/genesis/data/observations/repositories.json")
text = path.read_text()
for marker in ("ghs_", "ghp_", "github_pat_", "BEGIN ", "PRIVATE KEY", "eyJ"):
    if marker in text:
        raise SystemExit("journal contains " + marker)
journal = json.loads(text)
binding = journal["bindings"][0]
print("journal_version", journal["version"])
print("journal_repository_id", binding["repository_id"])
print("journal_node_id", binding["node_id"])
print("journal_name", binding["name"])
'
docker compose exec -T -u 0 genesis stat -c '%a %u' /var/lib/genesis/data/observations/repositories.json
```

The log script prints `token_post` without the installation id. A repeated
POST is a remint. It is still the only non-GET.

## 5. Declaration rename mismatch

This does not rename GitHub. It points the saved declaration at another
name and expects the persisted id to reject that retarget before any call.

```sh
SINCE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
sleep 1
python3 - <<'PY'
from pathlib import Path
path = Path("/tmp/genesis-verification.yaml")
text = path.read_text()
old = "name: verification\n"
new = "name: verification-observe-mismatch\n"
if old not in text:
    raise SystemExit("declaration name line missing")
Path("/tmp/genesis-verification-mismatch.yaml").write_text(text.replace(old, new, 1))
PY
docker compose exec -T -u genesis genesis sh -c 'cat > /var/lib/genesis/config/repos.d/verification.yaml' \
  < /tmp/genesis-verification-mismatch.yaml
curl -sS -D /tmp/genesis-mismatch.headers -o /tmp/genesis-mismatch.json \
  http://127.0.0.1:8790/api/sync \
  -H 'Content-Type: application/json' \
  --data '{"scope":["repos"]}'
head -n 1 /tmp/genesis-mismatch.headers
docker compose logs genesis --since "$SINCE" > /tmp/genesis-mismatch.logs 2>&1
python3 - <<'PY'
import json
calls = 0
saw = False
for raw in open("/tmp/genesis-mismatch.logs"):
    if "repository declaration no longer matches the persisted repository identity" in raw:
        saw = True
    start = raw.find("{")
    if start < 0:
        continue
    try:
        entry = json.loads(raw[start:])
    except json.JSONDecodeError:
        continue
    if entry.get("msg") == "github call":
        calls += 1
if not saw:
    raise SystemExit("retarget error was not logged")
if calls:
    raise SystemExit(f"retarget made {calls} github calls")
print("retarget_rejected")
print("retarget_github_calls", calls)
PY
docker compose exec -T -u genesis genesis python3 -c '
import json, pathlib
binding = json.loads(pathlib.Path("/var/lib/genesis/data/observations/repositories.json").read_text())["bindings"][0]
print("journal_name", binding["name"])
print("journal_repository_id", binding["repository_id"])
'
gh api "/repos/$ORG/$REPO" --jq '{full_name,id,node_id}'
docker compose exec -T -u genesis genesis sh -c 'cat > /var/lib/genesis/config/repos.d/verification.yaml' \
  < /tmp/genesis-verification.yaml
```

## 6. Reversible GitHub rename

The temporary name is `verification-observe-hold`. Restore uses the numeric
repository id, so it does not depend on the temporary name. Keep this block
together. If it stops early, run `restore_verification_name` before leaving
the shell.

```sh
trap restore_verification_name EXIT
gh api --method PATCH "/repos/$ORG/$REPO" -f name="$MISMATCH_NAME" --jq '{full_name,id,node_id}'
gh api "/repos/$ORG/$REPO" --jq .message || true
SINCE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
sleep 1
curl -sS -D /tmp/genesis-rename.headers -o /tmp/genesis-rename.json \
  http://127.0.0.1:8790/api/sync \
  -H 'Content-Type: application/json' \
  --data '{"scope":["repos"]}'
head -n 1 /tmp/genesis-rename.headers
docker compose logs genesis --since "$SINCE" > /tmp/genesis-rename.logs 2>&1
python3 - <<'PY'
import json
posts = gets = other = 0
saw = False
for raw in open("/tmp/genesis-rename.logs"):
    if "github collision" in raw:
        saw = True
    start = raw.find("{")
    if start < 0:
        continue
    try:
        entry = json.loads(raw[start:])
    except json.JSONDecodeError:
        continue
    blob = json.dumps(entry)
    for marker in ("ghs_", "ghp_", "github_pat_", "BEGIN ", "PRIVATE KEY", "eyJ"):
        if marker in blob:
            raise SystemExit("log contains " + marker)
    if entry.get("msg") == "github call":
        method, path, status = entry["method"], entry["path"], entry["status"]
        if method == "POST" and str(path).endswith("/access_tokens"):
            posts += 1
            if status != 422:
                raise SystemExit(f"token status {status}")
        elif method == "GET":
            gets += 1
        else:
            other += 1
if not saw or posts < 1 or gets or other:
    raise SystemExit(f"saw={saw} posts={posts} gets={gets} other={other}")
print("rename_rejected")
print("rename_token_posts", posts)
print("rename_gets", gets)
PY
docker compose exec -T -u genesis genesis python3 -c '
import json, os, pathlib
binding = json.loads(pathlib.Path("/var/lib/genesis/data/observations/repositories.json").read_text())["bindings"][0]
print("journal_repository_id", binding["repository_id"])
print("journal_name", binding["name"])
'
restore_verification_name
trap - EXIT
gh api "/repos/$ORG/$REPO" --jq '{full_name,id,node_id}'
gh api "/repos/$ORG/$REPO/keys" --jq 'length'
```

Genesis does not rename the repository back. The host `PATCH
/repositories/$REPO_ID` does. GitHub answers the token mint with 422
because the old name is no longer an installed repository, and Genesis
records that as `github collision` without a repository GET.

## 7. Confirm the restored name still observes

```sh
SINCE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
sleep 1
curl -sS -D /tmp/genesis-restored.headers -o /tmp/genesis-restored.json \
  http://127.0.0.1:8790/api/sync \
  -H 'Content-Type: application/json' \
  --data '{"scope":["repos"]}'
head -n 1 /tmp/genesis-restored.headers
python3 - <<'PY'
import json, os
plan = json.load(open("/tmp/genesis-restored.json"))["repository_plan"]
observed = plan["observed"][0]
assert plan["observation"] == "observed"
assert plan["remote_mutation"] == "none"
assert observed["repository_id"] == int(os.environ["REPO_ID"])
assert observed["node_id"] == os.environ["NODE_ID"]
print("restored_observation", observed["repository_id"], observed["node_id"])
PY
docker compose logs genesis --since "$SINCE" > /tmp/genesis-restored.logs 2>&1
python3 - <<'PY'
import json
other = 0
for raw in open("/tmp/genesis-restored.logs"):
    start = raw.find("{")
    if start < 0:
        continue
    try:
        entry = json.loads(raw[start:])
    except json.JSONDecodeError:
        continue
    if entry.get("msg") != "github call":
        continue
    method, path = entry["method"], entry["path"]
    if method == "GET":
        continue
    if method == "POST" and str(path).endswith("/access_tokens"):
        continue
    other += 1
if other:
    raise SystemExit(f"unexpected calls {other}")
print("restored_calls_get_or_token_post")
PY
```

## 8. Cleanup

This removes the probe declaration, the provider file, and the container
copy of the PEM. It does not delete `$PEM_FILE`, the App, or
`verification`. It does not run `docker compose down -v`.

```sh
restore_verification_name
docker compose exec -T -u genesis genesis rm -f /var/lib/genesis/config/repos.d/verification.yaml
docker compose exec -T -u 0 genesis rm -f \
  /etc/genesis/providers.d/verification.yaml \
  /var/lib/genesis/secrets/GITHUB_APP_RECONCILER_PEM
rm -f /tmp/genesis-verification.yaml /tmp/genesis-verification-mismatch.yaml \
  /tmp/genesis-probe-provider.yaml /tmp/genesis-observe.json /tmp/genesis-mismatch.json \
  /tmp/genesis-rename.json /tmp/genesis-restored.json /tmp/genesis-observe.logs \
  /tmp/genesis-mismatch.logs /tmp/genesis-rename.logs /tmp/genesis-restored.logs
gh api "/repos/$ORG/$REPO" --jq '{full_name,id,node_id,visibility,archived}'
test -f "$PEM_FILE" && echo pem_kept
```

## Expected evidence

Save these lines and nothing else:

| Step | Save |
|---|---|
| Repo and App | `genesis-id13-tech/verification`, public, not archived, one installation, permissions `administration: write` and `metadata: read`, `total_count` 1 |
| Secret file | `f 600` and the byte count, not the PEM |
| Repos-only sync | HTTP `200`, `repo_unchanged`, `keys_unchanged`, `observation observed`, `repository_remote_mutation none`, `grant_remote_registration none`, `repository_id` and `node_id` equal to the `gh` values |
| Token scope | two or more `token_scope administration=read,metadata=read administration=read,metadata=read` |
| Calls | `github_posts` and `github_gets` at least 2, no `unexpected_call`. The only POST path is installation `access_tokens` |
| Drift | `drift lifecycle.remove unsupported`. No `settings.` line with status `drift`. Extra rulesets or Actions allowances may print as `unobservable` or `unsupported` |
| Journal | `journal_version 1`, the same id, node id, and name `verification`, `stat` `600 65532`, no token marker |
| Declaration mismatch | HTTP `500`, `retarget_rejected`, `retarget_github_calls 0`, journal name still `verification` |
| GitHub rename | HTTP `500`, `rename_rejected`, `rename_gets 0`, then `restored_name verification` and the original id and node id |
| Restored read | HTTP `200`, `restored_observation` with the same id and node id, `restored_calls_get_or_token_post` |
| Cleanup | `full_name` `genesis-id13-tech/verification`, same id and node id, `pem_kept` |
