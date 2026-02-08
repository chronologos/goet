package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

var (
	ErrPayloadTooLarge = errors.New("payload exceeds maximum size")
	ErrUnknownMessage  = errors.New("unknown message type")
	ErrShortPayload    = errors.New("payload too short for message type")
)

// --- Message types ---

type AuthRequest struct {
	Token [32]byte
}

type AuthResponse struct {
	Status AuthStatus
}

type SequenceHeader struct {
	LastReceivedSeq uint64
}

type Heartbeat struct {
	TimestampMs int64
}

type Resize struct {
	Rows uint16
	Cols uint16
}

type Shutdown struct{}

type TerminalInfo struct {
	Term string
}

type Data struct {
	Seq     uint64
	Payload []byte
}

// --- Encoding ---

// WriteMessage writes a framed message (header + payload) to w.
//
// Fixed-size messages encode into stack buffers to avoid heap allocation.
// Data messages write the sequence number and payload separately to avoid
// copying the (potentially large) payload into an intermediate buffer.
func WriteMessage(w io.Writer, msg any) error {
	var msgType MessageType
	var payload []byte

	// Stack buffer for fixed-size message payloads (max 32 bytes for AuthRequest).
	var scratch [32]byte

	switch m := msg.(type) {
	case *AuthRequest:
		msgType = MsgAuthRequest
		payload = m.Token[:]
	case *AuthResponse:
		msgType = MsgAuthResponse
		scratch[0] = byte(m.Status)
		payload = scratch[:1]
	case *SequenceHeader:
		msgType = MsgSequenceHdr
		binary.BigEndian.PutUint64(scratch[:8], m.LastReceivedSeq)
		payload = scratch[:8]
	case *Heartbeat:
		msgType = MsgHeartbeat
		binary.BigEndian.PutUint64(scratch[:8], uint64(m.TimestampMs))
		payload = scratch[:8]
	case *Resize:
		msgType = MsgResize
		binary.BigEndian.PutUint16(scratch[0:2], m.Rows)
		binary.BigEndian.PutUint16(scratch[2:4], m.Cols)
		payload = scratch[:4]
	case *Shutdown:
		msgType = MsgShutdown
	case *TerminalInfo:
		msgType = MsgTerminalInfo
		termBytes := []byte(m.Term)
		payload = make([]byte, 2+len(termBytes))
		binary.BigEndian.PutUint16(payload[0:2], uint16(len(termBytes)))
		copy(payload[2:], termBytes)
	case *Data:
		return writeDataMessage(w, m)
	default:
		return fmt.Errorf("unsupported message type: %T", msg)
	}

	if len(payload) > MaxPayloadSize {
		return ErrPayloadTooLarge
	}

	var header [HeaderSize]byte
	binary.BigEndian.PutUint32(header[0:4], uint32(len(payload)))
	header[4] = byte(msgType)

	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// writeDataMessage writes a Data message without copying the payload into an
// intermediate buffer. This matters because Data payloads can be up to 4 MB.
func writeDataMessage(w io.Writer, m *Data) error {
	payloadLen := DataHeaderSize + len(m.Payload)
	if payloadLen > MaxPayloadSize {
		return ErrPayloadTooLarge
	}

	// Write frame header + sequence number together (13 bytes).
	var hdr [HeaderSize + DataHeaderSize]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(payloadLen))
	hdr[4] = byte(MsgData)
	binary.BigEndian.PutUint64(hdr[5:13], m.Seq)

	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(m.Payload) > 0 {
		if _, err := w.Write(m.Payload); err != nil {
			return err
		}
	}
	return nil
}

// --- Decoding ---

// ReadMessage reads a framed message from r.
func ReadMessage(r io.Reader) (any, error) {
	var header [HeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}

	payloadLen := binary.BigEndian.Uint32(header[0:4])
	msgType := MessageType(header[4])

	if payloadLen > MaxPayloadSize {
		return nil, ErrPayloadTooLarge
	}

	payload := make([]byte, payloadLen)
	if payloadLen > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
	}

	return DecodePayload(msgType, payload)
}

// DecodePayload decodes a raw payload given its message type.
func DecodePayload(msgType MessageType, payload []byte) (any, error) {
	switch msgType {
	case MsgAuthRequest:
		if len(payload) < AuthRequestSize {
			return nil, ErrShortPayload
		}
		msg := &AuthRequest{}
		copy(msg.Token[:], payload[:32])
		return msg, nil

	case MsgAuthResponse:
		if len(payload) < AuthResponseSize {
			return nil, ErrShortPayload
		}
		return &AuthResponse{Status: AuthStatus(payload[0])}, nil

	case MsgSequenceHdr:
		if len(payload) < SequenceHdrSize {
			return nil, ErrShortPayload
		}
		return &SequenceHeader{
			LastReceivedSeq: binary.BigEndian.Uint64(payload[0:8]),
		}, nil

	case MsgHeartbeat:
		if len(payload) < HeartbeatSize {
			return nil, ErrShortPayload
		}
		return &Heartbeat{
			TimestampMs: int64(binary.BigEndian.Uint64(payload[0:8])),
		}, nil

	case MsgResize:
		if len(payload) < ResizeSize {
			return nil, ErrShortPayload
		}
		return &Resize{
			Rows: binary.BigEndian.Uint16(payload[0:2]),
			Cols: binary.BigEndian.Uint16(payload[2:4]),
		}, nil

	case MsgShutdown:
		return &Shutdown{}, nil

	case MsgTerminalInfo:
		if len(payload) < 2 {
			return nil, ErrShortPayload
		}
		termLen := binary.BigEndian.Uint16(payload[0:2])
		if len(payload) < 2+int(termLen) {
			return nil, ErrShortPayload
		}
		return &TerminalInfo{Term: string(payload[2 : 2+termLen])}, nil

	case MsgData:
		if len(payload) < DataHeaderSize {
			return nil, ErrShortPayload
		}
		return &Data{
			Seq:     binary.BigEndian.Uint64(payload[0:8]),
			Payload: payload[8:],
		}, nil

	default:
		return nil, fmt.Errorf("%w: 0x%02x", ErrUnknownMessage, byte(msgType))
	}
}
