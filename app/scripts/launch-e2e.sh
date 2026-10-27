#!/usr/bin/env bash
#
# launch-e2e.sh — Playwright webServer launcher for InferMesh E2E tests (#181)
#
# Replaces the inline shell chain in app/frontend/playwright.config.ts with a
# single script that:
#   1. Writes temp settings.yml from env vars MOCK_ROUTER_PORT and WAILS_SERVER_PORT
#   2. Starts app/mock/mockRouter.js on MOCK_ROUTER_PORT and waits for TCP ready
#   3. Pre-builds the Wails server binary once (go build -tags server,production) and execs binary not go run
#   4. Waits for HTTP 200 on http://127.0.0.1:$WAILS_SERVER_PORT/ before returning
#   5. Cleans up mock router on exit
#
# Env (all overridable, sensible defaults):
#   MOCK_ROUTER_PORT          Port for app/mock/mockRouter.js      (default 8081)
#   WAILS_SERVER_PORT         Port for the Wails server build      (default 8080)
#   PLAYWRIGHT_TEMP_HOME      Config dir for settings.yml + binary  (default /tmp/infermesh-test-home)
#   INFERMESH_SERVER_BIN      Pre-built server binary path         (default $PLAYWRIGHT_TEMP_HOME/infermesh-server)
#   GO_BUILD_TAGS             Go build tags for server binary      (default server,production)
#   HEALTH_TIMEOUT            Seconds to wait for health probes   (default 60)
#
# No arbitrary sleeps: every wait is a real health probe (TCP connect for the mock,
# HTTP GET for the server). Ports are configurable via env vars so parallel CI runs
# and local development never collide (see #181).

set -euo pipefail

# ── env & defaults ───────────────────────────────────────────────────────────
MOCK_ROUTER_PORT="${MOCK_ROUTER_PORT:-8081}"
WAILS_SERVER_PORT="${WAILS_SERVER_PORT:-8080}"
PLAYWRIGHT_TEMP_HOME="${PLAYWRIGHT_TEMP_HOME:-${HOME:-/tmp/infermesh-test-home}}"
INFERMESH_SERVER_BIN="${INFERMESH_SERVER_BIN:-$PLAYWRIGHT_TEMP_HOME/infermesh-server}"
GO_BUILD_TAGS="${GO_BUILD_TAGS:-server,production}"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-60}"

# Derive app dir from cwd (Playwright runs this with cwd = app/)
APP_DIR="$(pwd)"
MOCK_SCRIPT="$APP_DIR/mock/mockRouter.js"
CONFIG_DIR="$PLAYWRIGHT_TEMP_HOME/.config/infermesh"
SETTINGS_FILE="$CONFIG_DIR/settings.yml"

log() { printf '[launch-e2e] %s\n' "$*" >&2; }

# ── helpers ──────────────────────────────────────────────────────────────────

# wait_tcp HOST PORT — poll until TCP connect succeeds or timeout
wait_tcp() {
  local host=$1 port=$2 timeout=$HEALTH_TIMEOUT
  local start=$(date +%s)
  log "waiting for TCP $host:$port ..."
  while true; do
    if (exec 3<>"/dev/tcp/${host}/${port}") 2>/dev/null; then
      exec 3>&- 3<&- 2>/dev/null || true
      log "TCP $host:$port ready"
      return 0
    fi
    local now=$(date +%s)
    if (( now - start >= timeout )); then
      log "TIMEOUT waiting for TCP $host:$port after ${timeout}s" >&2
      return 1
    fi
    sleep 0.25
  done
}

# wait_http URL — poll until HTTP GET returns 2xx or timeout
wait_http() {
  local url=$1 timeout=$HEALTH_TIMEOUT
  local start=$(date +%s)
  log "waiting for HTTP 200 on $url ..."
  while true; do
    if curl -fsS -o /dev/null "$url"; then
      log "HTTP 200 OK on $url"
      return 0
    fi
    local now=$(date +%s)
    if (( now - start >= timeout )); then
      log "TIMEOUT waiting for HTTP 200 on $url after ${timeout}s" >&2
      return 1
    fi
    sleep 0.25
  done
}

# build_if_needed — build binary if INFERMESH_SERVER_BIN missing or sources newer
build_if_needed() {
  if [ -x "$INFERMESH_SERVER_BIN" ]; then
    local bin_mtime src_files
    bin_mtime=$(stat -f %m "$INFERMESH_SERVER_BIN" 2>/dev/null || stat -c %Y "$INFERMESH_SERVER_BIN")
    src_files=$(find "$APP_DIR" \( -name '*.go' -o -path 'frontend/dist/*' \) -type f -print0 2>/dev/null | tr '\0' '\n')
    if [ -n "$src_files" ]; then
      local newest
      newest=$(printf '%s\n' "$src_files" | xargs -I{} stat -f %m {} 2>/dev/null || xargs stat -c %Y {} 2>/dev/null | sort -n | tail -1)
      if [ -n "$newest" ] && [ "$bin_mtime" -ge "$newest" ] 2>/dev/null; then
        log "server binary fresh, skipping build"
        return 0
      fi
    fi
  fi
  log "building server binary ($GO_BUILD_TAGS) -> $INFERMESH_SERVER_BIN"
  mkdir -p "$(dirname "$INFERMESH_SERVER_BIN")"
  (cd "$APP_DIR" && go build -tags "$GO_BUILD_TAGS" -o "$INFERMESH_SERVER_BIN" .)
  log "server binary built"
}

# ── 1. write settings.yml ────────────────────────────────────────────────────
mkdir -p "$CONFIG_DIR"
cat > "$SETTINGS_FILE" <<EOF
router_addr: http://127.0.0.1:${MOCK_ROUTER_PORT}
EOF
log "wrote $SETTINGS_FILE (router_addr -> http://127.0.0.1:$MOCK_ROUTER_PORT)"

# ── 2. start mock router + wait for TCP ready ───────────────────────────────
node "$MOCK_SCRIPT" &
MOCK_PID=$!
log "mock router started (pid $MOCK_PID) on 127.0.0.1:$MOCK_ROUTER_PORT"

# Cleanup mock on exit
cleanup() {
  log "shutting down ..."
  [ -n "${MOCK_PID:-}" ] && kill "$MOCK_PID" 2>/dev/null || true
  sleep 0.1
}
trap cleanup EXIT INT TERM

# Ensure mock didn't exit immediately (port collision)
if ! kill -0 "$MOCK_PID" 2>/dev/null; then
  log "mock router exited immediately — port collision?" >&2
  exit 1
fi

wait_tcp 127.0.0.1 "$MOCK_ROUTER_PORT"

# ── 3. build server binary once (pre-built, not go run) ──────────────────────
build_if_needed

# ── 4. start server + wait for HTTP 200 ──────────────────────────────────────
# HOME must be PLAYWRIGHT_TEMP_HOME so the server reads the temp settings.yml
HOME="$PLAYWRIGHT_TEMP_HOME" "$INFERMESH_SERVER_BIN" &
SERVER_PID=$!
log "server started (pid $SERVER_PID) on 127.0.0.1:$WAILS_SERVER_PORT"

# Keep SERVER_PID in cleanup
cleanup() {
  log "shutting down ..."
  [ -n "${SERVER_PID:-}" ] && kill "$SERVER_PID" 2>/dev/null || true
  [ -n "${MOCK_PID:-}" ] && kill "$MOCK_PID" 2>/dev/null || true
  sleep 0.1
}
trap cleanup EXIT INT TERM

wait_http "http://127.0.0.1:${WAILS_SERVER_PORT}/"

# ── 5. block until Playwright kills us ───────────────────────────────────────
log "webServer ready — waiting for Playwright"
wait "$SERVER_PID"