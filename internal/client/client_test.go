package client

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
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
func readUntilData(t *testing.T, conn transport.Conn, substr string, timeout time.Duration) string {
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
	conn1, err := transport.Dial(ctx1, transport.DialQUIC, "127.0.0.1", port, passkey, 0)
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

	// Drain stdout to prevent the client from blocking on pipe writes
	// when shell initialization output fills the pipe buffer.
	go io.Copy(io.Discard, stdoutR)
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
	ctx := t.Context()

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

// syncBuffer is a thread-safe bytes.Buffer for capturing stderr concurrently.
// The writer (client goroutine) and reader (test goroutine) can operate safely.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// pipeAccumulator reads from a PipeReader in a single goroutine, accumulating
// all data. waitFor polls the accumulated buffer for a substring, avoiding the
// race condition of multiple readUntilFromPipe goroutines competing for bytes.
type pipeAccumulator struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newPipeAccumulator(r *io.PipeReader) *pipeAccumulator {
	a := &pipeAccumulator{}
	go func() {
		tmp := make([]byte, 4096)
		for {
			n, err := r.Read(tmp)
			if n > 0 {
				a.mu.Lock()
				a.buf.Write(tmp[:n])
				a.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return a
}

func (a *pipeAccumulator) waitFor(t *testing.T, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			a.mu.Lock()
			got := a.buf.String()
			a.mu.Unlock()
			t.Fatalf("timeout waiting for %q in stdout (got: %q)", substr, got)
		case <-tick.C:
			a.mu.Lock()
			found := strings.Contains(a.buf.String(), substr)
			a.mu.Unlock()
			if found {
				return
			}
		}
	}
}

// startTestClientWithStderr creates a client with piped stdin/stdout and a
// syncBuffer for stderr, then runs it in a goroutine. Unlike startTestClient,
// this allows reading stderr while the client is still running.
func startTestClientWithStderr(t *testing.T, port int, passkey []byte) (
	stdinW *io.PipeWriter,
	stdoutR *io.PipeReader,
	stderrBuf *syncBuffer,
	errCh <-chan error,
	cancel func(),
) {
	t.Helper()

	stdinR, stdinWriter := io.Pipe()
	stdoutReader, stdoutW := io.Pipe()
	stderr := &syncBuffer{}

	cfg := Config{
		Host:    "127.0.0.1",
		Port:    port,
		Passkey: passkey,
	}

	c := newTestClient(cfg, stdinR, stdoutW, stderr)
	ctx, cancelFn := context.WithCancel(context.Background())

	ch := make(chan error, 1)
	go func() {
		err := c.Run(ctx)
		stdoutW.Close()
		ch <- err
	}()

	return stdinWriter, stdoutReader, stderr, ch, cancelFn
}

// displaceClient dials a raw QUIC connection to the session, completing the
// full handshake (read SequenceHeader, send TerminalInfo). This causes the
// session's handleNewConn to close the real client's connection, simulating
// a network disconnect. After the handshake completes, it waits briefly for
// the session to process, then closes the raw connection.
func displaceClient(t *testing.T, port int, passkey []byte) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := transport.Dial(ctx, transport.DialQUIC, "127.0.0.1", port, passkey, 0)
	if err != nil {
		t.Fatalf("displaceClient: dial failed: %v", err)
	}

	// Read SequenceHeader from session
	msg, err := conn.ReadControl()
	if err != nil {
		conn.Close()
		t.Fatalf("displaceClient: read control: %v", err)
	}
	if _, ok := msg.(*protocol.SequenceHeader); !ok {
		conn.Close()
		t.Fatalf("displaceClient: expected SequenceHeader, got %T", msg)
	}

	// Send TerminalInfo to complete handshake
	if err := conn.WriteControl(&protocol.TerminalInfo{Term: "xterm-256color"}); err != nil {
		conn.Close()
		t.Fatalf("displaceClient: write terminal info: %v", err)
	}

	// Wait for session to fully process the new connection
	time.Sleep(200 * time.Millisecond)

	// Close raw connection — session goes back to waiting for a new client
	conn.Close()
}

// waitForStderr polls a syncBuffer until it contains substr or timeout expires.
func waitForStderr(t *testing.T, buf *syncBuffer, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for %q in stderr (got: %q)", substr, buf.String())
		case <-tick.C:
			if strings.Contains(buf.String(), substr) {
				return
			}
		}
	}
}

// TestClientAutoReconnect verifies that the client's Run() loop survives
// network disconnects and automatically reconnects. It uses "client
// displacement" — dialing a raw connection causes the session to close the
// real client's connection — to simulate network failures without OS-level
// packet manipulation. Two disconnect/reconnect cycles are tested.
func TestClientAutoReconnect(t *testing.T) {
	port, passkey, sessionCleanup := startTestSession(t)
	defer sessionCleanup()

	stdinW, stdoutR, stderrBuf, errCh, cancel := startTestClientWithStderr(t, port, passkey)
	defer cancel()
	defer stdinW.Close()
	defer stdoutR.Close()

	stdout := newPipeAccumulator(stdoutR)

	// Phase 1: Baseline — verify the client is connected and functional
	time.Sleep(500 * time.Millisecond)

	marker1 := "BASELINE_1_" + time.Now().Format("150405")
	if _, err := stdinW.Write([]byte("echo " + marker1 + "\n")); err != nil {
		t.Fatalf("write baseline cmd: %v", err)
	}
	stdout.waitFor(t, marker1, 5*time.Second)

	// Schedule output that will fire during the disconnect window
	if _, err := stdinW.Write([]byte("sleep 2 && echo DURING_DISCONNECT_1\n")); err != nil {
		t.Fatalf("write sleep cmd: %v", err)
	}

	// Phase 2: First disconnect + reconnect
	time.Sleep(200 * time.Millisecond) // let the sleep command start
	displaceClient(t, port, passkey)

	waitForStderr(t, stderrBuf, "Connection lost", 5*time.Second)

	// The sleep command's output should arrive via catchup replay or live after reconnect
	stdout.waitFor(t, "DURING_DISCONNECT_1", 10*time.Second)

	waitForStderr(t, stderrBuf, "Reconnected.", 10*time.Second)

	// Verify post-reconnect I/O works
	marker2 := "AFTER_RECONNECT_1_" + time.Now().Format("150405")
	if _, err := stdinW.Write([]byte("echo " + marker2 + "\n")); err != nil {
		t.Fatalf("write after reconnect cmd: %v", err)
	}
	stdout.waitFor(t, marker2, 5*time.Second)

	// Phase 3: Second disconnect + reconnect
	if _, err := stdinW.Write([]byte("sleep 2 && echo DURING_DISCONNECT_2\n")); err != nil {
		t.Fatalf("write second sleep cmd: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	displaceClient(t, port, passkey)

	stdout.waitFor(t, "DURING_DISCONNECT_2", 10*time.Second)

	marker3 := "AFTER_RECONNECT_2_" + time.Now().Format("150405")
	if _, err := stdinW.Write([]byte("echo " + marker3 + "\n")); err != nil {
		t.Fatalf("write second after reconnect cmd: %v", err)
	}
	stdout.waitFor(t, marker3, 5*time.Second)

	// Verify stderr has the expected number of disconnect/reconnect messages
	stderr := stderrBuf.String()
	if n := strings.Count(stderr, "Connection lost"); n != 2 {
		t.Errorf("expected 2x 'Connection lost' in stderr, got %d:\n%s", n, stderr)
	}
	if n := strings.Count(stderr, "Reconnected."); n != 2 {
		t.Errorf("expected 2x 'Reconnected.' in stderr, got %d:\n%s", n, stderr)
	}

	// Phase 4: Clean exit
	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for client to exit")
	}
}
