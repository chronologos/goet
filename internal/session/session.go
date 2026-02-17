package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/chronologos/goet/internal/catchup"
	"github.com/chronologos/goet/internal/coalesce"
	"github.com/chronologos/goet/internal/protocol"
	"github.com/chronologos/goet/internal/transport"
)

const (
	ptyReadBufSize    = 32 * 1024 // 32 KB per PTY read
	heartbeatInterval = 5 * time.Second
	recvTimeout       = 30 * time.Second // detect dead clients (matches QUIC MaxIdleTimeout)
	shellExitGrace    = 2 * time.Second  // wait for shell after SIGHUP before SIGKILL
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
	conn   transport.Conn // which connection produced this event
	stream string         // "control" or "data"
	msg    any            // decoded message (nil if error)
	err    error          // read error (nil if message)
}

// Session is the server-side half of a reconnectable terminal.
// It accepts QUIC connections, spawns a PTY on the first client's
// TerminalInfo, relays terminal I/O, and handles reconnection with
// catchup replay.
type Session struct {
	cfg     Config
	log     *slog.Logger
	ptmx    *os.File
	cmd     *exec.Cmd
	ln      transport.Listener
	buf     *catchup.Buffer
	coal    *coalesce.Coalescer
	conn    transport.Conn // current connection (nil when disconnected)
	sendSeq uint64         // next outbound data sequence
	recvSeq uint64         // last received data sequence from client

	// PTY lifecycle — nil until first client connects with TerminalInfo.
	ptyDataCh chan []byte    // closed when PTY read goroutine exits
	shellDone chan struct{}  // closed when shell exits
	shellErr  error         // set before shellDone is closed

	// Ready is closed after the listener is bound, with Port set.
	// Callers (tests, CLI) can wait on this before dialing.
	Ready chan struct{}
	Port  int
}

// New creates a session but does not start it. Call Run() to begin.
func New(cfg Config) *Session {
	return &Session{
		cfg:   cfg,
		log:   slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", "session"),
		buf:   catchup.New(0), // default 64 MB
		Ready: make(chan struct{}),
	}
}

// Run is the session's main loop. It starts the QUIC listener, defers PTY
// spawn until the first client connects with TerminalInfo, and runs until
// the shell exits, the context is cancelled, or a client sends Shutdown.
func (s *Session) Run(ctx context.Context) error {
	// Start dual listener (QUIC + TCP on same port)
	ln, err := transport.ListenDual(s.cfg.Port, s.cfg.Passkey)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	s.ln = ln

	defer func() {
		s.closeConn()
		s.ln.Close()
		s.cleanupShell()
	}()

	// Signal readiness — set port and close channel so waiters unblock.
	s.Port = s.ln.Port()
	close(s.Ready)

	// --- Write coalescer ---

	s.coal = coalesce.New()
	defer s.coal.Stop()

	// --- Event loop ---
	// PTY channels start nil — their select cases are inactive until
	// the first client connects and sends TerminalInfo.

	var ptyDataCh <-chan []byte   // nil until PTY spawned
	var shellDone <-chan struct{} // nil until PTY spawned

	acceptCh := make(chan acceptResult, 1)
	go s.acceptLoop(ctx, acceptCh)

	streamCh := make(chan streamEvent, 8)
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	lastRecv := time.Now()

	for {
		select {
		case data, ok := <-ptyDataCh:
			if !ok {
				// PTY closed — flush pending data before disabling channel
				s.flushCoalesced()
				ptyDataCh = nil
				continue
			}
			if s.coal.Add(data) {
				s.flushCoalesced()
			}

		case <-s.coal.Timer():
			s.flushCoalesced()

		case ev := <-streamCh:
			if ev.conn == s.conn && ev.err == nil {
				lastRecv = time.Now()
			}
			if err := s.handleStreamEvent(ev); err != nil {
				return err
			}

		case res := <-acceptCh:
			if res.err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				s.log.Warn("accept error", "err", res.err)
			} else {
				if err := s.handleNewConn(res.conn, streamCh); err != nil {
					return fmt.Errorf("handle new conn: %w", err)
				}
				lastRecv = time.Now()
				// After handleNewConn, PTY channels may have been created.
				if s.ptyDataCh != nil && ptyDataCh == nil {
					ptyDataCh = s.ptyDataCh
					shellDone = s.shellDone
				}
			}
			// Re-arm accept loop
			go s.acceptLoop(ctx, acceptCh)

		case <-heartbeat.C:
			if s.conn != nil {
				if err := s.conn.WriteControl(&protocol.Heartbeat{
					TimestampMs: time.Now().UnixMilli(),
				}); err != nil {
					s.log.Warn("heartbeat write failed", "err", err)
					s.closeConn()
				} else if time.Since(lastRecv) > recvTimeout {
					s.log.Warn("client receive timeout", "since_last_recv", time.Since(lastRecv))
					s.closeConn()
				}
			}

		case <-shellDone:
			s.gracefulShutdown()
			if s.shellErr != nil {
				return fmt.Errorf("shell exited: %w", s.shellErr)
			}
			return nil

		case <-ctx.Done():
			s.gracefulShutdown()
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

// waitShell waits for the shell process to exit and signals via shellDone.
func (s *Session) waitShell() {
	s.shellErr = s.cmd.Wait()
	close(s.shellDone)
}

// cleanupShell gracefully shuts down the PTY and shell process.
// Closing ptmx sends SIGHUP to the shell's process group, giving it a
// chance to save history and run EXIT traps. Falls back to SIGKILL after
// shellExitGrace if the shell hasn't exited.
func (s *Session) cleanupShell() {
	if s.ptmx == nil {
		return
	}
	s.ptmx.Close()

	// Wait for shell to exit gracefully (SIGHUP from ptmx.Close).
	select {
	case <-s.shellDone:
		return
	case <-time.After(shellExitGrace):
	}

	// Shell didn't exit in time — force kill.
	if s.cmd.Process != nil {
		s.cmd.Process.Signal(syscall.SIGKILL)
	}
}

// acceptResult carries the result of a single Accept call.
type acceptResult struct {
	conn transport.Conn
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
func readStream(conn transport.Conn, stream string, ch chan<- streamEvent) {
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
		s.log.Info("stream closed", "stream", ev.stream, "err", ev.err)
		s.closeConn()
		return nil
	}

	switch msg := ev.msg.(type) {
	case *protocol.Resize:
		if s.ptmx != nil {
			if err := resizePTY(s.ptmx, msg.Rows, msg.Cols); err != nil {
				s.log.Warn("resize pty failed", "err", err)
			}
		}
	case *protocol.Data:
		if s.ptmx != nil {
			if _, err := s.ptmx.Write(msg.Payload); err != nil {
				s.log.Warn("pty write failed", "err", err)
				break // don't advance recvSeq — client will resend on reconnect
			}
		}
		// Update recvSeq only after successful write so the client
		// retains the data in its catchup buffer for replay on reconnect.
		s.recvSeq = msg.Seq
	case *protocol.Heartbeat:
		// Noted — no action needed
	case *protocol.Shutdown:
		return fmt.Errorf("client sent shutdown")
	case *protocol.SequenceHeader:
		// Unexpected mid-session seq header; ignore
	case *protocol.TerminalInfo:
		// Late TerminalInfo on reconnect — no-op (PTY already spawned)
	default:
		s.log.Warn("unknown message type", "type", fmt.Sprintf("%T", ev.msg))
	}

	return nil
}

// handleNewConn processes a newly accepted connection: closes the old one,
// reads TerminalInfo, spawns the PTY (on first connect), exchanges sequence
// headers, replays catchup data, bounces PTY size, and starts stream reader
// goroutines. Returns a non-nil error only if PTY spawn fails (fatal).
func (s *Session) handleNewConn(newConn transport.Conn, streamCh chan<- streamEvent) error {
	// Close old connection — its reader goroutines will exit with errors
	// and be discarded by the connection-tag check in handleStreamEvent.
	s.closeConn()

	// Flush coalesced data into catchup buffer before setting new conn.
	// Must happen while s.conn is still nil so data is stored in catchup
	// (for replay) without also being written to the new connection directly,
	// which would cause duplicate delivery.
	s.flushCoalesced()

	s.conn = newConn

	// Send our SequenceHeader on control stream (tells client what we've
	// received, so client knows what to resend from its own catchup buffer).
	if err := s.conn.WriteControl(&protocol.SequenceHeader{
		LastReceivedSeq: s.recvSeq,
	}); err != nil {
		s.log.Warn("write seq header failed", "err", err)
		s.closeConn()
		return nil
	}

	// Read TerminalInfo from client (before reader goroutines start).
	// Deadline prevents a misbehaving client from stalling the event loop.
	s.conn.SetControlReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := s.conn.ReadControl()
	s.conn.SetControlReadDeadline(time.Time{})
	if err != nil {
		s.log.Warn("read terminal info failed", "err", err)
		s.closeConn()
		return nil
	}
	ti, ok := msg.(*protocol.TerminalInfo)
	if !ok {
		s.log.Warn("unexpected handshake message", "expected", "TerminalInfo", "got", fmt.Sprintf("%T", msg))
		s.closeConn()
		return nil
	}

	// Spawn PTY on first connect, using client's TERM.
	if s.ptmx == nil {
		ptmx, cmd, err := spawnPTY(s.cfg.SessionID, ti.Term)
		if err != nil {
			s.closeConn()
			return fmt.Errorf("spawn PTY: %w", err)
		}
		s.ptmx = ptmx
		s.cmd = cmd
		s.ptyDataCh = make(chan []byte, 4)
		s.shellDone = make(chan struct{})
		go s.readPTY(s.ptyDataCh)
		go s.waitShell()
	}

	// Replay catchup: send all data the client missed.
	// newConn.LastClientSeq() is what the client told us it last received.
	entries := s.buf.ReplaySince(newConn.LastClientSeq())
	for _, e := range entries {
		if err := s.conn.WriteData(&protocol.Data{
			Seq:     e.Seq,
			Payload: e.Payload,
		}); err != nil {
			s.log.Warn("catchup replay failed", "err", err)
			s.closeConn()
			return nil
		}
	}

	// Bounce PTY size: shrink by 1 row. Client will send its real size
	// immediately, causing a genuine size change → SIGWINCH → TUI redraw.
	if err := bouncePTYSize(s.ptmx); err != nil {
		s.log.Warn("bounce pty size failed", "err", err)
	}

	// Start demux (no-op for QUIC; starts background reader for TCP)
	s.conn.StartDemux()

	// Start reader goroutines for the new connection
	go readStream(s.conn, "control", streamCh)
	go readStream(s.conn, "data", streamCh)

	return nil
}

// gracefulShutdown flushes pending data and notifies the client before teardown.
// The short sleep gives QUIC time to flush the Shutdown frame before the
// deferred conn.Close sends CONNECTION_CLOSE.
func (s *Session) gracefulShutdown() {
	s.flushCoalesced()
	if s.conn != nil {
		s.conn.WriteControl(&protocol.Shutdown{})
		time.Sleep(50 * time.Millisecond)
	}
}

// flushCoalesced sends any buffered coalesced data, storing it in the catchup
// buffer and writing to the current connection (if any).
func (s *Session) flushCoalesced() {
	data := s.coal.Flush()
	if data == nil {
		return
	}
	s.sendSeq++
	s.buf.Store(s.sendSeq, data)
	if s.conn != nil {
		if err := s.conn.WriteData(&protocol.Data{
			Seq:     s.sendSeq,
			Payload: data,
		}); err != nil {
			s.log.Warn("write coalesced failed", "err", err)
			s.closeConn()
		}
	}
}

// closeConn closes the current connection if any.
func (s *Session) closeConn() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}
