package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/chronologos/goet/internal/auth"
	"github.com/chronologos/goet/internal/protocol"
)

// tcpListener wraps a TLS-over-TCP listener for the session side.
type tcpListener struct {
	ln      net.Listener
	port    int
	passkey []byte
}

// listenTCP creates a TCP+TLS listener on the specified port.
// Takes a tls.Certificate so the dual listener can share one cert between
// QUIC and TCP.
func listenTCP(port int, passkey []byte, cert tls.Certificate) (*tcpListener, error) {
	tlsConf := ServerTLSConfig(cert)

	ln, err := tls.Listen("tcp4", ":"+strconv.Itoa(port), tlsConf)
	if err != nil {
		return nil, fmt.Errorf("TCP+TLS listen: %w", err)
	}

	localPort := ln.Addr().(*net.TCPAddr).Port

	return &tcpListener{
		ln:      ln,
		port:    localPort,
		passkey: passkey,
	}, nil
}

// Port returns the TCP port the listener is bound to.
func (l *tcpListener) Port() int {
	return l.port
}

// Accept waits for and authenticates a new TCP+TLS client connection.
func (l *tcpListener) Accept(ctx context.Context) (Conn, error) {
	// Use a channel so we can respect context cancellation
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := l.ln.Accept()
		ch <- result{conn, err}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			return nil, fmt.Errorf("accept TCP connection: %w", res.err)
		}
		tlsConn := res.conn.(*tls.Conn)
		conn, err := l.authenticate(tlsConn)
		if err != nil {
			tlsConn.Close()
			return nil, err
		}
		return conn, nil
	case <-ctx.Done():
		// Context cancelled — the goroutine may still be blocked on
		// l.ln.Accept(). It will unblock when the listener is closed by
		// the caller. If it accepted a connection before that, close it
		// so it doesn't leak.
		go func() {
			res := <-ch
			if res.conn != nil {
				res.conn.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

func (l *tcpListener) authenticate(tlsConn *tls.Conn) (*tcpConn, error) {
	// Read auth request
	msg, err := protocol.ReadMessage(tlsConn)
	if err != nil {
		return nil, fmt.Errorf("read auth request: %w", err)
	}

	authReq, ok := msg.(*protocol.AuthRequest)
	if !ok {
		return nil, fmt.Errorf("expected AuthRequest, got %T", msg)
	}

	// Verify HMAC token
	connState := tlsConn.ConnectionState()
	material, err := connState.ExportKeyingMaterial(
		"goet-auth-v1", nil, 32,
	)
	if err != nil {
		return nil, fmt.Errorf("export keying material: %w", err)
	}

	if !auth.VerifyAuthToken(l.passkey, material, authReq.Token) {
		protocol.WriteMessage(tlsConn, &protocol.AuthResponse{
			Status: protocol.AuthFailed,
		})
		return nil, fmt.Errorf("authentication failed: invalid passkey")
	}

	// Send success
	if err := protocol.WriteMessage(tlsConn, &protocol.AuthResponse{
		Status: protocol.AuthOK,
	}); err != nil {
		return nil, fmt.Errorf("write auth response: %w", err)
	}

	// Read the client's SequenceHeader.
	// Deadline prevents a misbehaving client from blocking the accept path.
	tlsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err = protocol.ReadMessage(tlsConn)
	tlsConn.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, fmt.Errorf("read seq header: %w", err)
	}
	seqHdr, ok := msg.(*protocol.SequenceHeader)
	if !ok {
		return nil, fmt.Errorf("expected SequenceHeader, got %T", msg)
	}

	return newTCPConn(tlsConn, seqHdr.LastReceivedSeq), nil
}

// Close shuts down the TCP listener.
func (l *tcpListener) Close() error {
	return l.ln.Close()
}
