#!/bin/sh
# Exact Docker Desktop + WSL smoke test for Genesis.
# Run this from the repository root inside a WSL distro that has Docker
# Desktop integration enabled. Do not run it from PowerShell or cmd.exe.
# Agents and rules live on the named volume genesis-config, not a host bind.
#
# Default keeps named volumes (non-destructive). Missing image defaults such
# as designer.yaml are copied; existing volume files are not overwritten.
# --reset-volumes is the explicit destructive path: docker compose down -v
# before up, which deletes genesis-config, genesis-data, genesis-credentials,
# and genesis-secrets.
set -eu

cd "$(dirname "$0")/.."

reset_volumes=0
for arg in "$@"; do
  case "$arg" in
    --reset-volumes)
      reset_volumes=1
      ;;
    -h|--help)
      echo "usage: sh scripts/wsl-docker-smoke.sh [--reset-volumes]" >&2
      exit 0
      ;;
    *)
      echo "unknown argument: $arg" >&2
      echo "usage: sh scripts/wsl-docker-smoke.sh [--reset-volumes]" >&2
      exit 1
      ;;
  esac
done

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is not on PATH. Enable Docker Desktop WSL integration for this distro." >&2
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  echo "docker is installed but the engine is not reachable. Start Docker Desktop." >&2
  exit 1
fi
if ! docker compose version >/dev/null 2>&1; then
  echo "docker compose plugin is missing." >&2
  exit 1
fi

pwd_path="$(pwd -P)"
case "$pwd_path" in
  /mnt/[a-zA-Z]/*)
    echo "repository is on a Windows drive mount ($pwd_path)." >&2
    echo "clone or copy the repo into the WSL filesystem, e.g. ~/src/genesis, and rerun." >&2
    exit 1
    ;;
esac

token="${GENESIS_SYNC_TOKEN:-compose-sync-token}"
export GENESIS_SYNC_TOKEN="$token"
smoke_rule=/var/lib/genesis/config/rules.d/wsl-smoke.yaml
canary_rule=/var/lib/genesis/config/rules.d/operator-canary.yaml

if [ "$reset_volumes" = 1 ]; then
  echo "DESTRUCTIVE: docker compose down -v (removes genesis-config, genesis-data, and genesis-credentials; keeps runtime/)" >&2
  docker compose down -v
fi

mkdir -p runtime/providers runtime/secrets

docker compose up --build -d
cleanup() {
  docker compose exec -T -u genesis genesis rm -f "$smoke_rule" "$canary_rule" >/dev/null 2>&1 || true
  docker compose down >/dev/null 2>&1 || true
}
trap cleanup EXIT

ready=0
i=0
while [ "$i" -lt 60 ]; do
  if curl -sf -o /dev/null http://127.0.0.1:8790/api/health; then
    ready=1
    break
  fi
  i=$((i + 1))
  sleep 0.25
done
if [ "$ready" != 1 ]; then
  docker compose logs >&2
  echo "genesis control never became reachable on 127.0.0.1:8790" >&2
  exit 1
fi

if curl -sf -m 1 -o /dev/null http://127.0.0.1:8787/health; then
  echo "listener port 8787 is published; only the control port should be" >&2
  exit 1
fi
if ! curl -sf http://127.0.0.1:8790/ | grep -q Genesis; then
  echo "control UI did not serve the Genesis panel" >&2
  exit 1
fi
if curl -sf http://127.0.0.1:8790/api/state | grep -q "$token"; then
  echo "control API exposed the sync token" >&2
  exit 1
fi
control_uid="$(docker compose exec -T control id -u | tr -d '\r\n')"
if [ "$control_uid" != "65532" ]; then
  echo "control is running as uid ${control_uid}, expected genesis uid 65532" >&2
  exit 1
fi
if docker compose exec -T control touch /var/lib/genesis/data/ro-probe >/dev/null 2>&1; then
  echo "control was able to write genesis-data" >&2
  exit 1
fi
if ! docker compose exec -T control sh -c 'touch /var/lib/genesis/config/rw-probe && rm -f /var/lib/genesis/config/rw-probe'; then
  echo "control could not write genesis-config" >&2
  exit 1
fi

persisted_pre_upgrade_volume() {
  cat >&2 <<'EOF'
workspace-janitor on genesis-config has no top-level user: field.
This is persisted pre-upgrade named-volume config, not a dedicated-user pass.
Expected on that volume: only the genesis OS user, genesis-owned output,
control-panel "Shared listener UID", and matching active/draft digests.
Those digests mean the listener cache equals the volume files, not that the
volume matches image defaults.

Non-destructive inspect/update: docs/designer.md
  docker compose exec -u genesis genesis sed -n '1,20p' \
    /var/lib/genesis/config/agents.d/workspace-janitor.yaml
  docker compose exec genesis sed -n '1,20p' \
    /usr/share/genesis/defaults/agents.d/workspace-janitor.yaml
Copy the default janitor onto the volume only if you intend to replace it,
then Sync. Do not treat matching digests as a dedicated-user success.

Destructive clean-volume smoke (deletes both named volumes):
  sh scripts/wsl-docker-smoke.sh --reset-volumes
EOF
}

if ! docker compose exec -T -u genesis genesis test -f /var/lib/genesis/config/agents.d/designer.yaml ||
   docker compose exec -T -u genesis genesis test -L /var/lib/genesis/config/agents.d/designer.yaml; then
  echo "designer.yaml is missing from genesis-config or is not a regular file" >&2
  exit 1
fi
if ! docker compose exec -T -u genesis genesis test -f /var/lib/genesis/config/rules.d/designer.yaml ||
   docker compose exec -T -u genesis genesis test -L /var/lib/genesis/config/rules.d/designer.yaml; then
  echo "rules.d/designer.yaml is missing from genesis-config or is not a regular file" >&2
  exit 1
fi
if docker compose exec -T -u genesis genesis grep -E '^user:' /var/lib/genesis/config/agents.d/designer.yaml >/dev/null; then
  echo "designer.yaml must omit user so it stays on the shared listener UID" >&2
  docker compose exec -T -u genesis genesis sed -n '1,20p' /var/lib/genesis/config/agents.d/designer.yaml >&2
  exit 1
fi
if ! docker compose exec -T -u genesis genesis grep -q '^cwd: /var/lib/genesis/config$' /var/lib/genesis/config/agents.d/designer.yaml; then
  echo "designer cwd is not the writable config root" >&2
  docker compose exec -T -u genesis genesis sed -n '1,20p' /var/lib/genesis/config/agents.d/designer.yaml >&2
  exit 1
fi
if ! curl -sf http://127.0.0.1:8790/api/agents | grep -q '"id":"designer"'; then
  echo "control API did not list the designer agent" >&2
  curl -s http://127.0.0.1:8790/api/agents >&2 || true
  exit 1
fi

janitor_sum="$(docker compose exec -T -u genesis genesis sha256sum /var/lib/genesis/config/agents.d/workspace-janitor.yaml | awk '{print $1}' | tr -d '\r\n')"
docker compose exec -T -u genesis genesis tee "$canary_rule" >/dev/null <<'YAML'
match:
  type: dev.genesis.operator-canary
agent: workspace-janitor
YAML
docker compose restart genesis
genesis_ready=0
i=0
while [ "$i" -lt 60 ]; do
  if docker compose exec -T genesis python3 -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:8787/health", timeout=1)' >/dev/null 2>&1; then
    genesis_ready=1
    break
  fi
  i=$((i + 1))
  sleep 0.25
done
if [ "$genesis_ready" != 1 ]; then
  docker compose logs genesis >&2
  echo "genesis listener did not return after restart" >&2
  exit 1
fi
if ! docker compose exec -T -u genesis genesis grep -q 'dev.genesis.operator-canary' "$canary_rule"; then
  echo "entrypoint overwrote or dropped the operator canary rule" >&2
  exit 1
fi
janitor_sum_after="$(docker compose exec -T -u genesis genesis sha256sum /var/lib/genesis/config/agents.d/workspace-janitor.yaml | awk '{print $1}' | tr -d '\r\n')"
if [ "$janitor_sum" != "$janitor_sum_after" ]; then
  echo "entrypoint overwrote workspace-janitor.yaml on restart" >&2
  exit 1
fi
docker compose exec -T -u genesis genesis rm -f "$canary_rule"

if ! docker compose logs | grep -q '"listener_user":"genesis"'; then
  docker compose logs >&2
  echo "orchestrator did not drop the listener to user genesis" >&2
  exit 1
fi

janitor_user_line="$(docker compose exec -T -u genesis genesis sed -n '/^user:/p' /var/lib/genesis/config/agents.d/workspace-janitor.yaml | tr -d '\r\n')"
if [ "$janitor_user_line" != "user: workspace-janitor" ]; then
  persisted_pre_upgrade_volume
  echo "volume janitor user field is ${janitor_user_line:-empty}" >&2
  exit 1
fi

if ! docker compose exec -T genesis getent passwd workspace-janitor >/dev/null; then
  docker compose logs >&2
  echo "launch did not reconcile OS user workspace-janitor" >&2
  exit 1
fi
janitor_shell="$(docker compose exec -T genesis getent passwd workspace-janitor | awk -F: '{print $7}' | tr -d '\r\n')"
if [ "$janitor_shell" != "/bin/bash" ]; then
  echo "workspace-janitor shell is ${janitor_shell}, expected /bin/bash" >&2
  exit 1
fi
janitor_home_stat="$(docker compose exec -T genesis stat -c '%U %a' /home/workspace-janitor | tr -d '\r\n')"
if [ "$janitor_home_stat" != "workspace-janitor 700" ]; then
  echo "workspace-janitor home is ${janitor_home_stat}, expected workspace-janitor 700" >&2
  exit 1
fi
if ! docker compose exec -T genesis test -d /home/workspace-janitor/workspace; then
  echo "workspace-janitor workspace directory is missing" >&2
  exit 1
fi

if docker compose exec -T -u genesis genesis sh -c 'touch /usr/local/bin/genesis' >/dev/null 2>&1; then
  echo "genesis was able to write the genesis binary" >&2
  exit 1
fi
if docker compose exec -T -u genesis genesis sh -c 'touch /app/.venv/genesis-write-probe' >/dev/null 2>&1; then
  echo "genesis was able to write the runtime venv" >&2
  exit 1
fi

docker compose exec -T -u genesis genesis tee "$smoke_rule" >/dev/null <<'YAML'
match:
  type: dev.genesis.wsl
agent: workspace-janitor
YAML

listener_request() {
  docker compose exec -T \
    -e M="$1" -e P="$2" -e C="${3:-}" -e B="${4:-}" -e A="${5:-}" \
    genesis python3 -c '
import os, urllib.error, urllib.request
body = os.environ["B"].encode() or None
req = urllib.request.Request("http://127.0.0.1:8787" + os.environ["P"], data=body, method=os.environ["M"])
if os.environ["C"]:
    req.add_header("Content-Type", os.environ["C"])
if os.environ["A"]:
    req.add_header("Authorization", os.environ["A"])
try:
    with urllib.request.urlopen(req) as resp:
        data, code = resp.read().decode(), resp.status
except urllib.error.HTTPError as exc:
    data, code = exc.read().decode(), exc.code
with open("/tmp/genesis-listener.out", "w") as handle:
    handle.write(data)
print(code)
'
}

copy_listener_out() {
  docker compose exec -T genesis cat /tmp/genesis-listener.out >"$1"
}

unsynced="$(listener_request POST /events application/cloudevents+json \
  '{"specversion":"1.0","id":"wsl-1","source":"urn:genesis:wsl","type":"dev.genesis.wsl"}')"
copy_listener_out /tmp/genesis-wsl-event.out
if [ "$unsynced" != "204" ]; then
  echo "expected 204 for a newly written rule before /sync, got $unsynced" >&2
  cat /tmp/genesis-wsl-event.out >&2 || true
  exit 1
fi

sync_code="$(listener_request POST /sync application/json '{"scope":["rules"]}' "Bearer $token")"
copy_listener_out /tmp/genesis-wsl-sync.out
if [ "$sync_code" != "200" ]; then
  echo "expected 200 from POST /sync, got $sync_code" >&2
  cat /tmp/genesis-wsl-sync.out >&2 || true
  exit 1
fi
if ! grep -q '"attached":true' /tmp/genesis-wsl-sync.out; then
  echo "POST /sync did not report attached privileged ipc" >&2
  cat /tmp/genesis-wsl-sync.out >&2
  exit 1
fi
if ! grep -q '"host_mutation":"none"' /tmp/genesis-wsl-sync.out; then
  echo "rules-only POST /sync claimed a host mutation" >&2
  cat /tmp/genesis-wsl-sync.out >&2
  exit 1
fi

agents_sync="$(listener_request POST /sync application/json '{"scope":["agents"]}' "Bearer $token")"
copy_listener_out /tmp/genesis-wsl-agents-sync.out
if [ "$agents_sync" != "200" ]; then
  echo "expected 200 from agents POST /sync, got $agents_sync" >&2
  cat /tmp/genesis-wsl-agents-sync.out >&2 || true
  exit 1
fi
if ! grep -q '"host_mutation":"applied"' /tmp/genesis-wsl-agents-sync.out; then
  echo "agents POST /sync did not apply dedicated OS users" >&2
  cat /tmp/genesis-wsl-agents-sync.out >&2
  exit 1
fi
if ! grep -q 'ensure_agent_user:workspace-janitor' /tmp/genesis-wsl-agents-sync.out; then
  echo "agents POST /sync did not report ensure_agent_user:workspace-janitor" >&2
  cat /tmp/genesis-wsl-agents-sync.out >&2
  exit 1
fi

synced="$(listener_request POST /events application/cloudevents+json \
  '{"specversion":"1.0","id":"wsl-2","source":"urn:genesis:wsl","type":"dev.genesis.wsl"}')"
copy_listener_out /tmp/genesis-wsl-synced.out
if [ "$synced" != "202" ]; then
  echo "expected 202 after /sync picked up the new rule, got $synced" >&2
  cat /tmp/genesis-wsl-synced.out >&2 || true
  exit 1
fi
run_id="$(sed -n 's/.*"run_id":"\([^"]*\)".*/\1/p' /tmp/genesis-wsl-synced.out | head -n 1)"
if [ -z "$run_id" ]; then
  echo "POST /events 202 did not include a run_id" >&2
  cat /tmp/genesis-wsl-synced.out >&2
  exit 1
fi
if ! docker compose exec -T -u genesis genesis test -f "/var/lib/genesis/data/runs/$run_id/events.jsonl"; then
  echo "run journal was not created before HTTP 202" >&2
  docker compose exec -T -u genesis genesis ls -la /var/lib/genesis/data/runs >&2 || true
  exit 1
fi

docker compose exec -T -u genesis genesis rm -f "$smoke_rule"

echo "WSL Docker smoke passed."
if [ "$reset_volumes" = 1 ]; then
  echo "Used --reset-volumes; named volumes were deleted and reseeded from image defaults."
else
  echo "Named volume genesis-config was kept; missing defaults (including designer) were seeded without overwriting existing files."
fi
echo "genesis-data keeps run journals across compose down; compose down -v reseeds defaults."
echo "Windows browsers can use http://127.0.0.1:8790/ . Select designer.yaml (dev.genesis.user.message) in the panel."
echo "The listener stays on the Compose network."
