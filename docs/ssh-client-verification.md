# WSL Docker and reconciler App checklist

For the existing `genesis-id13-tech/verification` App, use the shorter
read-only probe in [wsl-readonly-observation.md](wsl-readonly-observation.md).
This file is the deploy-key checklist. It registers a key in later sections.

Run this from a clone on the WSL filesystem (`~/src/genesis`, not `/mnt/...`),
in a distro with Docker Desktop integration. Do not run it from PowerShell
or `cmd.exe`. CI does not perform this check.

Do not print, commit, or paste a PEM, App JWT, installation token,
`GH_TOKEN`, `GITHUB_TOKEN`, or `id_ed25519`. Record only the evidence lines
in the last section. Do not `cat` `/var/lib/genesis/secrets` or
`/var/lib/genesis/credentials/grants/*/id_ed25519`.

`sh scripts/wsl-docker-ssh-check.sh` is only the no-credential smoke. It
does not register a deploy key. The later sections use one disposable
public organization repository and one reconciler App. A current repos
sync creates a missing adopted repository and applies settings, Actions,
the declared ruleset, and missing bootstrap files. The repos sections
below were written when that sync only read and asserted `repo_unchanged`
with `remote_mutation none`. Do not run them expecting GitHub settings to
stay unchanged. They still do not register a deploy key, write secrets, or
give agents an App token. Deploy-key registration is a later section.
Bootstrap needs Contents write; this checklist's declaration has no
template, so it does not request a contents token.

## 0. Placeholders

```sh
cd ~/src/genesis
git checkout cursor/repo-observe-drift-9434

export ORG=replace-with-org-login
export REPO=genesis-deploy-probe
export PEM_FILE=$HOME/genesis-probe-reconciler.pem
export GENESIS_SYNC_TOKEN=compose-sync-token
unset GITHUB_TOKEN GH_TOKEN GITHUB_APP_PEM GITHUB_PRIVATE_KEY \
  GITHUB_APP_PRIVATE_KEY GITHUB_APP_RECONCILER_PEM DEEPSEEK_API_KEY || true

case $ORG in ''|*[!A-Za-z0-9-]*|*[-] ) echo "ORG is not a GitHub login" >&2; exit 1 ;; esac
case $REPO in ''|*[!A-Za-z0-9._-]*|*.git) echo "REPO is not a repository name" >&2; exit 1 ;; esac
```

Set `APP_ID` and `INSTALLATION_ID` only after the App is installed, from
the `gh` output in the next section. Keep `GH_TOKEN` and `GITHUB_TOKEN`
unset for every `docker compose` command. `gh` on the host must already be
logged in as an organization owner (`gh auth status`); that login is not
passed into the container.

## 1. No-credential smoke

```sh
sh scripts/wsl-docker-ssh-check.sh
```

Expected: one `OpenSSH_` version line, then `ssh client check passed`.
An SSH server binary, a secrets directory the listener user can reach, or a
missing `ssh` fails the script. This command does not contact GitHub.

## 2. Disposable public repository and App

An organization repository that already exists is the expected target.
Do not create a second repository, and do not push a commit, README, or
branch. An empty repository has no commits, so `size` is `0`.
`default_branch` may be `null` or `"main"`. Deploy-key registration does
not read or apply `default_branch`. A repos sync does not rename the
default branch of a repository that already exists. It does apply the
other declared settings, including the description in the declaration
below.

Confirm the repository before creating an App. `gh` is the user login from
`gh auth status`, and that user must be an organization owner. This prints
visibility and counts, not file contents or credentials:

```sh
gh api "/repos/$ORG/$REPO" --jq '{visibility,private,size,default_branch}'
gh api "/repos/$ORG/$REPO/keys" --jq 'length'
```

Expected: `visibility` is `public`, `private` is `false`, `size` is `0`,
and the key length is `0`. `default_branch` is `null` or `"main"`. Stop if
`size` is not `0`, if `default_branch` is any other name, or if any deploy
key is already present.

The GitHub UI is required for the App itself. `gh` cannot create the App
or its private key. As an owner of `$ORG`:

1. Open `https://github.com/organizations/$ORG/settings/apps/new`.
   - GitHub App name: a new name used only for this probe.
   - Homepage URL: `https://github.com/$ORG/$REPO`.
   - Uncheck **Webhook → Active**. Do not set a webhook URL or secret.
   - Leave **Request user authorization (OAuth) during installation**
     unchecked.
   - Repository permission **Administration: Read and write**.
   - Repository permission **Metadata: Read-only** (GitHub requires this).
   - No other repository or organization permissions. Contents write is
     required only when a declaration sets `bootstrap.template`. This
     checklist's declaration does not, so do not grant Contents for this
     probe. A minted token is rejected unless its permission set is exact:
     discovery is only `metadata: read`, repository observation is only
     `administration: read` plus `metadata: read`, settings and Actions
     use only `administration: write` plus `metadata: read`, and bootstrap
     uses only `contents: write` plus `metadata: read`.
   - **Where can this GitHub App be installed?** Only on this account.
2. On the App's settings page, choose **Generate a private key**. The
   browser downloads one RSA PEM. Do not open it. Move that download onto
   `$PEM_FILE` without displaying it. It must be `RSA PRIVATE KEY` or
   `PRIVATE KEY`, not an OpenSSH key.
3. Choose **Install App**, select only `$ORG/$REPO`, and do not choose
   all repositories.

Then read the public ids. This does not print a token or a key:

```sh
gh api "/orgs/$ORG/installations" --jq '.installations[] | {id, app_id, app_slug, repository_selection}'
```

Export the `app_id` and `id` of the probe App from that output. Do not paste
a key or a token. A user `gh` login cannot call
`GET /orgs/{org}/installations/{installation_id}` or
`GET /orgs/{org}/installations/{installation_id}/repositories`; both return
404. App permissions stay on the organization installation list.
Repositories for that installation are read with the same user token:

```sh
export APP_ID=
export INSTALLATION_ID=
case $APP_ID in ''|*[!0-9]*|0*) echo "APP_ID must be a positive decimal" >&2; exit 1 ;; esac
case $INSTALLATION_ID in ''|*[!0-9]*|0*) echo "INSTALLATION_ID must be a positive decimal" >&2; exit 1 ;; esac
gh api "/orgs/$ORG/installations" --jq ".installations[] | select(.id == $INSTALLATION_ID and .app_id == $APP_ID) | {id, app_id, app_slug, repository_selection, permissions}"
gh api "/user/installations/$INSTALLATION_ID/repositories" --jq '{total_count, repository_selection, repositories: [.repositories[] | {full_name, name, private}]}'
```

Expected: one installation object, `repository_selection` is `selected` on
both responses, `permissions` are exactly `administration: write` and
`metadata: read`, `total_count` is `1`, and the only repository is
`$ORG/$REPO` with `private: false`. The per-repository `permissions` hash
on this user route is the user's access, not the App's. It is not printed.

Every `gh` call in this checklist uses that user token. Do not export
`GH_TOKEN`, an App JWT, or an installation token.

| Route | User token |
|---|---|
| `GET /repos/{org}/{repo}` | public repository metadata |
| `GET`, `POST`, and `DELETE /repos/{org}/{repo}/keys` | repository admin; an organization owner qualifies |
| `GET /orgs/{org}/installations` | organization owner; this list carries `permissions` and `repository_selection` |
| `GET /user/installations/{installation_id}/repositories` | the same user, for repositories that installation exposes to them |
| `gh repo delete {org}/{repo}` | repository admin |

Do not call `GET /installation/repositories`, `GET /app/installations/{id}`,
or `GET /orgs/{org}/installation`. Those require an App JWT or an
installation token.

Then:

```sh
test -f "$PEM_FILE"
test ! -L "$PEM_FILE"
chmod 0600 "$PEM_FILE"
test "$(stat -c %a "$PEM_FILE")" = 600
wc -c < "$PEM_FILE"
```

Record the byte count. Do not print the file.

## 3. Provider file and Docker secret

```sh
docker compose up --build -d
curl -sf -o /dev/null --retry 30 --retry-delay 1 --retry-connrefused \
  http://127.0.0.1:8790/api/health

docker compose exec -T -u 0 genesis sh -c '
  umask 077
  cat > /var/lib/genesis/secrets/GITHUB_APP_RECONCILER_PEM
  chown root:root /var/lib/genesis/secrets /var/lib/genesis/secrets/GITHUB_APP_RECONCILER_PEM
  chmod 0700 /var/lib/genesis/secrets
  chmod 0600 /var/lib/genesis/secrets/GITHUB_APP_RECONCILER_PEM
  find /var/lib/genesis/secrets -mindepth 1 -maxdepth 1 -printf "%y %m %s\n"
' < "$PEM_FILE"

python3 - <<'PY'
import os
from pathlib import Path
org, repo = os.environ["ORG"], os.environ["REPO"]
Path("/tmp/genesis-probe-provider.yaml").write_text(f"""provider: github
org: {org}
identities:
  - name: reconciler
    role: reconciler
    credential: app
    secret: GITHUB_APP_RECONCILER_PEM
  - name: programmer
    role: programmer
    credential: app
    secret: GITHUB_APP_PROGRAMMER_PEM
""")
Path("/tmp/genesis-probe-repo.yaml").write_text(f"""provider: github
org: {org}
name: {repo}
lifecycle:
  remove: retain
  existing: adopt
settings:
  visibility: public
  description: Disposable deploy-key probe. This declaration is not applied.
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
actions:
  enabled: false
  allowed: local_only
identities:
  - name: programmer
    role: programmer
""")
PY

docker compose exec -T -u 0 genesis sh -c 'cat > /etc/genesis/providers.d/probe.yaml && chown root:genesis /etc/genesis/providers.d/probe.yaml && chmod 0640 /etc/genesis/providers.d/probe.yaml' \
  < /tmp/genesis-probe-provider.yaml
docker compose exec -T -u genesis genesis sh -c 'cat > /var/lib/genesis/config/repos.d/probe.yaml' \
  < /tmp/genesis-probe-repo.yaml
rm -f /tmp/genesis-probe-provider.yaml /tmp/genesis-probe-repo.yaml

docker compose exec -T -u genesis genesis sh -c 'cat > /var/lib/genesis/config/agents.d/workspace-janitor.yaml' <<'EOF'
instructions: |
  You are the workspace janitor for this lab.
  Read the CloudEvent, do only the work it asks, write results in the workspace, then stop.
user: workspace-janitor
cwd: /home/workspace-janitor/workspace
home: /home/workspace-janitor
setup:
  workspace: private
env:
  PATH: /usr/local/bin:/usr/bin:/bin
  LANG: C.UTF-8
github:
  repository: probe
  identity: programmer
  git: write
  permissions:
    contents: write
    metadata: read
EOF

docker compose exec -T -u genesis genesis test -f /var/lib/genesis/config/rules.d/example.yaml
docker compose exec -T genesis getent passwd workspace-janitor
```

Expected from `find`: one line, `f 600` and the same byte count as
`wc -c`. The programmer secret name is only a reference. Do not create that
file. The provider file is in the container filesystem and must be rewritten
after `docker compose up --build`. The secret file is on the
`genesis-secrets` volume. Before a repos sync, also place the company App id
at `/var/lib/genesis/secrets/GITHUB_APP_ID` (mode `0600`, a decimal id, no
trailing newline). Do not put `app_id` or `installation_id` in the provider
file. Genesis discovers the installation from the organization.

`example.yaml` matches `type=dev.genesis.run`, `source=urn:genesis:example`,
`subject=hello`, and starts `workspace-janitor`.

## 4. Read-only repository observation

Do this before any sync that includes agents. The agent file from the
previous section is already on disk, and its grant is `git: write`. A
repos-only sync must not register a key. Every `gh` command here is a
`GET`. Do not post, patch, or delete.

```sh
gh api "/repos/$ORG/$REPO" --jq '{id,node_id,visibility,private,description,default_branch,archived,has_issues,has_wiki,has_projects,allow_squash_merge,allow_merge_commit,allow_rebase_merge,delete_branch_on_merge,size}' \
  > /tmp/genesis-repo-before.json
gh api "/repos/$ORG/$REPO/keys" --jq 'length'
cat /tmp/genesis-repo-before.json

curl -sS -D /tmp/genesis-observe.headers -o /tmp/genesis-observe.json \
  http://127.0.0.1:8790/api/sync \
  -H 'Content-Type: application/json' \
  --data '{"scope":["repos"]}'
head -n 1 /tmp/genesis-observe.headers

gh api "/repos/$ORG/$REPO" --jq '{id,node_id,visibility,private,description,default_branch,archived,has_issues,has_wiki,has_projects,allow_squash_merge,allow_merge_commit,allow_rebase_merge,delete_branch_on_merge,size}' \
  > /tmp/genesis-repo-after.json
gh api "/repos/$ORG/$REPO/keys" --jq 'length'
cmp /tmp/genesis-repo-before.json /tmp/genesis-repo-after.json && echo repo_unchanged || exit 1

python3 - <<'PY'
import json
plan = json.load(open("/tmp/genesis-observe.json"))
repo = plan["repository_plan"]
grant = plan["grant_plan"]
assert repo["observation"] == "observed"
assert repo["remote_mutation"] == "none"
assert repo["applied"] == []
assert grant["credential_active"] is False
assert grant["remote_registration"] == "none"
assert grant["key_material"] == "none"
observed = repo["observed"][0]
live = json.load(open("/tmp/genesis-repo-after.json"))
assert observed["repository_id"] == live["id"]
assert observed["node_id"] == live["node_id"]
assert observed["visibility"] == live["visibility"]
assert observed["org"] and observed["name"]
print("observation", repo["observation"])
print("repository_remote_mutation", repo["remote_mutation"])
print("repository_id", observed["repository_id"])
print("node_id", observed["node_id"])
print("drift_count", len(repo.get("drift") or []))
for item in repo.get("drift") or []:
    print("drift", item["field"], item["status"])
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
'
docker compose exec -T -u 0 genesis stat -c '%a %u' /var/lib/genesis/data/observations/repositories.json
curl -sf http://127.0.0.1:8790/api/repositories |
  python3 -c 'import json,sys; d=json.load(sys.stdin); one=next(item for item in d["repositories"] if item["id"]=="probe"); print(one.get("observation"), (one.get("observed") or {}).get("repository_id"), len(one.get("drift") or []))'
```

This script is the observation-stage check. A current repos sync applies
the declaration, so `repo_unchanged` and `repository_remote_mutation none`
are not the contract. Do not run it against a live repository. When it
was read-only, the expected evidence was: both key lengths are `0`. `cmp`
prints `repo_unchanged`. The sync is HTTP `200`. The script prints
`observation observed`, `repository_remote_mutation none`, and a
`repository_id` and `node_id` equal to `gh api /repos/$ORG/$REPO`. Drift
lines were allowed when the empty repository did not already match the
YAML, including a null `default_branch` against `main`. The journal prints
`journal_version 1` and the same id
and node id, with no token marker. `stat` prints `600 65532`. The
control line prints `observed`, the same repository id, and the drift
count. A `500` body is `privileged coordination failed`. Do not continue
to the key-creating sync. Logs may contain `github repository was not
found` when the org, name, or installation target does not match, or
`github auth scope does not match` when the observation token was not
exactly `administration: read` and `metadata: read`. Do not print log
bodies that might contain a token; those markers are the evidence.

## 5. Create, then list and adopt

Keep this shell open. Later sections reuse `show_sync`, `list_keys`, and
`probe_socket`.

```sh
gh api "/repos/$ORG/$REPO/keys" --jq 'length'

show_sync() {
  python3 - <<'PY'
import json
d=json.load(open("/tmp/genesis-sync.json"))
g=d["grant_plan"]
assert g["credential_active"] is False
assert g["remote_registration"] in ("unsupported", "none")
assert g["key_material"] in ("pending", "none")
print("credential_active", g["credential_active"])
print("plan_remote_registration", g["remote_registration"])
print("plan_key_material", g["key_material"])
print("repository_remote_mutation", d["repository_plan"]["remote_mutation"])
for row in g.get("material") or []:
    if row.get("agent") != "workspace-janitor":
        continue
    print("grant_id", row["grant_id"])
    print("remote_status", row["remote_status"])
    print("remote_key_id", row.get("remote_key_id") or "")
    print("fingerprint", row.get("fingerprint") or "")
    print("generation", row.get("generation"))
    print("material_key_material", row.get("key_material"))
print("retained_material", " ".join(g.get("retained_material") or []))
PY
}

list_keys() {
  gh api "/repos/$ORG/$REPO/keys" --jq '.[] | [.id, .title, .read_only, .key] | @tsv' |
    while IFS=$(printf '\t') read -r id title ro key; do
      fp=$(printf '%s\n' "$key" | ssh-keygen -lf -)
      printf 'id=%s title=%s read_only=%s %s\n' "$id" "$title" "$ro" "$fp"
    done
}

curl -sS -D /tmp/genesis-sync.headers -o /tmp/genesis-sync.json \
  http://127.0.0.1:8790/api/sync \
  -H 'Content-Type: application/json' \
  --data '{}'
head -n 1 /tmp/genesis-sync.headers
show_sync
list_keys
```

Create evidence: HTTP `200`. `credential_active False`.
`plan_remote_registration unsupported`. `repository_remote_mutation none`.
`remote_status ready`, `generation 1`, `material_key_material local`, and a
decimal `remote_key_id`. `list_keys` shows exactly one key, title
`genesis-` plus that `grant_id`, `read_only=false`, and the same
`SHA256:` fingerprint. The plan fields stay inactive on purpose. The live
result is `material`.

Adopt evidence is a second sync with the same YAML. The id must not change
and GitHub must still have one key:

```sh
cp /tmp/genesis-sync.json /tmp/genesis-sync-create.json
curl -sS -D /tmp/genesis-sync.headers -o /tmp/genesis-sync.json \
  http://127.0.0.1:8790/api/sync \
  -H 'Content-Type: application/json' \
  --data '{}'
head -n 1 /tmp/genesis-sync.headers
show_sync
list_keys
python3 - <<'PY'
import json
a=json.load(open("/tmp/genesis-sync-create.json"))["grant_plan"]["material"]
b=json.load(open("/tmp/genesis-sync.json"))["grant_plan"]["material"]
def row(items):
    return next(x for x in items if x["agent"]=="workspace-janitor")
assert row(a)["remote_key_id"]==row(b)["remote_key_id"]
assert row(a)["grant_id"]==row(b)["grant_id"]
assert row(a)["fingerprint"]==row(b)["fingerprint"]
print("adopted_same_key", row(b)["remote_key_id"])
PY
```

A `500` with `github auth` means the App id, installation id, PEM, or
Administration permission is wrong, or the minted token includes any
permission outside that exact set. `github missing` means the org, repo, or
installation target does not match. `github partial` means the installation
token is not exactly `$ORG/$REPO`. Do not print log bodies.

## 6. The dedicated process gets the socket

```sh
probe_socket() {
  expect=$1
  docker compose exec -T -u 0 genesis sh -c '
    set -eu
    expect=$1
    uid=$(getent passwd workspace-janitor | cut -d: -f3)
    echo "janitor_uid=$uid"
    i=0
    while [ "$i" -lt 200 ]; do
      sock=
      for candidate in /var/lib/genesis/credentials/sockets/*/agent.sock; do
        if [ -S "$candidate" ]; then
          sock=$candidate
          break
        fi
      done
      if [ -n "$sock" ]; then
        mode=$(stat -c %a "$sock")
        owner=$(stat -c %u "$sock")
        echo "socket_mode=$mode socket_uid=$owner"
        test "$mode" = 600
        test "$owner" = "$uid"
        test "$uid" != 65532
        has=0
        for proc in /proc/[0-9]*; do
          owner=$(stat -c %u "$proc" 2>/dev/null || true)
          if [ "$owner" = "$uid" ] && tr "\0" "\n" < "$proc/environ" 2>/dev/null | grep -q "^SSH_AUTH_SOCK="; then
            has=1
            echo "dedicated_process_has_socket=yes"
            break
          fi
        done
        test "$has" = 1
        echo "ssh_add:"
        SSH_AUTH_SOCK="$sock" ssh-add -l
        echo "github_ssh:"
        SSH_AUTH_SOCK="$sock" ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes -T git@github.com || true
        if [ "$expect" = absent ]; then
          echo "socket appeared when it must not" >&2
          exit 1
        fi
        exit 0
      fi
      i=$((i + 1))
      sleep 0.1
    done
    if [ "$expect" = present ]; then
      echo "no agent socket appeared" >&2
      exit 1
    fi
    echo "socket_absent=yes"
  ' probe "$expect"
}

post_probe() {
  id=$1
  curl -sS -D - -o /tmp/genesis-event.json \
    http://127.0.0.1:8790/api/events \
    -H 'Content-Type: application/cloudevents+json' \
    --data "{\"specversion\":\"1.0\",\"id\":\"$id\",\"source\":\"urn:genesis:example\",\"type\":\"dev.genesis.run\",\"subject\":\"hello\",\"data\":{}}"
  echo
}

probe_socket present > /tmp/genesis-socket-present.txt 2>&1 &
echo $! > /tmp/genesis-socket-present.pid
sleep 0.5
post_probe probe-present
wait "$(cat /tmp/genesis-socket-present.pid)"
cat /tmp/genesis-socket-present.txt
```

Expected: the event response is `202 Accepted` and names
`workspace-janitor`. The proof file contains `socket_mode=600`,
`socket_uid` equal to `janitor_uid` and not `65532`,
`dedicated_process_has_socket=yes`, an `ssh-add` line whose `SHA256:`
fingerprint matches `material`, comment `genesis`, and a GitHub SSH line
naming `$ORG/$REPO` and `successfully authenticated`. That SSH command
exits 1 because GitHub provides no shell. Do not print the socket path in
notes if you copy this file; the lines above are the evidence.

A missing `DEEPSEEK_API_KEY` still starts the process. If the watcher
reports no socket, post `probe-present-2` once while a new
`probe_socket present` is running. Do not export a GitHub token to keep
the process alive.

After the run ends the socket is removed:

```sh
sleep 5
docker compose exec -T -u 0 genesis sh -c 'find /var/lib/genesis/credentials/sockets -name agent.sock -type s | wc -l'
```

Expected: `0`.

## 7. A grant change drops the socket

`git: none` is a different grant. It must not call GitHub and must not
receive a socket. The existing write key stays on GitHub.

```sh
docker compose exec -T -u genesis genesis sh -c 'cat > /var/lib/genesis/config/agents.d/workspace-janitor.yaml' <<'EOF'
instructions: |
  You are the workspace janitor for this lab.
  Read the CloudEvent, do only the work it asks, write results in the workspace, then stop.
user: workspace-janitor
cwd: /home/workspace-janitor/workspace
home: /home/workspace-janitor
setup:
  workspace: private
env:
  PATH: /usr/local/bin:/usr/bin:/bin
  LANG: C.UTF-8
github:
  repository: probe
  identity: programmer
  git: none
  permissions:
    metadata: read
EOF

curl -sS -D /tmp/genesis-sync.headers -o /tmp/genesis-sync.json \
  http://127.0.0.1:8790/api/sync \
  -H 'Content-Type: application/json' \
  --data '{}'
head -n 1 /tmp/genesis-sync.headers
show_sync
list_keys

probe_socket absent > /tmp/genesis-socket-none.txt 2>&1 &
echo $! > /tmp/genesis-socket-none.pid
sleep 0.5
post_probe probe-none
wait "$(cat /tmp/genesis-socket-none.pid)"
cat /tmp/genesis-socket-none.txt
```

Expected: HTTP `200`. The janitor material is not `ready` and its
fingerprint is empty. `retained_material` contains the previous write
grant id. `list_keys` still shows exactly one key, with the original id.
The event is `202`. The proof file says `socket_absent=yes`.

That sync also removes a socket section 6 already handed out, before the
response returns, including while that process is still running. The old
process can keep a stale `SSH_AUTH_SOCK` value; `ssh-add -l` on it fails
because the socket file is gone. The new process never receives the
variable.

Restore write access. This is the adopt path again:

```sh
docker compose exec -T -u genesis genesis sh -c 'cat > /var/lib/genesis/config/agents.d/workspace-janitor.yaml' <<'EOF'
instructions: |
  You are the workspace janitor for this lab.
  Read the CloudEvent, do only the work it asks, write results in the workspace, then stop.
user: workspace-janitor
cwd: /home/workspace-janitor/workspace
home: /home/workspace-janitor
setup:
  workspace: private
env:
  PATH: /usr/local/bin:/usr/bin:/bin
  LANG: C.UTF-8
github:
  repository: probe
  identity: programmer
  git: write
  permissions:
    contents: write
    metadata: read
EOF

curl -sS -D /tmp/genesis-sync.headers -o /tmp/genesis-sync.json \
  http://127.0.0.1:8790/api/sync \
  -H 'Content-Type: application/json' \
  --data '{}'
head -n 1 /tmp/genesis-sync.headers
show_sync
python3 - <<'PY'
import json
old=json.load(open("/tmp/genesis-sync-create.json"))["grant_plan"]["material"]
new=json.load(open("/tmp/genesis-sync.json"))["grant_plan"]["material"]
def row(items):
    return next(x for x in items if x["agent"]=="workspace-janitor")
assert row(old)["remote_key_id"]==row(new)["remote_key_id"]
print("restored_same_key", row(new)["remote_key_id"])
PY
```

## 8. Refusal drops the socket

Add a second deploy key with the same title and a different public key.
Genesis must not delete either key. The next sync fails closed, stores
`refused`, and the following process has no socket.

```sh
TITLE=$(python3 - <<'PY'
import json
rows=json.load(open("/tmp/genesis-sync.json"))["grant_plan"]["material"]
print("genesis-"+next(x["grant_id"] for x in rows if x["agent"]=="workspace-janitor"))
PY
)
umask 077
docker compose run --rm --no-deps -T --entrypoint sh \
  -e GITHUB_TOKEN= -e GH_TOKEN= -e DEEPSEEK_API_KEY= \
  genesis -c 'ssh-keygen -q -t ed25519 -N "" -C genesis-collision-check -f /tmp/collision && cat /tmp/collision.pub && rm -f /tmp/collision /tmp/collision.pub' \
  > "$HOME/genesis-collision.pub"
chmod 0600 "$HOME/genesis-collision.pub"

gh api --method POST "/repos/$ORG/$REPO/keys" \
  -f title="$TITLE" \
  -f key="$(cat "$HOME/genesis-collision.pub")" \
  -F read_only=false \
  --jq '{id, title, read_only}'
rm -f "$HOME/genesis-collision.pub"

curl -sS -D /tmp/genesis-sync.headers -o /tmp/genesis-sync-body.txt \
  http://127.0.0.1:8790/api/sync \
  -H 'Content-Type: application/json' \
  --data '{}'
head -n 1 /tmp/genesis-sync.headers
docker compose logs genesis --since 5m 2>&1 | grep -E 'github collision|PRIVATE KEY|eyJ|ghs_|github_pat_|ghp_|GITHUB_APP_RECONCILER_PEM' || true
docker compose exec -T -u 0 genesis python3 -c 'import json, pathlib
for path in pathlib.Path("/var/lib/genesis/credentials/grants").glob("*/state.json"):
    d=json.loads(path.read_text())
    if d.get("remote_key_id"):
        print(d.get("remote_status"), d.get("remote_key_id"), d.get("generation"))'

probe_socket absent > /tmp/genesis-socket-refused.txt 2>&1 &
echo $! > /tmp/genesis-socket-refused.pid
sleep 0.5
post_probe probe-refused
wait "$(cat /tmp/genesis-socket-refused.pid)"
cat /tmp/genesis-socket-refused.txt
```

Do not sync again before the absent proof. A failed sync leaves the listener
generation in place and drops the deliverable grant before the `500`
returns. A process that already had `SSH_AUTH_SOCK` can keep that stale
value; the socket file is gone, and the next process does not get one.

Expected: HTTP `500` and body `privileged coordination failed`. Logs contain
`github collision` and none of the secret markers. `state.json` prints
`refused`, the previous numeric key id, and `1`. The event is `202`. The
proof file says `socket_absent=yes`. GitHub still has both keys.

## 9. Cleanup and revocation

Remove the grant before deleting the remote, so the last sync does not call
GitHub.

```sh
docker compose exec -T -u genesis genesis cp \
  /usr/share/genesis/defaults/agents.d/workspace-janitor.yaml \
  /var/lib/genesis/config/agents.d/workspace-janitor.yaml
docker compose exec -T -u genesis genesis rm -f /var/lib/genesis/config/repos.d/probe.yaml
docker compose exec -T -u 0 genesis rm -f /etc/genesis/providers.d/probe.yaml
curl -sS -D - -o /tmp/genesis-sync-clean.json \
  http://127.0.0.1:8790/api/sync \
  -H 'Content-Type: application/json' \
  --data '{}'
echo

gh api "/repos/$ORG/$REPO/keys" --jq '.[].id' | while read -r id; do
  gh api --method DELETE "/repos/$ORG/$REPO/keys/$id"
done
gh repo delete "$ORG/$REPO" --yes
```

Then, in the GitHub UI, uninstall the App from `$ORG` and delete the App.
That revokes the private key. Finally:

```sh
docker compose exec -T -u 0 genesis rm -f /var/lib/genesis/secrets/GITHUB_APP_RECONCILER_PEM
rm -f "$PEM_FILE" "$HOME/genesis-collision.pub"
docker compose down -v
```

`down -v` deletes `genesis-config`, `genesis-data`, `genesis-credentials`,
and `genesis-secrets`, including the generated deploy private key. Do this
only after the evidence above is copied out. Confirm the deleted
repository and the removed PEM. This `GET` uses the same user token:

```sh
gh api "/repos/$ORG/$REPO" --jq '.message'
test ! -e "$PEM_FILE" && echo pem_gone
```

Expected: `Not Found` and `pem_gone`.

## Expected evidence

Save these lines and nothing else:

| Step | Save |
|---|---|
| Smoke | `OpenSSH_...` and `ssh client check passed` |
| Secret file | `f 600` and the byte count, not the PEM |
| Read-only observation | `200`, key length `0` before and after, `repo_unchanged`, `observation observed`, `repository_remote_mutation none`, `repository_id` and `node_id` matching `gh`, journal `600 65532` with the same ids and no token marker |
| Create | `200`, `credential_active False`, `remote_status ready`, `generation 1`, one key, title `genesis-<grant_id>`, `read_only=false`, matching `SHA256:` |
| Adopt | `200` and `adopted_same_key` equal to the create id; still one key |
| Socket on | `202`, `socket_mode=600`, dedicated uid, `dedicated_process_has_socket=yes`, matching `ssh-add` fingerprint, GitHub text naming `$ORG/$REPO` |
| Socket off after the run | `0` |
| `git: none` | `200`, material not ready, one unchanged key, live socket file removed immediately, `202`, `socket_absent=yes` |
| Restore | `restored_same_key` equal to the create id |
| Refusal | `500`, `github collision`, no secret markers, `refused <id> 1`, live socket file removed before the response, `202`, `socket_absent=yes`, two keys still listed |
| Cleanup | repo `404`, PEM path gone |

After the key-creating sync, `credential_active` stays false. That HTTP
plan's `remote_registration` stays `unsupported`. That is the listener
summary, not the registrar result. The earlier repos-only sync reports
`remote_registration none` because it does not register a key.
