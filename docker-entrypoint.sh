#!/bin/sh
# Seed /var/lib/genesis/config from image defaults.
# Missing default files are copied onto an existing named volume so image
# upgrades can deliver newly shipped agents and rules. Existing files are
# left unchanged, including operator-edited YAML. Run as root before launch.
set -eu

config="${GENESIS_CONFIG_DIR:-/var/lib/genesis/config}"
data="${GENESIS_DATA_DIR:-/var/lib/genesis/data}"
defaults="${GENESIS_DEFAULTS_DIR:-/usr/share/genesis/defaults}"
agents="$config/agents.d"
rules="$config/rules.d"

copy_missing() {
	src=$1
	dest=$2
	mkdir -p "$dest"
	[ -d "$src" ] || return 0
	for path in "$src"/*; do
		[ -e "$path" ] || continue
		name=$(basename "$path")
		if [ -e "$dest/$name" ]; then
			continue
		fi
		cp -R "$path" "$dest/$name"
		echo "genesis: seeded missing $(basename "$src")/$name" >&2
	done
}

mkdir -p "$agents" "$rules" "$data/runs"
copy_missing "$defaults/agents.d" "$agents"
copy_missing "$defaults/rules.d" "$rules"

if [ "${1:-}" = "seed-config" ]; then
	exit 0
fi

chown -R genesis:genesis "$config" "$data"
chmod -R u+rwX,go+rX "$config"
chmod -R u=rwX,go= "$data"

exec /usr/local/bin/genesis "$@"
