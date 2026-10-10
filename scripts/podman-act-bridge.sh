#!/usr/bin/env bash
# Bridge the podman machine's API socket to the host so `act` can reach it.
#
# podman's Docker API rejects archive copies into paths that escape their
# parent via a symlink (Ubuntu ships /var/run -> /run), which breaks act's
# `docker cp` for the action cache. Two workarounds, both applied:
#   1. This script forwards the VM's /run/podman/podman.sock to
#      127.0.0.1:2376 via SSH local forwarding.
#   2. The runner image (act-ci:fixed, see Dockerfile.actci) materialises
#      /var/run as a real directory instead of a symlink.
#
# Usage:
#   ./scripts/podman-act-bridge.sh      # foreground; Ctrl-C to stop
#   DOCKER_HOST=tcp://127.0.0.1:2376 act -j build
set -euo pipefail

MACHINE_KEY="$HOME/.local/share/containers/podman/machine/machine"
# The SSH port is the podman machine's current forward port; re-read it
# each run so a restarted machine does not break the bridge.
SSH_PORT="$(podman system connection ls 2>/dev/null \
  | awk 'NR==2 {print $2}' \
  | sed -E 's/.*@127\.0\.0\.1:([0-9]+)\/.*/\1/')"

exec ssh -i "$MACHINE_KEY" \
  -p "$SSH_PORT" \
  -o StrictHostKeyChecking=no \
  -o UserKnownHostsFile=/dev/null \
  -o ServerAliveInterval=30 \
  -o ExitOnForwardFailure=yes \
  -N -L 2376:127.0.0.1:2375 root@127.0.0.1