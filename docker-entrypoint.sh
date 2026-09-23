#!/bin/sh
# Seed /var/lib/genesis/config from image defaults.
# Missing default YAML files are copied onto an existing named volume so image
# upgrades can deliver newly shipped agents and rules. repos.d is created when
# missing; image defaults are copied only if that directory exists in the image.
# providers.d is not seeded. It stays outside this volume, root-owned, at the
# path passed as -providers.
# Existing files, including
# dangling or live dest symlinks, are left unchanged. Only regular files with
# valid agent/rule names are copied; source and dest directories must not be
# symlinks. Run as root before launch.
set -eu

config="${GENESIS_CONFIG_DIR:-/var/lib/genesis/config}"
data="${GENESIS_DATA_DIR:-/var/lib/genesis/data}"
defaults="${GENESIS_DEFAULTS_DIR:-/usr/share/genesis/defaults}"
agents="$config/agents.d"
rules="$config/rules.d"
repos="$config/repos.d"

require_real_dir() {
	path=$1
	label=$2
	if [ -L "$path" ]; then
		echo "genesis: $label must not be a symlink: $path" >&2
		exit 1
	fi
	if [ ! -d "$path" ]; then
		echo "genesis: $label is not a directory: $path" >&2
		exit 1
	fi
}

valid_yaml_name() {
	name=$1
	case "$name" in
		*.yaml)
			stem=${name%.yaml}
			;;
		*.yml)
			stem=${name%.yml}
			;;
		*)
			return 1
			;;
	esac
	case "$stem" in
		'' | *[!A-Za-z0-9._-]* | [!A-Za-z0-9]*)
			return 1
			;;
	esac
	return 0
}

copy_missing() {
	src=$1
	dest=$2
	mkdir -p "$dest"
	require_real_dir "$dest" "config directory"
	if [ -L "$src" ]; then
		echo "genesis: defaults directory must not be a symlink: $src" >&2
		exit 1
	fi
	[ -d "$src" ] || return 0
	for path in "$src"/*; do
		if [ ! -e "$path" ] && [ ! -L "$path" ]; then
			continue
		fi
		name=$(basename "$path")
		if ! valid_yaml_name "$name"; then
			echo "genesis: refusing to seed unsafe $(basename "$src")/$name" >&2
			exit 1
		fi
		if [ -L "$path" ] || [ ! -f "$path" ]; then
			echo "genesis: refusing to seed non-regular $(basename "$src")/$name" >&2
			exit 1
		fi
	done
	for path in "$src"/*; do
		if [ ! -e "$path" ] && [ ! -L "$path" ]; then
			continue
		fi
		name=$(basename "$path")
		target="$dest/$name"
		if [ -e "$target" ] || [ -L "$target" ]; then
			if [ -L "$target" ]; then
				echo "genesis: skipping existing symlink $(basename "$src")/$name" >&2
			fi
			continue
		fi
		cp -n "$path" "$target"
		echo "genesis: seeded missing $(basename "$src")/$name" >&2
	done
}

own_tree() {
	root=$1
	dir_mode=$2
	file_mode=$3
	if [ -L "$root" ]; then
		echo "genesis: refusing to own a symlink: $root" >&2
		exit 1
	fi
	if [ ! -d "$root" ]; then
		echo "genesis: missing directory $root" >&2
		exit 1
	fi
	if [ "$(id -u)" = 0 ]; then
		find "$root" -xdev \( -type d -o -type f \) -exec chown genesis:genesis {} +
	fi
	find "$root" -xdev -type d -exec chmod "$dir_mode" {} +
	find "$root" -xdev -type f -exec chmod "$file_mode" {} +
}

if [ -e "$config" ] || [ -L "$config" ]; then
	require_real_dir "$config" "config root"
fi
if [ -e "$data" ] || [ -L "$data" ]; then
	require_real_dir "$data" "data root"
fi
mkdir -p "$agents" "$rules" "$repos" "$data/runs"
require_real_dir "$config" "config root"
require_real_dir "$data" "data root"
copy_missing "$defaults/agents.d" "$agents"
copy_missing "$defaults/rules.d" "$rules"
copy_missing "$defaults/repos.d" "$repos"

if [ "${1:-}" = "seed-config" ]; then
	exit 0
fi

own_tree "$config" 0755 0644
own_tree "$data" 0700 0600

if [ "${1:-}" = "own-config" ]; then
	exit 0
fi

exec /usr/local/bin/genesis "$@"
