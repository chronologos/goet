package transport

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/chronologos/goet/internal/auth"
	"github.com/chronologos/goet/internal/protocol"
)

// setupConnPair creates a Listener and dials into it, returning both sides.
func setupConnPair(t *testing.T) (serverConn, clientConn *Conn, cleanup func()) {
	t.Helper()

	passkey, err := auth.GeneratePasskey()
	if err != nil {
		t.Fatal(err)
	}

	ln, err := Listen(0, passkey)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	serverDone := make(chan *Conn, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept(ctx)
		if err != nil {
			serverErr <- err
			return
		}
		serverDone <- conn
	}()

	cc, err := Dial(ctx, "127.0.0.1", ln.Port(), passkey, 0)
	if err != nil {
		cancel()
		ln.Close()
		t.Fatalf("client dial: %v", err)
	}

	var sc *Conn
	select {
	case sc = <-serverDone:
	case err := <-serverErr:
		cancel()
		cc.Close()
		ln.Close()
		t.Fatalf("server accept: %v", err)
	case <-ctx.Done():
		cancel()
		cc.Close()
		ln.Close()
		t.Fatal("timeout waiting for server accept")
	}

	return sc, cc, func() {
		cancel()
		sc.Close()
		cc.Close()
		ln.Close()
	}
}

func TestConnectAndAuthenticate(t *testing.T) {
	_, _, cleanup := setupConnPair(t)
	defer cleanup()
	// If we get here, auth succeeded
}

func TestBidirectionalDataExchange(t *testing.T) {
	serverConn, clientConn, cleanup := setupConnPair(t)
	defer cleanup()

	// Client sends data to server
	clientPayload := []byte("hello from client")
	if err := clientConn.WriteData(&protocol.Data{Seq: 1, Payload: clientPayload}); err != nil {
		t.Fatalf("client write data: %v", err)
	}

	msg, err := serverConn.ReadData()
	if err != nil {
		t.Fatalf("server read data: %v", err)
	}
	data := msg.(*protocol.Data)
	if data.Seq != 1 || !bytes.Equal(data.Payload, clientPayload) {
		t.Fatalf("data mismatch: seq=%d payload=%q", data.Seq, data.Payload)
	}

	// Server sends data to client
	serverPayload := []byte("hello from server")
	if err := serverConn.WriteData(&protocol.Data{Seq: 1, Payload: serverPayload}); err != nil {
		t.Fatalf("server write data: %v", err)
	}

	msg, err = clientConn.ReadData()
	if err != nil {
		t.Fatalf("client read data: %v", err)
	}
	data = msg.(*protocol.Data)
	if !bytes.Equal(data.Payload, serverPayload) {
		t.Fatalf("data mismatch: %q", data.Payload)
	}
}

func TestControlMessages(t *testing.T) {
	serverConn, clientConn, cleanup := setupConnPair(t)
	defer cleanup()

	// Client sends resize on control stream
	if err := clientConn.WriteControl(&protocol.Resize{Rows: 24, Cols: 80}); err != nil {
		t.Fatalf("write resize: %v", err)
	}

	msg, err := serverConn.ReadControl()
	if err != nil {
		t.Fatalf("read resize: %v", err)
	}
	resize := msg.(*protocol.Resize)
	if resize.Rows != 24 || resize.Cols != 80 {
		t.Fatalf("resize mismatch: %dx%d", resize.Rows, resize.Cols)
	}

	// Server sends heartbeat on control stream
	ts := time.Now().UnixMilli()
	if err := serverConn.WriteControl(&protocol.Heartbeat{TimestampMs: ts}); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	msg, err = clientConn.ReadControl()
	if err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	hb := msg.(*protocol.Heartbeat)
	if hb.TimestampMs != ts {
		t.Fatalf("heartbeat mismatch: %d vs %d", hb.TimestampMs, ts)
	}
}

func TestWrongPasskeyRejected(t *testing.T) {
	serverPasskey, err := auth.GeneratePasskey()
	if err != nil {
		t.Fatal(err)
	}

	wrongPasskey, err := auth.GeneratePasskey()
	if err != nil {
		t.Fatal(err)
	}

	ln, err := Listen(0, serverPasskey)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverErr := make(chan error, 1)
	go func() {
		_, err := ln.Accept(ctx)
		serverErr <- err
	}()

	_, err = Dial(ctx, "127.0.0.1", ln.Port(), wrongPasskey, 0)
	if err == nil {
		t.Fatal("expected auth error, got nil")
	}

	select {
	case err := <-serverErr:
		if err == nil {
			t.Fatal("server should have rejected auth")
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for server rejection")
	}
}

func TestConcurrentControlAndData(t *testing.T) {
	serverConn, clientConn, cleanup := setupConnPair(t)
	defer cleanup()

	done := make(chan error, 2)

	go func() {
		for i := range 10 {
			if err := clientConn.WriteControl(&protocol.Heartbeat{
				TimestampMs: int64(i),
			}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	go func() {
		for i := range 10 {
			if err := clientConn.WriteData(&protocol.Data{
				Seq:     uint64(i),
				Payload: []byte("data"),
			}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("send error: %v", err)
		}
	}

	// Read all control messages
	for i := range 10 {
		msg, err := serverConn.ReadControl()
		if err != nil {
			t.Fatalf("read control %d: %v", i, err)
		}
		hb := msg.(*protocol.Heartbeat)
		if hb.TimestampMs != int64(i) {
			t.Fatalf("heartbeat %d: got timestamp %d", i, hb.TimestampMs)
		}
	}

	// Read all data messages
	for i := range 10 {
		msg, err := serverConn.ReadData()
		if err != nil {
			t.Fatalf("read data %d: %v", i, err)
		}
		data := msg.(*protocol.Data)
		if data.Seq != uint64(i) {
			t.Fatalf("data %d: got seq %d", i, data.Seq)
		}
	}
}

func TestSequenceHeaderExchange(t *testing.T) {
	serverConn, clientConn, cleanup := setupConnPair(t)
	defer cleanup()

	// Simulate reconnect sequence header exchange
	if err := clientConn.WriteControl(&protocol.SequenceHeader{LastReceivedSeq: 42}); err != nil {
		t.Fatalf("write seq header: %v", err)
	}
	if err := serverConn.WriteControl(&protocol.SequenceHeader{LastReceivedSeq: 99}); err != nil {
		t.Fatalf("write seq header: %v", err)
	}

	msg, err := serverConn.ReadControl()
	if err != nil {
		t.Fatal(err)
	}
	seqHdr := msg.(*protocol.SequenceHeader)
	if seqHdr.LastReceivedSeq != 42 {
		t.Fatalf("expected 42, got %d", seqHdr.LastReceivedSeq)
	}

	msg, err = clientConn.ReadControl()
	if err != nil {
		t.Fatal(err)
	}
	seqHdr = msg.(*protocol.SequenceHeader)
	if seqHdr.LastReceivedSeq != 99 {
		t.Fatalf("expected 99, got %d", seqHdr.LastReceivedSeq)
	}
}

// TestSequenceHeaderDeadlineTimeout verifies that a client which connects,
// authenticates, and opens a data stream but never sends a SequenceHeader
// gets disconnected by the 5-second deadline in session_listener.go.
func TestSequenceHeaderDeadlineTimeout(t *testing.T) {
	passkey, err := auth.GeneratePasskey()
	if err != nil {
		t.Fatal(err)
	}

	ln, err := Listen(0, passkey)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Accept with a timeout long enough for the 5s deadline to fire.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	acceptErr := make(chan error, 1)
	go func() {
		_, err := ln.Accept(ctx)
		acceptErr <- err
	}()

	// --- Manually replicate client-side auth, but skip SequenceHeader ---

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	defer udpConn.Close()

	tr := &quic.Transport{Conn: udpConn}
	defer tr.Close()

	serverAddr, err := net.ResolveUDPAddr("udp4",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(ln.Port())))
	if err != nil {
		t.Fatalf("resolve addr: %v", err)
	}

	tlsConf := ClientTLSConfig()
	quicConf := &quic.Config{
		MaxIdleTimeout:    30 * time.Second,
		InitialPacketSize: 1200,
	}

	qconn, err := tr.Dial(ctx, serverAddr, tlsConf, quicConf)
	if err != nil {
		t.Fatalf("QUIC dial: %v", err)
	}
	defer qconn.CloseWithError(0, "test done")

	// Open control stream and perform auth
	controlStream, err := qconn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open control stream: %v", err)
	}

	connState := qconn.ConnectionState()
	material, err := connState.TLS.ExportKeyingMaterial("goet-auth-v1", nil, 32)
	if err != nil {
		t.Fatalf("export keying material: %v", err)
	}
	token := auth.ComputeAuthToken(passkey, material)

	if err := protocol.WriteMessage(controlStream, &protocol.AuthRequest{Token: token}); err != nil {
		t.Fatalf("write auth request: %v", err)
	}

	msg, err := protocol.ReadMessage(controlStream)
	if err != nil {
		t.Fatalf("read auth response: %v", err)
	}
	resp, ok := msg.(*protocol.AuthResponse)
	if !ok {
		t.Fatalf("expected AuthResponse, got %T", msg)
	}
	if resp.Status != protocol.AuthOK {
		t.Fatalf("auth rejected: status %d", resp.Status)
	}

	// Open data stream and write a single byte to make it visible to the
	// server (QUIC only sends STREAM frames on first Write), but do NOT send
	// a valid SequenceHeader. This makes the server's AcceptStream succeed,
	// but the subsequent ReadMessage (which has a 5s deadline) will time out.
	dataStream, err := qconn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open data stream: %v", err)
	}
	// Write a single byte — not enough for a valid framed message header (5 bytes).
	if _, err := dataStream.Write([]byte{0x00}); err != nil {
		t.Fatalf("write partial data: %v", err)
	}

	// Wait for Accept to return with an error (deadline exceeded).
	start := time.Now()
	select {
	case err := <-acceptErr:
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("expected Accept to fail, got nil error")
		}
		// The deadline is 5 seconds; verify it fires in a reasonable range.
		if elapsed < 4*time.Second {
			t.Fatalf("Accept returned too quickly (%v), expected ~5s deadline", elapsed)
		}
		if elapsed > 8*time.Second {
			t.Fatalf("Accept took too long (%v), expected ~5s deadline", elapsed)
		}
		t.Logf("Accept correctly failed after %v: %v", elapsed, err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: Accept did not return within 10s")
	}
}
