#!/usr/bin/env bash
#
# Integration test for goet client ↔ session (direct mode).
#
# Tests (run for both QUIC and TCP transports):
# 1. Session starts, prints port
# 2. Client connects, sends command, reads output
# 3. Client reconnects after kill, receives catchup
# 4. Graceful shutdown via SIGTERM
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

# run_transport_tests runs the standard test suite for a given transport.
# Args: $1 = transport name ("QUIC" or "TCP"), $2 = extra client flags ("" or "--tcp")
run_transport_tests() {
    local TRANSPORT_NAME="$1"
    local EXTRA_FLAGS="$2"

    echo ""
    echo "=========================================="
    echo "  Transport: $TRANSPORT_NAME"
    echo "=========================================="

    # Generate a random passkey (32 bytes = 64 hex chars)
    local PASSKEY
    PASSKEY=$(head -c32 /dev/urandom | xxd -p -c64)

    # --- Test 1: Session starts and prints port ---
    echo ""
    echo "--- [$TRANSPORT_NAME] Test 1: Session startup ---"

    local SESSION_FIFO="$TMPDIR/session_stdin_${TRANSPORT_NAME}.fifo"
    mkfifo "$SESSION_FIFO"

    local SESSION_LOG="$TMPDIR/session_${TRANSPORT_NAME}.log"
    local PORT_FILE="$TMPDIR/port_${TRANSPORT_NAME}.txt"

    # Write passkey to FIFO, then hold it open
    echo "$PASSKEY" > "$SESSION_FIFO" &
    "$GOET_BIN" session -f "test-${TRANSPORT_NAME}" -p 0 < "$SESSION_FIFO" > "$PORT_FILE" 2>"$SESSION_LOG" &
    local SESSION_PID=$!

    # Hold session stdin open (use different FDs per transport to avoid conflicts)
    if [ "$TRANSPORT_NAME" = "QUIC" ]; then
        exec 10> "$SESSION_FIFO"
    else
        exec 20> "$SESSION_FIFO"
    fi

    # Wait for port to appear
    for i in $(seq 1 50); do
        if [ -s "$PORT_FILE" ]; then
            break
        fi
        sleep 0.1
    done

    if [ ! -s "$PORT_FILE" ]; then
        echo "FAIL: session did not print port within 5s"
        cat "$SESSION_LOG" || true
        exit 1
    fi

    local PORT
    PORT=$(head -1 "$PORT_FILE" | tr -d '[:space:]')
    echo "Session listening on port $PORT (PID $SESSION_PID)"

    if ! kill -0 "$SESSION_PID" 2>/dev/null; then
        echo "FAIL: session exited prematurely"
        cat "$SESSION_LOG"
        exit 1
    fi

    echo "PASS: session started and listening on port $PORT"

    # --- Test 2: Client connects and exchanges data ---
    echo ""
    echo "--- [$TRANSPORT_NAME] Test 2: Client basic I/O ---"

    local CLIENT_FIFO="$TMPDIR/client_stdin_${TRANSPORT_NAME}.fifo"
    mkfifo "$CLIENT_FIFO"
    local CLIENT_LOG="$TMPDIR/client_${TRANSPORT_NAME}.log"
    local CLIENT_OUT="$TMPDIR/client_out_${TRANSPORT_NAME}.txt"

    # Start client in background
    # shellcheck disable=SC2086
    "$GOET_BIN" --local $EXTRA_FLAGS -p "$PORT" -k "$PASSKEY" 127.0.0.1 < "$CLIENT_FIFO" > "$CLIENT_OUT" 2>"$CLIENT_LOG" &
    local CLIENT_PID=$!

    # Hold client stdin open
    if [ "$TRANSPORT_NAME" = "QUIC" ]; then
        exec 11> "$CLIENT_FIFO"
    else
        exec 21> "$CLIENT_FIFO"
    fi

    sleep 0.5

    local MARKER="INTEGRATION_${TRANSPORT_NAME}_MARKER_$(date +%s)"
    if [ "$TRANSPORT_NAME" = "QUIC" ]; then
        echo "echo $MARKER" >&11
    else
        echo "echo $MARKER" >&21
    fi

    # Wait for marker in client output
    for i in $(seq 1 50); do
        if grep -q "$MARKER" "$CLIENT_OUT" 2>/dev/null; then
            break
        fi
        sleep 0.1
    done

    if ! grep -q "$MARKER" "$CLIENT_OUT" 2>/dev/null; then
        echo "FAIL: marker not found in client output"
        echo "Client output:"
        cat "$CLIENT_OUT" || true
        echo "Client log:"
        cat "$CLIENT_LOG" || true
        exit 1
    fi

    echo "PASS: client connected and received shell output"

    # --- Test 3: Kill client, reconnect, verify catchup ---
    echo ""
    echo "--- [$TRANSPORT_NAME] Test 3: Reconnect with catchup ---"

    # Kill the client hard
    kill -9 "$CLIENT_PID" 2>/dev/null || true
    wait "$CLIENT_PID" 2>/dev/null || true
    if [ "$TRANSPORT_NAME" = "QUIC" ]; then
        exec 11>&-
    else
        exec 21>&-
    fi

    sleep 0.5

    # Verify session survived
    if ! kill -0 "$SESSION_PID" 2>/dev/null; then
        echo "FAIL: session died after client disconnect"
        cat "$SESSION_LOG"
        exit 1
    fi

    # Reconnect with a new client
    local CLIENT_FIFO2="$TMPDIR/client_stdin2_${TRANSPORT_NAME}.fifo"
    mkfifo "$CLIENT_FIFO2"
    local CLIENT_LOG2="$TMPDIR/client2_${TRANSPORT_NAME}.log"
    local CLIENT_OUT2="$TMPDIR/client_out2_${TRANSPORT_NAME}.txt"

    # shellcheck disable=SC2086
    "$GOET_BIN" --local $EXTRA_FLAGS -p "$PORT" -k "$PASSKEY" 127.0.0.1 < "$CLIENT_FIFO2" > "$CLIENT_OUT2" 2>"$CLIENT_LOG2" &
    local CLIENT_PID2=$!
    if [ "$TRANSPORT_NAME" = "QUIC" ]; then
        exec 12> "$CLIENT_FIFO2"
    else
        exec 22> "$CLIENT_FIFO2"
    fi

    # Wait for catchup — the marker from test 2 should appear
    for i in $(seq 1 50); do
        if grep -q "$MARKER" "$CLIENT_OUT2" 2>/dev/null; then
            break
        fi
        sleep 0.1
    done

    if ! grep -q "$MARKER" "$CLIENT_OUT2" 2>/dev/null; then
        echo "FAIL: catchup replay missing marker from previous connection"
        echo "Client2 output:"
        cat "$CLIENT_OUT2" || true
        echo "Client2 log:"
        cat "$CLIENT_LOG2" || true
        exit 1
    fi

    # Also verify we can still send new commands
    local MARKER2="RECONNECT_${TRANSPORT_NAME}_MARKER_$(date +%s)"
    if [ "$TRANSPORT_NAME" = "QUIC" ]; then
        echo "echo $MARKER2" >&12
    else
        echo "echo $MARKER2" >&22
    fi

    for i in $(seq 1 50); do
        if grep -q "$MARKER2" "$CLIENT_OUT2" 2>/dev/null; then
            break
        fi
        sleep 0.1
    done

    if ! grep -q "$MARKER2" "$CLIENT_OUT2" 2>/dev/null; then
        echo "FAIL: new command after reconnect failed"
        exit 1
    fi

    echo "PASS: reconnected and received catchup + new commands"

    # Clean up second client
    kill -9 "$CLIENT_PID2" 2>/dev/null || true
    wait "$CLIENT_PID2" 2>/dev/null || true
    if [ "$TRANSPORT_NAME" = "QUIC" ]; then
        exec 12>&-
    else
        exec 22>&-
    fi

    # --- Test 4: Graceful shutdown via SIGTERM ---
    echo ""
    echo "--- [$TRANSPORT_NAME] Test 4: Session SIGTERM shutdown ---"

    if ! kill -0 "$SESSION_PID" 2>/dev/null; then
        echo "FAIL: session already dead"
        exit 1
    fi

    kill -TERM "$SESSION_PID"
    wait "$SESSION_PID" 2>/dev/null || true
    if [ "$TRANSPORT_NAME" = "QUIC" ]; then
        exec 10>&-
    else
        exec 20>&-
    fi

    echo "PASS: session exited after SIGTERM"
}

# Run tests for both transports
run_transport_tests "QUIC" ""
run_transport_tests "TCP" "--tcp"

echo ""
echo "=== All integration tests passed (QUIC + TCP) ==="
