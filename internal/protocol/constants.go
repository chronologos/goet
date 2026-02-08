package protocol

// Wire format version.
const Version = 1

// Header: [4B payload_length big-endian][1B message_type]
const HeaderSize = 5

// Maximum payload size (4 MB).
const MaxPayloadSize = 4 * 1024 * 1024

// MessageType identifies the type of a framed message.
type MessageType byte

const (
	// Control stream (stream 0)
	MsgAuthRequest  MessageType = 0x01
	MsgAuthResponse MessageType = 0x02
	MsgSequenceHdr  MessageType = 0x03
	MsgHeartbeat    MessageType = 0x10
	MsgResize       MessageType = 0x11
	MsgShutdown     MessageType = 0x12
	MsgTerminalInfo MessageType = 0x13

	// Data stream (stream 1)
	MsgData MessageType = 0x20
)

// AuthStatus is the result of an authentication attempt.
type AuthStatus byte

const (
	AuthOK              AuthStatus = 0
	AuthFailed          AuthStatus = 1
	AuthSessionNotFound AuthStatus = 2
)

// Fixed message sizes (excluding header).
const (
	AuthRequestSize  = 32 // HMAC token
	AuthResponseSize = 1  // status byte
	SequenceHdrSize  = 8  // u64
	HeartbeatSize    = 8  // i64 unix ms
	ResizeSize       = 4  // u16 rows + u16 cols
	ShutdownSize     = 0
	DataHeaderSize   = 8  // u64 sequence number (payload follows)
)
