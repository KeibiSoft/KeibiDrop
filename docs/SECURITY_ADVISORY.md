# SECURITY ADVISORY (KeibiDrop 0.2.x)

**Status:** VULNERABILITIES ADDRESSED (Verifying Fixes)

## KD-SEC-2026-001: Predictable Seed on RNG Failure / Weak Entropy
- **Severity:** **CRITICAL**
- **Issue:** [Issue #34](https://github.com/KeibiSoft/KeibiDrop/issues/34)
- **Status:** **FIXED** (PR #40)
- **Impact:** RNG errors are now propagated. Handshake/Rekey aborts if entropy generation fails.

## KD-SEC-2026-002: Lack of Stream Integrity (Chunk Reordering)
- **Severity:** **HIGH**
- **Issue:** [Issue #35](https://github.com/KeibiSoft/KeibiDrop/issues/35)
- **Status:** **FIXED** (PR #42)
- **Impact:** Monotonic sequence numbers are now bound to every encrypted message and file chunk via AEAD AAD (Additional Authenticated Data). Reordering or dropping packets now triggers authentication failure.

## KD-SEC-2026-003: Malleable X25519 Key Encapsulation
- **Severity:** **MEDIUM**
- **Issue:** [Issue #36](https://github.com/KeibiSoft/KeibiDrop/issues/36)
- **Status:** **FIXED** (PR #42)
- **Impact:** Raw XOR seed wrapping replaced with ChaCha20-Poly1305 AEAD. Encapsulated seeds are now cryptographically authenticated.

## KD-SEC-2026-004: Path Traversal in gRPC Notify Handlers
- **Severity:** **CRITICAL**
- **Issue:** [Issue #37](https://github.com/KeibiSoft/KeibiDrop/issues/37)
- **Status:** **FIXED** (PR #41)
- **Impact:** All peer-provided paths are now validated against the shared directory root using `SecureJoin`.

## KD-SEC-2026-005: Lack of Perfect Forward Secrecy (PFS)
- **Severity:** **HIGH**
- **Issue:** [Issue #38](https://github.com/KeibiSoft/KeibiDrop/issues/38)
- **Status:** **IN PROGRESS** (Branch: security/fix-pfs)
- **Impact:** Current protocol uses static identity keys for encapsulation.

## KD-SEC-2026-006: Relay Privacy Leakage
- **Severity:** **MEDIUM**
- **Issue:** [Issue #39](https://github.com/KeibiSoft/KeibiDrop/issues/39)
- **Status:** **TODO**
- **Impact:** Relay lookup tokens are derived from user fingerprints.
