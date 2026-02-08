#!/usr/bin/env bash
#
# E2E test for goet SSH integration.
#
# Tests the full SSH bootstrapping flow:
# 1. `goet localhost` spawns SSH, launches session, connects via QUIC
# 2. Client sends a command, receives output
# 3. Client disconnects cleanly
#
# Prerequisites:
#   - SSH key auth to localhost (no password prompt)
#   - goet binary in PATH after build
#   - Go toolchain at /usr/local/go/bin/go
#
# Exit codes:
#   0  = passed
#   1  = failed
#   77 = skipped (no SSH key auth to localhost)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
GOET_BIN="$PROJECT_DIR/goet"
TMPDIR=$(mktemp -d)

cleanup() {
    jobs -p 2>/dev/null | xargs -r kill 2>/dev/null || true
    rm -rf "$TMPDIR"
    if [ "${NEED_CLEANUP_BIN:-false}" = true ]; then
        rm -f "$HOME/.local/bin/goet"
    fi
}
trap cleanup EXIT

# wait_for TRIES DELAY COMMAND... — poll until command succeeds or tries exhausted
wait_for() {
    local tries=$1 delay=$2; shift 2
    for _ in $(seq 1 "$tries"); do
        if "$@" 2>/dev/null; then return 0; fi
        sleep "$delay"
    done
    return 1
}

echo "=== goet SSH E2E test ==="

# --- Prerequisite: SSH key auth to localhost ---
echo ""
echo "--- Checking SSH key auth to localhost ---"

if ! ssh -o BatchMode=yes -o ConnectTimeout=5 localhost true 2>/dev/null; then
    echo "SKIP: SSH key auth to localhost not configured"
    exit 77
fi
echo "SSH key auth to localhost: OK"

# --- Build ---
echo ""
echo "--- Building goet ---"
/usr/local/go/bin/go build -o "$GOET_BIN" "$PROJECT_DIR/cmd/goet/"

# goet must be in PATH on the remote (localhost) for `ssh localhost goet ...` to work.
# SSH starts a login shell with its own PATH, so we symlink into ~/.local/bin/
# which is typically in the default PATH.
mkdir -p "$HOME/.local/bin"
INSTALLED_BIN="$HOME/.local/bin/goet"
NEED_CLEANUP_BIN=false
if [ ! -e "$INSTALLED_BIN" ]; then
    ln -sf "$GOET_BIN" "$INSTALLED_BIN"
    NEED_CLEANUP_BIN=true
fi

# --- Test 1: SSH connect, send command, read output ---
echo ""
echo "--- Test 1: SSH flow — connect and I/O ---"

CLIENT_FIFO="$TMPDIR/client_stdin.fifo"
mkfifo "$CLIENT_FIFO"
CLIENT_LOG="$TMPDIR/client.log"

# Start client via SSH mode (no --local, just hostname)
"$GOET_BIN" localhost < "$CLIENT_FIFO" > "$TMPDIR/client_out.txt" 2>"$CLIENT_LOG" &
CLIENT_PID=$!

# Hold stdin open
exec 10> "$CLIENT_FIFO"

# Wait for client to connect (SSH bootstrap + QUIC connect)
sleep 2

if ! kill -0 "$CLIENT_PID" 2>/dev/null; then
    echo "FAIL: client exited prematurely"
    echo "Client log:"
    cat "$CLIENT_LOG" || true
    exit 1
fi

# Send a command
MARKER="SSH_E2E_MARKER_$(date +%s)"
echo "echo $MARKER" >&10

# Wait for marker in output
if ! wait_for 50 0.1 grep -q "$MARKER" "$TMPDIR/client_out.txt"; then
    echo "FAIL: marker not found in client output"
    echo "Client output:"
    cat "$TMPDIR/client_out.txt" || true
    echo "Client log:"
    cat "$CLIENT_LOG" || true
    exit 1
fi

echo "PASS: SSH flow — connected and received shell output"

# --- Test 2: Clean disconnect ---
echo ""
echo "--- Test 2: Clean disconnect ---"

# Close stdin — triggers exitStdinEOF → client sends Shutdown → exits
exec 10>&-

# Wait for client to exit
for _ in $(seq 1 30); do
    if ! kill -0 "$CLIENT_PID" 2>/dev/null; then break; fi
    sleep 0.1
done

if kill -0 "$CLIENT_PID" 2>/dev/null; then
    echo "FAIL: client did not exit after stdin close"
    kill -9 "$CLIENT_PID" 2>/dev/null || true
    exit 1
fi

wait "$CLIENT_PID" 2>/dev/null || true

echo "PASS: client exited cleanly"

echo ""
echo "=== All SSH E2E tests passed ==="
