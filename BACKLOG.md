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

## Phase 3: Session (Direct Mode) ✅
- [x] PTY spawn with creack/pty
- [x] PTY resize + SIGWINCH bounce (shrink-by-1 trick)
- [x] Session goroutine orchestrator + select loop
- [x] Accept + authenticate QUIC connections
- [x] Reconnect: swap streams, sequence exchange, catchup replay
- [x] CLI: `goet session -f <session-id> -p <port>`

## Phase 4: Client (Direct Mode) ✅
- [x] Terminal raw mode via x/term (auto-detects pipes vs terminals)
- [x] SIGWINCH handler
- [x] `~.` escape detection state machine
- [x] Client goroutine orchestrator + select loop
- [x] QUIC connect + reconnect loop with 1s constant delay
- [x] Client-side catchup buffer for resend on reconnect
- [x] CLI: `goet --local -p <port> -k <passkey-hex> [host]`
- [x] Integration tests: connect, reconnect, escape, session shutdown
- [x] Shell integration test: `tests/integration_test.sh`
- [x] `-race` clean under `go test -race ./...`

## Phase 5: SSH Integration ✅
- [x] Spawn SSH, pass passkey via stdin, read port from stdout
- [x] Hostname resolution in transport.Dial (was IP-only)
- [x] CLI: `goet [user@]host`
- [x] E2E test: `tests/e2e_ssh_test.sh`
- [x] Fuzz tests: escape processor (single + multi-call), parseDestination, catchup buffer (store/replay + eviction)

## Phase 6: Polish
- [x] `--profile` flag for RTT measurement (QUIC-level stats via ConnectionStats)
- [x] Write coalescing (2ms timer)
- [x] `-race` clean under `go test -race ./...`
- [x] Propagate client TERM to session (TerminalInfo message, deferred PTY spawn)
- [x] Signal handling edge cases (graceful Shutdown on ctx cancel)
- [x] PTY cleanup on crash (SIGHUP via ptmx.Close, 2s grace, SIGKILL fallback)
