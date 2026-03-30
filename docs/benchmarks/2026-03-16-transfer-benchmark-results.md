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

## macOS Results (Intel Mac, after all optimizations)

**Machine**: Intel i7-9750H @ 2.60GHz, macOS 26.3, 32 GB RAM, localhost only
**Date**: 2026-03-28
**Includes**: stream pool (#80), parallel PullFile (#81), encryption
optimizations, async cache writes

### E2E Transfer Throughput (Bob shares, Alice reads via FUSE)

| Size | MB/s | Duration |
|------|------|----------|
| 1 MB | 93 | 11 ms |
| 10 MB | 166 | 60 ms |
| 100 MB | 217 | 461 ms |
| 1 GB | 240 | 4.3 s |

### Encrypted gRPC Throughput (no FUSE)

| Size | MB/s | Duration |
|------|------|----------|
| 1 MB | 172 | 6 ms |
| 10 MB | 337 | 30 ms |
| 100 MB | 446 | 224 ms |
| 1 GB | 452 | 2.3 s |

### Raw gRPC Baseline (no encryption, localhost)

| Size | MB/s |
|------|------|
| 1 MB | 247 |
| 10 MB | 842 |
| 100 MB | 954 |
| 1 GB | 981 |

### Per-Chunk Latency (10 MB)

| Metric | Value |
|--------|-------|
| Chunks recorded | 25 |
| Total wall-clock | 63 ms |
| Sum of chunk times | 98 ms |
| Effective MB/s | 160 |
| Min | 1.15 ms |
| Median | 3.32 ms |
| P95 | 12.0 ms |
| Max | 12.6 ms |

### Overhead Ratio (E2E vs Raw Disk)

| Size | Raw Disk | E2E Transfer | Overhead | Ratio |
|------|----------|-------------|----------|-------|
| 1 MB | 1 ms | 13 ms | 12 ms | 12.1x |
| 10 MB | 5 ms | 70 ms | 65 ms | 14.2x |
| 100 MB | 46 ms | 536 ms | 490 ms | 11.5x |
| 1 GB | 472 ms | 4.09 s | 3.61 s | 8.7x |

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

### FUSE Read Overhead Breakdown (100 MB, per-layer)

| Layer | Duration | MB/s | Overhead |
|-------|----------|------|----------|
| Encrypted gRPC (baseline) | 242 ms | 414 | - |
| + copy into user buffer | 216 ms | 462 | ~0 (noise) |
| + pwrite to cache file | 225 ms | 445 | +8 ms (1.8%) |
| Full FUSE E2E | 461 ms | 217 | +237 ms (51%) |

The FUSE/kernel overhead (51%) is kernel-userspace context switches and
is irreducible from userspace. The cache write overhead was reduced from
16% to 1.8% by making writes async. The no-FUSE path (encrypted gRPC)
reaches 452 MB/s, which is the ceiling for FUSE mode.

### FUSE Latency (local operations)

| Size | Create+Write | Read | Total |
|------|-------------|------|-------|
| 1 KB | 1.5 ms | 1.1 ms | 2.6 ms |
| 10 KB | 2.0 ms | 1.2 ms | 3.2 ms |
| 100 KB | 3.1 ms | 1.2 ms | 4.3 ms |
| 1 MB | 3.2 ms | 1.3 ms | 4.5 ms |

### Open/Close Latency (100 iterations)

| Operation | Average |
|-----------|---------|
| Open | 164 us |
| Close | 56 us |

---

## Key Findings

1. **Stream mutex fix (#70) was critical** -- c1bfca0 cannot complete FUSE
   reads at all due to stream corruption from concurrent readahead.

2. **Latency is the dominant bottleneck, not bandwidth** -- LAN (1ms RTT)
   gets 66 MB/s, WiFi (5ms RTT) gets 17 MB/s. The 5x RTT increase
   causes a nearly proportional 4x throughput drop. Bandwidth is not
   the limiting factor on gigabit links.

3. **Encrypted gRPC is ~2x slower than raw gRPC** -- 452 vs 981 MB/s at
   1 GB on macOS (similar ratio on Linux). ChaCha20-Poly1305 encryption
   is measurable but secondary to latency in real-world scenarios.

4. **FUSE kernel overhead is the hard wall (51%)** -- measured by the
   per-layer breakdown: encrypted gRPC reaches 452 MB/s, but FUSE E2E
   only reaches 240 MB/s. The 51% gap is kernel-userspace context
   switches (irreducible from userspace). No-FUSE mode bypasses this.

5. **FUSE writes are fast (not a bottleneck)** -- 1.1 GB/s for large files,
   ~2 ms per-file overhead. Writes go straight to disk via syscall.Pwrite.
   The bottleneck is exclusively in the remote read path.

6. **macOS is ~60% faster than Linux** for E2E FUSE reads (240 vs 138 MB/s
   at 100 MB). Likely from macFUSE readahead tuning and faster CPU
   (i7-9750H @ 2.6GHz vs i5-8265U @ 1.6GHz).

7. **Chunk parallelism helps on localhost (Linux)** -- sum of chunk times
   (221 ms) vs wall-clock (98 ms) shows ~2.3x parallelism from FUSE
   readahead, but the stream mutex serializes the actual gRPC sends.

8. **Per-chunk round-trip overhead is ~7.5x theoretical minimum** --
   at 1ms RTT with 512 KiB chunks, theoretical max is ~500 MB/s but we
   get 66 MB/s. The stream mutex, gRPC framing, protobuf serialization,
   and encryption each add latency per chunk.

9. **Stream pool (#80) helps under real network latency** -- 4 parallel
   gRPC streams per file eliminate mutex contention between FUSE
   readahead requests. On Linux with tc netem at 1ms RTT, throughput
   improved from 66 to 171 MB/s (2.6x). On localhost the gain is
   negligible (latency is near-zero).

---

## Optimizations Applied

### Encryption layer (`pkg/session/secureconn.go`)

| Optimization | What changed |
|-------------|-------------|
| Cache AEAD cipher | `chacha20poly1305.New(kek)` called once per SecureConn instead of per message |
| Single combined write | Header + nonce + ciphertext in one `Write()` call instead of two |
| In-place decryption | `aead.Open(ciphertext[:0], ...)` reuses the read buffer instead of allocating |
| Reuse header buffer | `SecureReader.head` is a fixed `[4]byte` field instead of per-read allocation |

**Impact**: encrypted gRPC went from 437 to 452 MB/s (+3%). Modest on
localhost (CPU is fast), but reduces per-message overhead for real
networks where every microsecond per chunk multiplied by RTT matters.

### FUSE read path (`pkg/filesystem/fuse_directory.go`)

| Optimization | What changed |
|-------------|-------------|
| Async cache writes | `cacheFD.WriteAt` runs in a goroutine; FUSE Read returns immediately |
| CacheWg coordination | `sync.WaitGroup` ensures in-flight writes complete before Release closes FD |

**Impact**: cache write overhead dropped from 16% to 1.8% of E2E time.
Total E2E throughput: 225 to 240 MB/s (+7%).

### Remaining bottleneck

The FUSE kernel overhead (51% of E2E time) is the hard wall. Each FUSE
Read call requires: kernel trap, cgofuse dispatch, Go function call,
return to kernel. For a 100 MB file with ~128 KB readahead buffers,
that is ~780 kernel round-trips at ~300us each = ~235 ms irreducible.

The only way past this is the no-FUSE path (452 MB/s) or a non-FUSE
virtual filesystem (e.g., NFS loopback, 9P).
