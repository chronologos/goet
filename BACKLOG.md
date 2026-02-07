# goet Backlog

## Phase 1: Protocol + Auth + Catchup ✅
- [x] Protocol constants and message types
- [x] Message encode/decode with round-trip tests
- [x] Fuzz tests for all decoders
- [x] Auth: passkey generation, HMAC token compute/verify
- [x] Catchup ring buffer with byte-based eviction

## Phase 2: QUIC Transport Layer ✅
- [x] Ephemeral TLS cert generation (self-signed, in-memory)
- [x] Session QUIC listener (`session_listener.go`)
- [x] Client QUIC dialer (`client_dialer.go`)
- [x] Stream multiplexing: open/accept control + data streams
- [x] Framed message reader/writer over QUIC streams (`streams.go`)
- [x] Loopback integration test: connect, auth, bidirectional exchange
- [x] Auth rejection test with wrong passkey

## Phase 3: Session (Direct Mode)
- [ ] PTY spawn with creack/pty
- [ ] PTY resize + SIGWINCH bounce (shrink-by-1 trick)
- [ ] Session goroutine orchestrator + select loop
- [ ] Accept + authenticate QUIC connections
- [ ] Reconnect: swap streams, sequence exchange, catchup replay
- [ ] Daemonize via re-exec pattern
- [ ] CLI: `goet-session -f <session-id> -p <port>`

## Phase 4: Client (Direct Mode)
- [ ] Terminal raw mode via x/term
- [ ] SIGWINCH handler
- [ ] `~.` escape detection state machine
- [ ] Client goroutine orchestrator + select loop
- [ ] QUIC connect + reconnect loop
- [ ] CLI: `goet --local -p <port> -k <key> -s <session> <host>`
- [ ] Integration tests: connect, reconnect, escape, wrong passkey

## Phase 5: SSH Integration
- [ ] Spawn SSH, pass passkey via stdin, read port from stdout
- [ ] CLI: `goet user@host`
- [ ] E2E test: full SSH flow

## Phase 6: Polish
- [ ] Write coalescing (2ms timer)
- [ ] `--profile` flag for RTT measurement
- [ ] `-race` clean under `go test -race ./...`
- [ ] Signal handling edge cases
- [ ] PTY cleanup on crash
