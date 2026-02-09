package protocol

import (
	"bytes"
	"testing"
	"time"
)

func TestAuthRequestRoundTrip(t *testing.T) {
	original := &AuthRequest{}
	for i := range original.Token {
		original.Token[i] = byte(i)
	}

	var buf bytes.Buffer
	if err := WriteMessage(&buf, original); err != nil {
		t.Fatal(err)
	}

	msg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}

	decoded, ok := msg.(*AuthRequest)
	if !ok {
		t.Fatalf("expected *AuthRequest, got %T", msg)
	}
	if decoded.Token != original.Token {
		t.Fatalf("token mismatch")
	}
}

func TestAuthResponseRoundTrip(t *testing.T) {
	for _, status := range []AuthStatus{AuthOK, AuthFailed, AuthSessionNotFound} {
		original := &AuthResponse{Status: status}
		var buf bytes.Buffer
		if err := WriteMessage(&buf, original); err != nil {
			t.Fatal(err)
		}
		msg, err := ReadMessage(&buf)
		if err != nil {
			t.Fatal(err)
		}
		decoded := msg.(*AuthResponse)
		if decoded.Status != original.Status {
			t.Fatalf("status mismatch: got %d, want %d", decoded.Status, original.Status)
		}
	}
}

func TestSequenceHeaderRoundTrip(t *testing.T) {
	for _, seq := range []uint64{0, 1, 1<<32 - 1, 1<<64 - 1} {
		original := &SequenceHeader{LastReceivedSeq: seq}
		var buf bytes.Buffer
		if err := WriteMessage(&buf, original); err != nil {
			t.Fatal(err)
		}
		msg, err := ReadMessage(&buf)
		if err != nil {
			t.Fatal(err)
		}
		decoded := msg.(*SequenceHeader)
		if decoded.LastReceivedSeq != original.LastReceivedSeq {
			t.Fatalf("seq mismatch: got %d, want %d", decoded.LastReceivedSeq, original.LastReceivedSeq)
		}
	}
}

func TestHeartbeatRoundTrip(t *testing.T) {
	original := &Heartbeat{TimestampMs: time.Now().UnixMilli()}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, original); err != nil {
		t.Fatal(err)
	}
	msg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	decoded := msg.(*Heartbeat)
	if decoded.TimestampMs != original.TimestampMs {
		t.Fatalf("timestamp mismatch: got %d, want %d", decoded.TimestampMs, original.TimestampMs)
	}
}

func TestResizeRoundTrip(t *testing.T) {
	original := &Resize{Rows: 24, Cols: 80}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, original); err != nil {
		t.Fatal(err)
	}
	msg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	decoded := msg.(*Resize)
	if decoded.Rows != original.Rows || decoded.Cols != original.Cols {
		t.Fatalf("resize mismatch: got %dx%d, want %dx%d",
			decoded.Rows, decoded.Cols, original.Rows, original.Cols)
	}
}

func TestShutdownRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, &Shutdown{}); err != nil {
		t.Fatal(err)
	}
	msg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := msg.(*Shutdown); !ok {
		t.Fatalf("expected *Shutdown, got %T", msg)
	}
}

func TestDataRoundTrip(t *testing.T) {
	payload := []byte("hello, terminal!")
	original := &Data{Seq: 42, Payload: payload}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, original); err != nil {
		t.Fatal(err)
	}
	msg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	decoded := msg.(*Data)
	if decoded.Seq != original.Seq {
		t.Fatalf("seq mismatch: got %d, want %d", decoded.Seq, original.Seq)
	}
	if !bytes.Equal(decoded.Payload, original.Payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestDataEmptyPayload(t *testing.T) {
	original := &Data{Seq: 1, Payload: []byte{}}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, original); err != nil {
		t.Fatal(err)
	}
	msg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	decoded := msg.(*Data)
	if decoded.Seq != 1 {
		t.Fatalf("seq mismatch")
	}
	if len(decoded.Payload) != 0 {
		t.Fatalf("expected empty payload, got %d bytes", len(decoded.Payload))
	}
}

func TestTerminalInfoRoundTrip(t *testing.T) {
	for _, term := range []string{"", "xterm-256color", "alacritty"} {
		original := &TerminalInfo{Term: term}
		var buf bytes.Buffer
		if err := WriteMessage(&buf, original); err != nil {
			t.Fatal(err)
		}
		msg, err := ReadMessage(&buf)
		if err != nil {
			t.Fatal(err)
		}
		decoded := msg.(*TerminalInfo)
		if decoded.Term != original.Term {
			t.Fatalf("term mismatch: got %q, want %q", decoded.Term, original.Term)
		}
	}
}

func TestMultipleMessagesInSequence(t *testing.T) {
	var buf bytes.Buffer

	msgs := []any{
		&Heartbeat{TimestampMs: 1000},
		&Resize{Rows: 50, Cols: 120},
		&TerminalInfo{Term: "alacritty"},
		&Data{Seq: 1, Payload: []byte("first")},
		&Data{Seq: 2, Payload: []byte("second")},
		&Shutdown{},
	}

	for _, msg := range msgs {
		if err := WriteMessage(&buf, msg); err != nil {
			t.Fatal(err)
		}
	}

	for i, expected := range msgs {
		msg, err := ReadMessage(&buf)
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		switch e := expected.(type) {
		case *Heartbeat:
			d := msg.(*Heartbeat)
			if d.TimestampMs != e.TimestampMs {
				t.Fatalf("message %d: heartbeat mismatch", i)
			}
		case *Resize:
			d := msg.(*Resize)
			if d.Rows != e.Rows || d.Cols != e.Cols {
				t.Fatalf("message %d: resize mismatch", i)
			}
		case *TerminalInfo:
			d := msg.(*TerminalInfo)
			if d.Term != e.Term {
				t.Fatalf("message %d: terminal info mismatch", i)
			}
		case *Data:
			d := msg.(*Data)
			if d.Seq != e.Seq || !bytes.Equal(d.Payload, e.Payload) {
				t.Fatalf("message %d: data mismatch", i)
			}
		case *Shutdown:
			if _, ok := msg.(*Shutdown); !ok {
				t.Fatalf("message %d: expected shutdown", i)
			}
		}
	}
}

func TestDecodeShortPayload(t *testing.T) {
	// AuthRequest needs 32 bytes, give it 10
	_, err := DecodePayload(MsgAuthRequest, make([]byte, 10))
	if err != ErrShortPayload {
		t.Fatalf("expected ErrShortPayload, got %v", err)
	}
}

func TestDecodeUnknownType(t *testing.T) {
	_, err := DecodePayload(MessageType(0xFF), nil)
	if err == nil {
		t.Fatal("expected error for unknown message type")
	}
}

func TestPayloadTooLarge(t *testing.T) {
	huge := &Data{Seq: 1, Payload: make([]byte, MaxPayloadSize+1)}
	var buf bytes.Buffer
	err := WriteMessage(&buf, huge)
	if err != ErrPayloadTooLarge {
		t.Fatalf("expected ErrPayloadTooLarge, got %v", err)
	}
}

// --- Fuzz tests ---

func FuzzDecodeAuthRequest(f *testing.F) {
	f.Add(make([]byte, 32))
	f.Fuzz(func(t *testing.T, data []byte) {
		DecodePayload(MsgAuthRequest, data)
	})
}

func FuzzDecodeHeartbeat(f *testing.F) {
	f.Add(make([]byte, 8))
	f.Fuzz(func(t *testing.T, data []byte) {
		DecodePayload(MsgHeartbeat, data)
	})
}

func FuzzDecodeResize(f *testing.F) {
	f.Add(make([]byte, 4))
	f.Fuzz(func(t *testing.T, data []byte) {
		DecodePayload(MsgResize, data)
	})
}

func FuzzDecodeData(f *testing.F) {
	f.Add(make([]byte, 16))
	f.Fuzz(func(t *testing.T, data []byte) {
		DecodePayload(MsgData, data)
	})
}

func FuzzDecodeSequenceHeader(f *testing.F) {
	f.Add(make([]byte, 8))
	f.Fuzz(func(t *testing.T, data []byte) {
		DecodePayload(MsgSequenceHdr, data)
	})
}

func FuzzDecodeTerminalInfo(f *testing.F) {
	f.Add([]byte{0, 0})                                        // empty term
	f.Add([]byte{0, 5, 'x', 't', 'e', 'r', 'm'})              // "xterm"
	f.Add([]byte{0, 16, 'x', 't', 'e', 'r', 'm', '-', '2', '5', '6', 'c', 'o', 'l', 'o', 'r', 0, 0}) // with trailing garbage
	f.Fuzz(func(t *testing.T, data []byte) {
		DecodePayload(MsgTerminalInfo, data)
	})
}

func FuzzReadMessage(f *testing.F) {
	// Seed with valid encoded messages of different types
	var buf bytes.Buffer
	WriteMessage(&buf, &Heartbeat{TimestampMs: 12345})
	f.Add(buf.Bytes())

	buf.Reset()
	WriteMessage(&buf, &TerminalInfo{Term: "xterm-256color"})
	f.Add(buf.Bytes())

	f.Fuzz(func(t *testing.T, data []byte) {
		ReadMessage(bytes.NewReader(data))
	})
}

// --- Round-trip fuzz tests ---
// These generate random *valid* message fields, encode with WriteMessage,
// decode with ReadMessage, and verify the result matches the original.
// Much higher value than crash-only fuzzing: catches endianness bugs, field
// transposition, off-by-one in length prefixes, etc.

func FuzzRoundTripHeartbeat(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(1706745600000))
	f.Add(int64(-1))
	f.Fuzz(func(t *testing.T, ts int64) {
		original := &Heartbeat{TimestampMs: ts}
		var buf bytes.Buffer
		if err := WriteMessage(&buf, original); err != nil {
			t.Fatal(err)
		}
		msg, err := ReadMessage(&buf)
		if err != nil {
			t.Fatal(err)
		}
		decoded := msg.(*Heartbeat)
		if decoded.TimestampMs != original.TimestampMs {
			t.Fatalf("timestamp mismatch: got %d, want %d", decoded.TimestampMs, original.TimestampMs)
		}
	})
}

func FuzzRoundTripResize(f *testing.F) {
	f.Add(uint16(24), uint16(80))
	f.Add(uint16(0), uint16(0))
	f.Add(uint16(65535), uint16(65535))
	f.Fuzz(func(t *testing.T, rows, cols uint16) {
		original := &Resize{Rows: rows, Cols: cols}
		var buf bytes.Buffer
		if err := WriteMessage(&buf, original); err != nil {
			t.Fatal(err)
		}
		msg, err := ReadMessage(&buf)
		if err != nil {
			t.Fatal(err)
		}
		decoded := msg.(*Resize)
		if decoded.Rows != original.Rows || decoded.Cols != original.Cols {
			t.Fatalf("resize mismatch: got %dx%d, want %dx%d",
				decoded.Rows, decoded.Cols, original.Rows, original.Cols)
		}
	})
}

func FuzzRoundTripSequenceHeader(f *testing.F) {
	f.Add(uint64(0))
	f.Add(uint64(1<<64 - 1))
	f.Fuzz(func(t *testing.T, seq uint64) {
		original := &SequenceHeader{LastReceivedSeq: seq}
		var buf bytes.Buffer
		if err := WriteMessage(&buf, original); err != nil {
			t.Fatal(err)
		}
		msg, err := ReadMessage(&buf)
		if err != nil {
			t.Fatal(err)
		}
		decoded := msg.(*SequenceHeader)
		if decoded.LastReceivedSeq != original.LastReceivedSeq {
			t.Fatalf("seq mismatch: got %d, want %d", decoded.LastReceivedSeq, original.LastReceivedSeq)
		}
	})
}

func FuzzRoundTripData(f *testing.F) {
	f.Add(uint64(1), []byte("hello"))
	f.Add(uint64(0), []byte{})
	f.Add(uint64(1<<64-1), []byte{0, 1, 2, 3})
	f.Fuzz(func(t *testing.T, seq uint64, payload []byte) {
		if len(payload) > 64*1024 { // cap to keep fuzz fast
			payload = payload[:64*1024]
		}
		original := &Data{Seq: seq, Payload: payload}
		var buf bytes.Buffer
		if err := WriteMessage(&buf, original); err != nil {
			t.Fatal(err)
		}
		msg, err := ReadMessage(&buf)
		if err != nil {
			t.Fatal(err)
		}
		decoded := msg.(*Data)
		if decoded.Seq != original.Seq {
			t.Fatalf("seq mismatch: got %d, want %d", decoded.Seq, original.Seq)
		}
		if !bytes.Equal(decoded.Payload, original.Payload) {
			t.Fatalf("payload mismatch: got %d bytes, want %d bytes", len(decoded.Payload), len(original.Payload))
		}
	})
}

func FuzzRoundTripTerminalInfo(f *testing.F) {
	f.Add("xterm-256color")
	f.Add("")
	f.Add("alacritty")
	f.Fuzz(func(t *testing.T, term string) {
		if len(term) > 65535 { // TerminalInfo uses uint16 length prefix
			term = term[:65535]
		}
		original := &TerminalInfo{Term: term}
		var buf bytes.Buffer
		if err := WriteMessage(&buf, original); err != nil {
			t.Fatal(err)
		}
		msg, err := ReadMessage(&buf)
		if err != nil {
			t.Fatal(err)
		}
		decoded := msg.(*TerminalInfo)
		if decoded.Term != original.Term {
			t.Fatalf("term mismatch: got %q, want %q", decoded.Term, original.Term)
		}
	})
}

func FuzzRoundTripAuthRequest(f *testing.F) {
	f.Add(make([]byte, 32))
	f.Fuzz(func(t *testing.T, token []byte) {
		if len(token) < 32 {
			return // need exactly 32 bytes for Token field
		}
		original := &AuthRequest{}
		copy(original.Token[:], token[:32])
		var buf bytes.Buffer
		if err := WriteMessage(&buf, original); err != nil {
			t.Fatal(err)
		}
		msg, err := ReadMessage(&buf)
		if err != nil {
			t.Fatal(err)
		}
		decoded := msg.(*AuthRequest)
		if decoded.Token != original.Token {
			t.Fatal("token mismatch")
		}
	})
}
