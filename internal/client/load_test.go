package client

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/chronologos/goet/internal/transport"
)

// Test flags for controlling load shape. Override via:
//
//	go test -v -run TestLoad -load.downstream=500000 ./internal/client/
var (
	flagDownstream = flag.Int("load.downstream", 100000, "number of lines for downstream throughput test")
	flagUpstream   = flag.Int("load.upstream", 5000, "number of lines for upstream integrity test")
	flagRapid      = flag.Int("load.rapid", 2000, "number of individual writes for rapid small writes test")
	flagBidir      = flag.Int("load.bidir", 500, "number of lines for bidirectional test")
	flagReconnect  = flag.Int("load.reconnect", 5000, "number of lines for reconnect catchup tests")
)

type transportMode struct {
	name string
	mode transport.DialMode
}

var transports = []transportMode{
	{"QUIC", transport.DialQUIC},
	{"TCP", transport.DialTCP},
}

// setupCleanShell switches from the user's login shell to a clean bash.
// The readiness probe "echo RDY_$(echo OK)" produces "RDY_OK" which cannot
// appear in ZLE's echo of the command text, ensuring reliable detection.
func setupCleanShell(t *testing.T, stdinW *io.PipeWriter, acc *pipeAccumulator) {
	t.Helper()
	writeCmd(t, stdinW, "echo RDY_$(echo OK)\n")
	acc.waitFor(t, "RDY_OK", 10*time.Second)

	writeCmd(t, stdinW, "exec bash --norc --noprofile\n")
	time.Sleep(500 * time.Millisecond)

	writeCmd(t, stdinW, "stty -echo -onlcr\n")
	time.Sleep(300 * time.Millisecond)
}

// writeCmd writes a command string to the client's stdin pipe.
func writeCmd(t *testing.T, w *io.PipeWriter, cmd string) {
	t.Helper()
	if _, err := w.Write([]byte(cmd)); err != nil {
		t.Fatalf("write cmd %q: %v", cmd, err)
	}
}

// TestLoadDownstreamThroughput verifies that large downstream data arrives
// intact through the full client stack. Configurable via -load.downstream.
func TestLoadDownstreamThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	n := *flagDownstream

	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			port, passkey, cleanup := startTestSession(t)
			defer cleanup()

			stdinW, stdoutR, stderrBuf, _, cancel := startTestClientWithStderr(t, tr.mode, port, passkey, true)
			defer cancel()
			defer stdinW.Close()

			acc := newPipeAccumulator(stdoutR)

			writeCmd(t, stdinW, fmt.Sprintf("seq 1 %d\n", n))

			// Wait for the last two lines as proof the full output arrived.
			needle := fmt.Sprintf("%d\r\n%d\r\n", n-1, n)
			acc.waitFor(t, needle, 60*time.Second)

			acc.mu.Lock()
			output := acc.buf.String()
			acc.mu.Unlock()

			// Verify some intermediate lines arrived too
			quarter := n / 4
			for _, q := range []int{1, quarter, quarter * 2, quarter * 3} {
				want := fmt.Sprintf("%d\r\n", q)
				if !strings.Contains(output, want) {
					t.Errorf("missing %q in output (len=%d)", want, len(output))
				}
			}
			t.Logf("received %d bytes of downstream data (%d lines)", len(output), n)
			t.Logf("profile stderr:\n%s", stderrBuf.String())
		})
	}
}

// TestLoadUpstreamIntegrity sends a large payload through the client to the
// remote shell, which checksums it. The client's write coalescer naturally
// batches the data into ~32KB messages, preventing PTY backpressure deadlock
// without any artificial sleeps. Configurable via -load.upstream.
func TestLoadUpstreamIntegrity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	numLines := *flagUpstream

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

			stdinW, stdoutR, stderrBuf, _, cancel := startTestClientWithStderr(t, tr.mode, port, passkey, true)
			defer cancel()
			defer stdinW.Close()

			acc := newPipeAccumulator(stdoutR)
			setupCleanShell(t, stdinW, acc)

			// Start cat to capture stdin to a file
			writeCmd(t, stdinW, "cat > /tmp/goet_load_up\n")
			time.Sleep(500 * time.Millisecond)

			// Write the full payload to the client's stdin pipe.
			// The coalescer batches this into ~N x 32KB messages automatically.
			if _, err := stdinW.Write([]byte(testData)); err != nil {
				t.Fatalf("write test data: %v", err)
			}

			// Send Ctrl-D to end cat (data ends with \n, so one Ctrl-D = EOF)
			writeCmd(t, stdinW, "\x04")
			time.Sleep(500 * time.Millisecond)

			// Ask the remote to checksum the file
			writeCmd(t, stdinW, "echo UPHASH_$(shasum -a 256 /tmp/goet_load_up | cut -d' ' -f1)\n")

			serverHash := acc.waitForHash(t, "UPHASH_", 30*time.Second)

			if serverHash != localHash {
				t.Fatalf("upstream hash mismatch:\n  local:  %s\n  server: %s", localHash, serverHash)
			}
			t.Logf("upstream integrity verified: %d bytes (%d lines), hash=%s", len(testData), numLines, localHash)
			t.Logf("profile stderr:\n%s", stderrBuf.String())

			writeCmd(t, stdinW, "rm -f /tmp/goet_load_up\n")
		})
	}
}

// TestLoadRapidSmallWrites sends thousands of individual small writes through
// the client. The write coalescer batches them into larger messages, exercising
// the coalescer's accumulation and flush logic under rapid-fire input.
// Configurable via -load.rapid.
func TestLoadRapidSmallWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	numWrites := *flagRapid

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

			stdinW, stdoutR, stderrBuf, _, cancel := startTestClientWithStderr(t, tr.mode, port, passkey, true)
			defer cancel()
			defer stdinW.Close()

			acc := newPipeAccumulator(stdoutR)
			setupCleanShell(t, stdinW, acc)

			// Start cat to capture all input
			writeCmd(t, stdinW, "cat > /tmp/goet_load_rapid\n")
			time.Sleep(300 * time.Millisecond)

			// Send many small individual writes. Each goes through the client's
			// stdin pipe -> coalescer, which batches them into larger messages.
			for i := 1; i <= numWrites; i++ {
				line := fmt.Sprintf("LINE_%05d\n", i)
				if _, err := stdinW.Write([]byte(line)); err != nil {
					t.Fatalf("write line %d: %v", i, err)
				}
			}

			// End cat input (one Ctrl-D, data ends with \n)
			writeCmd(t, stdinW, "\x04")
			time.Sleep(500 * time.Millisecond)

			// Checksum
			writeCmd(t, stdinW, "echo RAPIDHASH_$(shasum -a 256 /tmp/goet_load_rapid | cut -d' ' -f1)\n")

			serverHash := acc.waitForHash(t, "RAPIDHASH_", 30*time.Second)

			if serverHash != localHash {
				t.Fatalf("rapid write hash mismatch:\n  local:  %s\n  server: %s", localHash, serverHash)
			}
			t.Logf("rapid small writes integrity verified: %d writes, hash=%s", numWrites, localHash)
			t.Logf("profile stderr:\n%s", stderrBuf.String())

			writeCmd(t, stdinW, "rm -f /tmp/goet_load_rapid\n")
		})
	}
}

// TestLoadBidirectional sends data through a remote pipeline that transforms it
// (rev), verifying both upstream and downstream data paths simultaneously.
// Configurable via -load.bidir.
func TestLoadBidirectional(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	numLines := *flagBidir

	// Generate input lines and expected reversed output
	var inputBuf, expectedBuf strings.Builder
	for i := 1; i <= numLines; i++ {
		line := fmt.Sprintf("LINE_%05d", i)
		inputBuf.WriteString(line + "\n")
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

			stdinW, stdoutR, stderrBuf, _, cancel := startTestClientWithStderr(t, tr.mode, port, passkey, true)
			defer cancel()
			defer stdinW.Close()

			acc := newPipeAccumulator(stdoutR)
			setupCleanShell(t, stdinW, acc)

			// Capture input to a file
			writeCmd(t, stdinW, "cat > /tmp/goet_bidir_in\n")
			time.Sleep(300 * time.Millisecond)

			// Send input data through the client
			if _, err := stdinW.Write([]byte(inputBuf.String())); err != nil {
				t.Fatalf("write input data: %v", err)
			}

			// End cat (single Ctrl-D, data ends with \n)
			writeCmd(t, stdinW, "\x04")
			time.Sleep(500 * time.Millisecond)

			// Transform: reverse each line
			writeCmd(t, stdinW, "rev < /tmp/goet_bidir_in > /tmp/goet_load_bidir\n")
			time.Sleep(500 * time.Millisecond)

			// Checksum the reversed output
			writeCmd(t, stdinW, "echo BIDIRHASH_$(shasum -a 256 /tmp/goet_load_bidir | cut -d' ' -f1)\n")

			serverHash := acc.waitForHash(t, "BIDIRHASH_", 30*time.Second)

			if serverHash != expectedHash {
				t.Fatalf("bidirectional hash mismatch:\n  expected: %s\n  server:   %s", expectedHash, serverHash)
			}
			t.Logf("bidirectional integrity verified: %d lines through rev, hash=%s", numLines, expectedHash)
			t.Logf("profile stderr:\n%s", stderrBuf.String())

			writeCmd(t, stdinW, "rm -f /tmp/goet_bidir_in /tmp/goet_load_bidir\n")
		})
	}
}

// TestLoadReconnectDownstream verifies that large downstream data produced
// during a client disconnect is partially recovered via the session-side
// catchup ring buffer. With the default 4KB buffer, only the tail of the
// output survives eviction — this test confirms the tail is present and
// that post-reconnect I/O works. QUIC only (displaceClient is QUIC-specific).
// Configurable via -load.reconnect.
func TestLoadReconnectDownstream(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	n := *flagReconnect

	port, passkey, cleanup := startTestSession(t)
	defer cleanup()

	stdinW, stdoutR, stderrBuf, _, cancel := startTestClientWithStderr(t, transport.DialQUIC, port, passkey, true)
	defer cancel()
	defer stdinW.Close()

	acc := newPipeAccumulator(stdoutR)
	setupCleanShell(t, stdinW, acc)

	// Baseline: verify the connection works
	writeCmd(t, stdinW, "echo BASELINE_OK\n")
	acc.waitFor(t, "BASELINE_OK", 5*time.Second)

	// Schedule: sleep ensures seq runs while client is disconnected.
	// Displace happens at t≈0.2s, reconnect at t≈1.5s (1s delay),
	// sleep 1 expires at t≈1.0s, so seq runs entirely during disconnect.
	writeCmd(t, stdinW, fmt.Sprintf("sleep 1 && seq 1 %d && echo CATCHUP_DONE\n", n))
	time.Sleep(200 * time.Millisecond) // let the sleep command start

	// Sever the connection
	displaceClient(t, port, passkey)

	waitForStderr(t, stderrBuf, "Connection lost", 5*time.Second)
	waitForStderr(t, stderrBuf, "Reconnected.", 15*time.Second)

	// The CATCHUP_DONE marker is at the very end of the seq output,
	// so it's within the 4KB buffer tail.
	acc.waitFor(t, "CATCHUP_DONE", 30*time.Second)

	// Verify the last ~100 lines of seq output survived in the buffer tail.
	// With 4KB buffer and ~5 bytes per line, ~800 lines fit.
	// Note: setupCleanShell sets stty -onlcr, so output uses \n not \r\n.
	acc.mu.Lock()
	output := acc.buf.String()
	acc.mu.Unlock()

	for _, lineNum := range []int{n, n - 1, n - 50, n - 100} {
		if lineNum < 1 {
			continue
		}
		want := fmt.Sprintf("\n%d\n", lineNum)
		if !strings.Contains(output, want) {
			t.Errorf("missing line %d in catchup tail (output len=%d)", lineNum, len(output))
		}
	}

	// Post-reconnect I/O should work
	writeCmd(t, stdinW, "echo POST_OK\n")
	acc.waitFor(t, "POST_OK", 5*time.Second)

	acc.mu.Lock()
	t.Logf("received %d bytes total (downstream reconnect, %d lines seq)", len(acc.buf.String()), n)
	acc.mu.Unlock()
	t.Logf("profile stderr:\n%s", stderrBuf.String())
}

// TestLoadReconnectUpstream verifies that large upstream data arrives intact
// through the coalescer+QUIC pipeline, and that post-reconnect upstream I/O
// works. The test writes all data, waits for delivery, then disconnects —
// ensuring the cat process (attached to the session's PTY) survives the
// reconnect and the file is complete.
//
// Note: "mid-write disconnect" can't guarantee exact hash match because the
// coalescer may hold unflushed data at the moment of disconnect, and the 4KB
// catchup buffer can't cover large in-flight volumes. This test instead
// verifies the more important properties: pipeline integrity and reconnect
// functionality.
//
// QUIC only (displaceClient is QUIC-specific). Configurable via -load.reconnect.
func TestLoadReconnectUpstream(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	numLines := *flagReconnect

	// Generate test data locally
	var buf strings.Builder
	for i := 1; i <= numLines; i++ {
		fmt.Fprintf(&buf, "UPLINE_%06d\n", i)
	}
	testData := buf.String()
	localHash := sha256Hex([]byte(testData))

	port, passkey, sessionCleanup := startTestSession(t)
	defer sessionCleanup()

	stdinW, stdoutR, stderrBuf, _, cancel := startTestClientWithStderr(t, transport.DialQUIC, port, passkey, true)
	defer cancel()
	defer stdinW.Close()

	acc := newPipeAccumulator(stdoutR)
	setupCleanShell(t, stdinW, acc)

	// Start cat to capture stdin to a file
	writeCmd(t, stdinW, "cat > /tmp/goet_reconnect_up\n")
	time.Sleep(500 * time.Millisecond)

	// Write all test data. io.Pipe is synchronous — Write blocks until
	// the reader (readStdin) consumes it, so completion means all data
	// has entered the client's pipeline (stdinCh → coalescer → QUIC).
	if _, err := stdinW.Write([]byte(testData)); err != nil {
		t.Fatalf("write test data: %v", err)
	}

	// Wait for the coalescer's 2ms timer + QUIC delivery. On localhost
	// this takes <10ms, but we're generous to avoid flakiness.
	time.Sleep(500 * time.Millisecond)

	// End cat (data ends with \n, so one Ctrl-D = EOF)
	writeCmd(t, stdinW, "\x04")
	time.Sleep(500 * time.Millisecond)

	// Disconnect after data delivery — exercises reconnect with an active shell
	displaceClient(t, port, passkey)

	waitForStderr(t, stderrBuf, "Connection lost", 5*time.Second)
	waitForStderr(t, stderrBuf, "Reconnected.", 15*time.Second)

	// Checksum the remote file — verifies pipeline integrity
	writeCmd(t, stdinW, "echo UPHASH_$(shasum -a 256 /tmp/goet_reconnect_up | cut -d' ' -f1)\n")

	serverHash := acc.waitForHash(t, "UPHASH_", 30*time.Second)

	if serverHash != localHash {
		t.Fatalf("upstream reconnect hash mismatch:\n  local:  %s\n  server: %s", localHash, serverHash)
	}
	t.Logf("upstream reconnect integrity verified: %d bytes (%d lines), hash=%s", len(testData), numLines, localHash)
	t.Logf("profile stderr:\n%s", stderrBuf.String())

	writeCmd(t, stdinW, "rm -f /tmp/goet_reconnect_up\n")
}
