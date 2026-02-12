package transport

import (
	"context"
	"fmt"
)

// dualListener accepts connections from both QUIC (UDP) and TCP+TLS listeners
// on the same port number. Accept() returns whichever connection arrives first.
type dualListener struct {
	quic *quicListener
	tcp  *tcpListener
	port int

	// connCh receives authenticated connections from both accept loops.
	connCh chan acceptRes
	// cancel stops both accept loops on Close.
	cancel context.CancelFunc
}

type acceptRes struct {
	conn Conn
	err  error
}

// ListenDual creates both a QUIC (UDP) and TCP+TLS listener on the same port.
// Bind order: QUIC first (gets random port from OS), then TCP on the same port.
func ListenDual(port int, passkey []byte) (Listener, error) {
	cert, err := GenerateSelfSignedCert()
	if err != nil {
		return nil, fmt.Errorf("generate TLS cert: %w", err)
	}

	ql, err := listenQUIC(port, passkey, cert)
	if err != nil {
		return nil, fmt.Errorf("QUIC listen: %w", err)
	}

	// Bind TCP to the same port number (UDP and TCP don't conflict).
	tl, err := listenTCP(ql.Port(), passkey, cert)
	if err != nil {
		ql.Close()
		return nil, fmt.Errorf("TCP listen on port %d: %w", ql.Port(), err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	dl := &dualListener{
		quic:   ql,
		tcp:    tl,
		port:   ql.Port(),
		connCh: make(chan acceptRes, 4),
		cancel: cancel,
	}

	// Start persistent accept loops for both transports.
	go dl.quicAcceptLoop(ctx)
	go dl.tcpAcceptLoop(ctx)

	return dl, nil
}

func (dl *dualListener) quicAcceptLoop(ctx context.Context) {
	for {
		conn, err := dl.quic.Accept(ctx)
		select {
		case dl.connCh <- acceptRes{conn: conn, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func (dl *dualListener) tcpAcceptLoop(ctx context.Context) {
	for {
		conn, err := dl.tcp.Accept(ctx)
		select {
		case dl.connCh <- acceptRes{conn: conn, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

// Accept returns the next authenticated connection from either transport.
func (dl *dualListener) Accept(ctx context.Context) (Conn, error) {
	select {
	case res := <-dl.connCh:
		return res.conn, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Port returns the port number both listeners are bound to.
func (dl *dualListener) Port() int {
	return dl.port
}

// Close shuts down both listeners.
func (dl *dualListener) Close() error {
	dl.cancel()
	tcpErr := dl.tcp.Close()
	quicErr := dl.quic.Close()
	if quicErr != nil {
		return quicErr
	}
	return tcpErr
}
