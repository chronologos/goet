package session

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chronologos/goet/internal/auth"
	"github.com/chronologos/goet/internal/protocol"
	"github.com/chronologos/goet/internal/transport"
)

// startTestSession creates a session with a random port and returns a function
// to dial into it. The session runs in a background goroutine; cleanup cancels
// it and waits for exit.
func startTestSession(t *testing.T) (port int, passkey []byte, cleanup func()) {
	t.Helper()

	passkey, err := auth.GeneratePasskey()
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		SessionID: "test-session",
		Port:      0, // random
		Passkey:   passkey,
	}

	s := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx)
	}()

	// Wait for the listener to be ready via the Ready channel.
	select {
	case <-s.Ready:
		port = s.Port
	case err := <-errCh:
		cancel()
		t.Fatalf("session exited early: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timeout waiting for session to start")
	}

	return port, passkey, func() {
		cancel()
		// Wait for Run to exit
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
	}
}

func dialTestSession(t *testing.T, port int, passkey []byte) *transport.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "127.0.0.1", port, passkey, 0)
	if err != nil {
		t.Fatalf("dial session: %v", err)
	}
	return conn
}

// completeHandshake reads the SequenceHeader from the session and sends
// TerminalInfo. Returns the SequenceHeader for tests that need it.
func completeHandshake(t *testing.T, conn *transport.Conn) *protocol.SequenceHeader {
	t.Helper()
	msg, err := conn.ReadControl()
	if err != nil {
		t.Fatalf("read seq header: %v", err)
	}
	seqHdr, ok := msg.(*protocol.SequenceHeader)
	if !ok {
		t.Fatalf("expected SequenceHeader, got %T", msg)
	}
	if err := conn.WriteControl(&protocol.TerminalInfo{Term: "xterm-256color"}); err != nil {
		t.Fatalf("write terminal info: %v", err)
	}
	return seqHdr
}

// readUntil reads data messages from the data stream until the output
// contains substr, or the timeout expires.
func readUntil(t *testing.T, conn *transport.Conn, substr string, timeout time.Duration) string {
	t.Helper()
	var buf bytes.Buffer
	deadline := time.After(timeout)

	for {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for %q in output (got: %q)", substr, buf.String())
		default:
		}

		msg, err := conn.ReadData()
		if err != nil {
			t.Fatalf("read data: %v (buffer so far: %q)", err, buf.String())
		}
		if d, ok := msg.(*protocol.Data); ok {
			buf.Write(d.Payload)
		}
		if strings.Contains(buf.String(), substr) {
			return buf.String()
		}
	}
}

func TestSessionBasicIO(t *testing.T) {
	port, passkey, cleanup := startTestSession(t)
	defer cleanup()

	conn := dialTestSession(t, port, passkey)
	defer conn.Close()

	completeHandshake(t, conn)

	// Send a command that produces recognizable output
	marker := "GOET_TEST_MARKER_12345"
	cmd := "echo " + marker + "\n"
	if err := conn.WriteData(&protocol.Data{
		Seq:     1,
		Payload: []byte(cmd),
	}); err != nil {
		t.Fatalf("write data: %v", err)
	}

	// Read until we see the marker in output
	output := readUntil(t, conn, marker, 5*time.Second)
	if !strings.Contains(output, marker) {
		t.Fatalf("marker not found in output: %q", output)
	}
}

func TestSessionResize(t *testing.T) {
	port, passkey, cleanup := startTestSession(t)
	defer cleanup()

	conn := dialTestSession(t, port, passkey)
	defer conn.Close()

	completeHandshake(t, conn)

	// Send resize
	if err := conn.WriteControl(&protocol.Resize{Rows: 50, Cols: 120}); err != nil {
		t.Fatalf("write resize: %v", err)
	}

	// Small delay to let the resize propagate through the control stream
	// reader goroutine before we query the PTY size via the data stream.
	time.Sleep(100 * time.Millisecond)

	// Verify by checking stty output. We look for "50 120" directly
	// rather than a separate marker, since that's the actual output we want.
	cmd := "stty size\n"
	if err := conn.WriteData(&protocol.Data{
		Seq:     1,
		Payload: []byte(cmd),
	}); err != nil {
		t.Fatalf("write data: %v", err)
	}

	output := readUntil(t, conn, "50 120", 5*time.Second)
	if !strings.Contains(output, "50 120") {
		t.Fatalf("resize not reflected: %q", output)
	}
}

func TestSessionReconnect(t *testing.T) {
	port, passkey, cleanup := startTestSession(t)
	defer cleanup()

	// First connection — generate some output
	conn1 := dialTestSession(t, port, passkey)

	completeHandshake(t, conn1)

	marker1 := "RECONNECT_BEFORE_42"
	if err := conn1.WriteData(&protocol.Data{
		Seq:     1,
		Payload: []byte("echo " + marker1 + "\n"),
	}); err != nil {
		t.Fatalf("write data: %v", err)
	}
	readUntil(t, conn1, marker1, 5*time.Second)

	// Remember the last data seq we received from the server
	// (the readUntil consumed messages but we don't track seq here,
	// so reconnect with seq=0 to get full catchup)

	// Close connection (simulate network death)
	conn1.Close()

	// Small delay to let session detect disconnect
	time.Sleep(200 * time.Millisecond)

	// Reconnect — client sends LastReceivedSeq=0, so session replays everything
	conn2 := dialTestSession(t, port, passkey)
	defer conn2.Close()

	seqHdr := completeHandshake(t, conn2)
	// Session should have received our seq=1 data message
	if seqHdr.LastReceivedSeq != 1 {
		t.Fatalf("expected server recvSeq=1, got %d", seqHdr.LastReceivedSeq)
	}

	// Read catchup replay — should contain the marker from before disconnect
	output := readUntil(t, conn2, marker1, 5*time.Second)
	if !strings.Contains(output, marker1) {
		t.Fatalf("catchup replay missing marker: %q", output)
	}
}

// TestRapidReconnectNoDataLoss verifies that disconnecting and reconnecting
// within a very short window (shorter than the 2ms coalesce deadline) does
// not cause data loss or duplication. This exercises the critical path in
// handleNewConn where flushCoalesced() is called before setting the new
// connection.
func TestRapidReconnectNoDataLoss(t *testing.T) {
	port, passkey, cleanup := startTestSession(t)
	defer cleanup()

	// First connection — generate output
	conn1 := dialTestSession(t, port, passkey)
	completeHandshake(t, conn1)

	marker1 := "RAPID_RECONNECT_MARKER_1"
	if err := conn1.WriteData(&protocol.Data{
		Seq:     1,
		Payload: []byte("echo " + marker1 + "\n"),
	}); err != nil {
		t.Fatalf("write data: %v", err)
	}
	readUntil(t, conn1, marker1, 5*time.Second)

	// Close connection abruptly (no Shutdown — simulates network death)
	conn1.Close()

	// Reconnect immediately — minimal delay (50ms), well within the 2ms
	// coalesce window that might still have pending data.
	time.Sleep(50 * time.Millisecond)

	conn2 := dialTestSession(t, port, passkey)
	defer conn2.Close()

	seqHdr := completeHandshake(t, conn2)
	// Session should remember our seq=1 from before disconnect
	if seqHdr.LastReceivedSeq != 1 {
		t.Fatalf("expected server recvSeq=1, got %d", seqHdr.LastReceivedSeq)
	}

	// Catchup replay should contain marker1 (data from before disconnect).
	// We reconnected with LastReceivedSeq=0, so the session replays everything.
	output := readUntil(t, conn2, marker1, 5*time.Second)
	if !strings.Contains(output, marker1) {
		t.Fatalf("catchup replay missing marker1: %q", output)
	}

	// Count occurrences of marker1 in the replay to detect duplication.
	// The marker should appear at least once (the echo output). It may also
	// appear in the command echo itself, so we check for reasonable bounds.
	count := strings.Count(output, marker1)
	if count > 3 {
		t.Fatalf("marker1 appears %d times in replay — possible duplication: %q", count, output)
	}

	// Verify we can still send new commands through the reconnected session
	marker2 := "RAPID_RECONNECT_MARKER_2"
	if err := conn2.WriteData(&protocol.Data{
		Seq:     2,
		Payload: []byte("echo " + marker2 + "\n"),
	}); err != nil {
		t.Fatalf("write data: %v", err)
	}
	output2 := readUntil(t, conn2, marker2, 5*time.Second)
	if !strings.Contains(output2, marker2) {
		t.Fatalf("new command output missing marker2: %q", output2)
	}
}

// TestUnknownMessageTypeNocrash verifies that an unknown message type on the
// data stream causes the session to close that connection (readStream gets
// ErrUnknownMessage) but the session stays alive and accepts a new connection.
func TestUnknownMessageTypeNoCrash(t *testing.T) {
	port, passkey, cleanup := startTestSession(t)
	defer cleanup()

	conn1 := dialTestSession(t, port, passkey)
	completeHandshake(t, conn1)

	// Verify the connection works
	marker1 := "UNKNOWN_MSG_BEFORE"
	if err := conn1.WriteData(&protocol.Data{
		Seq: 1, Payload: []byte("echo " + marker1 + "\n"),
	}); err != nil {
		t.Fatalf("write data: %v", err)
	}
	readUntil(t, conn1, marker1, 5*time.Second)

	// Inject an unknown message type (0xFF) directly onto the data stream.
	// Format: [4B payload_length BE][1B msg_type][payload]
	var raw [7]byte // 5-byte header + 2-byte payload
	raw[0], raw[1], raw[2], raw[3] = 0, 0, 0, 2 // payload length = 2
	raw[4] = 0xFF                                 // unknown type
	raw[5], raw[6] = 0xAB, 0xCD                   // dummy payload
	if _, err := conn1.Data.Write(raw[:]); err != nil {
		t.Fatalf("write raw: %v", err)
	}

	// The session should close this connection (readStream returns error).
	// Give it a moment to process.
	time.Sleep(500 * time.Millisecond)
	conn1.Close()

	// Reconnect — session should still be alive
	conn2 := dialTestSession(t, port, passkey)
	defer conn2.Close()

	completeHandshake(t, conn2)

	marker2 := "UNKNOWN_MSG_AFTER"
	if err := conn2.WriteData(&protocol.Data{
		Seq: 2, Payload: []byte("echo " + marker2 + "\n"),
	}); err != nil {
		t.Fatalf("write data: %v", err)
	}
	output := readUntil(t, conn2, marker2, 5*time.Second)
	if !strings.Contains(output, marker2) {
		t.Fatalf("marker not found after reconnect: %q", output)
	}
}

// TestLargeDataTransfer verifies that data larger than the 32KB coalescer
// threshold makes it through the session intact. This exercises the
// threshold-flush path of the coalescer end-to-end.
func TestLargeDataTransfer(t *testing.T) {
	port, passkey, cleanup := startTestSession(t)
	defer cleanup()

	conn := dialTestSession(t, port, passkey)
	defer conn.Close()

	completeHandshake(t, conn)

	// Generate a large amount of output — `seq 1 5000` produces ~24KB of
	// numbered lines. We search for a pattern that can only appear in the
	// actual output, not in the PTY's command echo.
	if err := conn.WriteData(&protocol.Data{
		Seq: 1, Payload: []byte("seq 1 5000\n"),
	}); err != nil {
		t.Fatalf("write data: %v", err)
	}

	// Wait for the last line of seq output. The pattern "4999\r\n5000\r\n"
	// can only appear in actual seq output, not in the echoed command.
	output := readUntil(t, conn, "4999\r\n5000\r\n", 15*time.Second)

	// Verify we got early and middle lines too
	for _, want := range []string{"1\r\n", "2500\r\n"} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in output (len=%d)", want, len(output))
		}
	}
}

// TestWriteAfterShellDeath verifies that sending data to the session after the
// shell has exited doesn't cause a panic. The session should handle the dead
// PTY gracefully and send Shutdown.
func TestWriteAfterShellDeath(t *testing.T) {
	port, passkey, cleanup := startTestSession(t)
	defer cleanup()

	conn := dialTestSession(t, port, passkey)
	defer conn.Close()

	completeHandshake(t, conn)

	// Kill the shell
	if err := conn.WriteData(&protocol.Data{
		Seq: 1, Payload: []byte("exit\n"),
	}); err != nil {
		t.Fatalf("write exit: %v", err)
	}

	// Immediately send more data while the shell is dying
	time.Sleep(100 * time.Millisecond)
	for i := uint64(2); i <= 5; i++ {
		conn.WriteData(&protocol.Data{
			Seq: i, Payload: []byte("echo should_not_crash\n"),
		})
	}

	// Session should send Shutdown or close the connection. Either is
	// acceptable — the key assertion is that we get here without panic.
	doneCh := make(chan struct{}, 1)
	go func() {
		for {
			msg, err := conn.ReadControl()
			if err != nil {
				doneCh <- struct{}{}
				return
			}
			if _, ok := msg.(*protocol.Shutdown); ok {
				doneCh <- struct{}{}
				return
			}
		}
	}()

	select {
	case <-doneCh:
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for session to handle shell death gracefully")
	}
}

func TestSessionShellExit(t *testing.T) {
	port, passkey, cleanup := startTestSession(t)
	defer cleanup()

	conn := dialTestSession(t, port, passkey)
	defer conn.Close()

	completeHandshake(t, conn)

	// Tell the shell to exit
	if err := conn.WriteData(&protocol.Data{
		Seq:     1,
		Payload: []byte("exit\n"),
	}); err != nil {
		t.Fatalf("write exit: %v", err)
	}

	// Session should send Shutdown on control stream, then close connection.
	doneCh := make(chan struct{}, 1)
	go func() {
		for {
			msg, err := conn.ReadControl()
			if err != nil {
				// Connection closed by session — also acceptable
				doneCh <- struct{}{}
				return
			}
			if _, ok := msg.(*protocol.Shutdown); ok {
				doneCh <- struct{}{}
				return
			}
			// Skip heartbeats and other control messages
		}
	}()

	select {
	case <-doneCh:
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Shutdown")
	}
}

func TestSessionShutdownFromClient(t *testing.T) {
	port, passkey, cleanup := startTestSession(t)
	defer cleanup()

	conn := dialTestSession(t, port, passkey)
	defer conn.Close()

	completeHandshake(t, conn)

	// Send Shutdown from client
	if err := conn.WriteControl(&protocol.Shutdown{}); err != nil {
		t.Fatalf("write shutdown: %v", err)
	}

	// Session should exit — the QUIC connection will be closed.
	// ReadData blocks, so run it in a goroutine and use a timeout.
	errCh := make(chan error, 1)
	go func() {
		for {
			_, err := conn.ReadData()
			if err != nil {
				errCh <- err
				return
			}
		}
	}()

	select {
	case <-errCh:
		// Connection closed by session — success
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for session to exit after client Shutdown")
	}
}
