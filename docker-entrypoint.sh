#!/bin/sh
# Seed /var/lib/genesis/config from image defaults.
# Missing default YAML files are copied onto an existing named volume so image
# upgrades can deliver newly shipped agents and rules. repos.d is created when
# missing; image defaults are copied only if that directory exists in the image.
# providers.d is not seeded. It stays outside this volume. The secrets
# directory is not created. A missing directory, or a missing
# GITHUB_APP_RECONCILER_PEM, GITHUB_APP_ID, or GITHUB_APP_WEBHOOK_SECRET,
# exits. When
# GENESIS_PROVIDERS_DIR or /etc/genesis/providers.d exists, root tightens
# that directory in place (0750 root:genesis, files 0640) and does not
# follow symlinks. The example provider file is not copied here.
# Existing files, including
# dangling or live dest symlinks, are left unchanged. Only regular files with
# valid agent/rule names are copied; source and dest directories must not be
# symlinks. Run as root before launch.
set -eu

config="${GENESIS_CONFIG_DIR:-/var/lib/genesis/config}"
data="${GENESIS_DATA_DIR:-/var/lib/genesis/data}"
credentials="${GENESIS_CREDENTIALS_DIR:-/var/lib/genesis/credentials}"
secrets="${GENESIS_SECRETS_DIR:-/var/lib/genesis/secrets}"
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

# Transcripts sit beside runs. Another uid must traverse the data root and
# read the transcript file, and must not read runs. The listener must own
# transcripts: this mkdir runs after own_tree, so a new directory would
# otherwise stay root-owned and the listener chmod fails with EPERM. Chown
# on every start, including a root-owned directory reused from the volume.
# runs stays 0700.
open_transcripts() {
	root=$1
	if [ -L "$root" ] || [ ! -d "$root" ]; then
		echo "genesis: data root is not a directory: $root" >&2
		exit 1
	fi
	if [ -L "$root/runs" ] || [ -L "$root/transcripts" ]; then
		echo "genesis: refusing to chmod a symlink under $root" >&2
		exit 1
	fi
	mkdir -p "$root/runs" "$root/transcripts"
	if [ -L "$root/runs" ] || [ -L "$root/transcripts" ] || [ ! -d "$root/transcripts" ]; then
		echo "genesis: refusing to chmod a symlink under $root" >&2
		exit 1
	fi
	if [ "$(id -u)" = 0 ]; then
		chown genesis:genesis "$root" "$root/runs"
		find "$root/transcripts" -xdev \( -type d -o -type f \) -exec chown genesis:genesis {} +
	fi
	chmod 0711 "$root"
	chmod 0700 "$root/runs"
	chmod 0755 "$root/transcripts"
	find "$root/transcripts" -xdev -type f -exec chmod 0644 {} +
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

lock_providers() {
	path=${GENESIS_PROVIDERS_DIR:-/etc/genesis/providers.d}
	if [ ! -e "$path" ] && [ ! -L "$path" ]; then
		return 0
	fi
	if [ -L "$path" ]; then
		echo "genesis: providers directory must not be a symlink: $path" >&2
		exit 1
	fi
	if [ ! -d "$path" ]; then
		echo "genesis: providers path is not a directory: $path" >&2
		exit 1
	fi
	# find without -L does not follow file or directory symlinks. -xdev
	# refuses to walk a bind mount nested inside the provider directory.
	if [ "$(id -u)" = 0 ]; then
		find "$path" -xdev \( -type d -o -type f \) -exec chown root:genesis {} +
	fi
	find "$path" -xdev -type d -exec chmod 0750 {} +
	find "$path" -xdev -type f -exec chmod 0640 {} +
}

lock_credentials() {
	path=$credentials
	if [ -z "${GENESIS_CREDENTIALS_DIR:-}" ] && [ ! -e "$path" ] && [ ! -L "$path" ]; then
		return 0
	fi
	if [ -L "$path" ]; then
		echo "genesis: credentials directory must not be a symlink: $path" >&2
		exit 1
	fi
	case "$path" in
		"$config"|"$config"/*|"$data"|"$data"/*)
			echo "genesis: credentials directory must stay outside config and data: $path" >&2
			exit 1
			;;
	esac
	mkdir -p "$path"
	parent=$(dirname "$path")
	if [ "$(id -u)" = 0 ] && [ "$parent" = "/var/lib/genesis" ]; then
		if [ -L "$parent" ]; then
			echo "genesis: credential parent must not be a symlink: $parent" >&2
			exit 1
		fi
		chown root:root "$parent"
		chmod 0755 "$parent"
	fi
	if [ -L "$path" ] || [ ! -d "$path" ]; then
		echo "genesis: credentials path is not a directory: $path" >&2
		exit 1
	fi
	# find without -L does not follow symlinks into agent homes or config.
	if [ "$(id -u)" = 0 ]; then
		find "$path" -xdev \( -type d -o -type f \) -exec chown root:root {} +
	fi
	find "$path" -xdev -type d -exec chmod 0700 {} +
	find "$path" -xdev -type f -exec chmod 0600 {} +
}

lock_secrets() {
	path=$secrets
	if [ -z "${GENESIS_SECRETS_DIR:-}" ] && [ ! -e "$path" ] && [ ! -L "$path" ]; then
		return 0
	fi
	if [ -L "$path" ]; then
		echo "genesis: secrets directory must not be a symlink: $path" >&2
		exit 1
	fi
	case "$path" in
		"$config"|"$config"/*|"$data"|"$data"/*|"$credentials"|"$credentials"/*)
			echo "genesis: secrets directory must stay outside config, data, and credentials: $path" >&2
			exit 1
			;;
	esac
	if [ ! -d "$path" ]; then
		echo "genesis: secrets directory is missing: $path" >&2
		exit 1
	fi
	for name in GITHUB_APP_RECONCILER_PEM GITHUB_APP_ID GITHUB_APP_WEBHOOK_SECRET; do
		file=$path/$name
		if [ -L "$file" ] || [ ! -f "$file" ]; then
			echo "genesis: secret file is missing: $name" >&2
			exit 1
		fi
	done
	parent=$(dirname "$path")
	if [ "$(id -u)" = 0 ] && [ "$parent" = "/var/lib/genesis" ]; then
		if [ -L "$parent" ]; then
			echo "genesis: secret parent must not be a symlink: $parent" >&2
			exit 1
		fi
		chown root:root "$parent"
		chmod 0755 "$parent"
	fi
	if [ -L "$path" ] || [ ! -d "$path" ]; then
		echo "genesis: secrets path is not a directory: $path" >&2
		exit 1
	fi
	# find without -L does not follow a secret file that was replaced by a symlink.
	if [ "$(id -u)" = 0 ]; then
		find "$path" -xdev \( -type d -o -type f \) -exec chown root:root {} +
	fi
	find "$path" -xdev -type d -exec chmod 0700 {} +
	find "$path" -xdev -type f -exec chmod 0600 {} +
}

if [ "${1:-}" = "seed-config" ]; then
	exit 0
fi

own_tree "$config" 0755 0644
own_tree "$data" 0700 0600
# own_tree locks the whole data tree. Re-open traversal for transcripts only:
# the data root is execute-only (0711), runs stays 0700, transcript files are
# 0644. open_transcripts chowns transcripts to the listener on every start,
# including a directory this mkdir just created and a root-owned one reused
# from the volume.
open_transcripts "$data"
lock_providers
lock_credentials
lock_secrets

if [ "${1:-}" = "own-config" ]; then
	exit 0
fi

exec /usr/local/bin/genesis "$@"
