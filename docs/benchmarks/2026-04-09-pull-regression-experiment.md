# Pull Strategy Regression Experiment

**Date**: 2026-04-09  
**Machine**: Intel i5, 16GB RAM, Linux 6.17.0, Ubuntu (Wayland)  
**Branch**: `unikernel` (HEAD: `0b8ed91` — "Remove non parallel downloads")  
**Methodology**: McGeoch-style — Latin-square interleaved, 4 reps (rep 1 warmup discarded), load guard < 1.0

---

## 1. Background

Commit `0b8ed91` ("Remove non parallel downloads") removed `pullStreamFile()` — a zero-round-trip server-push streaming function. The remaining path, `pullParallelRead()`, uses 4 gRPC bidirectional streams with one send/recv per chunk.

Earlier ad-hoc measurements suggested:
- Loopback 1GB: −16% (196.7 → 165.5 MB/s)
- FUSE over WAN 1GB: −60% (11.9 → 4.7 MB/s)

This experiment was designed to confirm or refute that root cause under controlled conditions.

---

## 2. Treatments

| ID | Description |
|----|-------------|
| T0 | `cp` baseline — raw local I/O ceiling, no KeibiDrop |
| T1 | `unikernel` as-is — only `pullParallelRead()` (commit `0b8ed91` present) |
| T2 | `unikernel` with `0b8ed91` reverted — restores `pullStreamFile()`, always uses it |
| T3 | Same as T2 + adaptive: `pullStreamFile()` for files > 64 chunks (~32MB), `pullParallelRead()` for smaller |

All treatments used pre-compiled test binaries (`go test -c`) from isolated git worktrees. No changes to the actual repo.

---

## 3. Results

### Table 1: Mean Throughput MB/s (reps 2–4, ±1 stdev)

| Size  | Mode    | T0 (cp)         | T1 (current)    | T2 (reverted)   | T3 (adaptive)  |
|-------|---------|-----------------|-----------------|-----------------|----------------|
| 10MB  | no-FUSE | —               | 428.4 ±8.9      | 400.4 ±30.7     | 399.9 ±34.7    |
| 100MB | no-FUSE | —               | 567.3 ±33.2     | 575.8 ±14.9     | 569.1 ±26.5    |
| 1GB   | no-FUSE | —               | 409.6 ±142.6    | 441.4 ±134.4    | 252.7 ±76.1    |
| 10MB  | cp      | 2000.0          | —               | —               | —              |
| 100MB | cp      | 3774.9 ±100.7   | —               | —               | —              |
| 1GB   | cp      | 4188.6 ±60.6    | —               | —               | —              |

### Table 2: Speedup Ratios vs T1

| Size  | Mode    | T2/T1 | T3/T1 |
|-------|---------|-------|-------|
| 10MB  | no-FUSE | 0.93x | 0.93x |
| 100MB | no-FUSE | 1.01x | 1.00x |
| 1GB   | no-FUSE | 1.08x | 0.62x |

---

## 4. Analysis

### Primary finding: T2 ≈ T1

Reverting `0b8ed91` (T2) does **not** consistently outperform the current code (T1). Ratios are 0.93–1.08× — well within the noise of the 1GB measurements (stdev ±134–142 MB/s). The null hypothesis cannot be rejected.

**Conclusion: the 1GB throughput regression is NOT caused by pull strategy removal alone.**

### T3 notably worse at 1GB

T3 (adaptive streaming for >32MB files) measured 252 MB/s vs T1's 410 MB/s at 1GB — a 38% regression. This is the opposite of the expected direction. Possible explanations:
- `pullStreamFile()` has a startup cost that dominates at the 1GB scale in loopback (no actual network bottleneck to amortize)
- The adaptive threshold (64 chunks × 512KiB = 32MB) triggers streaming for 100MB and 1GB but loopback does not benefit from server-push
- `pullStreamFile()` may have a bug or suboptimal buffer sizing for loopback conditions

### High variance at 1GB

The 1GB no-FUSE standard deviations are 130–142 MB/s (~33% of mean). This suggests OS-level interference (page reclaim, scheduler jitter) is dominating at large sizes. Even with the load guard, background system activity varied significantly between runs.

### The earlier ad-hoc regression may have been real — but for different reasons

The −16% loopback regression observed in the initial benchmark (196 → 165 MB/s) could be:
1. **Real, but caused by a different factor** — BlockSize (1MiB) vs ChunkSize (512KiB) mismatch means `pullParallelRead()` issues 2 gRPC requests per bitmap chunk, while `pullStreamFile()` streamed continuously. The overhead may only manifest at specific OS scheduler states.
2. **Noise** — the earlier test had fewer reps and no interleaving.
3. **FUSE-path specific** — the WAN FUSE regression (11.9 → 4.7 MB/s) is more plausible as real since FUSE readahead stalls are predictable; that path was not benchmarked here.

---

## 5. Decision Matrix Outcome

| T2 vs T1 | T3 vs T1 | Conclusion |
|-----------|----------|------------|
| T2 ~= T1 (ratios 0.93–1.08) | T3 < T1 at 1GB | **Regression NOT from pull strategy removal** |

**Action**: Do NOT revert `0b8ed91` based on this data alone. Investigate:

1. **BlockSize/ChunkSize mismatch**: `config.BlockSize = 1MiB`, `filesystem.ChunkSize = 512KiB`. Each gRPC stream in `pullParallelRead()` requests 1MiB blocks but the bitmap tracks 512KiB chunks — double the request rate. Check whether aligning these reduces overhead.
2. **FUSE WAN regression separately**: The 4.7 MB/s FUSE-over-WAN result from earlier is more likely real. Test that path specifically with a WAN peer before deciding on `0b8ed91`.
3. **Bitmap save frequency**: Every 100 chunks triggers a disk write. At 512KiB/chunk and 1GB file = 2048 chunks → 20 bitmap saves per download. Profile whether this contributes.

---

## 6. Raw Data

Raw TSV archived at `/tmp/kd-bench-raw.tsv`. Copy preserved below:

```
timestamp	treatment	mode	size	rep	mbps	wall_sec
2026-04-09T21:14:11+03:00	T0	cp	10MB	1	1000.00	0.010
2026-04-09T21:14:11+03:00	T0	cp	10MB	2	2000.00	0.005
2026-04-09T21:14:11+03:00	T0	cp	10MB	3	2000.00	0.005
2026-04-09T21:14:11+03:00	T0	cp	100MB	1	3846.15	0.026
2026-04-09T21:14:11+03:00	T0	cp	100MB	2	3703.70	0.027
2026-04-09T21:14:11+03:00	T0	cp	100MB	3	3846.15	0.026
2026-04-09T21:14:11+03:00	T0	cp	1GB	1	4248.96	0.241
2026-04-09T21:14:12+03:00	T0	cp	1GB	2	4231.40	0.242
2026-04-09T21:14:12+03:00	T0	cp	1GB	3	4145.75	0.247
2026-04-09T21:14:12+03:00	T1	no-FUSE	10MB	1	423.92	0.024
2026-04-09T21:14:12+03:00	T1	no-FUSE	100MB	1	566.72	0.176
2026-04-09T21:14:13+03:00	T1	no-FUSE	1GB	1	409.69	2.499
2026-04-09T21:14:18+03:00	T2	no-FUSE	10MB	1	369.50	0.027
2026-04-09T21:14:43+03:00	T2	no-FUSE	100MB	1	555.64	0.180
2026-04-09T21:14:44+03:00	T2	no-FUSE	1GB	1	469.07	2.183
2026-04-09T21:14:49+03:00	T3	no-FUSE	10MB	1	400.89	0.025
2026-04-09T21:14:49+03:00	T3	no-FUSE	100MB	1	545.15	0.183
2026-04-09T21:14:49+03:00	T3	no-FUSE	1GB	1	347.91	2.943
2026-04-09T21:14:55+03:00	T2	no-FUSE	10MB	2	411.60	0.024
2026-04-09T21:14:55+03:00	T2	no-FUSE	100MB	2	571.83	0.175
2026-04-09T21:14:56+03:00	T2	no-FUSE	1GB	2	293.88	3.484
2026-04-09T21:15:02+03:00	T3	no-FUSE	10MB	2	432.20	0.023
2026-04-09T21:15:42+03:00	T3	no-FUSE	100MB	2	542.42	0.184
2026-04-09T21:15:42+03:00	T3	no-FUSE	1GB	2	333.30	3.072
2026-04-09T21:15:48+03:00	T1	no-FUSE	10MB	2	431.86	0.023
2026-04-09T21:16:08+03:00	T1	no-FUSE	100MB	2	548.37	0.182
2026-04-09T21:16:09+03:00	T1	no-FUSE	1GB	2	572.69	1.788
2026-04-09T21:16:13+03:00	T3	no-FUSE	10MB	3	404.38	0.025
2026-04-09T21:16:13+03:00	T3	no-FUSE	100MB	3	569.34	0.176
2026-04-09T21:16:14+03:00	T3	no-FUSE	1GB	3	182.12	5.623
2026-04-09T21:16:22+03:00	T1	no-FUSE	10MB	3	418.24	0.024
2026-04-09T21:17:12+03:00	T1	no-FUSE	100MB	3	547.98	0.182
2026-04-09T21:17:13+03:00	T1	no-FUSE	1GB	3	308.28	3.322
2026-04-09T21:17:19+03:00	T2	no-FUSE	10MB	3	365.72	0.027
2026-04-09T21:17:54+03:00	T2	no-FUSE	100MB	3	563.29	0.178
2026-04-09T21:17:55+03:00	T2	no-FUSE	1GB	3	556.94	1.839
2026-04-09T21:17:59+03:00	T1	no-FUSE	10MB	4	435.07	0.023
2026-04-09T21:17:59+03:00	T1	no-FUSE	100MB	4	605.64	0.165
2026-04-09T21:18:00+03:00	T1	no-FUSE	1GB	4	347.85	2.944
2026-04-09T21:18:05+03:00	T3	no-FUSE	10MB	4	363.19	0.028
2026-04-09T21:18:05+03:00	T3	no-FUSE	100MB	4	595.45	0.168
2026-04-09T21:18:06+03:00	T3	no-FUSE	1GB	4	242.74	4.218
2026-04-09T21:18:13+03:00	T2	no-FUSE	10MB	4	423.88	0.024
2026-04-09T21:18:18+03:00	T2	no-FUSE	100MB	4	592.21	0.169
2026-04-09T21:18:18+03:00	T2	no-FUSE	1GB	4	473.37	2.163
```

---

## 7. Environment Notes

- Page cache drops: skipped (no passwordless sudo in headless context) — warm cache present for all runs
- Load guard: active (< 1.0 before each run), Firefox closed to reduce background load
- GC: `GOGC=off` for all treatment runs
- Test pair: IPv6 loopback (`::1`), no network latency
- No FUSE path measured in this run (see `docs/benchmarks/2026-04-09-pr105-bridge-benchmark.md` for FUSE WAN data)
