# KeibiDrop networking library, design

How to package everything the spikes measured into one transport library that makes
the best use of gRPC-over-TCP *and* gRPC-over-QUIC, hides the latency of cache misses,
survives network changes, and stays correct. Grounded in the measured results in
[RATIONALE.md](RATIONALE.md); nothing here is aspirational beyond what a number backs.

## What the measurements decided

Every routing choice below follows from a measured result, not a preference:

| Fact (measured) | Consequence for the library |
|---|---|
| Random seek under saturating prefetch: **QUIC 109 ms vs TCP 3066 ms (28x)** on a real WAN | The **interactive read path runs on QUIC**, each urgent read on its own stream. |
| Pure bulk on a lossy Wi-Fi link: **quic-go 2.7–16x slower** than kernel TCP | **Bulk whole-file transfer runs on TCP** (default). The old "fall back to QUIC on lossy links" premise is inverted. |
| QUIC migrates across an IP change (connection ID), TCP cannot | **Control plane runs on QUIC**; it is the network-change detector and the always-on survivor. |
| macOS QUIC is syscall-bound at ~137 MB/s (no UDP GSO); `sendmsg_x` lifts it to ~700 MB/s | On **fast, clean links** bulk *may* move to QUIC+`sendmsg_x`; until that lands, TCP is the bulk default. |
| KeibiDrop PQC SecureConn over QUIC costs **0 MB/s (within noise)** | Wrap **every** connection (TCP and QUIC, control and data) in the PQC handshake. No perf reason not to. |
| Handshake is 0.82 ms; TCP dies on 5-tuple change; QUIC migration is a control property | Migration lives on the cheap always-on channel, so a network change never triggers a full re-handshake on the hot path. |

**One sentence:** QUIC for the things users *feel* (a seek that does not stall, a
session that survives a Wi-Fi hop); TCP for the thing users *wait on* (bulk bytes on a
bad link). The library's job is to route each byte to the transport that measured best
for it, and to make that routing invisible.

## Architecture: two planes, one manager

```
                        ┌─────────────────── Session (one peer) ───────────────────┐
                        │                                                            │
   control plane  ──────┤  QUIC connection (migratable)                              │
   (always QUIC)        │    • heartbeat / RTT probe        • presence               │
                        │    • metadata: file announce, • rekey signal           │
                        │      BatchNotify, edit-notify     • "network changed" event │
                        │                                                            │
   data plane   ────────┤  adaptive, per-transfer:                                   │
   (TCP or QUIC)        │    • interactive read  → QUIC, urgent read = its own stream │
                        │    • bulk transfer     → TCP (clean/lossy), QUIC+sendmsg_x  │
                        │                          on fast clean links               │
                        │                                                            │
                        │  every conn wrapped by the PQC handshake + SecureConn      │
                        └────────────────────────────────────────────────────────────┘
```

- **Control plane, always QUIC, always on.** Low bandwidth, so the macOS syscall
  ceiling is irrelevant to it. It carries heartbeats (which give a live RTT estimate),
  metadata, and rekey/notify signals. Its migration *is* the network-change detector:
  when the QUIC path moves to a new local address, the manager emits a "network
  changed" event that the data plane reacts to.
- **Data plane, adaptive per transfer.** A `TransportManager` chooses TCP or QUIC for
  each transfer from the link quality the control plane already measured. Interactive
  reads always go QUIC (the fast lane); bulk defaults to TCP.
- **One identity across both.** The same X25519 + ML-KEM-1024 identity and fingerprint
  authenticate every connection. `SecureKD` (proven) over QUIC; KeibiDrop's existing
  `handshake.go` over TCP. Both derive a `SecureConn`; gRPC runs on top of either.

**Concretely, two gRPC channels per peer** (this is the shape that matters): one QUIC
gRPC (control + interactive seeks) and one TCP gRPC (bulk, the transport KeibiDrop
already ships). Routing a seek to QUIC and bulk to TCP isolates them *by transport*, no
per-stream-key protocol, no listener rewrite. A cache miss is served in about one round
trip (~100 ms on a WAN) instead of queuing behind bulk for seconds.

**Full-duplex pair, mirroring today's TCP.** KeibiDrop runs two TCP connections per peer
(Alice-inbound ← Bob-outbound, Alice-outbound → Bob-inbound), each independently keyed.
The QUIC plane adds its own inbound/outbound pair the same way, and can reuse the same
port numbers on UDP. So a fully connected pair is four channels: TCP-in/out (bulk) and
QUIC-in/out (interactive+control). The `Manager` here models one peer's outbound side;
the symmetric inbound side and the four-channel wiring are the Phase-3 KeibiDrop
integration.

**Relay fallback is after the library.** When direct P2P is impossible (client
isolation, hostile NAT), both planes fall back to a forwarding relay where both peers
are clients to a stable relay (so both get QUIC migration). This is deferred until the
direct-path library is solid.

## The routing table (data plane)

```
traffic class            transport      why (measured)
─────────────────────────────────────────────────────────────────────────────
control / signaling      QUIC           migration; tiny, ceiling irrelevant
interactive read (seek)  QUIC stream    109 ms vs 3066 ms under prefetch (28x)
prefetch / read-ahead    QUIC stream    same conn as the seek, separate stream,
                                        paced + deprioritized (no self-congestion)
bulk whole-file, lossy   TCP            quic-go 2.7–16x slower on real Wi-Fi
bulk whole-file, fast    TCP → QUIC     TCP now; QUIC+sendmsg_x when it lands (~700 MB/s)
                         +sendmsg_x     and migration comes free
```

Link quality comes from the control-plane heartbeat (RTT) plus a loss signal
(retransmit/NACK rate observed on an active transfer). Two buckets is enough to start:
**clean** (LAN / datacenter, low RTT, ~0 loss) vs **degraded** (Wi-Fi / WAN, jitter,
real loss). Do not over-model; the seek path is QUIC either way, and only the bulk
transport flips.

## Hiding cache-miss latency (the core UX)

An on-demand filesystem read that misses the local cache is a network round trip the
user is blocked on. Three mechanisms, in order of impact:

1. **Fast-lane stream.** The urgent read gets its **own QUIC stream**, so its bytes
   never queue behind the prefetch already in flight. This is the entire 109 ms vs
   3066 ms result: on TCP the seek sits behind the prefetch in one byte stream (head-of-
   line blocking, the "self-congestion" we saw in playback); on QUIC the library
   interleaves the urgent stream's packets with the bulk. A cache miss costs ~1 RTT
   regardless of how much prefetch is running.
2. **Predictive read-ahead.** Keep the next blocks buffered ahead of the playhead
   (already in KeibiDrop). A miss that read-ahead already covered costs 0 RTT. The fast
   lane is the safety net for the misses read-ahead did not predict (a random seek).
3. **Paced, deprioritized prefetch.** Prefetch fills the pipe but must yield to an
   urgent read. Pace it (do not dump 16 MiB at once) and mark its stream low priority so
   the QUIC scheduler serves the urgent stream first. This is what stops prefetch from
   self-congesting the very reads it exists to accelerate.

Net effect: the cache miss the user triggers is served in about one round trip, and the
bulk keeps flowing underneath it. The frame tuned at 16 MiB in KeibiDrop stays for
bulk; the urgent read uses a small first chunk (512 KiB, KeibiDrop's block) so time-to-
first-byte is one RTT, not one 16 MiB window.

## Surviving network changes

```
  Wi-Fi ─────► LTE (laptop moves, local IP changes)
    │
    │  1. QUIC control plane migrates (connection ID, not 5-tuple). Session intact,
    │     no teardown, no relay round trip, no PQ re-handshake.
    │  2. Manager sees the migration → emits "network changed".
    │  3. Data plane reacts:
    │       • active interactive QUIC transfer: migrates with the connection, no stall.
    │       • active bulk TCP transfer: the TCP 5-tuple is dead → re-dial to the new
    │         path (or relay) and resume from the last acked offset (range read).
    │  4. If direct P2P is now impossible (client isolation, changed NAT), fall to the
    │     relay. On the relay path BOTH peers are QUIC clients to a stable relay, so
    │     both get migration, the one case direct P2P cannot cover transparently is
    │     exactly the case that already pushes you onto the relay.
```

Two properties make this good rather than merely working:

- **The session identity outlives the path.** Because the control plane migrates, the
  peer relationship, the negotiated keys, and the metadata state all survive. Only the
  bulk TCP byte-stream has to be re-pinned, and it resumes by offset, not from zero.
- **Fast, honest failure when direct is impossible.** From the field notes: on an
  isolated network (own IP is a /32, all direct dials fast-fail) skip the 15 s timeout,
  go straight to relay, and tell the user "this network blocks device-to-device
  (common on hotel/guest Wi-Fi), connected via relay" instead of the misleading "peer
  did not respond."

## Security: PQC everywhere, one identity

- Every connection, control or data, TCP or QUIC, runs KeibiDrop's post-quantum
  handshake and is wrapped in `SecureConn`. Proven for QUIC in Phase 2 (`SecureKD`),
  already shipping for TCP.
- QUIC's own TLS is left as a throwaway (Go 1.24+ already makes its key exchange
  post-quantum, so confidentiality there is a bonus, not the guarantee). The guarantee
  is our KEM+fingerprint layer, which is what authenticates the peer.
- One `Identity` (X25519 + ML-KEM-1024) and one fingerprint authenticate all of it, so
  a peer is verified once, out of band, and every transport to it inherits that trust.

## Package shape

Built and tested in the spike (`manager.go`); this is the actual API. One `Manager` per
peer, two gRPC channels, both `SecureKD`-authenticated with one identity/fingerprint:

```go
// bring the channels up
func DialControl(ctx, quicAddr string, id *Identity, peerFP string) (*Manager, error) // QUIC control plane
func (m *Manager) DialBulk(ctx, tcpAddr string) error   // TCP bulk channel

// QUIC channel: control + metadata + the fast cache-miss path (all latency-sensitive)
func (m *Manager) MissRead(ctx, offset uint64, length uint32) ([]byte, error) // ~1 RTT, isolated from bulk
func (m *Manager) InteractiveConn() *grpc.ClientConn     // the raw QUIC channel, for further RPCs

// TCP channel: bulk file transfer, resumes from the last offset after a network change
func (m *Manager) BulkGet(ctx, total uint64, chunk uint32, onChunk func(off uint64, b []byte)) error
func (m *Manager) BulkConn() *grpc.ClientConn

// health + network change: heartbeat RTT, link quality, migration, bulk reconstruct
func (m *Manager) RTT() time.Duration
func (m *Manager) LinkQuality() Quality                  // Clean | Degraded | Dead
func (m *Manager) OnNetworkChange(func(NetworkEvent))    // fired when the QUIC control plane migrates
func (m *Manager) Migrate(ctx) error                     // AddPath -> Probe -> Switch, then reconstruct bulk
func (m *Manager) Close() error
```

KeibiDrop integration: the session layer holds a `*Manager` per peer instead of raw
`Inbound`/`Outbound` sockets. FUSE reads ask the manager for an interactive conn; the
`kd add`/pull path asks for a bulk conn. `transport=tcp|quic|auto` config selects the
bulk default; `auto` uses `LinkQuality()`. Default `auto`, with a manual override for
debugging.

## Correctness and testing

- **Deterministic routing.** Transport selection is a pure function of `LinkQuality` +
  traffic class, so it is unit-testable without a network. Table test every cell of the
  routing table.
- **Teardown is the sharp edge.** The QUIC `Close` must stay `CancelWrite`-based (the
  Phase-1 deadlock). Every conn the manager hands out inherits `quicConn.Close`; keep
  the `-race` teardown tests when generalizing.
- **Migration under load.** Extend the migration proof: run an interactive transfer,
  migrate mid-stream, assert bytes stay correct and the bulk TCP transfer re-pins by
  offset. Add a `tc netem` / real-WAN variant.
- **Security tests travel with it.** The Phase-2 confidentiality / integrity / auth /
  directional-key tests are the acceptance bar for any conn the manager returns.

## Phased rollout

1. **Manager skeleton + control plane. DONE** (`manager.go`, `-race` clean). QUIC control
   conn secured with `SecureKD`, gRPC heartbeat (Echo for now) measuring live RTT,
   `LinkQuality()` (clean/degraded/dead), `OnNetworkChange` + `Migrate` (AddPath→Probe→
   Switch) proven to fire the event *and keep the control plane answering across an
   address change*, and `BulkTransport()` routing (clean→QUIC, else→TCP). Tests:
   `TestManagerControlPlane`, `TestManagerMigration`, `TestBulkRouting`.
2. **Interactive path. DONE** (`manager.go`, `-race`). Two gRPC channels per peer: the
   QUIC channel (`InteractiveConn`) for control + seeks, the TCP channel (`DialBulk`/
   `BulkConn`) for bulk. A seek on QUIC is isolated from bulk on TCP *by transport* (no
   per-stream-key protocol, no listener rewrite). On a network change, `Migrate`
   reconstructs the TCP bulk channel (closing it aborts its in-flight RPCs = the "cancel
   the TCP calls", then re-dials). Tests: `TestInteractiveFastLane` (fast lane vs muxed,
   loopback; magnitude is the WAN urgent_bench 109 ms vs 3066 ms), `TestManagerBulkReconstructOnMigrate`.
3. **Bulk path + `auto` + resume. DONE** (`manager.go`, `service.go`, proto, `-race`).
   `bulkTransportFor` gated on `quicBulkViable` (false today, so TCP everywhere honestly;
   flips to clean-link-QUIC when sendmsg_x lands). `BulkGet` streams over the TCP channel
   with a `start_offset` resume: interrupt it mid-transfer (a reconstruction) and it
   reissues Download from the last received offset, not from zero. Tests:
   `TestBulkRouting`, `TestBulkResumeAcrossReconstruct` (interrupted at 16 MiB of 32,
   resumed byte-correct). Measured: secured TCP `BulkGet` **907-939 MB/s** vs plain-TCP
   gRPC 1267 (the gap is framing + per-message alloc, not crypto). CPU profile of the
   secured bulk path: 34% syscalls, ~25% scheduling, 8% memclr, and only **0.63%
   AES-GCM** (AES-NI is nearly free on the fast path), so KeibiDrop's *pooled* SecureConn
   (the experiment's is unpooled) would recover most of that gap.
4. **`sendmsg_x` + `recvmsg_x` batching. DONE, wired into the fork** (`-race` + no-`-race`).
   Send batching (`sendmsg_x`) is unconditional in `sendQueue.Run`; receive batching
   (`recvmsg_x`) is an adaptive `batchConn`. Both ABI-verified standalone first. Measured:
   send **3.04 datagrams/syscall** (`sendmsg` 33% → 18% CPU); recv batches only when packets
   **burst** (**38/syscall under flood**), and loopback never bursts, so recv is adaptive
   (plain reads otherwise, never a regression, loopback back to ~140 MB/s). Two `-race`-
   masked bugs found and fixed (dual-stack v4-mapped sockaddr; partial `sendmsg_x` sends).
   Honest result: on a single-host round-trip the path is receive/pipeline-bound, so the win
   is **CPU freed on send** (matters against the FUSE filesystem), not loopback throughput;
   the throughput payoff needs a fast bursty real network. Full war story + methodology in
   [RATIONALE.md](RATIONALE.md). `quicBulkViable` still gated pending a real fast-link number.
5. **Retire the teardown-based reconnect** where the control plane's migration already
   keeps the session green.

## Open questions

- **Interactive vs bulk classification.** Who decides a read is "interactive"? Simplest:
  size + access pattern (small random read = interactive; large sequential = bulk). FUSE
  already distinguishes on-demand reads from prefetch, so the signal exists.
- **QUIC stream priority API.** quic-go's scheduler priority for the urgent-vs-prefetch
  split needs confirming (or a small fork hook, like `sendmsg_x`).
- **One QUIC conn, many streams vs several conns.** The spike used one stream per conn.
  The interactive path wants many streams on one migratable conn; verify migration still
  holds with N live streams.
- **Loss signal.** RTT is easy (heartbeat). A cheap, reliable loss estimate for the
  clean/degraded decision needs picking (transfer-observed retransmits vs an explicit
  probe).
