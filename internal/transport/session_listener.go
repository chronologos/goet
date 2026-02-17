package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/chronologos/goet/internal/auth"
	"github.com/chronologos/goet/internal/protocol"
)

// quicListener wraps a QUIC listener for the session side.
type quicListener struct {
	tr      *quic.Transport
	ln      *quic.Listener
	port    int
	passkey []byte
}

// Listen creates a QUIC listener on a random UDP port (or the specified port).
// The session uses this to accept client connections.
func Listen(port int, passkey []byte) (Listener, error) {
	cert, err := GenerateSelfSignedCert()
	if err != nil {
		return nil, fmt.Errorf("generate TLS cert: %w", err)
	}
	return listenQUIC(port, passkey, cert)
}

// listenQUIC creates a QUIC listener using the provided TLS certificate.
func listenQUIC(port int, passkey []byte, cert tls.Certificate) (*quicListener, error) {
	addr := &net.UDPAddr{IP: net.IPv4zero, Port: port}
	udpConn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("listen UDP: %w", err)
	}

	tr := &quic.Transport{Conn: udpConn}
	tlsConf := ServerTLSConfig(cert)
	quicConf := &quic.Config{
		MaxIdleTimeout:    30 * time.Second,
		KeepAlivePeriod:   10 * time.Second, // send PINGs to detect dead connections faster after sleep/wake
		InitialPacketSize: 1200,             // Tailscale MTU is 1280; default 1350 gets dropped
	}

	ln, err := tr.Listen(tlsConf, quicConf)
	if err != nil {
		udpConn.Close()
		return nil, fmt.Errorf("QUIC listen: %w", err)
	}

	localPort := udpConn.LocalAddr().(*net.UDPAddr).Port

	return &quicListener{
		tr:      tr,
		ln:      ln,
		port:    localPort,
		passkey: passkey,
	}, nil
}

// Port returns the UDP port the listener is bound to.
func (l *quicListener) Port() int {
	return l.port
}

// Accept waits for and authenticates a new client connection.
// Returns a Conn with control and data streams ready for use.
func (l *quicListener) Accept(ctx context.Context) (Conn, error) {
	qconn, err := l.ln.Accept(ctx)
	if err != nil {
		return nil, fmt.Errorf("accept QUIC connection: %w", err)
	}

	conn, err := l.authenticate(ctx, qconn)
	if err != nil {
		qconn.CloseWithError(1, "auth failed")
		return nil, err
	}

	return conn, nil
}

func (l *quicListener) authenticate(ctx context.Context, qconn *quic.Conn) (*quicConn, error) {
	// Accept control stream (opened by client).
	// Timeout prevents a misbehaving client from blocking the accept path
	// by completing the TLS handshake but never opening a stream.
	authCtx, authCancel := context.WithTimeout(ctx, 5*time.Second)
	defer authCancel()
	controlStream, err := qconn.AcceptStream(authCtx)
	if err != nil {
		return nil, fmt.Errorf("accept control stream: %w", err)
	}

	// Read auth request.
	// Deadline prevents a client from stalling after opening the stream.
	controlStream.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := protocol.ReadMessage(controlStream)
	controlStream.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, fmt.Errorf("read auth request: %w", err)
	}

	authReq, ok := msg.(*protocol.AuthRequest)
	if !ok {
		return nil, fmt.Errorf("expected AuthRequest, got %T", msg)
	}

	// Verify HMAC token
	connState := qconn.ConnectionState()
	material, err := connState.TLS.ExportKeyingMaterial(
		"goet-auth-v1", nil, 32,
	)
	if err != nil {
		return nil, fmt.Errorf("export keying material: %w", err)
	}

	if !auth.VerifyAuthToken(l.passkey, material, authReq.Token) {
		// Send rejection
		protocol.WriteMessage(controlStream, &protocol.AuthResponse{
			Status: protocol.AuthFailed,
		})
		return nil, fmt.Errorf("authentication failed: invalid passkey")
	}

	// Send success
	if err := protocol.WriteMessage(controlStream, &protocol.AuthResponse{
		Status: protocol.AuthOK,
	}); err != nil {
		return nil, fmt.Errorf("write auth response: %w", err)
	}

	// Accept data stream (opened by client).
	// The client writes a SequenceHeader as the first message to announce the stream.
	dataStream, err := qconn.AcceptStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("accept data stream: %w", err)
	}
	// Read the client's sequence header (last received seq).
	// Deadline prevents a misbehaving client from blocking the accept path.
	dataStream.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err = protocol.ReadMessage(dataStream)
	dataStream.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, fmt.Errorf("read data stream seq header: %w", err)
	}
	seqHdr, ok := msg.(*protocol.SequenceHeader)
	if !ok {
		return nil, fmt.Errorf("expected SequenceHeader on data stream, got %T", msg)
	}

	return &quicConn{
		qconn:         qconn,
		control:       controlStream,
		data:          dataStream,
		lastClientSeq: seqHdr.LastReceivedSeq,
	}, nil
}

// Close shuts down the listener and underlying transport.
func (l *quicListener) Close() error {
	l.ln.Close()
	return l.tr.Close()
}
