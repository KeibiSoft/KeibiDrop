# SECURITY ADVISORY (KeibiDrop 0.2.x)

**Status:** ALL VULNERABILITIES CONFIRMED VIA REPRO-SUITE

## KD-SEC-2026-001: Predictable Seed on RNG Failure / Weak Entropy
- **Severity:** **CRITICAL**
- **Issue:** [Issue #34](https://github.com/KeibiSoft/KeibiDrop/issues/34)
- **Impact:** RNG errors are swallowed, returning `nil` seeds. If entropy is weak, the protocol output (salt + KEM) becomes deterministic, allowing session key prediction and decryption.

## KD-SEC-2026-002: Lack of Stream Integrity (Chunk Reordering)
- **Severity:** **HIGH**
- **Issue:** [Issue #35](https://github.com/KeibiSoft/KeibiDrop/issues/35)
- **Impact:** File chunks are encrypted independently without sequence binding. An attacker can swap, duplicate, or drop chunks without triggering authentication failure, leading to silent data corruption.

## KD-SEC-2026-003: Malleable X25519 Key Encapsulation
- **Severity:** **MEDIUM**
- **Issue:** [Issue #36](https://github.com/KeibiSoft/KeibiDrop/issues/36)
- **Impact:** Seed encapsulation uses unauthenticated XOR. Ciphertext is malleable; flipping bits in the encrypted payload results in identical bit-flips in the decapsulated seed.

## KD-SEC-2026-004: Path Traversal in gRPC Notify Handlers
- **Severity:** **CRITICAL**
- **Issue:** [Issue #37](https://github.com/KeibiSoft/KeibiDrop/issues/37)
- **Impact:** \`RENAME_FILE\`, \`RENAME_DIR\`, and \`ADD_FILE\` handlers use raw peer-provided paths with \`filepath.Join\`. Allows writing/moving files anywhere on the system (e.g., \`~/.ssh/authorized_keys\`).

## KD-SEC-2026-005: Lack of Perfect Forward Secrecy (PFS)
- **Severity:** **HIGH**
- **Issue:** [Issue #38](https://github.com/KeibiSoft/KeibiDrop/issues/38)
- **Impact:** Static identity keys are used for seed encapsulation. If long-term keys are compromised, all past and future sessions can be decrypted.

## KD-SEC-2026-006: Relay Privacy Leakage
- **Severity:** **MEDIUM**
- **Issue:** [Issue #39](https://github.com/KeibiSoft/KeibiDrop/issues/39)
- **Impact:** Relay lookup tokens are derived from user fingerprints. Compromised fingerprints or relay logs allow tracking of user IP addresses and online activity.
