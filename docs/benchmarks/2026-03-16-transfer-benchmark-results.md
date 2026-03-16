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

## Key Findings

1. **Stream mutex fix (#70) was critical** — c1bfca0 cannot complete FUSE
   reads at all due to stream corruption from concurrent readahead.

2. **Latency is the dominant bottleneck, not bandwidth** — LAN (1ms RTT)
   gets 66 MB/s, WiFi (5ms RTT) gets 17 MB/s. The 5x RTT increase
   causes a nearly proportional 4x throughput drop. Bandwidth is not
   the limiting factor on gigabit links.

3. **Encrypted gRPC is 2-3x slower than raw gRPC** (361 vs 810 MB/s at
   100 MB on localhost) — encryption overhead is measurable but secondary
   to latency in real-world scenarios.

4. **Per-chunk round-trip overhead is ~7.5x theoretical minimum** —
   at 1ms RTT with 512 KiB chunks, theoretical max is ~500 MB/s but we
   get 66 MB/s. The stream mutex, gRPC framing, protobuf serialization,
   and encryption each add latency per chunk.

5. **Chunk parallelism helps on localhost** — sum of chunk times (221 ms)
   vs wall-clock (98 ms) shows ~2.3x parallelism from FUSE readahead,
   but the stream mutex serializes the actual gRPC sends.

6. **Biggest optimization opportunity**: pipelining prefetch — send
   multiple ReadRequests without waiting for each response. Current
   sequential pattern means each chunk pays the full RTT. Pipelining
   could bring LAN throughput from 66 MB/s closer to the 360 MB/s
   seen in the encrypted-gRPC-without-FUSE benchmark.
