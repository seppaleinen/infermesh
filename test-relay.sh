#!/usr/bin/env bash
# Automated end-to-end test for the relay architecture.
# Runs: relay → router (--relay-url) → worker (--relay-url) → inference request
# All three components connect OUTBOUND to the relay — no inbound firewall issues.

set -euo pipefail

RELAY_ADDR=":8090"
ROUTER_ADDR=":8080"
RELAY_URL="ws://127.0.0.1:8090"
BIN="./bin"

# Build binaries
echo "=== Building binaries ==="
make build

# Kill any leftover processes
cleanup() {
    echo "=== Cleaning up ==="
    pkill -f "infermesh-relay" 2>/dev/null || true
    pkill -f "infermesh-router" 2>/dev/null || true
    pkill -f "infermesh-worker" 2>/dev/null || true
}
trap cleanup EXIT INT TERM
cleanup

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
echo "=== Starting worker on $WORKER_ADDR with relay $RELAY_URL ==="
$BIN/infermesh-worker --dev-mode --router $RELAY_URL --backend llama-cpp > /tmp/worker.log 2>&1 &
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

# Wait a bit more for capability sync
sleep 2

# Send a test inference request to the router
echo "=== Sending test request to router ==="
RESPONSE=$(curl -s -X POST http://127.0.0.1:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "llama-3-8b", "messages": [{"role": "user", "content": "Hello via relay"}], "max_tokens": 16, "stream": false}')

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