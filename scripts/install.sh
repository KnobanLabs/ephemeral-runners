#!/bin/sh
# Builds and installs ephemeral-runners as a LaunchAgent for the current user,
# then (re)starts it. One controller serves every installed deployment.
#
#   ./scripts/install.sh <deployment.toml> ...   add or update deployments, then restart
#   ./scripts/install.sh                         restart with what's installed
#
# A deployment file is named <name>.toml or <name>/config.toml. Keep them
# outside this repo; see "Where to keep deployment configs" in README.md.
#
# Deployments already installed and not named stay installed. Everything is
# checked together before anything is replaced. Safe to rerun.
set -eu

repo=$(cd "$(dirname "$0")/.." && pwd)
label=local.ephemeral-runners
bin="$HOME/.local/bin/ephemeral-runners"
dir="$HOME/.config/ephemeral-runners"
plist="$HOME/Library/LaunchAgents/$label.plist"
domain="gui/$(id -u)"

command -v tart >/dev/null || { echo "tart is not installed; see Setup in README.md" >&2; exit 1; }
command -v go >/dev/null || { echo "go is not installed: brew install go" >&2; exit 1; }

deployment_name() {
	name=$(basename "$1" .toml)
	if [ "$name" = config ]; then name=$(basename "$(dirname "$1")"); fi
	echo "$name"
}

echo "Building..."
mkdir -p "$(dirname "$bin")"
(cd "$repo" && go build -o "$bin" .)

# Stage the would-be config and check it before touching the real one.
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/deployments"
if [ -f "$dir/host.toml" ]; then cp "$dir/host.toml" "$stage/"; fi
for f in "$dir"/deployments/*.toml; do
	if [ -f "$f" ]; then cp "$f" "$stage/deployments/"; fi
done
for f in "$@"; do
	[ -f "$f" ] || { echo "No such file: $f" >&2; exit 1; }
	cp "$f" "$stage/deployments/$(deployment_name "$f").toml"
done

echo "Checking config, credentials, images and budget..."
"$bin" -config-dir "$stage" check

mkdir -p "$dir/deployments" "$dir/keys"
chmod 700 "$dir" "$dir/keys"
for f in "$@"; do
	dest="$dir/deployments/$(deployment_name "$f").toml"
	cp "$f" "$dest"
	chmod 600 "$dest"
	echo "Installed $dest"
done

mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
sed -e "s|__HOME__|$HOME|g" -e "s|__LABEL__|$label|g" "$repo/launchd/ephemeral-runners.plist" > "$plist"

# bootout can return before the old process has drained and exited, and
# bootstrap fails (error 5) until it has.
launchctl bootout "$domain/$label" 2>/dev/null || true
while launchctl print "$domain/$label" >/dev/null 2>&1; do sleep 1; done
launchctl bootstrap "$domain" "$plist"
echo "Started. Logs: tail -f ~/Library/Logs/ephemeral-runners.log"
