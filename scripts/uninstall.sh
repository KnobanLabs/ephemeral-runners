#!/bin/sh
# Stops the LaunchAgent and removes it. Leaves the config, keys, binary and
# pulled images in place. --purge also deletes leftover VMs and their runner
# registrations. To stop serving just one deployment, use
# remove-deployment.sh instead.
set -eu

label=local.ephemeral-runners
bin="$HOME/.local/bin/ephemeral-runners"
domain="gui/$(id -u)"

launchctl bootout "$domain/$label" 2>/dev/null || true
while launchctl print "$domain/$label" >/dev/null 2>&1; do sleep 1; done
rm -f "$HOME/Library/LaunchAgents/$label.plist"
echo "Stopped and removed $label"

if [ "${1:-}" = "--purge" ]; then
	"$bin" cleanup
fi
