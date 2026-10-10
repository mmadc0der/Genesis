#!/bin/sh
# Prepare /var/lib/genesis/config for launch.
# A normal start creates agents.d, rules.d, and repos.d when missing and does
# not copy image defaults into them. Deleted default YAML stays deleted across
# restarts. The seed-config argument still copies missing defaults; that path
# is not a launch. providers.d is not seeded. It stays outside this volume.
# The secrets directory is not created. When that directory exists, startup
# locks it.
# GITHUB_APP_RECONCILER_PEM, GITHUB_APP_ID, and GITHUB_APP_WEBHOOK_SECRET
# may be absent. A symlink or other non-file at one of those names exits.
# When
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

# data_owned is a completed own_tree plus open_transcripts/open_sessions.
# The stamp lives on the data volume, so a restart does not walk every
# session and transcript again. GENESIS_OWN_TREE=1 forces that walk.
data_owned() {
	stamp=$data/.genesis-owned
	if [ "${GENESIS_OWN_TREE:-}" = "1" ]; then
		return 1
	fi
	if [ -L "$stamp" ] || [ ! -f "$stamp" ]; then
		return 1
	fi
	[ "$(stat -c %u:%g "$stamp")" = "$(id -u genesis):$(id -g genesis)" ]
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

# own_tree leaves sessions mode 0700/0600 and owned by genesis. Session logs
# are the oracle's review text. 0640 plus the oracle membership in group
# genesis lets that account read them. Directories are group-executable so
# the known path can be opened, and are not group-readable. Runs stay 0700.
# Transcripts stay the separate 0644 journal copies.
open_sessions() {
	root=$1/sessions
	if [ -L "$1/sessions" ]; then
		echo "genesis: refusing to chmod a symlink under $1" >&2
		exit 1
	fi
	if [ ! -d "$root" ]; then
		return 0
	fi
	chmod 0711 "$root"
	find "$root" -mindepth 1 -xdev -type d -exec chmod 0710 {} +
	find "$root" -xdev -type f \( -name 'session.jsonl' -o -name 'session.v*.jsonl' \) -exec chmod 0640 {} +
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
require_real_dir "$agents" "config directory"
require_real_dir "$rules" "config directory"
require_real_dir "$repos" "config directory"
# Image defaults are copied only for the explicit seed-config command.
# A restart must not put deleted agents, rules, or repos back.
if [ "${1:-}" = "seed-config" ]; then
	copy_missing "$defaults/agents.d" "$agents"
	copy_missing "$defaults/rules.d" "$rules"
	copy_missing "$defaults/repos.d" "$repos"
	exit 0
fi

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
		# Absent is a local-only start. A symlink or other non-file is not.
		if [ -L "$file" ] || { [ -e "$file" ] && [ ! -f "$file" ]; }; then
			echo "genesis: secret file is not a regular file: $name" >&2
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

own_tree "$config" 0755 0644
if data_owned; then
	# Top-level modes only. The stamp says the tree walk already finished.
	if [ "$(id -u)" = 0 ]; then
		chown genesis:genesis "$data" "$data/runs"
	fi
	chmod 0711 "$data"
	chmod 0700 "$data/runs"
	if [ -d "$data/transcripts" ] && [ ! -L "$data/transcripts" ]; then
		chmod 0755 "$data/transcripts"
	fi
	if [ -d "$data/sessions" ] && [ ! -L "$data/sessions" ]; then
		chmod 0711 "$data/sessions"
	fi
else
	own_tree "$data" 0700 0600
	# own_tree locks the whole data tree. Re-open traversal for transcripts only:
	# the data root is execute-only (0711), runs stays 0700, transcript files are
	# 0644. open_transcripts chowns transcripts to the listener on every start,
	# including a directory this mkdir just created and a root-owned one reused
	# from the volume.
	open_transcripts "$data"
	open_sessions "$data"
	touch "$data/.genesis-owned"
	if [ "$(id -u)" = 0 ]; then
		chown genesis:genesis "$data/.genesis-owned"
	fi
	chmod 0600 "$data/.genesis-owned"
fi
lock_providers
lock_credentials
lock_secrets

if [ "${1:-}" = "own-config" ]; then
	exit 0
fi

exec /usr/local/bin/genesis "$@"
