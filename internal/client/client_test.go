package client

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/chronologos/goet/internal/auth"
	"github.com/chronologos/goet/internal/protocol"
	"github.com/chronologos/goet/internal/session"
	"github.com/chronologos/goet/internal/transport"
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

	c := newTestClient(cfg, stdinR, stdoutW, io.Discard)
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

	// Read output until marker appears (readUntilFromPipe fatals on timeout)
	readUntilFromPipe(t, stdoutR, marker, 5*time.Second)

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

	// Complete handshake: read seq header + send TerminalInfo
	conn1.ReadControl()
	if err := conn1.WriteControl(&protocol.TerminalInfo{Term: "xterm-256color"}); err != nil {
		t.Fatalf("write terminal info: %v", err)
	}

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
	readUntilFromPipe(t, stdoutR2, marker1, 5*time.Second)

	// Verify we can still send new commands
	marker2 := "RECONNECT_MARKER_SECOND"
	if _, err := stdinW2.Write([]byte("echo " + marker2 + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntilFromPipe(t, stdoutR2, marker2, 5*time.Second)

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

// TestHeartbeatKeepsConnectionAlive verifies that the client stays connected
// when the session is sending heartbeats. The session sends heartbeats every
// 5 seconds, and the client's recvTimeout is 15 seconds. This test verifies
// the client survives through one complete heartbeat cycle (6+ seconds)
// and remains functional afterward.
func TestHeartbeatKeepsConnectionAlive(t *testing.T) {
	port, passkey, sessionCleanup := startTestSession(t)
	defer sessionCleanup()

	stdinW, stdoutR, errCh, cancel := startTestClient(t, port, passkey)
	defer cancel()
	defer stdinW.Close()
	defer stdoutR.Close()

	// Wait for connection
	time.Sleep(200 * time.Millisecond)

	// Send a command to prove the connection works initially
	marker1 := "HEARTBEAT_ALIVE_1"
	if _, err := stdinW.Write([]byte("echo " + marker1 + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntilFromPipe(t, stdoutR, marker1, 5*time.Second)

	// Wait for one full heartbeat cycle (heartbeatInterval=5s) plus margin.
	// The session should send at least one heartbeat during this time,
	// keeping the client alive (recvTimeout=15s).
	time.Sleep(7 * time.Second)

	// Client should still be running — errCh should be empty
	select {
	case err := <-errCh:
		t.Fatalf("client exited unexpectedly during heartbeat cycle: %v", err)
	default:
		// Good — client is still alive
	}

	// Verify the client is still functional by sending another command
	marker2 := "HEARTBEAT_ALIVE_2"
	if _, err := stdinW.Write([]byte("echo " + marker2 + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntilFromPipe(t, stdoutR, marker2, 5*time.Second)

	cancel()
	select {
	case <-errCh:
		// Clean exit
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for client to exit after cancel")
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input uint64
		want  string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1048576, "1.0MB"},
		{1073741824, "1.0GB"},
	}
	for _, tt := range tests {
		got := formatBytes(tt.input)
		if got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestProfileOutput(t *testing.T) {
	port, passkey, sessionCleanup := startTestSession(t)
	defer sessionCleanup()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	var stderrBuf bytes.Buffer
	cfg := Config{
		Host:    "127.0.0.1",
		Port:    port,
		Passkey: passkey,
		Profile: true,
	}
	c := newTestClient(cfg, stdinR, stdoutW, &stderrBuf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		err := c.Run(ctx)
		stdoutW.Close()
		errCh <- err
	}()

	// Drain stdout to prevent pipe backpressure
	go io.Copy(io.Discard, stdoutR)
	defer stdoutR.Close()

	// Wait long enough for at least one heartbeat tick (5s) to fire
	time.Sleep(6 * time.Second)

	// Trigger exit — ~. from initial AfterNewline state
	stdinW.Write([]byte("~."))
	stdinW.Close()

	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for client exit")
	}

	output := stderrBuf.String()

	// Periodic profile line should have appeared
	if !strings.Contains(output, "[profile] rtt=") {
		t.Errorf("missing periodic profile line in stderr:\n%s", output)
	}

	// Summary should appear on exit
	if !strings.Contains(output, "[profile] === Connection Profile ===") {
		t.Errorf("missing profile summary in stderr:\n%s", output)
	}
	if !strings.Contains(output, "[profile] Duration:") {
		t.Errorf("missing duration line in stderr:\n%s", output)
	}
	if !strings.Contains(output, "[profile] Traffic:") {
		t.Errorf("missing traffic line in stderr:\n%s", output)
	}
}
