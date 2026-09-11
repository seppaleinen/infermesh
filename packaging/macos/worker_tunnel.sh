#!/usr/bin/env bash
# worker_tunnel.sh — Run InferMesh worker on macOS behind the Application Firewall.
#
# WHY: On macOS Sequoia+ (26.6.1 confirmed), the Application Firewall silently
# blocks inbound LAN connections to unsigned / ad-hoc-signed binaries. There is
# NO "Allow incoming connections?" prompt for Developer-ID-absent apps — the
# block is permanent and un-surfaced. The only no-admin workaround is an SSH
# reverse tunnel that exposes the worker's loopback listener on the router host's
# loopback, so the router dials 127.0.0.1 and never touches the LAN.
#
# TOPOLOGY (both directions via SSH):
#   GPU node (router host)          this machine (macOS, firewall-blocked)
#     sshd listener 127.0.0.1:PORT       worker :PORT
#     ssh -R PORT:127.0.0.1:PORT ◀────────────
#     router dials 127.0.0.1:PORT           |
#     ssh -L  ROUTER_PORT:127.0.0.1:ROUTER_PORT ────▶ worker POSTs to
#                                                       127.0.0.1:ROUTER_PORT
#
# The -L tunnel makes the worker's registration POST appear to originate from
# the router host's loopback, so even older router code that derives the dial
# IP from r.RemoteAddr will use 127.0.0.1.  The -R tunnel handles the
# callback.  Both together make the full round-trip firewall-safe.
#
# REQUIREMENTS on the router host:
#   - sshd running with AllowTcpForwarding yes (default on most distros)
#   - SSH key access for $SSH_USER (agent forwarding or key in ~/.ssh/)
#
# Usage:
#   ./packaging/macos/worker_tunnel.sh \
#     --router-host 192.168.1.216 \
#     --backend lmstudio \
#     --model-path /path/to/model
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Defaults
# ---------------------------------------------------------------------------
ROUTER_HOST=""
ROUTER_PORT=8080
PORT=8082
BACKEND="lmstudio"
MODEL_PATH=""
SSH_USER="${USER}"

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------
while [ $# -gt 0 ]; do
    case "$1" in
        --router-host)  ROUTER_HOST="$2";  shift 2 ;;
        --router-port)  ROUTER_PORT="$2";  shift 2 ;;
        --port)         PORT="$2";         shift 2 ;;
        --backend)      BACKEND="$2";      shift 2 ;;
        --model-path)   MODEL_PATH="$2";   shift 2 ;;
        --ssh-user)     SSH_USER="$2";     shift 2 ;;
        -h|--help)
            echo "Usage: $0 --router-host <host> [OPTIONS]"
            echo ""
            echo "Required:"
            echo "  --router-host HOST   IP or hostname of the machine running the router"
            echo ""
            echo "Optional:"
            echo "  --router-port PORT   Router listen port (default: 8080)"
            echo "  --port PORT          Worker listen port, tunneled to router host (default: 8082)"
            echo "  --backend BACKEND    Inference backend: llama-cpp, ollama, lmstudio, vllm (default: lmstudio)"
            echo "  --model-path PATH    Path to model file (optional, omit for auto-detect backends)"
            echo "  --ssh-user USER      SSH username for the router host (default: \$USER)"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            echo "Run with --help for usage."
            exit 1
            ;;
    esac
done

if [ -z "$ROUTER_HOST" ]; then
    echo "ERROR: --router-host is required."
    echo "Run with --help for usage."
    exit 1
fi

# ---------------------------------------------------------------------------
# Resolve worker binary
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
WORKER_BIN="$REPO_ROOT/bin/infermesh-worker"

if [ ! -f "$WORKER_BIN" ]; then
    echo "ERROR: $WORKER_BIN not found."
    echo "Build first: make build"
    exit 1
fi

# ---------------------------------------------------------------------------
# Print instructions
# ---------------------------------------------------------------------------
echo "=== InferMesh Worker via SSH Reverse Tunnel ==="
echo ""
echo "This script bypasses the macOS Application Firewall by tunneling the"
echo "worker's loopback port to the router host via SSH.  The router will"
echo "dial 127.0.0.1:$PORT on the router host — no LAN traffic."
echo ""
echo "Topology:"
echo "  router host ($ROUTER_HOST)       this machine"
echo "    sshd :$PORT <─────────────────  worker :$PORT"
echo "    router dials 127.0.0.1:$PORT"
echo ""
echo "Worker:   $WORKER_BIN --dev-mode --port $PORT --backend $BACKEND"
echo "Tunnel:   ssh -N -R 127.0.0.1:$PORT:127.0.0.1:$PORT -L 127.0.0.1:$ROUTER_PORT:127.0.0.1:$ROUTER_PORT $SSH_USER@$ROUTER_HOST"
echo "Router:   http://127.0.0.1:$ROUTER_PORT (via tunnel)"
echo ""

# ---------------------------------------------------------------------------
# Start SSH reverse tunnel in background
# ---------------------------------------------------------------------------
echo "Starting SSH reverse tunnel..."
#
# Two forwards, one SSH connection:
#   -R PORT:127.0.0.1:PORT                 router-host:PORT -> this machine:PORT
#                                         (worker callback; router dials 127.0.0.1)
#   -L 127.0.0.1:ROUTER_PORT:127.0.0.1:ROUTER_PORT
#                                         this machine:ROUTER_PORT -> router-host:ROUTER_PORT
#                                         (worker registers via loopback so the router
#                                          sees a loopback peer and dials the tunnel,
#                                          regardless of the router code's IP logic)
ssh -N -o ExitOnForwardFailure=yes \
    -R 127.0.0.1:"$PORT":127.0.0.1:"$PORT" \
    -L 127.0.0.1:"$ROUTER_PORT":127.0.0.1:"$ROUTER_PORT" \
    "$SSH_USER@$ROUTER_HOST" &
SSH_PID=$!

# Ensure the SSH tunnel is killed on exit (ctrl-C or normal termination)
cleanup() {
    echo ""
    echo "Shutting down SSH tunnel (PID $SSH_PID)..."
    kill "$SSH_PID" 2>/dev/null || true
    wait "$SSH_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# Give SSH a moment to establish; check it's still running
sleep 1
if ! kill -0 "$SSH_PID" 2>/dev/null; then
    echo "ERROR: SSH tunnel failed to start."
    echo "Check that sshd is running on $ROUTER_HOST with AllowTcpForwarding enabled,"
    echo "that port $PORT is free on $ROUTER_HOST, that local port $ROUTER_PORT is free here,"
    echo "and that SSH key access works: ssh $SSH_USER@$ROUTER_HOST echo ok"
    echo "For diagnosis, run the tunnel manually with -v:"
    echo "  ssh -v -N -R 127.0.0.1:$PORT:127.0.0.1:$PORT -L 127.0.0.1:$ROUTER_PORT:127.0.0.1:$ROUTER_PORT $SSH_USER@$ROUTER_HOST"
    exit 1
fi

echo "SSH tunnel established (PID $SSH_PID)."
echo "Starting worker (ctrl-C to stop both)..."
echo ""

# ---------------------------------------------------------------------------
# Build worker command
# ---------------------------------------------------------------------------
CMD=("$WORKER_BIN" --dev-mode --port "$PORT" --backend "$BACKEND")
if [ -n "$MODEL_PATH" ]; then
    CMD+=(--model-path "$MODEL_PATH")
fi
# Register via the local -L forward so the router sees a loopback peer and
# dials back through the -R tunnel.  This works with both the legacy router
# (derives dial IP from RemoteAddr) and the loopback-only router contract.
CMD+=(--router "http://127.0.0.1:$ROUTER_PORT")

# Run the worker in the background and wait on it so the EXIT trap above stays
# alive and kills the SSH tunnel no matter how the worker exits (ctrl-C,
# crash, backend failure). The terminal SIGINTs the whole foreground process
# group, so ctrl-C reaches both the worker and the trap.
"${CMD[@]}" &
WORKER_PID=$!
wait "$WORKER_PID"
