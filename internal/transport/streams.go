package transport

import (
	"github.com/quic-go/quic-go"

	"github.com/iantay/goet/internal/protocol"
)

// Conn wraps a QUIC connection with its control and data streams.
// After a successful handshake, both streams are ready for framed
// message I/O via protocol.WriteMessage / protocol.ReadMessage.
type Conn struct {
	QConn         *quic.Conn
	Control       *quic.Stream // stream 0: auth, resize, heartbeat, shutdown, seq headers
	Data          *quic.Stream // stream 1: terminal stdin/stdout with sequence numbers
	LastClientSeq uint64       // from client's SequenceHeader (0 on first connect)
	tr            *quic.Transport // keep alive to prevent GC of underlying UDP socket
}

// Close closes both streams and the underlying QUIC connection.
func (c *Conn) Close() error {
	if c.Control != nil {
		c.Control.CancelRead(0)
		c.Control.Close()
	}
	if c.Data != nil {
		c.Data.CancelRead(0)
		c.Data.Close()
	}
	if c.QConn != nil {
		c.QConn.CloseWithError(0, "closed")
	}
	if c.tr != nil {
		return c.tr.Close()
	}
	return nil
}

// WriteControl writes a framed message to the control stream.
func (c *Conn) WriteControl(msg any) error {
	return protocol.WriteMessage(c.Control, msg)
}

// ReadControl reads a framed message from the control stream.
func (c *Conn) ReadControl() (any, error) {
	return protocol.ReadMessage(c.Control)
}

// WriteData writes a framed message to the data stream.
func (c *Conn) WriteData(msg any) error {
	return protocol.WriteMessage(c.Data, msg)
}

// ReadData reads a framed message from the data stream.
func (c *Conn) ReadData() (any, error) {
	return protocol.ReadMessage(c.Data)
}

// ExportKeyingMaterial derives keying material from the TLS session,
// used for HMAC-based authentication.
func (c *Conn) ExportKeyingMaterial() ([]byte, error) {
	state := c.QConn.ConnectionState()
	return state.TLS.ExportKeyingMaterial(
		"goet-auth-v1", nil, 32,
	)
}
