# Rationale: why this transport looks the way it does

This is the argument behind `keibidrop-transport`: what problem it solves, which
choices are load-bearing, and what we measured rather than assumed. Every number
here comes from real hardware (a macOS laptop, a Linux VPS, and a laptop-to-VPS
link across a real WAN), not from a model.

## The problem

KeibiDrop is a peer-to-peer filesystem. You mount a remote peer's files and read
them on demand: open a 40 GB video, seek to the middle, and only the bytes you
touch cross the wire. Two things about that are hard over a plain TCP connection.

**A network change kills the connection.** A laptop that moves from cafe Wi-Fi to
a phone hotspot changes its IP address. TCP identifies a connection by its
5-tuple, so the socket dies, and the transfer pays a full recovery: tear down,
re-register and look up on the relay, redo the handshake, rebuild the gRPC
server and client. Seconds of dead air, every in-flight read dropped. This is
worst on exactly the networks where it happens most, hotel and guest Wi-Fi,
where client isolation already forces everything through a relay.

**A cache miss freezes the read.** During playback a prefetch pulls bulk data in
the background to stay ahead of the playhead. When the viewer seeks to a spot
that is not buffered, that read is small and urgent, but if it shares one byte
stream with the prefetch it sits behind everything already queued in the send
buffer. On a real link that is a multi-second stall on a single click.

## The idea: two transports, each for what it measured best at

We did not set out to keep two transports. The measurements argued for it.

- **QUIC for the things you feel.** QUIC keys a connection on a connection ID,
  not the 5-tuple, so it survives an address change with the streams intact. And
  it gives each stream its own flow control, so an urgent read on its own stream
  is not head-of-line-blocked by a bulk transfer.
- **TCP for the thing you wait on.** For pure bulk throughput on a lossy link,
  mature kernel TCP still wins by a lot.

So the QUIC channel carries control, metadata, and interactive cache-miss reads;
the TCP channel carries bulk file transfer. Routing each byte to the transport
that measured best for it is the whole design.

## What we measured (including where we were wrong)

- **Migration works.** A live gRPC channel moved to a new UDP socket mid-transfer
  (AddPath, Probe, Switch), verified with per-socket packet counters: the old
  path goes silent, the new path carries both directions, the transfer continues.
  TCP cannot do this at all.
- **The seek is the decisive result.** On the real WAN, a random 16 KiB read
  while a bulk stream saturates the link took **109 ms on QUIC and 3066 ms on
  TCP**, a 28x difference. TCP head-of-line-blocks the seek behind the prefetch;
  QUIC gives it its own stream. For on-demand seekable media this is the whole
  point.
- **Bulk on a lossy link favors TCP.** We went in half-expecting the common claim
  that QUIC does better on lossy links. On a real hotel Wi-Fi link the opposite
  held: a single quic-go stream collapsed to about 2 Mbps of a 45 Mbps link while
  kernel TCP filled it, 2.7 to 16x slower. So bulk stays on TCP, and the "fall
  back to QUIC on a bad link" idea we started with was dropped.
- **The post-quantum layer is nearly free.** Running KeibiDrop's ML-KEM-1024
  handshake and its AEAD SecureConn over QUIC cost 0 MB/s within measurement
  noise, because on the syscall-bound path the crypto is a fraction of a percent
  of the CPU.
- **macOS batching is a CPU win, not a throughput one.** macOS has no UDP
  segmentation offload, so QUIC issues one send syscall per packet. The
  undocumented `sendmsg_x` syscall batches them (about 3 datagrams per call in
  practice, `sendmsg` from 33% to 18% of CPU). The receive counterpart only
  engages when the network itself delivers a burst; you cannot force it from the
  receiver, because slowing the receiver just makes QUIC's flow control stall the
  sender. The full story is in the blog post at keibisoft.com/blog/grpc-over-quic-seek-latency.html.

## What is load-bearing

- **gRPC runs over one QUIC stream via a small `net.Conn` adapter.** gRPC only
  needs a reliable, ordered byte stream, which a QUIC stream is. This is the same
  seam KeibiDrop already uses to run gRPC over its encrypted `net.Conn`, so QUIC
  slots in underneath without touching the RPC layer. It buys migration, not
  HTTP/3's cross-stream multiplexing (that would need real HTTP/3, out of scope).
- **Authentication is by fingerprint. There are no certificates and no PKI.** A
  device's identity is a pair of KEM keys (X25519 + ML-KEM-1024). A peer is
  trusted because only the holder of the private keys that hash to the fingerprint
  you verified out of band can complete the handshake and read the channel,
  exactly as KeibiDrop already does over TCP. QUIC's protocol mandates a TLS 1.3
  credential, so the spike hands it a throwaway self-signed one that carries no
  identity and is never verified: a protocol placeholder, not part of the trust
  model. On Go 1.24+ that key exchange is already post-quantum, which adds
  confidentiality beneath our layer, but the authentication is the fingerprint.
- **The miss path is a dedicated fast lane, not a tuning knob.** Hiding cache-miss
  latency comes from putting the urgent read on its own QUIC stream, isolated from
  bulk by transport. It owes nothing to the syscall batching or to clever pacing.

## What this is not

Not HTTP/3. Not a claim that QUIC is faster than TCP; for bulk on a lossy link it
is measurably slower, and we route around that. Not wired into KeibiDrop yet: this
is the spike and the design (Track B of the 0.4.0 plan). The integration, the
full-duplex inbound side, and the relay fallback are the next steps, tracked in
STATUS.md.
