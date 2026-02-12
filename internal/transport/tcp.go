package transport

import (
	"crypto/tls"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chronologos/goet/internal/protocol"
)

// tcpConn wraps a TLS-over-TCP connection that multiplexes control and data
// messages over a single stream. Before StartDemux() is called, reads are
// serial (for the handshake phase). After StartDemux(), a background goroutine
// reads all messages and routes them to controlCh or dataCh by message type.
type tcpConn struct {
	conn          *tls.Conn
	controlCh     chan readResult // buffered, capacity 16
	dataCh        chan readResult // buffered, capacity 16
	writeMu       sync.Mutex     // serializes all writes (control + data share one conn)
	lastClientSeq uint64
	demuxStarted  atomic.Bool
	done          chan struct{}
	closeOnce     sync.Once
}

// readResult carries a decoded message or error from the demux goroutine.
type readResult struct {
	msg any
	err error
}

// newTCPConn creates a tcpConn from an established TLS connection.
func newTCPConn(conn *tls.Conn, lastClientSeq uint64) *tcpConn {
	return &tcpConn{
		conn:          conn,
		controlCh:     make(chan readResult, 16),
		dataCh:        make(chan readResult, 16),
		lastClientSeq: lastClientSeq,
		done:          make(chan struct{}),
	}
}

// ReadControl reads the next control message.
// Before StartDemux: reads directly from the TLS conn (serial handshake).
// After StartDemux: pulls from the controlCh channel.
func (c *tcpConn) ReadControl() (any, error) {
	if !c.demuxStarted.Load() {
		return protocol.ReadMessage(c.conn)
	}
	res, ok := <-c.controlCh
	if !ok {
		return nil, fmt.Errorf("control channel closed")
	}
	return res.msg, res.err
}

// ReadData reads the next data message.
// Before StartDemux: reads directly from the TLS conn (serial handshake).
// After StartDemux: pulls from the dataCh channel.
func (c *tcpConn) ReadData() (any, error) {
	if !c.demuxStarted.Load() {
		return protocol.ReadMessage(c.conn)
	}
	res, ok := <-c.dataCh
	if !ok {
		return nil, fmt.Errorf("data channel closed")
	}
	return res.msg, res.err
}

// WriteControl writes a framed control message, serialized with data writes.
func (c *tcpConn) WriteControl(msg any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return protocol.WriteMessage(c.conn, msg)
}

// WriteData writes a framed data message, serialized with control writes.
func (c *tcpConn) WriteData(msg any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return protocol.WriteMessage(c.conn, msg)
}

// SetControlReadDeadline sets the read deadline on the underlying TLS conn.
// Only safe to call before StartDemux (during the serial handshake phase).
func (c *tcpConn) SetControlReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// LastClientSeq returns the client's last received sequence number.
func (c *tcpConn) LastClientSeq() uint64 {
	return c.lastClientSeq
}

// StartDemux transitions from serial reads to concurrent demuxed reads.
// Must be called exactly once, after the handshake is complete.
func (c *tcpConn) StartDemux() {
	if c.demuxStarted.Swap(true) {
		return // already started
	}
	// Clear any deadline left from the handshake phase.
	c.conn.SetReadDeadline(time.Time{})
	go c.demuxLoop()
}

// demuxLoop reads messages from the TLS conn and routes them to the
// appropriate channel based on message type. Exits when the conn is closed
// or an error occurs.
func (c *tcpConn) demuxLoop() {
	defer close(c.controlCh)
	defer close(c.dataCh)

	for {
		msg, err := protocol.ReadMessage(c.conn)
		if err != nil {
			// Send error to both channels so both ReadControl and ReadData unblock.
			// Use non-blocking sends — if a channel is full or nobody is reading,
			// we still need to send to the other channel.
			result := readResult{err: err}
			select {
			case c.controlCh <- result:
			default:
			}
			select {
			case c.dataCh <- result:
			default:
			}
			return
		}

		switch msg.(type) {
		case *protocol.Data:
			select {
			case c.dataCh <- readResult{msg: msg}:
			case <-c.done:
				return
			}
		default:
			// All non-Data messages are control messages
			select {
			case c.controlCh <- readResult{msg: msg}:
			case <-c.done:
				return
			}
		}
	}
}

// Close closes the underlying TLS connection, which unblocks the demux
// goroutine. Both channels will receive errors and then be closed.
func (c *tcpConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.done)
		err = c.conn.Close()
	})
	return err
}
