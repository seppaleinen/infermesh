#!/usr/bin/env bash
# Automated end-to-end test for the relay architecture.
# Runs: relay → router (--relay-url) → worker (--relay-url) → mock backend → inference request
# All three components connect OUTBOUND to the relay — no inbound firewall issues.

set -euo pipefail

RELAY_ADDR=":8090"
ROUTER_ADDR=":8080"
RELAY_URL="ws://127.0.0.1:8090"
BIN="./bin"
MOCK_ADDR="127.0.0.1"
MOCK_PORT=18080
MOCK_SCRIPT="$(mktemp -t infermesh-mock)"

# Build binaries
echo "=== Building binaries ==="
make build

# Kill any leftover processes
cleanup() {
    echo "=== Cleaning up ==="
    pkill -f "infermesh-relay" 2>/dev/null || true
    pkill -f "infermesh-router" 2>/dev/null || true
    pkill -f "infermesh-worker" 2>/dev/null || true
    pkill -f "infermesh-mock" 2>/dev/null || true
    rm -f "$MOCK_SCRIPT"
    if [ -n "${MOCK_PID:-}" ]; then
        kill "$MOCK_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT INT TERM
cleanup

# Minimal OpenAI-compatible mock backend (python3 stdlib only). It serves a
# stable 200 on /v1/models for the whole run (the worker's health loop gates
# requests and would otherwise open its circuit breaker after 5 failures) and
# a canned chat completion on /v1/chat/completions.
cat > "$MOCK_SCRIPT" <<'PYEOF'
#!/usr/bin/env python3
"""Minimal OpenAI-compatible mock backend for the InferMesh relay e2e test."""
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

MODEL = "llama-3-8b"


class Handler(BaseHTTPRequestHandler):
    def _json(self, status, obj):
        body = json.dumps(obj).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/v1/models":
            self._json(200, {"data": [{"id": MODEL, "object": "model", "loaded": True}]})
        else:
            self._json(404, {"error": {"message": "not found: %s" % self.path}})

    def do_POST(self):
        if self.path == "/v1/chat/completions":
            length = int(self.headers.get("Content-Length", 0) or 0)
            if length:
                self.rfile.read(length)
            self._json(200, {
                "id": "mock-1",
                "object": "chat.completion",
                "created": int(time.time()),
                "model": MODEL,
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": "Hello from mock backend!"},
                        "finish_reason": "stop",
                    }
                ],
                "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
            })
        else:
            self._json(404, {"error": {"message": "not found: %s" % self.path}})

    def log_message(self, fmt, *args):
        pass


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 18080
    HTTPServer(("127.0.0.1", port), Handler).serve_forever()
PYEOF
chmod +x "$MOCK_SCRIPT"

# Start mock backend (if python3 is available; otherwise skip inference).
# Prefer an interpreter that can actually accept localhost connections:
# macOS Application Firewall can silently drop listeners from new binaries
# (e.g. a freshly installed Homebrew python), while the system /usr/bin/python3
# is pre-approved. Try candidates until one serves.
MOCK_AVAILABLE=0
MOCK_PID=""
CANDIDATES=()
if command -v python3 >/dev/null 2>&1; then
    CANDIDATES+=("$(command -v python3)")
fi
if [ -x /usr/bin/python3 ]; then
    CANDIDATES+=("/usr/bin/python3")
fi
if [ "${#CANDIDATES[@]}" -gt 0 ]; then
    for py in "${CANDIDATES[@]}"; do
        [ -x "$py" ] || continue
        echo "=== Starting mock backend on $MOCK_ADDR:$MOCK_PORT (interpreter: $py) ==="
        "$py" "$MOCK_SCRIPT" "$MOCK_PORT" > /tmp/mock_backend.log 2>&1 &
        MOCK_PID=$!
        sleep 1
        if curl -fsS -m 3 "http://$MOCK_ADDR:$MOCK_PORT/v1/models" >/dev/null 2>&1; then
            MOCK_AVAILABLE=1
            echo "Mock backend PID: $MOCK_PID"
            break
        fi
        echo "  mock backend did not accept connections with $py; trying next"
        kill "$MOCK_PID" 2>/dev/null || true
        MOCK_PID=""
    done
    if [ "$MOCK_AVAILABLE" != "1" ]; then
        echo "python3 mock backend unavailable (none of the python3 interpreters accepted localhost connections); inference verification will be SKIPPED"
    fi
else
    echo "python3 not found; inference verification will be SKIPPED"
fi

# Start relay
echo "=== Starting relay on $RELAY_ADDR ==="
$BIN/infermesh-relay --listen $RELAY_ADDR --dev-mode > /tmp/relay.log 2>&1 &
RELAY_PID=$!
sleep 1

# Verify relay is listening
if ! nc -z 127.0.0.1 8090; then
    echo "ERROR: Relay failed to start"
    cat /tmp/relay.log
    exit 1
fi
echo "Relay PID: $RELAY_PID"

# Start router (dials relay outbound)
echo "=== Starting router on $ROUTER_ADDR with relay $RELAY_URL ==="
$BIN/infermesh-router --dev-mode --addr $ROUTER_ADDR --relay-url $RELAY_URL > /tmp/router.log 2>&1 &
ROUTER_PID=$!
sleep 2

# Start worker (dials relay outbound)
# Note: --relay-url overrides the router WS address; worker HTTP port is 8081 by default.
echo "=== Starting worker (HTTP :8081) with relay $RELAY_URL ==="
WORKER_ARGS=(--dev-mode --relay-url "$RELAY_URL" --backend llama-cpp)
if [ "$MOCK_AVAILABLE" = "1" ]; then
    WORKER_ARGS+=(--backend-url "http://$MOCK_ADDR:$MOCK_PORT")
fi
"$BIN/infermesh-worker" "${WORKER_ARGS[@]}" > /tmp/worker.log 2>&1 &
WORKER_PID=$!
sleep 4

# Verify registration succeeded (check router log for websocket worker registered)
if ! grep -q "websocket worker registered" /tmp/router.log; then
    echo "ERROR: Worker did not register via relay"
    echo "--- Router log ---"
    cat /tmp/router.log
    echo "--- Worker log ---"
    cat /tmp/worker.log
    exit 1
fi
echo "✓ Worker registered via relay"

if [ "$MOCK_AVAILABLE" != "1" ]; then
    echo ""
    echo "inference verification SKIPPED: python3 mock backend unavailable"
    echo "=== Registration check passed; relay architecture works end-to-end (inference unverified) ==="
    exit 0
fi

# Wait a bit more for capability sync
sleep 2

# Send a test inference request to the router
echo "=== Sending test request to router ==="
if ! RESPONSE=$(curl -fsS -m 15 -X POST http://127.0.0.1:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "llama-3-8b", "messages": [{"role": "user", "content": "Hello via relay"}], "max_tokens": 16, "stream": false}'); then
    echo "ERROR: Inference request failed (curl exited nonzero: HTTP error or timeout)"
    echo "--- Router log ---"
    cat /tmp/router.log
    echo "--- Worker log ---"
    cat /tmp/worker.log
    exit 1
fi

echo "Response: $RESPONSE"

# Check if response looks valid (has choices array)
if echo "$RESPONSE" | grep -q '"choices"'; then
    echo "✓ Inference request succeeded through relay"
else
    echo "ERROR: Inference failed"
    echo "--- Router log ---"
    cat /tmp/router.log
    echo "--- Worker log ---"
    cat /tmp/worker.log
    exit 1
fi

echo ""
echo "=== All checks passed! Relay architecture works end-to-end ==="