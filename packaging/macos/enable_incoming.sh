#!/usr/bin/env bash
# Enable incoming connections / local network access for the InferMesh .app
# bundles built by build_app.sh.
#
# On macOS Sequoia+ with the Application Firewall enabled, unsigned CLI
# binaries are silently blocked for inbound LAN connections. The .app bundles
# have a real identity and trigger the one-time "Allow incoming connections?"
# GUI prompt — no admin required. This script automates the socketfilterfw
# step when sudo is available, and prints GUI instructions otherwise.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
DIST_DIR="$REPO_ROOT/dist"

WORKER_EXE="$DIST_DIR/InferMesh Worker.app/Contents/MacOS/infermesh-worker"
ROUTER_EXE="$DIST_DIR/InferMesh Router.app/Contents/MacOS/infermesh-router"

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
    for exe in "$WORKER_EXE" "$ROUTER_EXE"; do
        sudo /usr/libexec/ApplicationFirewall/socketfilterfw --add "$exe"
        sudo /usr/libexec/ApplicationFirewall/socketfilterfw --unblockapp "$exe"
        echo "Allowed incoming connections for: $exe"
    done
    echo ""
    echo "Done. Next steps:"
    echo "    System Settings → Privacy & Security → Local Network"
    echo "    Ensure both 'InferMesh Router' and 'InferMesh Worker' are toggled ON."
    echo "    System Settings → Network → Firewall → Options (if visible): confirm both apps are set to \"Allow incoming connections\""
else
    echo "No passwordless sudo available — using the GUI workflow:"
    echo ""
    echo "1. Launch the worker bundle once to trigger the"
    echo "   'Allow incoming connections for InferMesh Worker?' prompt:"
    echo "       ./dist/InferMesh\\ Worker.app/Contents/MacOS/infermesh-worker \\"
    echo "           --dev-mode --backend custom --enable-health-checks=false"
    echo "   Click 'Allow'."
    echo ""
    echo "2. Launch the router bundle once:"
    echo "       ./dist/InferMesh\\ Router.app/Contents/MacOS/infermesh-router --dev-mode"
    echo "   Click 'Allow' when prompted."
    echo ""
    echo "3. Open System Settings → Privacy & Security → Local Network and"
    echo "   ensure both 'InferMesh Router' and 'InferMesh Worker' are toggled ON."
fi

echo ""
echo "Note: the ad-hoc signature changes on every 'make app' rebuild, so the"
echo "firewall prompt may re-fire after a rebuild. A self-signed Keychain cert"
echo "with 'codesign --sign \"<cert-name>\"' is the stable alternative — see"
echo "packaging/macos/ for the scripts and template (documented only, not implemented)."