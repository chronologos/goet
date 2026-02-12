package client

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/chronologos/goet/internal/catchup"
	"github.com/chronologos/goet/internal/coalesce"
	"github.com/chronologos/goet/internal/protocol"
	"github.com/chronologos/goet/internal/transport"
)

// discardHandler is a no-op slog handler that discards all log records.
// Used when --profile is off to suppress client logging with zero overhead.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler            { return d }

const (
	stdinBufSize      = 32 * 1024 // 32 KB per stdin read
	heartbeatInterval = 5 * time.Second
	recvTimeout       = 15 * time.Second
	reconnectDelay    = 1 * time.Second
)

// Config holds client configuration.
type Config struct {
	Host     string
	Port     int
	Passkey  []byte
	Profile  bool               // emit RTT/traffic stats to stderr
	DialMode transport.DialMode // QUIC (default) or TCP
}

// Client is the terminal-facing half of a reconnectable terminal session.
// It connects to a session via QUIC, relays stdin/stdout, handles the ~.
// escape sequence, and reconnects with catchup replay on network failures.
type Client struct {
	cfg          Config
	log          *slog.Logger
	buf          *catchup.Buffer
	escape       *EscapeProcessor
	sendSeq      uint64
	recvSeq      uint64
	stdin        io.Reader
	stdout       io.Writer
	stderr       io.Writer          // for --profile output (os.Stderr or test buffer)
	stdinFd      int                // for MakeRaw/Restore; -1 if pipe (skip raw mode)
	profileStart time.Time          // set on first successful dial
}

// New creates a client with the given config. Uses os.Stdin/os.Stdout
// for terminal I/O. If stdin is not a terminal (pipe, FIFO), raw mode
// is skipped automatically. For testing, use newTestClient instead.
func New(cfg Config) *Client {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		fd = -1
	}
	var logger *slog.Logger
	if cfg.Profile {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", "client")
	} else {
		logger = slog.New(&discardHandler{})
	}
	return &Client{
		cfg:     cfg,
		log:     logger,
		buf:     catchup.New(0),
		escape:  NewEscapeProcessor(),
		stdin:   os.Stdin,
		stdout:  os.Stdout,
		stderr:  os.Stderr,
		stdinFd: fd,
	}
}

// newTestClient creates a client wired to pipes instead of the real terminal.
// stdinFd is set to -1 to skip MakeRaw (pipes aren't terminals).
func newTestClient(cfg Config, stdin io.Reader, stdout, stderr io.Writer) *Client {
	return &Client{
		cfg:     cfg,
		log:     slog.New(&discardHandler{}),
		buf:     catchup.New(0),
		escape:  NewEscapeProcessor(),
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		stdinFd: -1,
	}
}

// exitReason describes why the ioLoop exited.
type exitReason int

const (
	exitNetwork    exitReason = iota // connection error / heartbeat timeout
	exitEscape                       // ~. detected
	exitShutdown                     // session sent Shutdown
	exitStdinEOF                     // stdin closed
	exitCancelled                    // context cancelled
)

// Run is the client's main entry point. It connects to the session, relays
// I/O, and reconnects on network failures. Returns when the session shuts
// down, the user types ~., stdin hits EOF, or the context is cancelled.
func (c *Client) Run(ctx context.Context) error {
	// Permanent goroutine: read stdin
	stdinCh := make(chan []byte, 4)
	go c.readStdin(stdinCh)

	for {
		conn, err := c.dial(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.log.Warn("connect failed, retrying", "err", err, "delay", reconnectDelay)
			select {
			case <-time.After(reconnectDelay):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if c.profileStart.IsZero() {
			c.profileStart = time.Now()
		}

		if err := c.handleSequenceExchange(conn); err != nil {
			c.log.Warn("sequence exchange failed, retrying", "err", err)
			conn.Close()
			select {
			case <-time.After(reconnectDelay):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if err := c.sendTerminalInfo(conn); err != nil {
			c.log.Warn("send terminal info failed, retrying", "err", err)
			conn.Close()
			select {
			case <-time.After(reconnectDelay):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		// Enter raw mode while connected (skip for pipes/tests)
		var oldState *term.State
		if c.stdinFd >= 0 {
			oldState, err = term.MakeRaw(c.stdinFd)
			if err != nil {
				conn.Close()
				return fmt.Errorf("make raw: %w", err)
			}
		}

		// Send initial resize
		c.sendResize(conn)

		// Reset escape state for new connection
		c.escape.Reset()

		// Start demux (no-op for QUIC; starts background reader for TCP)
		conn.StartDemux()

		reason := c.ioLoop(ctx, conn, stdinCh)

		// Restore terminal before logging or sleeping
		if oldState != nil {
			term.Restore(c.stdinFd, oldState)
		}

		switch reason {
		case exitNetwork:
			conn.Close()
			c.log.Info("connection lost, reconnecting", "delay", reconnectDelay)
			select {
			case <-time.After(reconnectDelay):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		case exitShutdown:
			conn.Close()
			return nil
		default:
			// exitEscape, exitStdinEOF, exitCancelled: tell session we're leaving
			conn.WriteControl(&protocol.Shutdown{}) // best-effort
			conn.Close()
			if reason == exitCancelled {
				return ctx.Err()
			}
			return nil
		}
	}
}

// dial connects to the session with the current recvSeq for catchup.
func (c *Client) dial(ctx context.Context) (transport.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return transport.Dial(dialCtx, c.cfg.DialMode, c.cfg.Host, c.cfg.Port, c.cfg.Passkey, c.recvSeq)
}

// handleSequenceExchange reads the session's SequenceHeader from the control
// stream and resends any data the session missed from our catchup buffer.
func (c *Client) handleSequenceExchange(conn transport.Conn) error {
	msg, err := conn.ReadControl()
	if err != nil {
		return fmt.Errorf("read seq header: %w", err)
	}

	seqHdr, ok := msg.(*protocol.SequenceHeader)
	if !ok {
		return fmt.Errorf("expected SequenceHeader, got %T", msg)
	}

	// Resend data the session missed
	entries := c.buf.ReplaySince(seqHdr.LastReceivedSeq)
	for _, e := range entries {
		if err := conn.WriteData(&protocol.Data{
			Seq:     e.Seq,
			Payload: e.Payload,
		}); err != nil {
			return fmt.Errorf("catchup resend: %w", err)
		}
	}

	return nil
}

// sendTerminalInfo sends the client's TERM value on the control stream.
func (c *Client) sendTerminalInfo(conn transport.Conn) error {
	return conn.WriteControl(&protocol.TerminalInfo{Term: os.Getenv("TERM")})
}

// sendResize sends the current terminal size on the control stream.
func (c *Client) sendResize(conn transport.Conn) {
	if c.stdinFd < 0 {
		return
	}
	cols, rows, err := term.GetSize(c.stdinFd)
	if err != nil {
		return
	}
	conn.WriteControl(&protocol.Resize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	})
}

// ioLoop is the per-connection event loop. It blocks until the connection
// fails, the user types ~., the session shuts down, or the context is done.
func (c *Client) ioLoop(ctx context.Context, conn transport.Conn, stdinCh <-chan []byte) exitReason {
	if c.cfg.Profile {
		defer c.logProfileSummary(conn)
	}

	coal := coalesce.New()
	defer coal.Stop()

	controlCh := make(chan streamResult, 4)
	dataCh := make(chan streamResult, 4)

	go readStreamLoop(conn.ReadControl, controlCh)
	go readStreamLoop(conn.ReadData, dataCh)

	// SIGWINCH for terminal resize
	sigwinchCh := make(chan os.Signal, 1)
	if c.stdinFd >= 0 {
		signal.Notify(sigwinchCh, syscall.SIGWINCH)
		defer signal.Stop(sigwinchCh)
	}

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	lastRecv := time.Now()
	escapeBuf := make([]byte, stdinBufSize+2) // extra for held ~

	for {
		select {
		case data, ok := <-stdinCh:
			if !ok {
				c.flushStdinCoalesced(coal, conn) // best-effort; error ignored — we're exiting anyway
				return exitStdinEOF
			}
			n, action := c.escape.Process(data, escapeBuf)
			if action == EscDisconnect {
				return exitEscape // intentional disconnect, don't flush
			}
			if n > 0 {
				if coal.Add(escapeBuf[:n]) {
					if c.flushStdinCoalesced(coal, conn) != nil {
						return exitNetwork
					}
				}
			}

		case <-coal.Timer():
			if c.flushStdinCoalesced(coal, conn) != nil {
				return exitNetwork
			}

		case res := <-controlCh:
			if res.err != nil {
				return exitNetwork
			}
			lastRecv = time.Now()
			switch res.msg.(type) {
			case *protocol.Heartbeat:
				// Noted — no action needed
			case *protocol.Shutdown:
				return exitShutdown
			case *protocol.SequenceHeader:
				// unexpected mid-session; ignore
			}

		case res := <-dataCh:
			if res.err != nil {
				// Connection closing — drain the control channel briefly
				// in case a Shutdown message arrived before the error.
				if drainForShutdown(controlCh) {
					return exitShutdown
				}
				return exitNetwork
			}
			lastRecv = time.Now()
			if d, ok := res.msg.(*protocol.Data); ok {
				c.recvSeq = d.Seq
				c.stdout.Write(d.Payload)
			}

		case <-sigwinchCh:
			c.sendResize(conn)

		case <-heartbeat.C:
			if err := conn.WriteControl(&protocol.Heartbeat{
				TimestampMs: time.Now().UnixMilli(),
			}); err != nil {
				return exitNetwork
			}
			if time.Since(lastRecv) > recvTimeout {
				c.log.Warn("heartbeat timeout", "since_last_recv", time.Since(lastRecv))
				return exitNetwork
			}
			if c.cfg.Profile {
				c.logProfile(conn)
			}

		case <-ctx.Done():
			// Don't flush coalescer — at most 2ms of stdin lost, acceptable on cancellation
			return exitCancelled
		}
	}
}

// readStdin reads from stdin in a loop, sending chunks to ch.
// This goroutine is permanent — it survives reconnections.
func (c *Client) readStdin(ch chan<- []byte) {
	defer close(ch)
	for {
		buf := make([]byte, stdinBufSize)
		n, err := c.stdin.Read(buf)
		if n > 0 {
			ch <- buf[:n]
		}
		if err != nil {
			return
		}
	}
}

// flushStdinCoalesced sends any buffered coalesced stdin data, storing it in
// the catchup buffer and writing to the connection.
func (c *Client) flushStdinCoalesced(coal *coalesce.Coalescer, conn transport.Conn) error {
	data := coal.Flush()
	if data == nil {
		return nil
	}
	c.sendSeq++
	c.buf.Store(c.sendSeq, data)
	return conn.WriteData(&protocol.Data{
		Seq:     c.sendSeq,
		Payload: data,
	})
}

// streamResult carries a message or error from a stream reader goroutine.
type streamResult struct {
	msg any
	err error
}

// drainForShutdown checks the control channel for a buffered Shutdown message.
// When QUIC closes after the session sends Shutdown, the data stream may see
// the error before the control reader goroutine has a chance to deliver the
// Shutdown. This gives it a brief window to arrive.
func drainForShutdown(controlCh <-chan streamResult) bool {
	select {
	case res := <-controlCh:
		if res.err == nil {
			if _, ok := res.msg.(*protocol.Shutdown); ok {
				return true
			}
		}
	case <-time.After(100 * time.Millisecond):
	}
	return false
}

// readStreamLoop reads messages from a stream and sends them to ch.
func readStreamLoop(readFn func() (any, error), ch chan<- streamResult) {
	for {
		msg, err := readFn()
		if err != nil {
			ch <- streamResult{err: err}
			return
		}
		ch <- streamResult{msg: msg}
	}
}
