# KeibiDrop v0.1.0 Release Roadmap

**Date**: 2026-03-08
**Target**: v0.1.0
**Codename**: justfkinship

---

## Reasoning Transcript

### Key Findings from Deep PR Inspection

1. **PR #43 (PFS) is DANGEROUS** — will not compile after PR #40 merges (calls old `GenerateSeed()` signature), overlaps with PR #42 in `symmetric.go`/`secureconn.go`, and the ephemeral ML-KEM key is generated but **never actually used in decapsulation**. E2E testing revealed gRPC stream sync failures during re-keying. **Must be deferred or significantly reworked.**

2. **PRs #40, #41, #42 are independently safe** — no cross-conflicts on production code. Each touches different packages/functions.

3. **PR #44 is safe but weak** — static public salt adds marginal anti-rainbow-table protection. Touches `DeriveRelayKeys()` while #42 touches `X25519Encapsulate/Decapsulate()` — no conflict.

4. **PR #55 (fuzz test) is zero risk** — pure test addition, no production code changes.

5. **UI stack (#25 → #26 → #27) status**:
   - Commit `4ae15af` on main already landed the fusecheck split (same content as PR #27's fusecheck work).
   - PRs are stacked: #26 is superset of #25, #27 is superset of #26.
   - All merge cleanly with current main (verified via `git merge-tree`).
   - `docs/SECURITY_ADVISORY.md` and `docs/security/protocol.spdl` are touched by ALL security PRs — expect trivial merge conflicts (appended sections) after the first security PR merges.

6. **No CI/CD exists** — all gates are manual (`make test`, `make lint`, `make sec`, manual E2E).

### What v0 Means

First public release. The bar:
- Zero CRITICAL or HIGH security vulnerabilities
- Core flows work end-to-end (create room, join, transfer, disconnect)
- No data corruption or arbitrary file write primitives
- Rough edges acceptable (upload progress not wired, resize quirks)

---

## Phase 0: Establish Baseline

- [ ] **0.1** Run `make test` on current main — confirm green
- [ ] **0.2** Run `make lint && make sec` — confirm clean
- [ ] **0.3** Build full binary: `make build-static-rust-bridge && cd rust && cargo build --release`
- [ ] **0.4** Manual smoke test (Alice + Bob via relay):
  - Create room, join room, transfer file both ways, disconnect

**Exit criteria**: Known-good baseline before any merges.

---

## Phase 1: Security PRs — Wave 1 (Independent, Safe)

These PRs touch different packages. Merge in any order. All verified MERGEABLE with no cross-conflicts on production code.

**Note**: `docs/SECURITY_ADVISORY.md` and `docs/security/protocol.spdl` are touched by all PRs. After the first merge, subsequent PRs may need trivial conflict resolution on these doc files.

### 1.1 — PR #55: Fuzz gRPC Notify handler [ZERO RISK]
- Pure test addition (+362 lines, 2 files)
- 2M+ executions, no panics or races
- **Merge**: `gh pr merge 55 --merge`
- **Verify**: `go test -fuzz=FuzzNotifyHandler -fuzztime=30s ./pkg/logic/service/`

### 1.2 — PR #41: Path traversal fix via SecureJoin [CRITICAL security fix]
- Fixes Issue #37 — the **only true release blocker**
- Replaces all `filepath.Join` with `SecureJoin` (+355/-54, 6 files)
- Unit tests verify `../../etc/passwd` blocked, legitimate ops work
- Touches: `pkg/filesystem/`, `pkg/logic/service/` (no overlap with other PRs)
- **Merge**: `gh pr merge 41 --merge`
- **Verify**: `make test`

### 1.3 — PR #40: Handle RNG errors in GenerateSeed() [MEDIUM security fix]
- Changes `GenerateSeed()` → `([]byte, error)` (+519/-5, 9 files)
- All callers updated, E2E verified
- Touches: `pkg/crypto/utils.go`, `pkg/session/handshake.go`, `pkg/session/rekey.go`
- **IMPORTANT**: PR #43 depends on this — merging #40 will break #43's compilation (by design; #43 needs rebase anyway)
- **Merge**: `gh pr merge 40 --merge`
- **Verify**: `make test`

### 1.4 — PR #42: Stream integrity + AEAD key encapsulation [MEDIUM security fix]
- Fixes two vulns: message reordering (monotonic nonce AAD) + malleable XOR KEM → ChaCha20-Poly1305 AEAD
- Touches: `pkg/crypto/asymmetric.go` (X25519Encapsulate/Decapsulate), `pkg/crypto/symmetric.go`, `pkg/session/secureconn.go`
- No overlap with #40 or #41 on production code
- E2E verified, unit tests pass
- **Merge**: `gh pr merge 42 --merge`
- **Verify**: `make test`

### Wave 1 Gate
- [ ] **1.5** Full test suite: `make test && make lint && make sec`
- [ ] **1.6** Full rebuild: `make build-static-rust-bridge && cd rust && cargo build --release`
- [ ] **1.7** Manual E2E smoke test (create room, join, transfer file, disconnect)

---

## Phase 2: Security PRs — Wave 2 (After Wave 1)

### 2.1 — PR #44: Relay lookup salt [LOW-MEDIUM, safe but weak]
- Adds static salt to HKDF relay lookup derivation (+151/-6, 4 files)
- Touches `DeriveRelayKeys()` in `asymmetric.go` — different function than #42's changes, no conflict
- **Triage note**: Static public salt is weak (anyone with source can precompute). Encryption key still uses nil salt (inconsistency). Not harmful, marginal improvement.
- May need trivial conflict resolution on `docs/SECURITY_ADVISORY.md`
- **Merge**: `gh pr merge 44 --merge` (resolve doc conflicts if any)
- **Verify**: `make test`

### 2.2 — PR #43: PFS via ephemeral re-keying [DEFER TO v0.2]

**DO NOT MERGE FOR v0.1.0. Reasons:**

1. **Compile-time break**: Calls `GenerateSeed()` with old signature (no error return). Will not compile after PR #40 merges.
2. **Code overlap**: Duplicates changes in `symmetric.go` and `secureconn.go` from PR #42. Merge conflicts guaranteed.
3. **Incomplete implementation**: Ephemeral ML-KEM key is generated and transmitted but **never actually used** in decapsulation. The PFS property claimed is not fully delivered.
4. **E2E failures**: gRPC stream synchronization fragility during re-keying — EOF/auth failures because initiator and responder don't switch read/write states in lockstep.
5. **Test breakage**: `repro_test.go` calls old `GenerateSeed()` signature.

**Required rework for v0.2**:
- Rebase onto main (after #40 + #42 merged)
- Update all `GenerateSeed()` call sites to handle error return
- Resolve conflicts in `symmetric.go` and `secureconn.go`
- **Actually use the ephemeral ML-KEM key** in key derivation (not just transmit it)
- Fix gRPC stream sync during re-keying
- Full E2E re-verification

**Mitigation for v0.1.0**: ML-KEM's `Encapsulate()` already generates fresh entropy per call, providing practical PFS even with static X25519 keys (documented in issue #38 severity assessment). The v0 release is not PFS-zero — it's PFS-partial via ML-KEM.

### Wave 2 Gate
- [ ] **2.3** Full test suite: `make test && make lint && make sec`
- [ ] **2.4** E2E relay test (full transfer cycle)

---

## Phase 3: UI / Refactoring PRs (Stacked Chain)

**Context**: Commit `4ae15af` on main already contains the fusecheck platform split. PRs #25/#26/#27 are stacked (each is a superset of the previous). All merge cleanly with current main.

### 3.1 — PR #25: Slint layout reflow + logo fixes
- Fixes Issue #24 (partially — ScrollView height hardcoded to 400px)
- Removes unused `CustomButton` component, fixes logo positioning
- **Known quality gap**: ScrollView won't adapt to resize. Acceptable for v0.
- **Merge**: `gh pr merge 25 --merge`
- **Verify**: Build + visual UI check

### 3.2 — Rebase PR #26 onto main, then merge: Save dir cleanup
- Depends on #25 being merged first (superset)
- Clears Save directory on startup (4 test cases included)
- `git checkout fix/save-dir-cleanup && git rebase main && git push --force-with-lease`
- **Merge**: `gh pr merge 26 --merge`
- **Verify**: `make test` + launch app and confirm Save dir cleaned

### 3.3 — PR #27: FUSE detection platform split
- **Assessment needed**: The fusecheck split is already on main via `4ae15af`. PR #27 may be redundant for the fusecheck part, adding only the #25/#26 changes.
- If after rebasing #26 the diff of #27 is only the fusecheck split (already on main), **close PR #27 as redundant**.
- If #27 has additional changes beyond what's in #25/#26 and `4ae15af`, rebase and merge.
- **Action**: `git diff main...origin/refactor/fusecheck-platform-split -- cmd/internal/checkfuse/` — if empty, the fusecheck work is already on main and #27 can be closed.

### Phase 3 Gate
- [ ] **3.4** Full rebuild: `make build-static-rust-bridge && cd rust && cargo build --release`
- [ ] **3.5** Full test suite: `make test && make lint && make sec`

---

## Phase 4: Manual Testing Protocol

### 4.1 Core Flows (MUST PASS for release)

| # | Test Case | Steps | Expected |
|---|-----------|-------|----------|
| M1 | Create room | Launch → Create Room | Code displayed, waiting for peer |
| M2 | Join room | Peer enters code → Join | Both see connected screen |
| M3 | Share file (no FUSE) | Add File → select → peer sees it | File in peer's list |
| M4 | Download file | Save → select location | Downloads, matches original |
| M5 | Open downloaded file | Click Open | System handler launches |
| M6 | Drag & drop | Drag file onto window | File shared to peer |
| M7 | Large file (>100MB) | Share large file | Transfers completely |
| M8 | Disconnect | Click Disconnect | Returns to connect screen |
| M9 | Reconnect | Disconnect → Create/Join again | Fresh session works |
| M10 | FUSE mount | Launch with FUSE → connect | Mount dir browsable |

### 4.2 Security Validation (MUST PASS)

| # | Test Case | Expected |
|---|-----------|----------|
| S1 | Path traversal blocked | `SecureJoin` rejects `../../etc/passwd` |
| S2 | Fingerprint match | Displayed codes match on both peers |
| S3 | Encrypted traffic | `tcpdump` shows no plaintext |
| S4 | RNG error propagation | `GenerateSeed()` returns error (code review) |

### 4.3 Edge Cases (SHOULD PASS)

| # | Test Case | Expected |
|---|-----------|----------|
| E1 | Unicode filenames | Transfers and displays correctly |
| E2 | Empty file | Transfers without error |
| E3 | Rapid add/remove | Peer state stays consistent |
| E4 | Network interruption | Heartbeat detects, reconnect attempts |
| E5 | Relay rate limit | Error message, not crash |

### 4.4 Platform Testing

| Platform | Priority | Notes |
|----------|----------|-------|
| Linux (Ubuntu 24.04+) | **MUST** | Primary target, FUSE3 |
| macOS (14+) | SHOULD | macFUSE, CoreFoundation |
| Windows | DEFER | WinFsp, target v0.2 |

---

## Phase 5: Release Preparation

### 5.1 Version Alignment
- [ ] Update `Makefile` VERSION: `0.0.1` → `0.1.0`
- [ ] Confirm `rust/Cargo.toml` version is `0.1.0` (already correct)
- [ ] Update `Security.md` to reflect merged protocol changes (AEAD KEM, stream integrity)

### 5.2 Release Artifacts
- [ ] Create `CHANGELOG.md` with v0.1.0 section
- [ ] Update `sbom.json` if dependencies changed
- [ ] Final build: `make build-static-rust-bridge && cd rust && cargo build --release`
- [ ] Tag: `git tag -a v0.1.0 -m "KeibiDrop v0.1.0"`

### 5.3 Release Notes (Draft)

```
KeibiDrop v0.1.0 — justfkinship

First public release. P2P encrypted file sharing with post-quantum
cryptography (ML-KEM-1024 + X25519 → ChaCha20-Poly1305).

Security:
- Fixed CRITICAL path traversal in ADD_FILE/RENAME_FILE (KD-SEC-2026-004)
- Hardened RNG error handling in seed generation (KD-SEC-2026-001)
- Added stream integrity via monotonic nonce AAD (KD-SEC-2026-002)
- Replaced malleable XOR KEM with AEAD encapsulation (KD-SEC-2026-003)
- Hardened relay lookup key derivation (KD-SEC-2026-006)

Features:
- Rust/Slint desktop UI (Linux primary, macOS secondary)
- FUSE virtual filesystem for transparent file access
- Drag-and-drop file sharing
- Relay-assisted peer discovery, direct P2P transfer
- Auto-reconnect with health monitoring

Known Limitations:
- Upload progress indicator not wired (files transfer fine, no spinner)
- Window resize may not reflow all UI elements (#24)
- PFS via ephemeral re-keying deferred to v0.2 (#43)
  (ML-KEM Encapsulate() provides partial PFS in v0.1)
- SHA-512 fingerprint codes are long (#8)
- No Windows testing yet
```

---

## Phase 6: Post-Release Backlog (v0.2)

### High Priority
| Item | Description |
|------|-------------|
| PR #43 rework | PFS ephemeral re-keying — rebase, fix ML-KEM usage, fix E2E |
| CI/CD | GitHub Actions: test, lint, sec, build on push/PR |
| Issue #50 | OOM via SecureReader length prefix (one-liner max-size check) |
| Issue #19 | DoS protection during connection interval |

### Medium Priority
| Item | Description |
|------|-------------|
| Issues #45-49 | Security test suite (fuzz, PFS verification, boundary, concurrency, edge cases) |
| Upload progress | Wire `uploading` flag from Go events to Rust UI |
| D&D error feedback | Show UI alerts for drag-and-drop/file picker failures |
| Network loss UI | File watcher should check heartbeat, trigger reconnect UI |

### Low Priority
| Item | Description |
|------|-------------|
| Issue #24 | Full window resize reflow |
| Issue #15 | Visual polish |
| Issue #8 | Truncate SHA-512 codes |
| `.gitignore` | Add runtime artifacts (Log_*.txt, Mount*, Save*, libkeibidrop.*) |
| Mutex safety | Replace `unwrap()` on Rust mutex locks with proper error handling |

---

## Critical Path Summary

```
Phase 0: Baseline                    ~30 min
Phase 1: Security Wave 1             ~1 hour
  #55 (fuzz) + #41 (traversal) + #40 (RNG) + #42 (AEAD/integrity)
  Gate: test + build + E2E smoke
Phase 2: Security Wave 2             ~30 min
  #44 (relay salt) — #43 DEFERRED
  Gate: test + E2E
Phase 3: UI stack                    ~1 hour
  #25 → rebase #26 → assess #27
  Gate: build + test
Phase 4: Manual testing              ~2-3 hours
  Core flows + security validation + edge cases
Phase 5: Release prep                ~1 hour
  Version bump, changelog, tag, final build

Total: ~6-8 hours of focused work (1 day)
```

---

## Decision Log

| Decision | Choice | Rationale |
|----------|--------|-----------|
| PR #43 (PFS) | **DEFER** to v0.2 | Won't compile, incomplete ML-KEM usage, E2E failures. ML-KEM provides partial PFS already. |
| PR #44 (relay salt) | **MERGE** | Safe, not harmful, marginal improvement. |
| PR #27 (fusecheck) | **ASSESS** after #26 | Fusecheck split already on main; may be redundant. |
| Windows testing | **DEFER** to v0.2 | No Windows test environment available. |
| CI/CD | **AFTER** v0.1 | Manual testing sufficient for single release. |
| Upload progress TODO | **DEFER** | Cosmetic — files transfer correctly without spinner. |
