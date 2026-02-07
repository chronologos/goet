package client

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/iantay/goet/internal/catchup"
	"github.com/iantay/goet/internal/protocol"
	"github.com/iantay/goet/internal/transport"
)

const (
	stdinBufSize      = 32 * 1024 // 32 KB per stdin read
	heartbeatInterval = 5 * time.Second
	recvTimeout       = 15 * time.Second
	reconnectDelay    = 1 * time.Second
)

// Config holds client configuration.
type Config struct {
	Host    string
	Port    int
	Passkey []byte
}

// Client is the terminal-facing half of a reconnectable terminal session.
// It connects to a session via QUIC, relays stdin/stdout, handles the ~.
// escape sequence, and reconnects with catchup replay on network failures.
type Client struct {
	cfg     Config
	buf     *catchup.Buffer
	escape  *EscapeProcessor
	sendSeq uint64
	recvSeq uint64
	stdin   io.Reader
	stdout  io.Writer
	stdinFd int // for MakeRaw/Restore; -1 if pipe (skip raw mode)
}

// New creates a client with the given config. Uses os.Stdin/os.Stdout
// for terminal I/O. If stdin is not a terminal (pipe, FIFO), raw mode
// is skipped automatically. For testing, use newTestClient instead.
func New(cfg Config) *Client {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		fd = -1
	}
	return &Client{
		cfg:     cfg,
		buf:     catchup.New(0),
		escape:  NewEscapeProcessor(),
		stdin:   os.Stdin,
		stdout:  os.Stdout,
		stdinFd: fd,
	}
}

// newTestClient creates a client wired to pipes instead of the real terminal.
// stdinFd is set to -1 to skip MakeRaw (pipes aren't terminals).
func newTestClient(cfg Config, stdin io.Reader, stdout io.Writer) *Client {
	return &Client{
		cfg:     cfg,
		buf:     catchup.New(0),
		escape:  NewEscapeProcessor(),
		stdin:   stdin,
		stdout:  stdout,
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
			log.Printf("client: connect failed: %v, retrying in %v", err, reconnectDelay)
			select {
			case <-time.After(reconnectDelay):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if err := c.handleSequenceExchange(conn); err != nil {
			log.Printf("client: sequence exchange failed: %v, retrying", err)
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

		reason := c.ioLoop(ctx, conn, stdinCh)

		// Restore terminal before logging or sleeping
		if oldState != nil {
			term.Restore(c.stdinFd, oldState)
		}

		switch reason {
		case exitEscape:
			conn.WriteControl(&protocol.Shutdown{}) // best-effort
			conn.Close()
			return nil
		case exitShutdown:
			conn.Close()
			return nil
		case exitStdinEOF:
			conn.WriteControl(&protocol.Shutdown{}) // best-effort
			conn.Close()
			return nil
		case exitCancelled:
			conn.WriteControl(&protocol.Shutdown{}) // best-effort
			conn.Close()
			return ctx.Err()
		case exitNetwork:
			conn.Close()
			log.Printf("client: connection lost, reconnecting in %v", reconnectDelay)
			select {
			case <-time.After(reconnectDelay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// dial connects to the session with the current recvSeq for catchup.
func (c *Client) dial(ctx context.Context) (*transport.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return transport.Dial(dialCtx, c.cfg.Host, c.cfg.Port, c.cfg.Passkey, c.recvSeq)
}

// handleSequenceExchange reads the session's SequenceHeader from the control
// stream and resends any data the session missed from our catchup buffer.
func (c *Client) handleSequenceExchange(conn *transport.Conn) error {
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

// sendResize sends the current terminal size on the control stream.
func (c *Client) sendResize(conn *transport.Conn) {
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
func (c *Client) ioLoop(ctx context.Context, conn *transport.Conn, stdinCh <-chan []byte) exitReason {
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
				return exitStdinEOF
			}
			n, action := c.escape.Process(data, escapeBuf)
			if action == EscDisconnect {
				return exitEscape
			}
			if n > 0 {
				c.sendSeq++
				payload := make([]byte, n)
				copy(payload, escapeBuf[:n])
				c.buf.Store(c.sendSeq, payload)
				if err := conn.WriteData(&protocol.Data{
					Seq:     c.sendSeq,
					Payload: payload,
				}); err != nil {
					return exitNetwork
				}
			}

		case res := <-controlCh:
			if res.err != nil {
				return exitNetwork
			}
			lastRecv = time.Now()
			switch res.msg.(type) {
			case *protocol.Heartbeat:
				// noted
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
				log.Printf("client: heartbeat timeout (%v since last recv)", time.Since(lastRecv))
				return exitNetwork
			}

		case <-ctx.Done():
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
