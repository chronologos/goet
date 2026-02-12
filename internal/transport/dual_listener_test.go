package transport

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/chronologos/goet/internal/auth"
	"github.com/chronologos/goet/internal/protocol"
)

func TestDualListenerAcceptQUIC(t *testing.T) {
	passkey, err := auth.GeneratePasskey()
	if err != nil {
		t.Fatal(err)
	}

	dl, err := ListenDual(0, passkey)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverDone := make(chan Conn, 1)
	go func() {
		conn, err := dl.Accept(ctx)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		serverDone <- conn
	}()

	// QUIC client connects to dual listener
	cc, err := dialQUIC(ctx, "127.0.0.1", dl.Port(), passkey, 0)
	if err != nil {
		t.Fatalf("QUIC dial: %v", err)
	}
	defer cc.Close()

	select {
	case sc := <-serverDone:
		defer sc.Close()
		// Verify it's a QUIC connection
		if _, ok := sc.(ProfileableConn); !ok {
			t.Fatal("expected QUIC connection (ProfileableConn)")
		}
	case <-ctx.Done():
		t.Fatal("timeout")
	}
}

func TestDualListenerAcceptTCP(t *testing.T) {
	passkey, err := auth.GeneratePasskey()
	if err != nil {
		t.Fatal(err)
	}

	dl, err := ListenDual(0, passkey)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverDone := make(chan Conn, 1)
	go func() {
		conn, err := dl.Accept(ctx)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		serverDone <- conn
	}()

	// TCP client connects to dual listener
	cc, err := dialTCP(ctx, "127.0.0.1", dl.Port(), passkey, 0)
	if err != nil {
		t.Fatalf("TCP dial: %v", err)
	}
	defer cc.Close()

	select {
	case sc := <-serverDone:
		defer sc.Close()
		// Verify it's a TCP connection (not ProfileableConn)
		if _, ok := sc.(ProfileableConn); ok {
			t.Fatal("expected TCP connection (not ProfileableConn)")
		}
	case <-ctx.Done():
		t.Fatal("timeout")
	}
}

func TestDualListenerBothTransportsWork(t *testing.T) {
	passkey, err := auth.GeneratePasskey()
	if err != nil {
		t.Fatal(err)
	}

	dl, err := ListenDual(0, passkey)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Accept two connections — one QUIC, one TCP
	serverConns := make(chan Conn, 2)
	go func() {
		for range 2 {
			conn, err := dl.Accept(ctx)
			if err != nil {
				return
			}
			serverConns <- conn
		}
	}()

	// QUIC client
	qc, err := dialQUIC(ctx, "127.0.0.1", dl.Port(), passkey, 0)
	if err != nil {
		t.Fatalf("QUIC dial: %v", err)
	}
	defer qc.Close()

	// TCP client
	tc, err := dialTCP(ctx, "127.0.0.1", dl.Port(), passkey, 0)
	if err != nil {
		t.Fatalf("TCP dial: %v", err)
	}
	defer tc.Close()

	// Wait for both server-side connections
	var sconns []Conn
	for range 2 {
		select {
		case sc := <-serverConns:
			sconns = append(sconns, sc)
			defer sc.Close()
		case <-ctx.Done():
			t.Fatal("timeout waiting for connections")
		}
	}

	// Both clients should be able to exchange data via their respective connections
	testPayload := []byte("dual listener test")
	if err := qc.WriteData(&protocol.Data{Seq: 1, Payload: testPayload}); err != nil {
		t.Fatalf("QUIC write: %v", err)
	}

	tc.StartDemux()
	if err := tc.WriteData(&protocol.Data{Seq: 1, Payload: testPayload}); err != nil {
		t.Fatalf("TCP write: %v", err)
	}

	// Read from both server connections
	for _, sc := range sconns {
		sc.StartDemux()
		msg, err := sc.ReadData()
		if err != nil {
			t.Fatalf("server read: %v", err)
		}
		d := msg.(*protocol.Data)
		if !bytes.Equal(d.Payload, testPayload) {
			t.Fatalf("payload mismatch: %q", d.Payload)
		}
	}
}
