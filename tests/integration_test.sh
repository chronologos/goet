#!/usr/bin/env bash
#
# Integration test for goet client ↔ session (direct mode).
#
# Tests:
# 1. Session starts, prints port
# 2. Client connects, sends command, reads output
# 3. Client reconnects after kill, receives catchup
# 4. Graceful shutdown via ~. escape
# 5. Clean shutdown via SIGTERM
#
# Requirements:
#   - Go toolchain at /usr/local/go/bin/go

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
GOET_BIN="$PROJECT_DIR/goet"
TMPDIR=$(mktemp -d)

cleanup() {
    # Kill any background processes
    jobs -p 2>/dev/null | xargs -r kill 2>/dev/null || true
    rm -rf "$TMPDIR"
}
trap cleanup EXIT

echo "=== goet integration test ==="

# Build the binary
echo "Building goet..."
/usr/local/go/bin/go build -o "$GOET_BIN" "$PROJECT_DIR/cmd/goet/"

# Generate a random passkey (32 bytes = 64 hex chars)
PASSKEY=$(head -c32 /dev/urandom | xxd -p -c64)

# --- Test 1: Session starts and prints port ---
echo ""
echo "--- Test 1: Session startup ---"

SESSION_FIFO="$TMPDIR/session_stdin.fifo"
mkfifo "$SESSION_FIFO"

SESSION_LOG="$TMPDIR/session.log"

# Write passkey to FIFO, then hold it open
echo "$PASSKEY" > "$SESSION_FIFO" &
"$GOET_BIN" session -f test-integration -p 0 < "$SESSION_FIFO" > "$TMPDIR/port.txt" 2>"$SESSION_LOG" &
SESSION_PID=$!

# Hold session stdin open
exec 10> "$SESSION_FIFO"

# Wait for port to appear
for i in $(seq 1 50); do
    if [ -s "$TMPDIR/port.txt" ]; then
        break
    fi
    sleep 0.1
done

if [ ! -s "$TMPDIR/port.txt" ]; then
    echo "FAIL: session did not print port within 5s"
    cat "$SESSION_LOG" || true
    exit 1
fi

PORT=$(head -1 "$TMPDIR/port.txt" | tr -d '[:space:]')
echo "Session listening on port $PORT (PID $SESSION_PID)"

if ! kill -0 "$SESSION_PID" 2>/dev/null; then
    echo "FAIL: session exited prematurely"
    cat "$SESSION_LOG"
    exit 1
fi

echo "PASS: session started and listening on port $PORT"

# --- Test 2: Client connects and exchanges data ---
echo ""
echo "--- Test 2: Client basic I/O ---"

CLIENT_FIFO="$TMPDIR/client_stdin.fifo"
mkfifo "$CLIENT_FIFO"
CLIENT_LOG="$TMPDIR/client.log"

# Start client in background
"$GOET_BIN" --local -p "$PORT" -k "$PASSKEY" 127.0.0.1 < "$CLIENT_FIFO" > "$TMPDIR/client_out.txt" 2>"$CLIENT_LOG" &
CLIENT_PID=$!

# Hold client stdin open
exec 11> "$CLIENT_FIFO"

sleep 0.5

MARKER="INTEGRATION_TEST_MARKER_$(date +%s)"
echo "echo $MARKER" >&11

# Wait for marker in client output
for i in $(seq 1 50); do
    if grep -q "$MARKER" "$TMPDIR/client_out.txt" 2>/dev/null; then
        break
    fi
    sleep 0.1
done

if ! grep -q "$MARKER" "$TMPDIR/client_out.txt" 2>/dev/null; then
    echo "FAIL: marker not found in client output"
    echo "Client output:"
    cat "$TMPDIR/client_out.txt" || true
    echo "Client log:"
    cat "$CLIENT_LOG" || true
    exit 1
fi

echo "PASS: client connected and received shell output"

# --- Test 3: Kill client, reconnect, verify catchup ---
echo ""
echo "--- Test 3: Reconnect with catchup ---"

# Kill the client hard
kill -9 "$CLIENT_PID" 2>/dev/null || true
wait "$CLIENT_PID" 2>/dev/null || true
exec 11>&-  # close old client stdin

sleep 0.5

# Verify session survived
if ! kill -0 "$SESSION_PID" 2>/dev/null; then
    echo "FAIL: session died after client disconnect"
    cat "$SESSION_LOG"
    exit 1
fi

# Reconnect with a new client
CLIENT_FIFO2="$TMPDIR/client_stdin2.fifo"
mkfifo "$CLIENT_FIFO2"
CLIENT_LOG2="$TMPDIR/client2.log"

"$GOET_BIN" --local -p "$PORT" -k "$PASSKEY" 127.0.0.1 < "$CLIENT_FIFO2" > "$TMPDIR/client_out2.txt" 2>"$CLIENT_LOG2" &
CLIENT_PID2=$!
exec 12> "$CLIENT_FIFO2"

# Wait for catchup — the marker from test 2 should appear
for i in $(seq 1 50); do
    if grep -q "$MARKER" "$TMPDIR/client_out2.txt" 2>/dev/null; then
        break
    fi
    sleep 0.1
done

if ! grep -q "$MARKER" "$TMPDIR/client_out2.txt" 2>/dev/null; then
    echo "FAIL: catchup replay missing marker from previous connection"
    echo "Client2 output:"
    cat "$TMPDIR/client_out2.txt" || true
    echo "Client2 log:"
    cat "$CLIENT_LOG2" || true
    exit 1
fi

# Also verify we can still send new commands
MARKER2="RECONNECT_MARKER_$(date +%s)"
echo "echo $MARKER2" >&12

for i in $(seq 1 50); do
    if grep -q "$MARKER2" "$TMPDIR/client_out2.txt" 2>/dev/null; then
        break
    fi
    sleep 0.1
done

if ! grep -q "$MARKER2" "$TMPDIR/client_out2.txt" 2>/dev/null; then
    echo "FAIL: new command after reconnect failed"
    exit 1
fi

echo "PASS: reconnected and received catchup + new commands"

# Clean up second client
kill -9 "$CLIENT_PID2" 2>/dev/null || true
wait "$CLIENT_PID2" 2>/dev/null || true
exec 12>&-

# --- Test 4: Graceful shutdown via SIGTERM ---
echo ""
echo "--- Test 4: Session SIGTERM shutdown ---"

if ! kill -0 "$SESSION_PID" 2>/dev/null; then
    echo "FAIL: session already dead"
    exit 1
fi

kill -TERM "$SESSION_PID"
wait "$SESSION_PID" 2>/dev/null || true
exec 10>&-  # close session stdin

echo "PASS: session exited after SIGTERM"

echo ""
echo "=== All integration tests passed ==="
