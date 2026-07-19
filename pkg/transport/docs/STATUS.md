# keibidrop-transport, status

A standalone QUIC/TCP connection manager (Track B of KeibiDrop 0.4.0, see
`../../KeibiDrop/plan-0.4.0/01-quic-transport.md`). Module:
`github.com/KeibiSoft/keibidrop-transport`.

## Built and tested (green, `-race` clean)

- **Two channels per peer** (`manager.go`): a migratable QUIC channel (control +
  metadata + latency-sensitive cache-miss reads) and a TCP channel (bulk file transfer),
  both authenticated by KeibiDrop's post-quantum handshake (ML-KEM-1024 + X25519 +
  fingerprint) run over the stream (`secure_kd.go`, imports KeibiDrop `pkg/crypto`).
- **Survives a network change**: QUIC connection-ID migration (`Migrate`:
  AddPath/Probe/Switch), packet-counted; the TCP bulk channel reconstructs and `BulkGet`
  resumes by offset.
- **No 3-4s freeze**: `MissRead(ctx, offset, length)` fetches a byte range on the QUIC
  channel, isolated from TCP bulk by transport (512 KiB p50 ~7 ms under saturating bulk,
  byte-correct; ~1 RTT on a WAN vs 3066 ms head-of-line-blocked).
- **Heartbeat** via the transport's own `Control.Ping` (`proto/control`), decoupled from
  the app's service; 16 MiB gRPC frame (matches KeibiDrop's tuned StreamFile frame).
- **macOS syscall batching** in the quic-go fork (`github.com/KeibiSoft/quic-go`):
  `sendmsg_x` (send, unconditional) + adaptive `recvmsg_x` (receive, engages only on real
  bursts). Cuts send CPU; the accumulate-delay to force receive batching is a measured
  dead end (QUIC flow control).

## Dependencies

- grpc: stable `google.golang.org/grpc v1.72.1` (no fork).
- quic-go: **local `replace => ../quic-go`**, switch to `github.com/KeibiSoft/quic-go`
  before a public push.
- KeibiDrop (for `pkg/crypto`): **local `replace => ../../KeibiDrop`**, switch to
  `require github.com/KeibiSoft/KeibiDrop@<commit>` (it is public) before a public push.

## Remaining (target-focused)

1. Switch the two local replaces to public deps, then commit + push to the new repo.
2. Thesis / rationale doc (the WHY), in the style of iroh's docs.
3. Rewrite `README.md` (from throwaway-experiment framing to a package README).
4. KeibiDrop integration: the full-duplex inbound side (mirror the TCP pair, QUIC pair
   reusing the same port numbers on UDP), wire behind a `transport=tcp|quic` config flag,
   then the relay fallback.

## Docs

- `docs/NETWORKING_LIBRARY_DESIGN.md`, architecture + the package API.
