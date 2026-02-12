package transport

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/chronologos/goet/internal/auth"
	"github.com/chronologos/goet/internal/protocol"
)

// setupTCPConnPair creates a TCP listener and dials into it, returning both sides.
func setupTCPConnPair(t *testing.T) (serverConn, clientConn Conn, cleanup func()) {
	t.Helper()

	passkey, err := auth.GeneratePasskey()
	if err != nil {
		t.Fatal(err)
	}

	cert, err := GenerateSelfSignedCert()
	if err != nil {
		t.Fatal(err)
	}

	ln, err := listenTCP(0, passkey, cert)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	serverDone := make(chan Conn, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept(ctx)
		if err != nil {
			serverErr <- err
			return
		}
		serverDone <- conn
	}()

	cc, err := dialTCP(ctx, "127.0.0.1", ln.Port(), passkey, 0)
	if err != nil {
		cancel()
		ln.Close()
		t.Fatalf("TCP dial: %v", err)
	}

	var sc Conn
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

func TestTCPConnectAndAuth(t *testing.T) {
	_, _, cleanup := setupTCPConnPair(t)
	defer cleanup()
}

func TestTCPSerialReadsBeforeDemux(t *testing.T) {
	serverConn, clientConn, cleanup := setupTCPConnPair(t)
	defer cleanup()

	// Before StartDemux, reads are serial — we can send a control message
	// and read it without the demux goroutine.
	if err := clientConn.WriteControl(&protocol.Heartbeat{TimestampMs: 42}); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	msg, err := serverConn.ReadControl()
	if err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	hb, ok := msg.(*protocol.Heartbeat)
	if !ok || hb.TimestampMs != 42 {
		t.Fatalf("unexpected message: %T %v", msg, msg)
	}
}

func TestTCPDemuxRouting(t *testing.T) {
	serverConn, clientConn, cleanup := setupTCPConnPair(t)
	defer cleanup()

	// Start demux on both sides
	serverConn.StartDemux()
	clientConn.StartDemux()

	// Client sends a mix of control and data messages
	if err := clientConn.WriteControl(&protocol.Heartbeat{TimestampMs: 100}); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}
	if err := clientConn.WriteData(&protocol.Data{Seq: 1, Payload: []byte("hello")}); err != nil {
		t.Fatalf("write data: %v", err)
	}
	if err := clientConn.WriteControl(&protocol.Resize{Rows: 24, Cols: 80}); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	if err := clientConn.WriteData(&protocol.Data{Seq: 2, Payload: []byte("world")}); err != nil {
		t.Fatalf("write data 2: %v", err)
	}

	// Server reads from the correct channels
	// Control: Heartbeat, Resize
	msg1, err := serverConn.ReadControl()
	if err != nil {
		t.Fatalf("read control 1: %v", err)
	}
	if _, ok := msg1.(*protocol.Heartbeat); !ok {
		t.Fatalf("expected Heartbeat, got %T", msg1)
	}

	msg2, err := serverConn.ReadControl()
	if err != nil {
		t.Fatalf("read control 2: %v", err)
	}
	if _, ok := msg2.(*protocol.Resize); !ok {
		t.Fatalf("expected Resize, got %T", msg2)
	}

	// Data: seq 1, seq 2
	msg3, err := serverConn.ReadData()
	if err != nil {
		t.Fatalf("read data 1: %v", err)
	}
	d1 := msg3.(*protocol.Data)
	if d1.Seq != 1 || !bytes.Equal(d1.Payload, []byte("hello")) {
		t.Fatalf("data 1 mismatch: seq=%d payload=%q", d1.Seq, d1.Payload)
	}

	msg4, err := serverConn.ReadData()
	if err != nil {
		t.Fatalf("read data 2: %v", err)
	}
	d2 := msg4.(*protocol.Data)
	if d2.Seq != 2 || !bytes.Equal(d2.Payload, []byte("world")) {
		t.Fatalf("data 2 mismatch: seq=%d payload=%q", d2.Seq, d2.Payload)
	}
}

func TestTCPWriteSerialization(t *testing.T) {
	serverConn, clientConn, cleanup := setupTCPConnPair(t)
	defer cleanup()

	serverConn.StartDemux()
	clientConn.StartDemux()

	// Concurrent writes from client: control + data goroutines.
	// Must also read concurrently on the server side — TCP's single conn
	// means the demux channels (capacity 16) can fill up if not drained.
	const n = 50
	var wg sync.WaitGroup
	wg.Add(4) // 2 writers + 2 readers

	// Writers
	go func() {
		defer wg.Done()
		for i := range n {
			if err := clientConn.WriteControl(&protocol.Heartbeat{
				TimestampMs: int64(i),
			}); err != nil {
				t.Errorf("write control %d: %v", i, err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := range n {
			if err := clientConn.WriteData(&protocol.Data{
				Seq:     uint64(i),
				Payload: []byte("data"),
			}); err != nil {
				t.Errorf("write data %d: %v", i, err)
				return
			}
		}
	}()

	// Readers (must run concurrently with writers to avoid deadlock)
	controlCount := make(chan int, 1)
	dataCount := make(chan int, 1)

	go func() {
		defer wg.Done()
		count := 0
		for range n {
			msg, err := serverConn.ReadControl()
			if err != nil {
				t.Errorf("read control: %v", err)
				break
			}
			if _, ok := msg.(*protocol.Heartbeat); !ok {
				t.Errorf("expected Heartbeat, got %T", msg)
				break
			}
			count++
		}
		controlCount <- count
	}()

	go func() {
		defer wg.Done()
		count := 0
		for range n {
			msg, err := serverConn.ReadData()
			if err != nil {
				t.Errorf("read data: %v", err)
				break
			}
			if _, ok := msg.(*protocol.Data); !ok {
				t.Errorf("expected Data, got %T", msg)
				break
			}
			count++
		}
		dataCount <- count
	}()

	wg.Wait()

	if cc := <-controlCount; cc != n {
		t.Errorf("expected %d control messages, got %d", n, cc)
	}
	if dc := <-dataCount; dc != n {
		t.Errorf("expected %d data messages, got %d", n, dc)
	}
}

func TestTCPCloseUnblocksReaders(t *testing.T) {
	serverConn, clientConn, cleanup := setupTCPConnPair(t)
	defer cleanup()

	serverConn.StartDemux()

	// Close the client side — server's demux should unblock
	clientConn.Close()

	errCh := make(chan error, 2)
	go func() {
		_, err := serverConn.ReadControl()
		errCh <- err
	}()
	go func() {
		_, err := serverConn.ReadData()
		errCh <- err
	}()

	for range 2 {
		select {
		case err := <-errCh:
			if err == nil {
				t.Fatal("expected error from blocked reader, got nil")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for blocked reader to unblock")
		}
	}
}

func TestTCPBidirectionalData(t *testing.T) {
	serverConn, clientConn, cleanup := setupTCPConnPair(t)
	defer cleanup()

	serverConn.StartDemux()
	clientConn.StartDemux()

	// Client → Server
	clientPayload := []byte("from client")
	if err := clientConn.WriteData(&protocol.Data{Seq: 1, Payload: clientPayload}); err != nil {
		t.Fatalf("client write: %v", err)
	}

	msg, err := serverConn.ReadData()
	if err != nil {
		t.Fatalf("server read: %v", err)
	}
	d := msg.(*protocol.Data)
	if !bytes.Equal(d.Payload, clientPayload) {
		t.Fatalf("payload mismatch: %q", d.Payload)
	}

	// Server → Client
	serverPayload := []byte("from server")
	if err := serverConn.WriteData(&protocol.Data{Seq: 1, Payload: serverPayload}); err != nil {
		t.Fatalf("server write: %v", err)
	}

	msg, err = clientConn.ReadData()
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	d = msg.(*protocol.Data)
	if !bytes.Equal(d.Payload, serverPayload) {
		t.Fatalf("payload mismatch: %q", d.Payload)
	}
}

func TestTCPLastClientSeq(t *testing.T) {
	passkey, err := auth.GeneratePasskey()
	if err != nil {
		t.Fatal(err)
	}

	cert, err := GenerateSelfSignedCert()
	if err != nil {
		t.Fatal(err)
	}

	ln, err := listenTCP(0, passkey, cert)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverDone := make(chan Conn, 1)
	go func() {
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		serverDone <- conn
	}()

	// Dial with lastReceivedSeq=42
	cc, err := dialTCP(ctx, "127.0.0.1", ln.Port(), passkey, 42)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cc.Close()

	select {
	case sc := <-serverDone:
		defer sc.Close()
		if sc.LastClientSeq() != 42 {
			t.Fatalf("expected LastClientSeq=42, got %d", sc.LastClientSeq())
		}
	case <-ctx.Done():
		t.Fatal("timeout")
	}
}

func TestTCPWrongPasskey(t *testing.T) {
	serverPasskey, err := auth.GeneratePasskey()
	if err != nil {
		t.Fatal(err)
	}
	wrongPasskey, err := auth.GeneratePasskey()
	if err != nil {
		t.Fatal(err)
	}

	cert, err := GenerateSelfSignedCert()
	if err != nil {
		t.Fatal(err)
	}

	ln, err := listenTCP(0, serverPasskey, cert)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		ln.Accept(ctx)
	}()

	_, err = dialTCP(ctx, "127.0.0.1", ln.Port(), wrongPasskey, 0)
	if err == nil {
		t.Fatal("expected auth error, got nil")
	}
}
