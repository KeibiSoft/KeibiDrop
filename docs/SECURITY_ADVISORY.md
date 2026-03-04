# SECURITY ADVISORY (KeibiDrop 0.2.x)

**Status:** VULNERABILITIES ADDRESSED (Verified via Unit Tests)

## KD-SEC-2026-001: Predictable Seed on RNG Failure / Weak Entropy
- **Severity:** **CRITICAL**
- **Issue:** [Issue #34](https://github.com/KeibiSoft/KeibiDrop/issues/34)
- **Status:** **FIXED** (PR #40)
- **Impact:** RNG errors are now propagated. Handshake/Rekey aborts if entropy generation fails. Verified with `pkg/crypto/seed_test.go`.

## KD-SEC-2026-002: Lack of Stream Integrity (Chunk Reordering)
- **Severity:** **HIGH**
- **Issue:** [Issue #35](https://github.com/KeibiSoft/KeibiDrop/issues/35)
- **Status:** **FIXED** (PR #42)
- **Impact:** Monotonic sequence numbers are now bound to every encrypted message and file chunk via AEAD AAD (Additional Authenticated Data). Reordering or dropping packets now triggers authentication failure. Verified with `pkg/session/secureconn_test.go` and `pkg/crypto/verify_fixes_test.go`.

## KD-SEC-2026-003: Malleable X25519 Key Encapsulation
- **Severity:** **MEDIUM**
- **Issue:** [Issue #36](https://github.com/KeibiSoft/KeibiDrop/issues/36)
- **Status:** **FIXED** (PR #42)
- **Impact:** Raw XOR seed wrapping replaced with ChaCha20-Poly1305 AEAD. Encapsulated seeds are now cryptographically authenticated. Verified with `pkg/crypto/verify_fixes_test.go`.

## KD-SEC-2026-004: Path Traversal in gRPC Notify Handlers
- **Severity:** **CRITICAL**
- **Issue:** [Issue #37](https://github.com/KeibiSoft/KeibiDrop/issues/37)
- **Status:** **FIXED** (PR #41)
- **Impact:** All peer-provided paths are now validated against the shared directory root using `SecureJoin`. Verified with `pkg/filesystem/utils_test.go`.

## KD-SEC-2026-005: Lack of Perfect Forward Secrecy (PFS)
- **Severity:** **HIGH**
- **Issue:** [Issue #38](https://github.com/KeibiSoft/KeibiDrop/issues/38)
- **Status:** **FIXED** (PR #43)
- **Impact:** Handshake now uses ephemeral key exchange for all re-keying operations. Verified with `pkg/session/rekey_test.go`.

## KD-SEC-2026-006: Relay Privacy Leakage
- **Severity:** **MEDIUM**
- **Issue:** [Issue #39](https://github.com/KeibiSoft/KeibiDrop/issues/39)
- **Status:** **FIXED** (PR #44)
- **Impact:** Relay lookup tokens are now derived using a salted HKDF, preventing pre-computation attacks on fingerprints. Verified with `pkg/crypto/relay_test.go`.
