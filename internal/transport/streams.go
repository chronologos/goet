package transport

import (
	"time"

	"github.com/quic-go/quic-go"

	"github.com/chronologos/goet/internal/protocol"
)

// quicConn wraps a QUIC connection with its control and data streams.
// After a successful handshake, both streams are ready for framed
// message I/O via protocol.WriteMessage / protocol.ReadMessage.
type quicConn struct {
	qconn         *quic.Conn
	control       *quic.Stream // stream 0: auth, resize, heartbeat, shutdown, seq headers
	data          *quic.Stream // stream 1: terminal stdin/stdout with sequence numbers
	lastClientSeq uint64       // from client's SequenceHeader (0 on first connect)
	tr            *quic.Transport // keep alive to prevent GC of underlying UDP socket
}

// Close closes both streams and the underlying QUIC connection.
func (c *quicConn) Close() error {
	if c.control != nil {
		c.control.CancelRead(0)
		c.control.Close()
	}
	if c.data != nil {
		c.data.CancelRead(0)
		c.data.Close()
	}
	if c.qconn != nil {
		c.qconn.CloseWithError(0, "closed")
	}
	if c.tr != nil {
		return c.tr.Close()
	}
	return nil
}

// WriteControl writes a framed message to the control stream.
func (c *quicConn) WriteControl(msg any) error {
	return protocol.WriteMessage(c.control, msg)
}

// ReadControl reads a framed message from the control stream.
func (c *quicConn) ReadControl() (any, error) {
	return protocol.ReadMessage(c.control)
}

// WriteData writes a framed message to the data stream.
func (c *quicConn) WriteData(msg any) error {
	return protocol.WriteMessage(c.data, msg)
}

// ReadData reads a framed message from the data stream.
func (c *quicConn) ReadData() (any, error) {
	return protocol.ReadMessage(c.data)
}

// SetControlReadDeadline sets the read deadline on the control stream.
func (c *quicConn) SetControlReadDeadline(t time.Time) error {
	return c.control.SetReadDeadline(t)
}

// LastClientSeq returns the client's last received sequence number
// from the SequenceHeader sent during the handshake.
func (c *quicConn) LastClientSeq() uint64 {
	return c.lastClientSeq
}

// StartDemux is a no-op for QUIC — streams are independently readable.
func (c *quicConn) StartDemux() {}

// ConnectionStats returns QUIC-level connection statistics.
// Satisfies the ProfileableConn optional interface.
func (c *quicConn) ConnectionStats() quic.ConnectionStats {
	return c.qconn.ConnectionStats()
}

// ExportKeyingMaterial derives keying material from the TLS session,
// used for HMAC-based authentication.
func (c *quicConn) ExportKeyingMaterial() ([]byte, error) {
	state := c.qconn.ConnectionState()
	return state.TLS.ExportKeyingMaterial(
		"goet-auth-v1", nil, 32,
	)
}

// WriteRawData writes raw bytes to the data stream, bypassing protocol framing.
// This is only used by tests to inject malformed messages.
func (c *quicConn) WriteRawData(p []byte) (int, error) {
	return c.data.Write(p)
}
