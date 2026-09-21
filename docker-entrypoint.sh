#!/bin/sh
# Seed /var/lib/genesis/config from image defaults on first volume creation.
# Existing YAML is left alone. Run as root before genesis launch.
set -eu

config="${GENESIS_CONFIG_DIR:-/var/lib/genesis/config}"
data="${GENESIS_DATA_DIR:-/var/lib/genesis/data}"
defaults="${GENESIS_DEFAULTS_DIR:-/usr/share/genesis/defaults}"
agents="$config/agents.d"
rules="$config/rules.d"

dir_empty() {
	[ ! -e "$1" ] || [ -z "$(ls -A "$1" 2>/dev/null)" ]
}

mkdir -p "$agents" "$rules" "$data/runs"
if dir_empty "$agents" && dir_empty "$rules"; then
	if [ -d "$defaults/agents.d" ]; then
		cp -R "$defaults/agents.d/." "$agents/"
	fi
	if [ -d "$defaults/rules.d" ]; then
		cp -R "$defaults/rules.d/." "$rules/"
	fi
fi

chown -R genesis:genesis "$config" "$data"
chmod -R u+rwX,go+rX "$config"
chmod -R u=rwX,go= "$data"

exec /usr/local/bin/genesis "$@"
