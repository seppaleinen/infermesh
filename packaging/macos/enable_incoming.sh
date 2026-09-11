#!/usr/bin/env bash
# Enable incoming connections / local network access for the InferMesh .app
# bundles built by build_app.sh.
#
# IMPORTANT: On macOS Sequoia+ (26.6.1 confirmed), the Application Firewall
# silently blocks inbound LAN connections to unsigned / ad-hoc-signed binaries.
# There is NO "Allow incoming connections?" prompt for apps that lack a
# Developer-ID certificate.  Launching the app with `open` or running the
# bare binary does NOT surface a prompt — the block is permanent and silent.
#
# This script provides two paths:
#
#   1. SUDO AVAILABLE (admin):
#      Use socketfilterfw to explicitly allow the inner Mach-O binary.
#      This DOES require root and does NOT need a prompt.
#
#   2. NO SUDO (non-admin / no passwordless sudo):
#      On Sequoia+ there is no prompt-based workaround.
#      Use the SSH reverse tunnel instead — see packaging/macos/worker_tunnel.sh.
#
# On macOS versions before Sequoia (Ventura, Monterey, etc.), launching an
# unsigned binary DID trigger a one-time Allow prompt — that path still
# works on those older versions.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
DIST_DIR="$REPO_ROOT/dist"

WORKER_APP="$DIST_DIR/InferMesh Worker.app"
ROUTER_APP="$DIST_DIR/InferMesh Router.app"

# The inner Mach-O binary is what socketfilterfw operates on.
WORKER_EXE="$WORKER_APP/Contents/MacOS/infermesh-worker"
ROUTER_EXE="$ROUTER_APP/Contents/MacOS/infermesh-router"

for exe in "$WORKER_EXE" "$ROUTER_EXE"; do
    if [ ! -f "$exe" ]; then
        echo "ERROR: $exe not found."
        echo "Build the app bundles first: make app"
        exit 1
    fi
done

HAS_SUDO=false
if sudo -n true 2>/dev/null; then
    HAS_SUDO=true
fi

if [ "$HAS_SUDO" = true ]; then
    echo "Passwordless sudo available — configuring socketfilterfw."
    echo ""
    echo "Note: this adds and unblocks the inner Mach-O binary directly."
    echo "This is the only no-prompt way we have verified on Sequoia+;"
    echo "it requires root. No GUI prompt will appear."
    echo ""

    for exe in "$WORKER_EXE" "$ROUTER_EXE"; do
        sudo /usr/libexec/ApplicationFirewall/socketfilterfw --add "$exe"
        sudo /usr/libexec/ApplicationFirewall/socketfilterfw --unblockapp "$exe"
        echo "Allowed incoming connections for: $exe"
    done

    echo ""
    echo "Done. Next steps:"
    echo "    1. System Settings -> Privacy & Security -> Local Network"
    echo "       Ensure both 'InferMesh Router' and 'InferMesh Worker' are toggled ON."
    echo "    2. System Settings -> Network -> Firewall -> Options (if visible):"
    echo "       Confirm both apps are set to 'Allow incoming connections'."
    echo ""
    echo "If the app bundle was built with 'make app', note that the ad-hoc"
    echo "signature changes on every rebuild.  Re-run this script after each"
    echo "'make app' to re-allow the updated binary."
else
    echo "No passwordless sudo available."
    echo ""
    echo "On macOS Sequoia+, there is NO Allow prompt for unsigned or"
    echo "ad-hoc-signed binaries.  Launching the app with 'open' or running"
    echo "the binary directly does NOT trigger a prompt — the firewall"
    echo "silently blocks all inbound connections."
    echo ""
    echo "The only no-admin workaround is an SSH reverse tunnel, which"
    echo "exposes the worker on the router host's loopback (127.0.0.1)"
    echo "so the firewall is never consulted."
    echo ""
    echo "Use the tunnel script:"
    echo ""
    echo "    ./packaging/macos/worker_tunnel.sh \\"
    echo "        --router-host <ROUTER_HOST_IP> \\"
    echo "        --backend lmstudio \\"
    echo "        --model-path /path/to/model"
    echo ""
    echo "Requirements on the router host:"
    echo "    - sshd running with AllowTcpForwarding yes (default on most distros)"
    echo "    - SSH key access for your user (ssh $USER@<host> echo ok)"
    echo ""
    echo "For older macOS versions (before Sequoia), the Allow prompt DID work."
    echo "On those systems, launch each app bundle once and click Allow when prompted."
fi
