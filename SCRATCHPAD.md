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
