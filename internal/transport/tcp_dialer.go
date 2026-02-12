package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"

	"github.com/chronologos/goet/internal/auth"
	"github.com/chronologos/goet/internal/protocol"
)

// dialTCP connects to a session's TCP+TLS listener, authenticates with the
// passkey, and returns a Conn ready for use.
func dialTCP(ctx context.Context, host string, port int, passkey []byte, lastReceivedSeq uint64) (Conn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	dialer := &tls.Dialer{
		Config: ClientTLSConfig(),
	}

	rawConn, err := dialer.DialContext(ctx, "tcp4", addr)
	if err != nil {
		return nil, fmt.Errorf("TCP+TLS dial: %w", err)
	}

	tlsConn := rawConn.(*tls.Conn)

	conn, err := performTCPAuth(tlsConn, passkey, lastReceivedSeq)
	if err != nil {
		tlsConn.Close()
		return nil, err
	}

	return conn, nil
}

func performTCPAuth(tlsConn *tls.Conn, passkey []byte, lastReceivedSeq uint64) (*tcpConn, error) {
	// Compute auth token from TLS exporter material
	connState := tlsConn.ConnectionState()
	material, err := connState.ExportKeyingMaterial(
		"goet-auth-v1", nil, 32,
	)
	if err != nil {
		return nil, fmt.Errorf("export keying material: %w", err)
	}

	token := auth.ComputeAuthToken(passkey, material)

	// Send auth request
	if err := protocol.WriteMessage(tlsConn, &protocol.AuthRequest{
		Token: token,
	}); err != nil {
		return nil, fmt.Errorf("write auth request: %w", err)
	}

	// Read auth response
	msg, err := protocol.ReadMessage(tlsConn)
	if err != nil {
		return nil, fmt.Errorf("read auth response: %w", err)
	}

	resp, ok := msg.(*protocol.AuthResponse)
	if !ok {
		return nil, fmt.Errorf("expected AuthResponse, got %T", msg)
	}

	if resp.Status != protocol.AuthOK {
		return nil, fmt.Errorf("authentication rejected: status %d", resp.Status)
	}

	// Send SequenceHeader (same as QUIC path — tells session our last received seq)
	if err := protocol.WriteMessage(tlsConn, &protocol.SequenceHeader{
		LastReceivedSeq: lastReceivedSeq,
	}); err != nil {
		return nil, fmt.Errorf("write seq header: %w", err)
	}

	return newTCPConn(tlsConn, 0), nil
}
