package transport

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/chronologos/goet/internal/auth"
	"github.com/chronologos/goet/internal/protocol"
)

// Dial connects to a session's QUIC listener, authenticates with the passkey,
// and returns a Conn with control and data streams ready for use.
// lastReceivedSeq is the last data sequence number received from the session
// (0 on first connect, >0 on reconnect for catchup).
func Dial(ctx context.Context, host string, port int, passkey []byte, lastReceivedSeq uint64) (*Conn, error) {
	addr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("resolve %s:%d: %w", host, port, err)
	}

	// Use a fresh UDP socket for the client
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, fmt.Errorf("listen UDP: %w", err)
	}

	tr := &quic.Transport{Conn: udpConn}
	tlsConf := ClientTLSConfig()
	quicConf := &quic.Config{
		MaxIdleTimeout:    30 * time.Second,
		InitialPacketSize: 1200, // Tailscale MTU is 1280; default 1350 gets dropped
	}

	qconn, err := tr.Dial(ctx, addr, tlsConf, quicConf)
	if err != nil {
		tr.Close()
		return nil, fmt.Errorf("QUIC dial: %w", err)
	}

	conn, err := performAuth(ctx, qconn, passkey, lastReceivedSeq)
	if err != nil {
		qconn.CloseWithError(1, "auth failed")
		tr.Close()
		return nil, err
	}

	conn.tr = tr
	return conn, nil
}

func performAuth(ctx context.Context, qconn *quic.Conn, passkey []byte, lastReceivedSeq uint64) (*Conn, error) {
	// Open control stream (stream 0)
	controlStream, err := qconn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("open control stream: %w", err)
	}

	// Compute auth token from TLS exporter material
	connState := qconn.ConnectionState()
	material, err := connState.TLS.ExportKeyingMaterial(
		"goet-auth-v1", nil, 32,
	)
	if err != nil {
		return nil, fmt.Errorf("export keying material: %w", err)
	}

	token := auth.ComputeAuthToken(passkey, material)

	// Send auth request
	if err := protocol.WriteMessage(controlStream, &protocol.AuthRequest{
		Token: token,
	}); err != nil {
		return nil, fmt.Errorf("write auth request: %w", err)
	}

	// Read auth response
	msg, err := protocol.ReadMessage(controlStream)
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

	// Open data stream (stream 1).
	// Write SequenceHeader immediately — this serves dual purpose:
	// 1. Announces the stream to the server (QUIC doesn't send STREAM frames until first Write)
	// 2. Tells the server our last received sequence (0 for first connect, >0 for reconnect)
	dataStream, err := qconn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("open data stream: %w", err)
	}
	if err := protocol.WriteMessage(dataStream, &protocol.SequenceHeader{
		LastReceivedSeq: lastReceivedSeq,
	}); err != nil {
		return nil, fmt.Errorf("write data stream seq header: %w", err)
	}

	return &Conn{
		QConn:   qconn,
		Control: controlStream,
		Data:    dataStream,
	}, nil
}
