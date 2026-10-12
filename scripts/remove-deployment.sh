#!/bin/sh
# Retires one deployment from this Mac: the controller stops serving it, then
# its runner scale sets are deleted from GitHub. Other deployments keep
# running. Usage: ./scripts/remove-deployment.sh <name>
set -eu

[ $# -eq 1 ] || { echo "usage: $0 <deployment-name>" >&2; exit 2; }
name=$1
repo=$(cd "$(dirname "$0")/.." && pwd)
bin="$HOME/.local/bin/ephemeral-runners"
dir="$HOME/.config/ephemeral-runners"
file="$dir/deployments/$name.toml"

[ -f "$file" ] || { echo "No deployment named $name in $dir/deployments" >&2; exit 1; }

# Take it out first, so nothing is listening on the scale sets when they go.
mkdir -p "$dir/removed"
mv "$file" "$dir/removed/$name.toml"
if ls "$dir"/deployments/*.toml >/dev/null 2>&1; then
	"$repo/scripts/install.sh"
else
	"$repo/scripts/uninstall.sh"
fi

"$bin" remove-scale-sets "$dir/removed/$name.toml"

echo "Removed $name. Its config is kept in $dir/removed/."
echo "Delete its key ($(sed -n 's/^private_key_path *= *"\(.*\)"/\1/p' "$dir/removed/$name.toml")) here and on the GitHub App's settings page."
