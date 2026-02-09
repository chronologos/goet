# Architecture Comparison: goet vs zet vs EternalTerminal

Three projects solving the same problem — surviving network interruptions in remote terminal sessions — with fundamentally different transport and process-model decisions.

## At a Glance

| | **goet** | **zet** | **EternalTerminal** |
|---|---|---|---|
| **Language** | Go (~2.5K LOC) | Zig (~8K LOC) | C++ (~11K LOC) |
| **Transport** | QUIC (UDP) + TLS 1.3 | TCP + app-level encryption | TCP + app-level encryption |
| **Process model** | 2-process (client + session) | 3-process (client + broker + session) | 3-process (et + etserver + etterminal) |
| **Encryption** | TLS 1.3 (via QUIC) | XChaCha20-Poly1305 + BLAKE3 KDF | XSalsa20-Poly1305 (libsodium) |
| **Auth binding** | HMAC-SHA256 over TLS exporter material | Passkey -> derived keys | Passkey -> NaCl secretbox keys |
| **Wire format** | 5B header (len + type) | 6B header (len + enc + type) | 4B len + 2B internal (enc + type) |
| **Serialization** | Hand-rolled binary | Hand-rolled binary | Protobuf |
| **Catchup buffer** | 64MB ring buffer (entry-slot based) | 64MB ring buffer (byte-offset based) | deque-based backup (byte limit) |
| **Write coalescing** | Yes (2ms deadline, 32KB threshold) | No | No |
| **Port forwarding** | No | OAuth-only (automatic) | Full (forward + reverse + jumphosts) |
| **Reverse channel** | No | Yes (URL, clipboard, OAuth) | No |
| **Dependencies** | quic-go, creack/pty, x/term | Zero (Zig stdlib only) | protobuf, libsodium, gflags, boost |

## Transport: QUIC vs TCP

The most consequential architectural divergence.

### goet: QUIC

- **Fast reconnect** — 1-RTT QUIC handshake (vs 2-RTT TCP+TLS) on IP changes; true connection migration not yet enabled in quic-go
- **Built-in TLS 1.3** — no need for application-level encryption
- **Stream multiplexing** — control (stream 0) and data (stream 1) are independent, preventing head-of-line blocking between heartbeats and terminal data
- **MTU tuning** — initial packet size set to 1200 (Tailscale compatibility; default 1350 gets dropped)

Tradeoff: QUIC has higher per-packet overhead than TCP, requires UDP hole-punching through some NATs, and the dependency on quic-go is substantial.

### zet and ET: TCP

- Reliable delivery guaranteed by kernel
- Works through all NATs without special configuration
- Requires application-level encryption (XChaCha20/XSalsa20) since TCP has no built-in crypto
- Single multiplexed stream — all message types share one TCP connection
- Reconnection requires a full TCP reconnect (no connection migration)

## Process Model

### goet: 2-Process (client + session)

```
Client                          Server
+-------+                    +-----------+
|  goet |---- QUIC/TLS ----->|goet session| --> shell (PTY)
+-------+                    +-----------+
                              (own UDP port)
```

- Client SSHs to remote, launches `goet session -p 0` (random port)
- Session listens on its own QUIC port
- Client kills SSH, connects directly via QUIC
- No broker — each session owns its own UDP port
- Simpler architecture, but can't multiplex sessions on port 2022

### zet: 3-Process (client + broker + session)

```
Client                          Server
+-----+                    +------------+
| zet |---- TCP/TLS ------->| zet-server | (broker, port 2022)
+-----+                    +-----+------+
                                 | SCM_RIGHTS (fd passing)
                           +-----v------+
                           |zet-session | (per-user)
                           +-----+------+
                                 | PTY
                           +-----v------+
                           |   shell    |
                           +------------+
```

- Client SSHs to remote, launches `zet-session`
- Session daemonizes, registers with `zet-server` broker via Unix socket
- Broker owns port 2022, routes client connections by session ID
- Broker passes client fd to session via SCM_RIGHTS — zero-copy handoff, broker doesn't proxy traffic

### ET: 3-Process (et + etserver + etterminal)

Same conceptual model as zet. Broker uses FIFO for IPC with sessions. Key difference: ET's broker stays in the data path via `ServerClientConnection`, while zet's broker hands off the fd entirely.

### Why 3 Processes?

The 3-process model offloads authentication to SSH — the broker runs unprivileged, sessions run as the connecting user, nobody needs root. goet achieves this differently: SSH only bootstraps the session, and HMAC-SHA256 bound to TLS exporter material handles auth. Both avoid custom password auth, but goet requires an ephemeral port per session while the 3-process model multiplexes through port 2022.

## Encryption

| | **goet** | **zet** | **ET** |
|---|---|---|---|
| **Algorithm** | TLS 1.3 (QUIC built-in) | XChaCha20-Poly1305 | XSalsa20-Poly1305 (libsodium) |
| **Key exchange** | HMAC-SHA256(passkey, TLS exporter) | BLAKE3 KDF(passkey, nonces) | Passkey used directly as NaCl key |
| **Key derivation** | N/A (TLS handles it) | Separate c2s/s2c keys via domain separation | Nonce MSB differentiates directions |
| **Nonce scheme** | N/A (TLS handles it) | Counter-based (base_nonce + u64 counter) | Counter-based (manual increment) |
| **Dependencies** | None (QUIC built-in) | None (Zig std.crypto) | libsodium |

zet's crypto is the most carefully designed: proper KDF with BLAKE3, separate keys per direction (prevents reflection attacks), fresh nonces on every reconnect. ET uses the passkey directly with a nonce MSB to differentiate directions. goet sidesteps the problem entirely by letting TLS handle everything — less custom crypto means fewer potential bugs.

## Wire Protocol

**goet** — 5-byte header, no encryption flag needed (QUIC encrypts everything):
```
[4B payload_length BE][1B msg_type]
```
Two QUIC streams prevent head-of-line blocking. 8 message types.

**zet** — 6-byte header, encryption flag needed for plaintext handshake:
```
[4B payload_length BE][1B encrypted][1B msg_type]
```
17 message types (includes reverse channel and port forwarding). Single TCP connection.

**ET** — 4-byte length + 2-byte internal header, protobuf for structured messages:
```
[4B packet_length BE] + [1B encrypted_flag][1B header_type][payload]
```
11 terminal packet types + 3 base types across two `.proto` files.

Both goet and zet prove that hand-rolled binary protocols work fine for this domain — the message types are simple enough that protobuf adds overhead without much benefit.

## Catchup Buffer

All three buffer sent data with sequence numbers and replay on reconnect.

**goet**: Entry-slot ring buffer. Fixed slot count (estimated from `maxBytes/1024`), each storing `(seq, []byte)`. Evicts by byte count OR slot count. Linear scan for replay. Both client and session maintain independent buffers.

**zet**: Byte-offset ring buffer. 64MB backing store with separate index ring (1M entries max). Data written contiguously with 4-byte length prefix. O(log n) binary search for lookup. Iterator-based replay.

**ET**: `deque<Packet>` with byte-size limit. Packets stored front-to-back, recovered by counting backwards. `reverse()` on recovery result. Heap-allocated strings per entry.

All three maintain catchup buffers on both sides (client and session), enabling bidirectional replay.

## Write Coalescing

Only goet has this. The `Coalescer` batches small PTY reads into fewer, larger Data messages:

- **2ms deadline** from first byte (not debounced)
- **32KB threshold** for immediate flush (matches read buffer size)
- Reduces per-message 13-byte header overhead on fast output

zet and ET send each read as its own message. TCP's Nagle algorithm provides some coalescing, but it's less controlled.

Write coalescing matters more for QUIC than TCP. TCP has Nagle and kernel-level send buffering. QUIC, being userspace, sends each `Write()` as a discrete QUIC STREAM frame. Without coalescing, goet would send far more UDP packets than necessary.

## PTY (Pseudo-Terminal)

A PTY is a kernel mechanism that fakes a hardware terminal — a pair of file descriptors where the slave side looks like a real terminal to the shell (supports `isatty()`, `ioctl` for window size, generates `SIGWINCH`, handles Ctrl+C -> SIGINT), while the master side is a regular fd that the session process reads/writes.

Without a PTY, piped stdin/stdout breaks vim, less, htop, line editing, job control, and anything that checks `isatty()`.

goet's `spawnPTY()` starts a login shell (prepends `-` to argv[0] for profile loading) and starts at 24x80. The client sends its `$TERM` value via `TerminalInfo` on first connect; the session defers PTY spawn until this arrives, so the remote shell gets the correct terminal type (falling back to `xterm-256color` if empty or invalid). The `bouncePTYSize()` trick shrinks the PTY by 1 row on reconnect so the client's real size always triggers a genuine `SIGWINCH`, forcing TUI redraw.

## Features Beyond Core Terminal

| Feature | goet | zet | ET |
|---|---|---|---|
| Forward port forwarding | No | No | Yes |
| Reverse port forwarding | No | OAuth-only (automatic) | Yes |
| Jumphosts | No | No | Yes |
| Clipboard sharing | No | Yes (get/set/image) | No |
| URL opening | No | Yes (zet-open) | No |
| Multiplexer (HTM) | No | No | Yes (separate) |
| RTT profiling | Yes (--profile) | No | No |
| ~. escape | Yes | Yes | No |

zet's reverse channel (clipboard, URL opening, OAuth port forwarding) is the most user-facing innovation — `gh auth login` "just works" inside a remote zet session.

## Reliability

**goet wins in mobile/roaming scenarios, zet/ET win everywhere else.**

goet's QUIC connection migration means IP changes don't register as disconnections. But QUIC uses UDP, which is blocked by many corporate firewalls, handled poorly by some NATs, and requires each session to open its own ephemeral port.

zet and ET's TCP works through every NAT, every firewall, every corporate proxy. The broker on port 2022 means one port to open. For a tool you rely on daily, TCP's universality is the safer bet.

## Latency

### Interactive (keystroke -> echo)

zet is likely fastest — no coalescing delay, direct syscalls, `writev()` for header+payload in single syscall. goet adds up to 2ms per keystroke from write coalescing on the client side. ET has protobuf serialization overhead and mutex contention from the thread-pool model.

### Bulk throughput (cat bigfile)

goet likely wins — write coalescing batches many small PTY reads into fewer, larger QUIC packets. QUIC's stream multiplexing means heartbeats don't block data delivery. zet and ET send each PTY read as its own message.

### QUIC vs TCP performance

| | QUIC (goet) | TCP (zet, ET) |
|---|---|---|
| Connection setup | 1 RTT (faster) | 2 RTTs |
| Reconnect after IP change | 1 RTT (app-level reconnect) | Full reconnect (~3 RTTs) |
| Head-of-line blocking | None (independent streams) | Yes (all data ordered) |
| Raw throughput | Lower (userspace, Go scheduler) | Higher (kernel, HW offload) |
| Per-packet CPU | Higher (userspace TLS) | Lower (kTLS, NIC offload) |
| Firewall compatibility | Worse (UDP often blocked) | Universal |

For terminal I/O specifically (a few KB/s of keystrokes, maybe MB/s of output), the protocol difference is negligible. Physical RTT (10-200ms) dominates. The choice is really about operational properties — migration vs firewall traversal — not speed.

### Summary

| | Most reliable | Lowest interactive latency | Best bulk throughput |
|---|---|---|---|
| **Winner** | zet (TCP universality + clean broker) | zet (no coalescing, direct syscalls) | goet (write coalescing + QUIC multiplexing) |
| **Runner-up** | ET (same TCP, proven in production) | goet (2ms coalescing penalty) | zet (TCP buffering helps) |

## Lineage

ET established the architecture (3-process, TCP, NaCl, catchup buffers, SSH bootstrapping). zet reimplemented it in Zig, removing protobuf/libsodium, adding reverse channel and SCM_RIGHTS fd passing. goet branched off with QUIC, dropped the broker, and added write coalescing.

Each iteration removes dependencies and complexity while keeping the core insight: **sequence-numbered catchup buffers over encrypted connections, bootstrapped via SSH**.
