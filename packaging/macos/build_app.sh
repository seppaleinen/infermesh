#!/usr/bin/env bash
# Build macOS .app bundles for the InferMesh router and worker binaries.
#
# The .app bundle gives the CLI binaries a real bundle identity so macOS'
# Application Firewall / Local Network prompts can identify them and the
# one-time "Allow incoming connections?" prompt is triggered — no admin
# required. This is a workaround for unsigned CLI binaries being silently
# blocked for inbound LAN connections on macOS Sequoia+.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
VERSION="0.1.0"
TEMPLATE="$SCRIPT_DIR/Info.plist.template"
DIST_DIR="$REPO_ROOT/dist"

# Ensure binaries exist before bundling.
if [ ! -f "$REPO_ROOT/bin/infermesh-router" ] || [ ! -f "$REPO_ROOT/bin/infermesh-worker" ]; then
    echo "Missing binaries — running 'make build' first..."
    make -C "$REPO_ROOT" build
fi

APP_DEFS=(
    "InferMesh Router|com.infermesh.router|infermesh-router"
    "InferMesh Worker|com.infermesh.worker|infermesh-worker"
)

for app_def in "${APP_DEFS[@]}"; do
    IFS='|' read -r APP_NAME IDENTIFIER EXECUTABLE <<< "$app_def"

    APP_DIR="$DIST_DIR/$APP_NAME.app"
    MACOS_DIR="$APP_DIR/Contents/MacOS"
    mkdir -p "$MACOS_DIR"
    cp "$REPO_ROOT/bin/$EXECUTABLE" "$MACOS_DIR/$EXECUTABLE"
    chmod +x "$MACOS_DIR/$EXECUTABLE"

    sed -e "s/@CFBundleIdentifier@/$IDENTIFIER/g" \
        -e "s/@CFBundleName@/$APP_NAME/g" \
        -e "s/@CFBundleDisplayName@/$APP_NAME/g" \
        -e "s/@CFBundleExecutable@/$EXECUTABLE/g" \
        -e "s/@VERSION@/$VERSION/g" \
        "$TEMPLATE" > "$APP_DIR/Contents/Info.plist"

    codesign --force --sign - --identifier "$IDENTIFIER" "$APP_DIR"
    codesign --verify --deep --strict "$APP_DIR"

    echo "Built: $APP_DIR"
done

echo ""
echo "Done. Bundle paths:"
echo "    $DIST_DIR/InferMesh Router.app"
echo "    $DIST_DIR/InferMesh Worker.app"
echo ""
echo "Run the bundled worker:"
echo "    ./dist/InferMesh\\ Worker.app/Contents/MacOS/infermesh-worker --dev-mode --backend custom --router http://127.0.0.1:8080"
echo ""
echo "Enable firewall / local network access:"
echo "    bash packaging/macos/enable_incoming.sh"