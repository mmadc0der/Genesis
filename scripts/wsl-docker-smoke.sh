#!/bin/sh
# Exact Docker Desktop + WSL smoke test for Genesis.
# Run this from the repository root inside a WSL distro that has Docker
# Desktop integration enabled. Do not run it from PowerShell or cmd.exe.
set -eu

cd "$(dirname "$0")/.."

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

docker compose up --build -d
cleanup() {
  docker compose down >/dev/null 2>&1 || true
}
trap cleanup EXIT

ready=0
i=0
while [ "$i" -lt 60 ]; do
  if curl -s -o /dev/null http://127.0.0.1:8787/; then
    ready=1
    break
  fi
  i=$((i + 1))
  sleep 0.25
done
if [ "$ready" != 1 ]; then
  docker compose logs >&2
  echo "genesis never became reachable on 127.0.0.1:8787" >&2
  exit 1
fi

unsynced="$(curl -s -o /tmp/genesis-wsl-event.out -w '%{http_code}' \
  http://127.0.0.1:8787/events \
  -H 'Content-Type: application/cloudevents+json' \
  --data '{"specversion":"1.0","id":"wsl-1","source":"urn:genesis:wsl","type":"dev.genesis.wsl"}')"
if [ "$unsynced" != "204" ]; then
  echo "expected 204 for unmatched event before adding a rule, got $unsynced" >&2
  cat /tmp/genesis-wsl-event.out >&2 || true
  exit 1
fi

sync_code="$(curl -s -o /tmp/genesis-wsl-sync.out -w '%{http_code}' \
  http://127.0.0.1:8787/sync \
  -H 'Authorization: Bearer '"$token" \
  -H 'Content-Type: application/json' \
  --data '{"scope":["agents","rules"]}')"
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
  echo "POST /sync claimed a host mutation" >&2
  cat /tmp/genesis-wsl-sync.out >&2
  exit 1
fi

echo "WSL Docker smoke passed."
echo "Windows browsers can use http://127.0.0.1:8787/ because Docker Desktop publishes the port on localhost."
