package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chronologos/goet/internal/protocol"
	"github.com/chronologos/goet/internal/transport"
)

// dialWithMode dials into a test session using the specified transport mode.
func dialWithMode(t *testing.T, mode transport.DialMode, port int, passkey []byte) transport.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, mode, "127.0.0.1", port, passkey, 0)
	if err != nil {
		t.Fatalf("dial (%v): %v", mode, err)
	}
	return conn
}

// outputDrainer reads Data messages in a background goroutine and provides
// thread-safe access to the accumulated output. This prevents PTY
// backpressure deadlocks when sending large payloads (echo fills the QUIC
// send buffer if nobody is reading).
type outputDrainer struct {
	conn transport.Conn
	mu   sync.Mutex
	buf  bytes.Buffer
	done chan struct{}
}

func startDrainer(conn transport.Conn) *outputDrainer {
	d := &outputDrainer{conn: conn, done: make(chan struct{})}
	go d.run()
	return d
}

func (d *outputDrainer) run() {
	defer close(d.done)
	for {
		msg, err := d.conn.ReadData()
		if err != nil {
			return
		}
		if data, ok := msg.(*protocol.Data); ok {
			d.mu.Lock()
			d.buf.Write(data.Payload)
			d.mu.Unlock()
		}
	}
}

// waitFor polls the accumulated output until substr appears or timeout.
func (d *outputDrainer) waitFor(t *testing.T, substr string, timeout time.Duration) string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			d.mu.Lock()
			got := d.buf.String()
			d.mu.Unlock()
			t.Fatalf("timeout waiting for %q in output (len=%d)", substr, len(got))
		case <-time.After(50 * time.Millisecond):
			d.mu.Lock()
			s := d.buf.String()
			d.mu.Unlock()
			if strings.Contains(s, substr) {
				return s
			}
		}
	}
}

// waitForHash polls output until prefix followed by a 64-char hex SHA-256 hash
// appears. This avoids false-matching the shell's ZLE echo of the command text,
// which contains the prefix literally but followed by $(...) not hex digits.
func (d *outputDrainer) waitForHash(t *testing.T, prefix string, timeout time.Duration) string {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(prefix) + `([0-9a-f]{64})`)
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			d.mu.Lock()
			got := d.buf.String()
			d.mu.Unlock()
			t.Fatalf("timeout waiting for hash with prefix %q in output (len=%d)", prefix, len(got))
		case <-time.After(50 * time.Millisecond):
			d.mu.Lock()
			s := d.buf.String()
			d.mu.Unlock()
			if m := re.FindStringSubmatch(s); m != nil {
				return m[1]
			}
		}
	}
}

// sha256Hex returns the lowercase hex SHA-256 of data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}

// writeCmd sends a command string as a Data message with auto-incrementing seq.
func writeCmd(t *testing.T, conn transport.Conn, seq *uint64, cmd string) {
	t.Helper()
	*seq++
	if err := conn.WriteData(&protocol.Data{Seq: *seq, Payload: []byte(cmd)}); err != nil {
		t.Fatalf("write data (seq %d): %v", *seq, err)
	}
}

// writeBytes sends raw bytes as a Data message with auto-incrementing seq.
func writeBytes(t *testing.T, conn transport.Conn, seq *uint64, data []byte) {
	t.Helper()
	*seq++
	if err := conn.WriteData(&protocol.Data{Seq: *seq, Payload: data}); err != nil {
		t.Fatalf("write data (seq %d): %v", *seq, err)
	}
}

// setupCleanShell switches from the user's login shell (which may be zsh with
// ZLE and complex init files) to a clean bash. This ensures stty -echo works
// reliably: ZLE does its own userspace echo that ignores stty, and shell init
// files can restore terminal settings unpredictably.
//
// The readiness probe "echo RDY_$(echo OK)" is key: the actual output "RDY_OK"
// cannot appear in ZLE's echo of the command text ("echo RDY_$(echo OK)"),
// because the substring "RDY_OK" isn't contiguous in the echo. This lets
// waitFor reliably detect that the command executed.
func setupCleanShell(t *testing.T, conn transport.Conn, drainer *outputDrainer, seq *uint64) {
	t.Helper()
	// Wait for initial shell startup (zshrc, direnv, etc.) to complete.
	// The probe only matches actual command output, not ZLE's echo.
	writeCmd(t, conn, seq, "echo RDY_$(echo OK)\n")
	drainer.waitFor(t, "RDY_OK", 10*time.Second)

	// Switch to clean bash (no init files → no ZLE, direnv, etc.)
	writeCmd(t, conn, seq, "exec bash --norc --noprofile\n")
	time.Sleep(500 * time.Millisecond)

	// Disable PTY echo and output newline translation
	writeCmd(t, conn, seq, "stty -echo -onlcr\n")
	time.Sleep(300 * time.Millisecond)
}

type transportMode struct {
	name string
	mode transport.DialMode
}

var transports = []transportMode{
	{"QUIC", transport.DialQUIC},
	{"TCP", transport.DialTCP},
}

// TestLoadDownstreamThroughput verifies that large downstream data
// (100K lines from seq) arrives intact through the transport + coalescer.
func TestLoadDownstreamThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			port, passkey, cleanup := startTestSession(t)
			defer cleanup()

			conn := dialWithMode(t, tr.mode, port, passkey)
			defer conn.Close()

			completeHandshake(t, conn)

			var seq uint64
			// Generate 100K lines of output (~588KB).
			// Use "99999\r\n100000\r\n" as the readUntil needle — a consecutive
			// line pair that only appears in actual seq output, not in the
			// ZLE echo of the command text.
			writeCmd(t, conn, &seq, "seq 1 100000\n")

			output := readUntil(t, conn, "99999\r\n100000\r\n", 60*time.Second)

			// Verify intermediate lines arrived too
			for _, want := range []string{"1\r\n", "25000\r\n", "50000\r\n", "75000\r\n"} {
				if !strings.Contains(output, want) {
					t.Errorf("missing %q in output (len=%d)", want, len(output))
				}
			}
			t.Logf("received %d bytes of downstream data", len(output))
		})
	}
}

// TestLoadUpstreamIntegrity sends a large payload to the remote shell,
// which checksums it. Verifies the checksum matches the locally-computed one.
// Uses an outputDrainer to prevent PTY backpressure deadlock: the PTY echoes
// input back, so the QUIC send buffer fills unless we drain concurrently.
func TestLoadUpstreamIntegrity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	const numLines = 5000

	// Generate test data locally
	var buf strings.Builder
	for i := 1; i <= numLines; i++ {
		fmt.Fprintf(&buf, "UPSTREAM_LINE_%06d\n", i)
	}
	testData := buf.String()
	localHash := sha256Hex([]byte(testData))

	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			port, passkey, cleanup := startTestSession(t)
			defer cleanup()

			conn := dialWithMode(t, tr.mode, port, passkey)
			defer conn.Close()

			completeHandshake(t, conn)

			// Start draining output to prevent backpressure deadlock
			drainer := startDrainer(conn)

			var seq uint64
			setupCleanShell(t, conn, drainer, &seq)

			// Start cat to capture stdin to a file
			writeCmd(t, conn, &seq, "cat > /tmp/goet_load_up\n")
			time.Sleep(500 * time.Millisecond)

			// Send the test data in small chunks with brief pauses.
			// The session writes each Data payload to the PTY master
			// synchronously in its event loop. Small chunks + pauses
			// let the loop interleave ptyDataCh draining between writes,
			// preventing a deadlock when the PTY buffer (~1KB) fills up.
			data := []byte(testData)
			for len(data) > 0 {
				n := 256
				if n > len(data) {
					n = len(data)
				}
				writeBytes(t, conn, &seq, data[:n])
				data = data[n:]
				time.Sleep(2 * time.Millisecond)
			}

			// Send Ctrl-D to end cat. One is enough since data ends with \n
			// (PTY canonical mode: Ctrl-D on empty buffer = EOF).
			writeBytes(t, conn, &seq, []byte{0x04})
			time.Sleep(500 * time.Millisecond)

			// Ask the remote to checksum the file.
			// waitForHash uses regex to match prefix+64 hex chars, avoiding
			// false matches from the shell's ZLE echo of the command text.
			writeCmd(t, conn, &seq, "echo UPHASH_$(shasum -a 256 /tmp/goet_load_up | cut -d' ' -f1)\n")

			serverHash := drainer.waitForHash(t, "UPHASH_", 30*time.Second)

			if serverHash != localHash {
				t.Fatalf("upstream hash mismatch:\n  local:  %s\n  server: %s", localHash, serverHash)
			}
			t.Logf("upstream integrity verified: %d bytes, hash=%s", len(testData), localHash)

			// Cleanup
			writeCmd(t, conn, &seq, "rm -f /tmp/goet_load_up\n")
		})
	}
}

// TestLoadRapidSmallWrites sends thousands of individual small Data messages
// to exercise the transport and PTY input path under rapid-fire writes.
// The remote checksums the received data to verify integrity.
func TestLoadRapidSmallWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	const numWrites = 2000

	// Generate expected data and local hash
	var buf strings.Builder
	for i := 1; i <= numWrites; i++ {
		fmt.Fprintf(&buf, "LINE_%05d\n", i)
	}
	expectedData := buf.String()
	localHash := sha256Hex([]byte(expectedData))

	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			port, passkey, cleanup := startTestSession(t)
			defer cleanup()

			conn := dialWithMode(t, tr.mode, port, passkey)
			defer conn.Close()

			completeHandshake(t, conn)

			// Start draining to prevent backpressure
			drainer := startDrainer(conn)

			var seq uint64
			setupCleanShell(t, conn, drainer, &seq)

			// Start cat to capture all input
			writeCmd(t, conn, &seq, "cat > /tmp/goet_load_rapid\n")
			time.Sleep(300 * time.Millisecond)

			// Send many small individual writes. Each is a separate protocol
			// message (exercising the coalescer), with a brief pause to
			// prevent PTY backpressure deadlock.
			for i := 1; i <= numWrites; i++ {
				line := fmt.Sprintf("LINE_%05d\n", i)
				writeBytes(t, conn, &seq, []byte(line))
				time.Sleep(1 * time.Millisecond)
			}

			// End cat input (one Ctrl-D, data ends with \n)
			writeBytes(t, conn, &seq, []byte{0x04})
			time.Sleep(500 * time.Millisecond)

			// Checksum
			writeCmd(t, conn, &seq, "echo RAPIDHASH_$(shasum -a 256 /tmp/goet_load_rapid | cut -d' ' -f1)\n")

			serverHash := drainer.waitForHash(t, "RAPIDHASH_", 30*time.Second)

			if serverHash != localHash {
				t.Fatalf("rapid write hash mismatch:\n  local:  %s\n  server: %s", localHash, serverHash)
			}
			t.Logf("rapid small writes integrity verified: %d writes, hash=%s", numWrites, localHash)

			// Cleanup
			writeCmd(t, conn, &seq, "rm -f /tmp/goet_load_rapid\n")
		})
	}
}

// TestLoadBidirectional sends data to the remote through a pipeline that
// transforms it (rev), and verifies the transformed output matches expectations.
// This exercises both upstream (client→session) and downstream (session→client)
// data paths simultaneously.
func TestLoadBidirectional(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	const numLines = 500

	// Generate input lines and expected reversed output
	var inputBuf, expectedBuf strings.Builder
	for i := 1; i <= numLines; i++ {
		line := fmt.Sprintf("LINE_%05d", i)
		inputBuf.WriteString(line + "\n")
		// Reverse the line
		runes := []rune(line)
		for l, r := 0, len(runes)-1; l < r; l, r = l+1, r-1 {
			runes[l], runes[r] = runes[r], runes[l]
		}
		expectedBuf.WriteString(string(runes) + "\n")
	}
	expectedHash := sha256Hex([]byte(expectedBuf.String()))

	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			port, passkey, cleanup := startTestSession(t)
			defer cleanup()

			conn := dialWithMode(t, tr.mode, port, passkey)
			defer conn.Close()

			completeHandshake(t, conn)

			// Start draining for concurrent read/write
			drainer := startDrainer(conn)

			var seq uint64
			setupCleanShell(t, conn, drainer, &seq)

			// Capture input to a file (same proven pattern as upstream test).
			// Heredoc approach can't be used because bash's PS2 prompts
			// ("> " per line) flood PTY output and cause backpressure deadlock.
			writeCmd(t, conn, &seq, "cat > /tmp/goet_bidir_in\n")
			time.Sleep(300 * time.Millisecond)

			// Send input lines in small chunks
			inputData := []byte(inputBuf.String())
			for len(inputData) > 0 {
				n := 256
				if n > len(inputData) {
					n = len(inputData)
				}
				writeBytes(t, conn, &seq, inputData[:n])
				inputData = inputData[n:]
				time.Sleep(2 * time.Millisecond)
			}

			// End cat (single Ctrl-D, data ends with \n)
			writeBytes(t, conn, &seq, []byte{0x04})
			time.Sleep(500 * time.Millisecond)

			// Transform: reverse each line
			writeCmd(t, conn, &seq, "rev < /tmp/goet_bidir_in > /tmp/goet_load_bidir\n")
			time.Sleep(500 * time.Millisecond)

			// Checksum the reversed output
			writeCmd(t, conn, &seq, "echo BIDIRHASH_$(shasum -a 256 /tmp/goet_load_bidir | cut -d' ' -f1)\n")

			serverHash := drainer.waitForHash(t, "BIDIRHASH_", 30*time.Second)

			if serverHash != expectedHash {
				t.Fatalf("bidirectional hash mismatch:\n  expected: %s\n  server:   %s", expectedHash, serverHash)
			}
			t.Logf("bidirectional integrity verified: %d lines through rev, hash=%s", numLines, expectedHash)

			// Cleanup
			writeCmd(t, conn, &seq, "rm -f /tmp/goet_bidir_in /tmp/goet_load_bidir\n")
		})
	}
}
