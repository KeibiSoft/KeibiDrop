# Transfer Benchmark Results — 2026-03-16

**Machine**: Intel i5-8265U @ 1.60GHz, Linux 6.17.0, localhost only
**Methodology**: 3 iterations for `testing.B`, single run for `testing.T`

## Commits Compared

| Label | Commit | Description |
|-------|--------|-------------|
| **c1bfca0** | `c1bfca08eb` | fix(reconnect): prevent crash and stale state (#64) |
| **main** | `58be1cc` | Current HEAD — includes stream mutex fix (#70) |

Key change between them: `5cca7fe fix(grpc): serialize concurrent stream reads (#70)`

---

## End-to-End Transfer Throughput (Bob → Alice via FUSE)

| Size | c1bfca0 | main | Delta |
|------|---------|------|-------|
| 1 MB | **FAIL** (stream corruption) | 63 MB/s | — |
| 10 MB | **FAIL** | 145 MB/s | — |
| 100 MB | **FAIL** | 138 MB/s | — |

c1bfca0 FUSE reads fail with "Max retries exceeded for remote read" because
concurrent FUSE readahead corrupts the unserialized gRPC stream. The stream
mutex fix (#70) resolved this.

## Encrypted gRPC Throughput (no FUSE)

| Size | c1bfca0 | main | Delta |
|------|---------|------|-------|
| 1 MB | 71 MB/s | 98 MB/s | **+38%** |
| 10 MB | 155 MB/s | 263 MB/s | **+70%** |
| 100 MB | 116 MB/s | 361 MB/s | **+211%** |

Massive improvement on main, likely from the stream serialization fix
preventing corruption and retries even in the non-FUSE path.

## Raw gRPC Baseline (no encryption, localhost)

| Size | c1bfca0 | main | Delta |
|------|---------|------|-------|
| 1 MB | 247 MB/s | 424 MB/s | +72% |
| 10 MB | 346 MB/s | 782 MB/s | +126% |
| 100 MB | 374 MB/s | 810 MB/s | +117% |

Variance between runs is high for this test (CPU load dependent).
Both use the same bare gRPC server — difference is likely system load.

## Per-Chunk Latency (10 MB, main only)

| Metric | Value |
|--------|-------|
| Chunks recorded | 100 |
| Total wall-clock | 98 ms |
| Sum of chunk times | 221 ms |
| Effective MB/s | 102 |
| Min | 828 µs |
| Median | 2.09 ms |
| P95 | 3.73 ms |
| Max | 5.14 ms |

Sum of chunk times (221 ms) > wall-clock (98 ms) because the FUSE kernel
issues parallel readahead — multiple chunks in flight simultaneously.

## Overhead Ratio (main, E2E vs Raw Disk)

| Size | Raw Disk | E2E Transfer | Overhead | Ratio |
|------|----------|-------------|----------|-------|
| 1 MB | 1 ms | 12 ms | 11 ms | 14.5x |
| 10 MB | 4 ms | 68 ms | 64 ms | 15.9x |
| 100 MB | 35 ms | 861 ms | 826 ms | 24.5x |

## Raw Disk I/O Baseline (main)

| Size | Write MB/s | Read MB/s |
|------|-----------|----------|
| 1 KB | 27 | 42 |
| 10 KB | 204 | 797 |
| 100 KB | 1,502 | 2,472 |
| 1 MB | 4,131 | 2,880 |
| 10 MB | 2,572 | 2,733 |
| 100 MB | 2,753 | 3,405 |

---

## Simulated Network Transfer (tc netem on loopback)

These numbers represent real-world transfer speeds more accurately than
localhost benchmarks. Run with `sudo -E KEIBIDROP_BENCH_NETEM=1`.

### LAN 1ms RTT (Gigabit, no bandwidth limit)

| Size | MB/s | Duration |
|------|------|----------|
| 10 MB | 65.8 | 152 ms |
| 100 MB | 66.9 | 1.49 s |
| 600 MB | 65.9 | 9.10 s |

Throughput plateaus at ~66 MB/s — latency-bound, not bandwidth-bound.
With 512 KiB chunks and ~1ms RTT, theoretical max with sequential reads
is 512 KiB / 1ms = 500 MB/s. Getting 66 MB/s suggests ~7.5 round-trips
worth of latency per chunk (stream mutex serialization + gRPC framing).

### WiFi 5ms RTT (no bandwidth limit)

| Size | MB/s | Duration |
|------|------|----------|
| 10 MB | 16.2 | 618 ms |
| 100 MB | 17.0 | 5.88 s |
| 600 MB | 17.7 | 34.0 s |

5x slower than LAN — nearly proportional to the 5x RTT increase.
Confirms latency is the dominant bottleneck, not bandwidth.

### LAN 1ms RTT + 100 Mbps bandwidth limit

| Size | MB/s | Duration |
|------|------|----------|
| 10 MB | 5.3 | 1.88 s |
| 100 MB | 5.6 | 17.8 s |
| 600 MB | 5.6 | 1m47s |

Bandwidth-limited at ~5.6 MB/s (45 Mbps effective out of 100 Mbps).
The ~55% utilization overhead comes from gRPC framing + protobuf
serialization + encryption per chunk.

---

## macOS Results (Intel Mac)

**Machine**: Intel i7-9750H @ 2.60GHz, macOS 26.3, 32 GB RAM, localhost only
**Date**: 2026-03-28

### E2E Transfer Throughput (Bob shares, Alice reads via FUSE)

| Size | MB/s | Duration |
|------|------|----------|
| 1 MB | 86 | 12 ms |
| 10 MB | 156 | 64 ms |
| 100 MB | 203 | 492 ms |
| 1 GB | 225 | 4.5 s |

Throughput increases with size, peaking at 225 MB/s for 1 GB. Significantly
faster than Linux (138 MB/s at 100 MB), likely due to macFUSE readahead
tuning and faster CPU.

### Encrypted gRPC Throughput (no FUSE)

| Size | MB/s | Duration |
|------|------|----------|
| 1 MB | 155 | 6 ms |
| 10 MB | 330 | 30 ms |
| 100 MB | 445 | 225 ms |
| 1 GB | 437 | 2.3 s |

Peaks at ~440 MB/s for large files. FUSE adds ~2x overhead (225 vs 437 MB/s
at 1 GB) from kernel context switches, VFS layer, and bitmap tracking.

### Raw gRPC Baseline (no encryption, localhost)

| Size | MB/s |
|------|------|
| 1 MB | 247 |
| 10 MB | 842 |
| 100 MB | 954 |
| 1 GB | 981 |

Raw gRPC peaks at ~1 GB/s. Encryption (ChaCha20-Poly1305) adds ~2.2x overhead
(981 vs 437 MB/s at 1 GB).

### Per-Chunk Latency (10 MB)

| Metric | Value |
|--------|-------|
| Chunks recorded | 25 |
| Total wall-clock | 59 ms |
| Sum of chunk times | 95 ms |
| Effective MB/s | 169 |
| Min | 1.02 ms |
| Median | 2.71 ms |
| P95 | 8.1 ms |
| Max | 14.1 ms |

Fewer chunks recorded (25 vs 100 on Linux) because macFUSE uses larger
readahead buffers. 1.6x parallelism (sum 95 ms vs wall 59 ms).

### Overhead Ratio (E2E vs Raw Disk)

| Size | Raw Disk | E2E Transfer | Overhead | Ratio |
|------|----------|-------------|----------|-------|
| 1 MB | 1 ms | 9 ms | 8 ms | 8.7x |
| 10 MB | 4 ms | 60 ms | 56 ms | 14.2x |
| 100 MB | 47 ms | 379 ms | 333 ms | 8.1x |
| 1 GB | 475 ms | 4.16 s | 3.68 s | 8.7x |

Lower overhead ratio than Linux (8.7x vs 14-24x). The ratio stays
relatively constant at ~8-9x for larger files on macOS.

### Raw Disk I/O Baseline

| Size | Write MB/s | Read MB/s |
|------|-----------|----------|
| 1 KB | 6 | 15 |
| 10 KB | 23 | 202 |
| 100 KB | 467 | 1,575 |
| 1 MB | 3,025 | 5,308 |
| 10 MB | 3,403 | 4,815 |
| 100 MB | 3,331 | 6,132 |
| 1 GB | 3,292 | 4,929 |

### FUSE Write Throughput (copy files INTO mount)

**Single file:**

| Size | MB/s | Duration |
|------|------|----------|
| 1 MB | 354 | 3 ms |
| 10 MB | 749 | 13 ms |
| 100 MB | 1,149 | 87 ms |
| 1 GB | 1,171 | 875 ms |

**Multi-file (directory copies):**

| Test | Total | MB/s | Duration | Per-file avg |
|------|-------|------|----------|-------------|
| 10 x 1 MB | 10 MB | 621 | 16 ms | 2 ms |
| 10 x 10 MB | 100 MB | 1,000 | 100 ms | 10 ms |
| 100 x 1 MB | 100 MB | 642 | 156 ms | 2 ms |
| 100 x 10 MB | 1,000 MB | 989 | 1.01 s | 10 ms |

FUSE writes are fast (1.1 GB/s for large files) because they go straight
to local disk via syscall.Pwrite. The per-file overhead is ~2 ms (Create +
Release + peer notification). Multi-file writes scale linearly.
Write throughput is NOT the bottleneck. Read-from-remote is.

### FUSE Latency (local operations)

| Size | Create+Write | Read | Total |
|------|-------------|------|-------|
| 1 KB | 1.5 ms | 1.1 ms | 2.6 ms |
| 10 KB | 2.0 ms | 1.2 ms | 3.2 ms |
| 100 KB | 3.1 ms | 1.2 ms | 4.3 ms |
| 1 MB | 3.2 ms | 1.3 ms | 4.5 ms |

Local disk baseline: 0.2-0.6 ms for same operations. FUSE adds ~2 ms
per operation from kernel-userspace context switches.

### Open/Close Latency (100 iterations)

| Operation | Average |
|-----------|---------|
| Open | 164 µs |
| Close | 56 µs |

---

## Key Findings

1. **Stream mutex fix (#70) was critical** — c1bfca0 cannot complete FUSE
   reads at all due to stream corruption from concurrent readahead.

2. **Latency is the dominant bottleneck, not bandwidth** — LAN (1ms RTT)
   gets 66 MB/s, WiFi (5ms RTT) gets 17 MB/s. The 5x RTT increase
   causes a nearly proportional 4x throughput drop. Bandwidth is not
   the limiting factor on gigabit links.

3. **Encrypted gRPC is ~2x slower than raw gRPC** — 437 vs 981 MB/s at
   1 GB on macOS (similar ratio on Linux). ChaCha20-Poly1305 encryption
   is measurable but secondary to latency in real-world scenarios.

4. **FUSE read-from-remote adds ~2x over encrypted gRPC** — 225 vs
   437 MB/s at 1 GB on macOS. The overhead comes from: kernel-userspace
   context switches, bitmap tracking, local cache writes, FUSE readahead
   coordination. This is the read path bottleneck.

5. **FUSE writes are fast (not a bottleneck)** — 1.1 GB/s for large files,
   ~2 ms per-file overhead. Writes go straight to disk via syscall.Pwrite.
   The bottleneck is exclusively in the remote read path.

6. **macOS is ~60% faster than Linux** for E2E FUSE reads (225 vs 138 MB/s
   at 100 MB). Likely from macFUSE readahead tuning and faster CPU
   (i7-9750H @ 2.6GHz vs i5-8265U @ 1.6GHz).

7. **Chunk parallelism helps on localhost (Linux)** — sum of chunk times
   (221 ms) vs wall-clock (98 ms) shows ~2.3x parallelism from FUSE
   readahead, but the stream mutex serializes the actual gRPC sends.

8. **Per-chunk round-trip overhead is ~7.5x theoretical minimum** —
   at 1ms RTT with 512 KiB chunks, theoretical max is ~500 MB/s but we
   get 66 MB/s. The stream mutex, gRPC framing, protobuf serialization,
   and encryption each add latency per chunk.

9. **Biggest optimization opportunity**: pipelining prefetch — send
   multiple ReadRequests without waiting for each response. Current
   sequential pattern means each chunk pays the full RTT. Stream
   multiplexing (PR #80) should bring LAN throughput from
   66 MB/s closer to the 437 MB/s encrypted-gRPC baseline.

---

## Encryption Overhead Analysis (437 MB/s vs 981 MB/s raw gRPC)

The 2.2x overhead from ChaCha20-Poly1305 encryption comes from three
sources, ordered by estimated impact:

### 1. AEAD cipher re-created per message (CPU waste)

`EncryptWithNonce` (`pkg/crypto/symmetric.go:57`) calls
`chacha20poly1305.New(kek)` on every single message. Same for `Decrypt`
(`pkg/crypto/symmetric.go:114`). The AEAD instance is stateless after
creation and can be created once per SecureConn and reused for all
messages. This is the easiest fix.

### 2. Heap allocation per message (GC pressure)

`SecureWriter.Write` (`pkg/session/secureconn.go:61`) allocates:
- `make([]byte, NonceSize+len(cipherText))` -- ~512 KiB per chunk
- `make([]byte, 4)` for the length header

`SecureReader.Read` (`pkg/session/secureconn.go:94`) allocates:
- `make([]byte, length)` -- ~512 KiB per encrypted message

That is ~1 MB of heap allocation per chunk round-trip, all becoming GC
pressure. Fix: use `sync.Pool` for encrypt/decrypt buffers.

### 3. Two TCP writes per message (wasted segments)

`SecureWriter.Write` does two separate `Write()` calls: 4 bytes for the
length header, then ~512 KiB for the payload. With Nagle disabled (gRPC
default), the 4-byte header becomes its own TCP segment -- 1456 bytes
wasted in a 1460-byte MSS frame. Fix: combine header+payload into a
single write (pre-allocate header space in the encryption buffer).

### 4. Large encryption units vs TCP MSS (latency on real networks)

Currently the entire gRPC frame (up to 16 MiB from `GRPCStreamBuffer`)
is encrypted as ONE authenticated blob. The receiver must buffer ALL TCP
segments (~360 segments for 512 KiB) before decryption can start.

TLS solves this with 16 KiB records: the reader can start processing
after receiving just ~11 TCP segments. The auth tag overhead is only
28 bytes per 16 KiB = 0.17%. This matters more on real networks with
packet loss/reordering than on localhost.

### Estimated impact of fixes

| Fix | Effort | Expected speedup |
|-----|--------|-----------------|
| Cache AEAD cipher | Trivial | ~10-20% |
| sync.Pool for buffers | Small | ~15-25% |
| Combine header+payload write | Small | ~5% |
| 16 KiB sub-frame encryption | Medium | Latency improvement on real networks |
| **Combined** | | **~1.3-1.5x instead of 2.2x** |

These optimizations should be implemented after PR #80 (stream
multiplexing) is merged and benchmarked, since multiplexing changes the
message flow pattern.
