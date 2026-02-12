package transport

import (
	"context"
	"time"

	"github.com/quic-go/quic-go"
)

// DialMode selects which transport to use when dialing.
type DialMode int

const (
	DialQUIC DialMode = iota
	DialTCP
)

func (m DialMode) String() string {
	switch m {
	case DialQUIC:
		return "QUIC"
	case DialTCP:
		return "TCP"
	default:
		return "unknown"
	}
}

// Conn is the transport-level connection between a client and session.
// Both QUIC and TCP implementations satisfy this interface.
type Conn interface {
	ReadControl() (any, error)
	WriteControl(msg any) error
	ReadData() (any, error)
	WriteData(msg any) error
	SetControlReadDeadline(t time.Time) error
	LastClientSeq() uint64
	// StartDemux transitions from serial reads to concurrent demuxed reads.
	// For QUIC this is a no-op (streams are independently readable).
	// For TCP this starts the background demux goroutine.
	StartDemux()
	Close() error
}

// Listener accepts authenticated transport connections.
type Listener interface {
	Accept(ctx context.Context) (Conn, error)
	Port() int
	Close() error
}

// ProfileableConn is an optional interface for connections that can
// provide QUIC-level connection statistics (used by --profile).
type ProfileableConn interface {
	ConnectionStats() quic.ConnectionStats
}
