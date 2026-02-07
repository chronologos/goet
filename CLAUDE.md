# goet — Claude Code Instructions

For architecture and design rationale, see [README.md](README.md).
For remaining work, see [BACKLOG.md](BACKLOG.md).

## Go Version

Go 1.25.7 installed at `/usr/local/go/bin/go` (ARM64).
Not in PATH by default — use full path or add to shell config.

## Running Tests

```bash
/usr/local/go/bin/go test ./...            # unit tests
/usr/local/go/bin/go test -race ./...      # race detector
/usr/local/go/bin/go test -fuzz=FuzzReadMessage -fuzztime=30s ./internal/protocol/
```

## Architecture

- **QUIC transport** with TLS 1.3 (quic-go)
- **Two-process model**: client + session (no broker)
- **Two QUIC streams**: control (stream 0) + data (stream 1)
- **HMAC-SHA256 auth** bound to TLS exporter material
- **64MB ring buffer** for reconnect catchup

## Protocol

5-byte header: `[4B length BE][1B type]`. All multi-byte integers big-endian.
Max payload 4MB. See `internal/protocol/constants.go` for message types.

## Style Notes

- Prefer stdlib over dependencies
- No allocations in hot paths where avoidable
- `internal/` for all non-cmd packages
- Tests colocated with source (`_test.go`)
- Fuzz tests for all decoders

## Dependencies

```
github.com/quic-go/quic-go  # QUIC transport
golang.org/x/sys             # Unix syscalls
golang.org/x/term            # Terminal raw mode
github.com/creack/pty        # PTY management
```
