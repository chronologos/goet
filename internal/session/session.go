package session

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/chronologos/goet/internal/catchup"
	"github.com/chronologos/goet/internal/protocol"
	"github.com/chronologos/goet/internal/transport"
)

const (
	ptyReadBufSize    = 32 * 1024 // 32 KB per PTY read
	heartbeatInterval = 5 * time.Second
)

// Config holds session configuration.
type Config struct {
	SessionID string
	Port      int
	Passkey   []byte
}

// streamEvent is a tagged message from a connection's stream reader goroutine.
// Tagging with the source connection lets the select loop discard stale events
// from old connections after a reconnect.
type streamEvent struct {
	conn   *transport.Conn // which connection produced this event
	stream string          // "control" or "data"
	msg    any             // decoded message (nil if error)
	err    error           // read error (nil if message)
}

// Session is the server-side half of a reconnectable terminal.
// It spawns a PTY, accepts QUIC connections, relays terminal I/O,
// and handles reconnection with catchup replay.
type Session struct {
	cfg     Config
	ptmx    *os.File
	cmd     *exec.Cmd
	ln      *transport.Listener
	buf     *catchup.Buffer
	conn    *transport.Conn // current connection (nil when disconnected)
	sendSeq uint64          // next outbound data sequence
	recvSeq uint64          // last received data sequence from client

	// Ready is closed after the listener is bound, with Port set.
	// Callers (tests, CLI) can wait on this before dialing.
	Ready chan struct{}
	Port  int
}

// New creates a session but does not start it. Call Run() to begin.
func New(cfg Config) *Session {
	return &Session{
		cfg:   cfg,
		buf:   catchup.New(0), // default 64 MB
		Ready: make(chan struct{}),
	}
}

// Run is the session's main loop. It spawns the PTY, starts the QUIC listener,
// and runs until the shell exits, the context is cancelled, or a client sends
// Shutdown.
func (s *Session) Run(ctx context.Context) error {
	// Spawn PTY with shell
	ptmx, cmd, err := spawnPTY(s.cfg.SessionID)
	if err != nil {
		return fmt.Errorf("spawn PTY: %w", err)
	}
	s.ptmx = ptmx
	s.cmd = cmd

	// Start QUIC listener
	ln, err := transport.Listen(s.cfg.Port, s.cfg.Passkey)
	if err != nil {
		s.ptmx.Close()
		s.cmd.Process.Kill()
		return fmt.Errorf("listen: %w", err)
	}
	s.ln = ln

	// Single defer for cleanup — order matters: close QUIC conn first
	// (sends CONNECTION_CLOSE to client), then close listener transport,
	// then PTY and shell.
	defer func() {
		s.closeConn()
		s.ln.Close()
		s.ptmx.Close()
		if s.cmd.Process != nil {
			s.cmd.Process.Kill()
		}
	}()

	// Signal readiness — set port and close channel so waiters unblock.
	s.Port = s.ln.Port()
	close(s.Ready)

	// --- Permanent goroutines ---

	ptyDataCh := make(chan []byte, 4)
	go s.readPTY(ptyDataCh)

	shellDoneCh := make(chan error, 1)
	go s.waitShell(shellDoneCh)

	acceptCh := make(chan acceptResult, 1)
	go s.acceptLoop(ctx, acceptCh)

	// --- Event loop ---

	streamCh := make(chan streamEvent, 8)
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case data, ok := <-ptyDataCh:
			if !ok {
				// PTY closed — shell is exiting, wait for shellDoneCh
				ptyDataCh = nil
				continue
			}
			s.sendSeq++
			s.buf.Store(s.sendSeq, data)
			if s.conn != nil {
				if err := s.conn.WriteData(&protocol.Data{
					Seq:     s.sendSeq,
					Payload: data,
				}); err != nil {
					log.Printf("session: write to client: %v", err)
					s.closeConn()
				}
			}

		case ev := <-streamCh:
			if err := s.handleStreamEvent(ev); err != nil {
				return err
			}

		case res := <-acceptCh:
			if res.err != nil {
				// Accept errors are often transient (bad auth, etc.)
				// Log and continue accepting.
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.Printf("session: accept error: %v", res.err)
			} else {
				s.handleNewConn(res.conn, streamCh)
			}
			// Re-arm accept loop
			go s.acceptLoop(ctx, acceptCh)

		case <-heartbeat.C:
			if s.conn != nil {
				if err := s.conn.WriteControl(&protocol.Heartbeat{
					TimestampMs: time.Now().UnixMilli(),
				}); err != nil {
					log.Printf("session: heartbeat write: %v", err)
					s.closeConn()
				}
			}

		case err := <-shellDoneCh:
			// Shell exited — notify client and return.
			// Short delay gives QUIC time to flush the Shutdown frame
			// before the deferred conn.Close sends CONNECTION_CLOSE.
			if s.conn != nil {
				s.conn.WriteControl(&protocol.Shutdown{})
				time.Sleep(50 * time.Millisecond)
			}
			if err != nil {
				return fmt.Errorf("shell exited: %w", err)
			}
			return nil

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// --- Goroutines ---

// readPTY continuously reads from the PTY master and sends chunks to ptyDataCh.
// Exits when PTY is closed (shell exit or cleanup).
func (s *Session) readPTY(ch chan<- []byte) {
	defer close(ch)
	for {
		buf := make([]byte, ptyReadBufSize)
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			ch <- buf[:n]
		}
		if err != nil {
			return
		}
	}
}

// waitShell waits for the shell process to exit.
func (s *Session) waitShell(ch chan<- error) {
	ch <- s.cmd.Wait()
}

// acceptResult carries the result of a single Accept call.
type acceptResult struct {
	conn *transport.Conn
	err  error
}

// acceptLoop calls Accept once and sends the result. Each call to acceptLoop
// handles exactly one connection attempt — the main loop re-arms it after
// processing the result.
func (s *Session) acceptLoop(ctx context.Context, ch chan<- acceptResult) {
	conn, err := s.ln.Accept(ctx)
	ch <- acceptResult{conn: conn, err: err}
}

// readStream reads framed messages from a stream and sends them as events.
// Exits when the stream returns an error (including connection close).
func readStream(conn *transport.Conn, stream string, ch chan<- streamEvent) {
	var readFn func() (any, error)
	if stream == "control" {
		readFn = conn.ReadControl
	} else {
		readFn = conn.ReadData
	}
	for {
		msg, err := readFn()
		if err != nil {
			ch <- streamEvent{conn: conn, stream: stream, err: err}
			return
		}
		ch <- streamEvent{conn: conn, stream: stream, msg: msg}
	}
}

// --- Event handlers ---

// handleStreamEvent processes a single stream event, returning a non-nil error
// only when the session should terminate (Shutdown received).
func (s *Session) handleStreamEvent(ev streamEvent) error {
	// Discard events from old connections
	if ev.conn != s.conn {
		return nil
	}

	if ev.err != nil {
		log.Printf("session: %s stream error: %v", ev.stream, ev.err)
		s.closeConn()
		return nil
	}

	switch msg := ev.msg.(type) {
	case *protocol.Resize:
		if err := resizePTY(s.ptmx, msg.Rows, msg.Cols); err != nil {
			log.Printf("session: resize PTY: %v", err)
		}
	case *protocol.Data:
		s.recvSeq = msg.Seq
		if _, err := s.ptmx.Write(msg.Payload); err != nil {
			log.Printf("session: write to PTY: %v", err)
		}
	case *protocol.Heartbeat:
		// Noted — no action needed
	case *protocol.Shutdown:
		return fmt.Errorf("client sent shutdown")
	case *protocol.SequenceHeader:
		// Unexpected mid-session seq header; ignore
	default:
		log.Printf("session: unknown message type: %T", ev.msg)
	}

	return nil
}

// handleNewConn processes a newly accepted connection: closes the old one,
// exchanges sequence headers, replays catchup data, bounces PTY size,
// and starts stream reader goroutines.
func (s *Session) handleNewConn(newConn *transport.Conn, streamCh chan<- streamEvent) {
	// Close old connection — its reader goroutines will exit with errors
	// and be discarded by the connection-tag check in handleStreamEvent.
	s.closeConn()

	s.conn = newConn

	// Send our SequenceHeader on control stream (tells client what we've
	// received, so client knows what to resend from its own catchup buffer).
	if err := s.conn.WriteControl(&protocol.SequenceHeader{
		LastReceivedSeq: s.recvSeq,
	}); err != nil {
		log.Printf("session: write seq header: %v", err)
		s.closeConn()
		return
	}

	// Replay catchup: send all data the client missed.
	// newConn.LastClientSeq is what the client told us it last received.
	entries := s.buf.ReplaySince(newConn.LastClientSeq)
	for _, e := range entries {
		if err := s.conn.WriteData(&protocol.Data{
			Seq:     e.Seq,
			Payload: e.Payload,
		}); err != nil {
			log.Printf("session: catchup replay: %v", err)
			s.closeConn()
			return
		}
	}

	// Bounce PTY size: shrink by 1 row. Client will send its real size
	// immediately, causing a genuine size change → SIGWINCH → TUI redraw.
	if err := bouncePTYSize(s.ptmx); err != nil {
		log.Printf("session: bounce PTY size: %v", err)
	}

	// Start reader goroutines for the new connection
	go readStream(s.conn, "control", streamCh)
	go readStream(s.conn, "data", streamCh)
}

// closeConn closes the current connection if any.
func (s *Session) closeConn() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}
