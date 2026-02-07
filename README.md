# goet — Reconnectable Remote Terminal

A reconnectable remote terminal built in Go with QUIC transport. Inspired by [zet](https://github.com/iantay/zet) but redesigned from scratch.

## Why QUIC?

- **Connection migration** — survives WiFi→cellular IP changes transparently
- **TLS 1.3 built in** — no custom crypto
- **Multiplexed streams** — resize/heartbeat never blocked behind bulk data
- **No broker needed** — each session listens on its own UDP port

## Architecture

```
Client ──SSH──→ goet-session (spawns, daemonizes, opens QUIC listener)
Client ←──────→ goet-session (direct QUIC, multiplexed streams)
```

Two QUIC streams:
- **Stream 0 (Control)**: Auth, resize, heartbeat, shutdown, sequence headers
- **Stream 1 (Data)**: Terminal stdin/stdout with sequence numbers for catchup

## Building

```bash
go build -o goet ./cmd/goet
```

## Testing

```bash
go test ./...              # unit tests
go test -race ./...        # race detector
go test -fuzz=FuzzReadMessage -fuzztime=30s ./internal/protocol/  # fuzz
```

## Direct Mode (for development)

```bash
# Generate credentials
PASSKEY=$(head -c32 /dev/urandom | xxd -p -c64)
SESSION=$(head -c16 /dev/urandom | xxd -p -c32)

# Terminal 1: Session
echo "$PASSKEY" | goet-session -f "$SESSION" -p 3000

# Terminal 2: Client
goet --local -p 3000 -k "$PASSKEY" -s "$SESSION" 127.0.0.1
```

## Status

Phase 1 complete: protocol encoding/decoding, authentication (HMAC-SHA256), catchup ring buffer.

See [BACKLOG.md](BACKLOG.md) for remaining work.
