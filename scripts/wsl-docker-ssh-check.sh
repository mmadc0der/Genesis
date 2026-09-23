#!/bin/sh
# Guarded check that the Genesis image contains an OpenSSH client and that
# the listener user cannot read the root secret directory.
# Run from the repository root inside WSL. This does not contact GitHub,
# register a key, print a private key, or delete volumes.
set -eu

cd "$(dirname "$0")/.."

if [ "$#" -gt 0 ]; then
  echo "usage: sh scripts/wsl-docker-ssh-check.sh" >&2
  exit 1
fi

refuse_env() {
  name=$1
  eval "value=\${$name-}"
  if [ -n "$value" ]; then
    echo "refusing to run with $name set" >&2
    exit 1
  fi
}

refuse_env GITHUB_TOKEN
refuse_env GH_TOKEN
refuse_env GITHUB_APP_PEM
refuse_env GITHUB_PRIVATE_KEY
refuse_env GITHUB_APP_PRIVATE_KEY
refuse_env GITHUB_APP_RECONCILER_PEM

pwd_path="$(pwd -P)"
case "$pwd_path" in
  /mnt/[a-zA-Z]/*)
    echo "repository is on a Windows drive mount ($pwd_path)." >&2
    echo "clone or copy the repo into the WSL filesystem and rerun." >&2
    exit 1
    ;;
esac

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

compose_text=$(cat compose.yaml)
secrets_mounts=$(printf '%s\n' "$compose_text" | grep -c 'target: /var/lib/genesis/secrets' || true)
if [ "$secrets_mounts" != "1" ]; then
  echo "compose must mount the secret directory on the root service only" >&2
  exit 1
fi

run_ssh() {
  # Empty GitHub and model variables so the one-off container does not
  # receive host credentials. ssh -V does not open a connection.
  docker compose run --rm --no-deps --entrypoint ssh \
    -e GITHUB_TOKEN= \
    -e GH_TOKEN= \
    -e DEEPSEEK_API_KEY= \
    genesis -V
}

if ! run_ssh >/tmp/genesis-ssh-version.txt 2>&1; then
  echo "ssh -V failed inside the image" >&2
  exit 1
fi
if ! grep -q 'OpenSSH_' /tmp/genesis-ssh-version.txt; then
  echo "image ssh is not an OpenSSH client" >&2
  exit 1
fi
grep 'OpenSSH_' /tmp/genesis-ssh-version.txt
rm -f /tmp/genesis-ssh-version.txt

docker compose run --rm --no-deps --entrypoint sh \
  -e GITHUB_TOKEN= \
  -e GH_TOKEN= \
  -e DEEPSEEK_API_KEY= \
  genesis -c 'if command -v sshd >/dev/null 2>&1; then echo "image contains an SSH server" >&2; exit 1; fi'

docker compose run --rm --no-deps --user 0:0 --entrypoint sh \
  -e GITHUB_TOKEN= \
  -e GH_TOKEN= \
  -e DEEPSEEK_API_KEY= \
  genesis -c 'chown root:root /var/lib/genesis/secrets && chmod 0700 /var/lib/genesis/secrets'

docker compose run --rm --no-deps --user 65532:65532 --entrypoint sh \
  -e GITHUB_TOKEN= \
  -e GH_TOKEN= \
  -e DEEPSEEK_API_KEY= \
  genesis -c 'if [ -r /var/lib/genesis/secrets ] || [ -w /var/lib/genesis/secrets ] || [ -x /var/lib/genesis/secrets ]; then echo "listener user can reach secrets" >&2; exit 1; fi'

echo "ssh client check passed"
