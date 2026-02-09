# goet — Reconnectable Remote Terminal

A reconnectable remote terminal built in Go with QUIC transport. Inspired by [Eternal Terminal](https://github.com/MystenLabs/EternalTerminal) but redesigned from scratch — fewer processes, fewer dependencies, and QUIC instead of TCP.

The remote host must have `goet` installed. SSH is used only to bootstrap the session; all terminal I/O flows over QUIC/UDP. For scrollback and window management, use a terminal multiplexer like [tmux](https://github.com/tmux/tmux).

## Why QUIC?

- **Fast reconnect** — 1-RTT QUIC handshake vs 2-RTT TCP+TLS on network changes
- **TLS 1.3 built in** — no application-level crypto needed
- **Multiplexed streams** — resize/heartbeat never blocked behind bulk data
- **No broker needed** — each session listens on its own UDP port

## Architecture

```
Client ──SSH──→ goet session (spawns, opens QUIC listener, prints port)
       ← kill SSH ←
Client ←─QUIC/UDP─→ goet session (direct, multiplexed streams)
```

Two QUIC streams:
- **Stream 0 (Control)**: Auth, TerminalInfo, resize, heartbeat, shutdown, session→client sequence header
- **Stream 1 (Data)**: Terminal stdin/stdout with sequence numbers, client→session sequence header

On first connect, the client sends `TerminalInfo` with its `$TERM` value. The session defers PTY spawn until this arrives, so the remote shell gets the correct terminal type.

Write coalescing batches small writes into fewer, larger Data messages (2ms deadline, 32KB threshold) to reduce per-message overhead during fast output.

See [docs/architecture.md](docs/architecture.md) for detailed diagrams of the system topology and full connection lifecycle.

## Usage

```bash
# SSH mode (normal usage)
goet user@host
# ~. to disconnect

# With RTT profiling (stats to stderr every 5s + summary on exit)
goet --profile user@host
```

## Direct Mode (for development)

```bash
# Generate a 32-byte passkey
PASSKEY=$(head -c32 /dev/urandom | xxd -p -c64)

# Terminal 1: Session (reads passkey from stdin, prints port to stdout)
echo "$PASSKEY" | goet session -f test-session -p 0

# Terminal 2: Client (passkey via -k flag, host defaults to 127.0.0.1)
goet --local -p <PORT> -k "$PASSKEY"
# ~. to disconnect
```

## UDP Buffer Sizes

QUIC requires larger UDP buffers than most OS defaults. If you see:

```
failed to sufficiently increase receive buffer size (was: 208 kiB, wanted: 7168 kiB, got: 416 kiB)
```

Note: in SSH mode, this warning comes from the **remote** machine (the session's stderr is forwarded via SSH), not your local machine.

**Linux** (on the remote host):

```bash
sudo sysctl -w net.core.rmem_max=26214400
sudo sysctl -w net.core.wmem_max=26214400

# Persist across reboots
printf 'net.core.rmem_max=26214400\nnet.core.wmem_max=26214400\n' | sudo tee /etc/sysctl.d/quic-buffers.conf
```

**macOS**:

```bash
sudo sysctl -w kern.ipc.maxsockbuf=26214400
```

## Building

```bash
go build -o goet ./cmd/goet
```

## Testing

```bash
go test ./...              # unit tests
go test -race ./...        # race detector
./tests/integration_test.sh  # client↔session E2E
./tests/e2e_ssh_test.sh     # SSH E2E (requires SSH key auth to localhost)

# Fuzz tests
go test -fuzz=FuzzReadMessage -fuzztime=30s ./internal/protocol/
go test -fuzz='^FuzzEscapeProcess$' -fuzztime=30s ./internal/client/
```

## Status

Core implementation complete (Phases 1–6). See [BACKLOG.md](BACKLOG.md) for future work.

See [COMPARISON.md](COMPARISON.md) for an architectural comparison with [zet](https://github.com/chronologos/zet) and [EternalTerminal](https://github.com/MisterTea/EternalTerminal).
