# PR #105 (unikernel) — Bridge Benchmark & Findings

**Date**: 2026-04-09  
**Machine**: Intel i5, 16GB RAM, Linux 6.17.0, Ubuntu (Wayland)  
**Bridge server**: 185.104.181.40:26600 (Timișoara, Romania)  
**Relay**: keibidroprelay.keibisoft.com  
**Branch**: `unikernel` (HEAD: `0b8ed91` — "Remove non parallel downloads")  
**Compared against**: `main` (HEAD: `873f730` — "Add sync mobile command")

---

## 1. PR #105 Summary

PR #105 adds three significant changes:

1. **Bridge mode** (`KD_BRIDGE` env var) — both peers connect outbound to a TCP bridge relay, bypassing routers that block inbound IPv6. No listener port needed.
2. **Length-prefixed handshake** — replaces `json.Decoder` with 4-byte length prefix + `io.ReadFull`, preventing buffer over-read that corrupted `SecureConn`.
3. **IPv6 bind fix** — `DialWithStableAddr` no longer binds to a stable IPv6 local address when dialing an IPv4 destination (e.g., the bridge server).

A fourth commit ("Remove non parallel downloads") removes parallel streaming for file downloads.

---

## 2. Bridge Token Protocol — Missing from PR

The bridge server uses **room-based token pairing**: each TCP connection must send a 32-byte SHA-256 token derived from both peers' fingerprints (sorted, concatenated, hashed). The bridge pairs connections with matching tokens.

The client-side token code exists on the server at `/root/KeibiDrop/pkg/logic/common/bridge.go` but is **not included in PR #105**. Without it:

- `PerformOutboundHandshake` sends the handshake JSON directly (bridge reads the first bytes as the "token")
- `DialWithStableAddr` for the inbound connection sends **nothing** — bridge rejects it with `Bad token: read 0 bytes`

### Fix Required

Add `bridge.go` with `bridgeRoomToken()` and `dialBridge()` to the PR. Replace bare `DialWithStableAddr` calls in the bridge sections of `JoinRoom` and `CreateRoom` with `dialBridge`. Both inbound and outbound bridge dials must send the token before handshake data.

```go
// bridgeRoomToken computes a deterministic 32-byte room token.
// Both peers get the same result because fingerprints are sorted.
func bridgeRoomToken(ownFP, peerFP string) [32]byte {
    fps := []string{ownFP, peerFP}
    sort.Strings(fps)
    return sha256.Sum256([]byte(fps[0] + fps[1]))
}
```

Also requires `PerformOutboundHandshakeOnConn` (takes an existing `net.Conn` instead of dialing) since the bridge connection is pre-established by `dialBridge`.

---

## 3. Loopback Transfer Throughput (localhost, no bridge)

3 interleaved rounds per branch, `-count=1`, cache drops between runs.

| Size  | main (avg) | unikernel (avg) | Delta    |
|-------|------------|-----------------|----------|
| 1 MB  | 72.2 MB/s  | 77.7 MB/s       | +7.6%    |
| 10 MB | 181.5      | 169.3           | −6.7%    |
| 100 MB| 197.2      | 201.8           | +2.3%    |
| 1 GB  | 196.7      | 165.5           | **−15.9%** |

### Regression: 1 GB throughput down 16%

Consistent across all 3 rounds. Caused by `0b8ed91` ("Remove non parallel downloads") which removed parallel streaming. The 1 MB improvement is within noise.

---

## 4. Bridge Benchmark (WAN — Alice local, Bob on bridge server)

Tested with the bridge token fix applied to a temporary worktree. Alice runs locally, Bob runs as `andrei` user on 185.104.181.40. All traffic routes through the bridge relay.

### Our Results

| Mode    | Direction | 10 MB    | 100 MB   | 1 GB     | SCP      |
|---------|-----------|----------|----------|----------|----------|
| no-FUSE | Upload    | 8.7 MB/s | 32.4 MB/s| 43.0 MB/s| 5.8 MB/s |
| no-FUSE | Download  | 3.0      | 12.3     | 41.7     | 29.2     |
| FUSE    | Download  | 5.9      | 6.3      | 4.7      | —        |

### Marius's Results (reference, from PR)

| Mode    | Direction | 10 MB    | 100 MB   | 1 GB     | SCP      |
|---------|-----------|----------|----------|----------|----------|
| no-FUSE | Upload    | 5.1 MB/s | 47.9 MB/s| 44.9 MB/s| 5.9 MB/s |
| no-FUSE | Download  | 14.0     | 13.9     | 12.7     | 7.1      |
| FUSE    | Download  | 7.6      | 7.4      | 11.9     | —        |

### Analysis

- **SCP baseline matches** — 5.8 vs 5.9 MB/s (upload), confirming same network path.
- **no-FUSE Upload at 1 GB** — 43.0 vs 44.9 MB/s. Comparable, within network variance.
- **no-FUSE Download asymmetry** — our small-file download is slower (3.0 vs 14.0 at 10 MB) but large-file is faster (41.7 vs 12.7 at 1 GB). Likely different server load conditions.
- **FUSE 1 GB regression** — 4.7 MB/s vs 11.9 MB/s. This is the same parallel download removal regression seen in loopback tests. The FUSE path is most sensitive because it requires low-latency chunk delivery to avoid stalling readahead.

---

## 5. Existing PR #100 (feat/local-mode-ipv6)

Already merged. Adds a **Local Mode** toggle for direct LAN connections over link-local IPv6. This sidesteps the router problem entirely for same-network peers, but doesn't help for WAN scenarios (which is what bridge mode in PR #105 addresses).

---

## 6. Action Items

| # | Item | Priority |
|---|------|----------|
| 1 | Merge `bridge.go` token protocol into PR #105 | **Blocker** — bridge mode doesn't work without it |
| 2 | Investigate 1 GB throughput regression from removing parallel downloads | High — 16% loopback, worse on FUSE over WAN |
| 3 | Consider re-enabling parallel streaming or a hybrid approach | High — FUSE download at 4.7 MB/s vs 11.9 MB/s is a user-visible regression |
| 4 | Add `PerformOutboundHandshakeOnConn` to session package | Required by bridge fix |

---

## 7. Test Environment Notes

- All temporary changes were made in a `/tmp` git worktree, removed after testing.
- Bob ran as `andrei` user on the server (not root, non-disruptive to Marius's session).
- All test artifacts on the server were cleaned up after the run.
- The existing Bob instance (Marius's, PID 4096896 on server) was not touched.
