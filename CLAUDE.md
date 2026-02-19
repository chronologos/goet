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
./tests/integration_test.sh                # client↔session E2E (builds binary, uses FIFOs)
./tests/e2e_ssh_test.sh                    # SSH E2E (requires SSH key auth to localhost)
./tests/load_test.sh                       # E2E load/integrity (QUIC + TCP)

# Fuzz tests (use ^ and $ for exact match when names share prefix)
# Crash-finding (no-panic on malformed input)
/usr/local/go/bin/go test -fuzz=FuzzReadMessage -fuzztime=30s ./internal/protocol/
/usr/local/go/bin/go test -fuzz='^FuzzEscapeProcess$' -fuzztime=30s ./internal/client/
/usr/local/go/bin/go test -fuzz='^FuzzEscapeProcessMultiCall$' -fuzztime=30s ./internal/client/
/usr/local/go/bin/go test -fuzz='^FuzzParseDestination$' -fuzztime=30s ./internal/client/
/usr/local/go/bin/go test -fuzz='^FuzzBufferStoreReplay$' -fuzztime=30s ./internal/catchup/
/usr/local/go/bin/go test -fuzz='^FuzzBufferEviction$' -fuzztime=30s ./internal/catchup/
# Round-trip / invariant (encode→decode equality, data integrity)
/usr/local/go/bin/go test -fuzz='^FuzzRoundTripData$' -fuzztime=30s ./internal/protocol/
/usr/local/go/bin/go test -fuzz='^FuzzRoundTripHeartbeat$' -fuzztime=30s ./internal/protocol/
/usr/local/go/bin/go test -fuzz='^FuzzRoundTripResize$' -fuzztime=30s ./internal/protocol/
/usr/local/go/bin/go test -fuzz='^FuzzRoundTripSequenceHeader$' -fuzztime=30s ./internal/protocol/
/usr/local/go/bin/go test -fuzz='^FuzzRoundTripTerminalInfo$' -fuzztime=30s ./internal/protocol/
/usr/local/go/bin/go test -fuzz='^FuzzRoundTripAuthRequest$' -fuzztime=30s ./internal/protocol/
/usr/local/go/bin/go test -fuzz='^FuzzBufferDataIntegrity$' -fuzztime=30s ./internal/catchup/
/usr/local/go/bin/go test -fuzz='^FuzzCoalescerDataIntegrity$' -fuzztime=30s ./internal/coalesce/
```

## Local Development Testing

```bash
# SSH mode (easiest — single command)
./goet user@host
# ~. to disconnect

# Auto-install or upgrade goet on remote, then connect
./goet --install user@host

# Use TCP+TLS instead of QUIC (for networks that block UDP)
./goet --tcp user@host

# With RTT profiling (stats to stderr every 5s + summary on exit)
./goet --profile user@host

# Direct mode (manual credentials, for protocol debugging)
PASSKEY=$(head -c32 /dev/urandom | xxd -p -c64)

# Terminal 1: Session (passkey via stdin, prints port to stdout)
echo "$PASSKEY" | ./goet session -f test -p 0

# Terminal 2: Client (passkey via -k flag)
./goet --local -p <PORT> -k "$PASSKEY" 127.0.0.1
# ~. to disconnect
```

## Architecture

- **QUIC transport** with TLS 1.3 (quic-go)
- **Two-process model**: client + session (no broker)
- **Two QUIC streams**: control (stream 0) + data (stream 1)
- **HMAC-SHA256 auth** bound to TLS exporter material
- **4KB ring buffer** for reconnect catchup (both sides, configurable via `--replay-size`)
- **Write coalescing** — 2ms deadline timer + 32KB threshold batches small writes into fewer Data messages (`internal/coalesce/`)
- **`~.` escape** detection via 3-state machine in client
- **SSH bootstrapping**: `goet user@host` spawns SSH to launch remote session, then QUIC for data
- **TCP+TLS fallback**: `goet --tcp` for networks that block UDP; session always listens on both
- **Auto-install/upgrade**: `goet --install user@host` installs or upgrades goet on remote (compares commit hash, self-transfer or GitHub release)

## Protocol

5-byte header: `[4B length BE][1B type]`. All multi-byte integers big-endian.
Max payload 4MB. See `internal/protocol/constants.go` for message types.

## Style Notes

- Prefer stdlib over dependencies
- No allocations in hot paths where avoidable
- `internal/` for all non-cmd packages
- Tests colocated with source (`_test.go`)
- Fuzz tests for decoders, escape processor, parseDestination, catchup buffer

## Releasing

1. Bump `VERSION` file (e.g. `0.3.0`)
2. Push to main

That's it. GitHub Actions watches for VERSION changes, creates the git tag, and runs goreleaser.
Do NOT manually `git tag` — CI handles tagging from the VERSION file.
`version.VERSION` and `version.Commit` are set via `-ldflags` at build time.

## Dependencies

```
github.com/quic-go/quic-go  # QUIC transport
golang.org/x/term            # Terminal raw mode
github.com/creack/pty        # PTY management
```
