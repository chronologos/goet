# goet Scratchpad

## Phase 1 Notes

### Protocol design
- 5-byte header (vs zet's 6) — no encryption flag needed, QUIC/TLS handles it
- Chose `any` return from ReadMessage/DecodePayload — type switch at call site
- Exported DecodePayload for fuzz testing (decodePayload is private)

### Catchup buffer
- Dual eviction: by byte count AND by slot count
- Slot count estimated from maxBytes/1024 — assumes ~1KB average message
- Payload copied on Store() to prevent caller mutation bugs
- sync.Mutex for concurrent safety (readers and writers)

### Auth
- HMAC-SHA256 is stdlib only — no external crypto deps
- Token = HMAC(passkey, TLS_exporter_material) — binds auth to TLS session
- Passkey never sent over wire, only HMAC of it

## Phase 2 Notes

### quic-go v0.59 API changes
- `quic.Connection` → `*quic.Conn` (concrete struct, not interface)
- `quic.Stream` methods have pointer receivers → need `*quic.Stream` for io.Reader/Writer
- `tls.ConnectionState.ExportKeyingMaterial` has pointer receiver — must store state in variable first

### CRITICAL: quic-go stream lazy creation
`OpenStreamSync` reserves a stream ID locally but does NOT send QUIC STREAM frames.
The remote peer's `AcceptStream` only fires on first `Write()` to the stream.
**Fix**: Client sends SequenceHeader{seq=0} on data stream immediately after opening.
This serves dual purpose: announces stream + carries reconnect info.

### Transport ownership
Client's `quic.Transport` (and its UDP socket) must be stored in `Conn.tr` to prevent GC.
Without this, the transport could be collected after `Dial()` returns.

### Handshake flow (updated)
```
Client → Server: QUIC connect (TLS 1.3)
Client → Server: AuthRequest{hmac_token}     (control stream)
Server → Client: AuthResponse{ok}             (control stream)
Client → Server: SequenceHeader{last_seq=0}   (data stream — triggers AcceptStream)
═══════ bidirectional data flow ═══════
```

## Phase 3 Notes

### Defer LIFO trap
Go defers run in LIFO order. When cleanup has ordering requirements (close QUIC conn before closing listener transport), use a single consolidated defer rather than multiple separate defers.

### Connection-tagged stream events
Old reader goroutines may still be draining after a reconnect. By tagging each `streamEvent` with `conn *transport.Conn` and comparing `ev.conn != s.conn`, stale events from old connections are silently discarded. No per-connection contexts needed.

### Race-free startup signaling
Polling `s.Port()` from the test goroutine while `Run()` writes to `s.ln` is a data race. Fixed with `Ready chan struct{}` — `Run()` closes it after binding, tests `select` on it.

### PTY bounce trick
On reconnect, shrink PTY by 1 row. Client sends its real size immediately, guaranteeing a genuine TIOCSWINSZ change → SIGWINCH → TUI apps redraw. Same technique as zet.

## Phase 4 Notes

### Pipe backpressure deadlock
When the client writes to `stdout` (a pipe) and nobody reads the other end, the pipe buffer fills and `Write` blocks forever, freezing the ioLoop. Tests that don't care about output must drain stdout with `io.Copy(io.Discard, ...)`.

### QUIC Shutdown/CONNECTION_CLOSE race
QUIC's `CloseWithError` sends CONNECTION_CLOSE immediately, racing with Shutdown frame data still in the send buffer. Fixed with 50ms sleep in session after writing Shutdown before returning (which triggers deferred `conn.Close()`).

### Terminal auto-detection
`New()` uses `term.IsTerminal(fd)` to skip raw mode for FIFOs/pipes. This lets the same binary work interactively (real terminal) and scripted (FIFOs in integration tests).

### Reconnect test: no Shutdown on disconnect
When simulating network death for reconnect testing, the first connection must NOT send Shutdown (which kills the session). Use raw `transport.Dial` for the first connection and `conn.Close()` without Shutdown. The full client's `cancel()` + `exitCancelled` path sends Shutdown, which is wrong for reconnect tests.

## Phase 5 Notes

### SSH as bootstrapping channel
SSH is only used to launch the remote session and exchange credentials. All terminal data flows over QUIC. The SSH subprocess is killed once the QUIC port is obtained.

### Hostname resolution fix
`transport.Dial` used `net.ParseIP(host)` which only accepts literal IPs. Changed to `net.ResolveUDPAddr("udp4", host:port)` which handles both IPs and hostnames with DNS resolution.

### BatchMode=yes
`SpawnSSH` passes `-o BatchMode=yes` to SSH so it fails fast if key auth isn't configured, rather than blocking on a password prompt that the user can't see (stdin is being used for the passkey protocol).

### E2E test symlink trick
SSH starts a fresh login shell with its own PATH. The E2E test symlinks the just-built binary to `~/.local/bin/goet` (which is in the default SSH PATH) for the duration of the test.

### Tailscale QUIC MTU fix
QUIC's default initial handshake packet is 1350 bytes with DF (Don't Fragment) set. Tailscale's WireGuard tunnel MTU is 1280 — packets over 1280 are silently dropped. Fix: `InitialPacketSize: 1200` in `quic.Config` (both client and server). 1200 is the RFC 9000 minimum. PMTU discovery finds the real path MTU after the handshake. See: https://github.com/tailscale/tailscale/issues/2633

## Phase 6a Plan: --profile Flag

### Key decision: QUIC-level RTT, no protocol changes
quic-go already computes RTT at the transport layer via `(*quic.Conn).ConnectionStats()`:
- `MinRTT`, `LatestRTT`, `SmoothedRTT` (EWMA per RFC 9002), `MeanDeviation`
- `BytesSent/Received`, `PacketsSent/Received`, `PacketsLost`

This is strictly better than application-level heartbeat RTT: higher sample rate (every ACK, not every 5s), no protocol changes, more accurate. `conn.QConn` is already an exported field on `transport.Conn`.

### Output format (stderr, never pollute terminal data)

**Periodic** (every heartbeat tick, 5s):
```
[profile] rtt=12.3ms (min=11.1ms smooth=12.0ms jitter=0.8ms) loss=0/142pkts
```

**Summary on exit** (via defer in ioLoop):
```
[profile] === Connection Profile ===
[profile] Duration: 2m34s
[profile] RTT: min=11.1ms smooth=12.0ms latest=12.3ms jitter=0.8ms
[profile] Traffic: sent=1.2MB/142pkts recv=45.3KB/89pkts lost=0pkts
```

### Files to modify
| File | Change |
|------|--------|
| `internal/client/client.go` | Add `Profile bool` to Config, `profileStart time.Time` to Client |
| `internal/client/profile.go` (new) | `logProfile()`, `logProfileSummary()`, `formatBytes()` |
| `cmd/goet/main.go` | Add `--profile` flag to both `runSSHClient` and `runClient` |
| `internal/client/client_test.go` | `TestProfileOutput` — verify `[profile]` appears on stderr |

### Wiring
- `ioLoop`: in `case <-heartbeat.C`, call `c.logProfile(conn)` if `c.cfg.Profile`
- `ioLoop`: `defer` calls `c.logProfileSummary(conn)` if `c.cfg.Profile`
- `Run()`: set `c.profileStart` on first successful dial

### Implementation order
1. Add `Profile` to Config, `profileStart` to Client
2. Create `internal/client/profile.go`
3. Wire into ioLoop (heartbeat + defer)
4. Add `--profile` to CLI
5. Test manually, add `TestProfileOutput`
