package client

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/iantay/goet/internal/auth"
	"github.com/iantay/goet/internal/protocol"
	"github.com/iantay/goet/internal/session"
	"github.com/iantay/goet/internal/transport"
)

// startTestSession creates a real session on a random port.
func startTestSession(t *testing.T) (port int, passkey []byte, cleanup func()) {
	t.Helper()

	passkey, err := auth.GeneratePasskey()
	if err != nil {
		t.Fatal(err)
	}

	cfg := session.Config{
		SessionID: "test-client",
		Port:      0,
		Passkey:   passkey,
	}

	s := session.New(cfg)
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx)
	}()

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
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
	}
}

// startTestClient creates a client with piped stdin/stdout and runs it in a goroutine.
func startTestClient(t *testing.T, port int, passkey []byte) (
	stdinW *io.PipeWriter,
	stdoutR *io.PipeReader,
	errCh <-chan error,
	cancel func(),
) {
	t.Helper()

	stdinR, stdinWriter := io.Pipe()
	stdoutReader, stdoutW := io.Pipe()

	cfg := Config{
		Host:    "127.0.0.1",
		Port:    port,
		Passkey: passkey,
	}

	c := newTestClient(cfg, stdinR, stdoutW)
	ctx, cancelFn := context.WithCancel(context.Background())

	ch := make(chan error, 1)
	go func() {
		err := c.Run(ctx)
		stdoutW.Close() // unblock readers on exit
		ch <- err
	}()

	return stdinWriter, stdoutReader, ch, cancelFn
}

// readUntilFromPipe reads from a pipe until substr is found or timeout expires.
func readUntilFromPipe(t *testing.T, r *io.PipeReader, substr string, timeout time.Duration) string {
	t.Helper()

	type result struct {
		output string
		found  bool
	}

	ch := make(chan result, 1)

	go func() {
		var buf bytes.Buffer
		tmp := make([]byte, 4096)
		for {
			n, err := r.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
				if strings.Contains(buf.String(), substr) {
					ch <- result{output: buf.String(), found: true}
					return
				}
			}
			if err != nil {
				ch <- result{output: buf.String(), found: false}
				return
			}
		}
	}()

	select {
	case res := <-ch:
		if !res.found {
			t.Fatalf("pipe closed before finding %q in output (got: %q)", substr, res.output)
		}
		return res.output
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for %q in output", substr)
		return ""
	}
}

// readUntilData reads Data messages from a raw transport.Conn until substr
// is found or timeout expires.
func readUntilData(t *testing.T, conn *transport.Conn, substr string, timeout time.Duration) string {
	t.Helper()
	var buf bytes.Buffer
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for %q in data stream (got: %q)", substr, buf.String())
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

func TestClientBasicIO(t *testing.T) {
	port, passkey, sessionCleanup := startTestSession(t)
	defer sessionCleanup()

	stdinW, stdoutR, errCh, cancel := startTestClient(t, port, passkey)
	defer cancel()
	defer stdinW.Close()
	defer stdoutR.Close()

	// Wait briefly for client to connect
	time.Sleep(200 * time.Millisecond)

	// Send a command
	marker := "CLIENT_TEST_MARKER_67890"
	cmd := "echo " + marker + "\n"
	if _, err := stdinW.Write([]byte(cmd)); err != nil {
		t.Fatalf("write stdin: %v", err)
	}

	// Read output until marker appears
	output := readUntilFromPipe(t, stdoutR, marker, 5*time.Second)
	if !strings.Contains(output, marker) {
		t.Fatalf("marker not found in output: %q", output)
	}

	// Clean exit
	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for client to exit")
	}
}

func TestClientReconnect(t *testing.T) {
	port, passkey, sessionCleanup := startTestSession(t)
	defer sessionCleanup()

	// First connection: use raw transport.Dial so we can close without
	// sending Shutdown (simulating network death, keeping session alive).
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	conn1, err := transport.Dial(ctx1, "127.0.0.1", port, passkey, 0)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Read initial seq header
	conn1.ReadControl()

	// Generate output
	marker1 := "RECONNECT_MARKER_FIRST"
	if err := conn1.WriteData(&protocol.Data{
		Seq:     1,
		Payload: []byte("echo " + marker1 + "\n"),
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read until marker appears in data stream
	readUntilData(t, conn1, marker1, 5*time.Second)

	// Close connection (no Shutdown sent — simulates network death)
	conn1.Close()

	// Wait for session to detect disconnect
	time.Sleep(500 * time.Millisecond)

	// Second connection: use full client to verify catchup works end-to-end
	stdinW2, stdoutR2, errCh2, cancel2 := startTestClient(t, port, passkey)
	defer cancel2()
	defer stdinW2.Close()
	defer stdoutR2.Close()

	// The catchup replay should contain marker1 (session replays its buffer)
	output := readUntilFromPipe(t, stdoutR2, marker1, 5*time.Second)
	if !strings.Contains(output, marker1) {
		t.Fatalf("catchup replay missing marker: %q", output)
	}

	// Verify we can still send new commands
	marker2 := "RECONNECT_MARKER_SECOND"
	if _, err := stdinW2.Write([]byte("echo " + marker2 + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	output = readUntilFromPipe(t, stdoutR2, marker2, 5*time.Second)
	if !strings.Contains(output, marker2) {
		t.Fatalf("second marker not found: %q", output)
	}

	cancel2()
	select {
	case <-errCh2:
	case <-time.After(5 * time.Second):
	}
}

func TestClientEscapeDisconnect(t *testing.T) {
	port, passkey, sessionCleanup := startTestSession(t)
	defer sessionCleanup()

	stdinW, stdoutR, errCh, cancel := startTestClient(t, port, passkey)
	defer cancel()
	defer stdoutR.Close()

	// Wait for connection
	time.Sleep(200 * time.Millisecond)

	// Send ~. (escape disconnect). Client starts in AfterNewline state,
	// so ~. works immediately.
	if _, err := stdinW.Write([]byte("~.")); err != nil {
		t.Fatalf("write escape: %v", err)
	}
	stdinW.Close()

	// Client should exit cleanly (Run returns nil)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("expected nil error from Run, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for client to exit after ~.")
	}
}

func TestClientSessionShutdown(t *testing.T) {
	port, passkey, sessionCleanup := startTestSession(t)
	defer sessionCleanup()

	stdinW, stdoutR, errCh, cancel := startTestClient(t, port, passkey)
	defer cancel()
	defer stdinW.Close()

	// Drain stdout to prevent the client from blocking on pipe writes.
	go io.Copy(io.Discard, stdoutR)
	defer stdoutR.Close()

	// Wait for connection
	time.Sleep(200 * time.Millisecond)

	// Tell the shell to exit — session will send Shutdown
	if _, err := stdinW.Write([]byte("exit\n")); err != nil {
		t.Fatalf("write exit: %v", err)
	}

	// Client should exit when session sends Shutdown
	select {
	case <-errCh:
		// success — either nil or connection closed error
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for client to exit after session shutdown")
	}
}
