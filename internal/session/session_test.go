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

	// Read the initial sequence header from server
	msg, err := conn.ReadControl()
	if err != nil {
		t.Fatalf("read seq header: %v", err)
	}
	if _, ok := msg.(*protocol.SequenceHeader); !ok {
		t.Fatalf("expected SequenceHeader, got %T", msg)
	}

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

	// Read initial seq header
	conn.ReadControl()

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

	// Read initial seq header
	conn1.ReadControl()

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

	// Read the sequence header — session tells us what it received from us
	msg, err := conn2.ReadControl()
	if err != nil {
		t.Fatalf("read seq header: %v", err)
	}
	seqHdr, ok := msg.(*protocol.SequenceHeader)
	if !ok {
		t.Fatalf("expected SequenceHeader, got %T", msg)
	}
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

func TestSessionShellExit(t *testing.T) {
	port, passkey, cleanup := startTestSession(t)
	defer cleanup()

	conn := dialTestSession(t, port, passkey)
	defer conn.Close()

	// Read initial seq header
	conn.ReadControl()

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

	// Read initial seq header
	conn.ReadControl()

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
