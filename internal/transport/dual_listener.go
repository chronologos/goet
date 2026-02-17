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
		if err != nil {
			// Context cancelled means shutdown — exit the loop.
			if ctx.Err() != nil {
				return
			}
			// Transient error (auth failure, bad client) — log and continue
			// accepting. Only fatal errors (listener closed) will also
			// trigger ctx.Err() since Close() cancels the context first.
			continue
		}
		select {
		case dl.connCh <- acceptRes{conn: conn}:
		case <-ctx.Done():
			conn.Close()
			return
		}
	}
}

func (dl *dualListener) tcpAcceptLoop(ctx context.Context) {
	for {
		conn, err := dl.tcp.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		select {
		case dl.connCh <- acceptRes{conn: conn}:
		case <-ctx.Done():
			conn.Close()
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

// Close shuts down both listeners and drains any queued connections.
func (dl *dualListener) Close() error {
	dl.cancel()
	tcpErr := dl.tcp.Close()
	quicErr := dl.quic.Close()

	// Drain any connections that were accepted but not consumed.
	for {
		select {
		case res := <-dl.connCh:
			if res.conn != nil {
				res.conn.Close()
			}
		default:
			goto done
		}
	}
done:

	if quicErr != nil {
		return quicErr
	}
	return tcpErr
}
