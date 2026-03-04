# KeibiDrop Security Analysis Report

**Date:** March 4, 2026  
**Status:** ACTIVE (Issues Tracked)  
**Analyst:** Gemini CLI (Security Specialist)

## Executive Summary
KeibiDrop's cryptographic architecture is "Post-Quantum Hybrid," yet it suffers from several implementation and design flaws that undermine its security guarantees. The most critical issues involve **predictable session keys** on RNG failure, **arbitrary file writes** via path traversal, and a **lack of stream integrity** that allows attackers to reorder or duplicate file chunks.

---

## Detailed Findings

### Category A: Cryptographic Failures (OWASP A04)

#### Finding A.1: Predictable Seeds on RNG Failure (KD-SEC-2026-001 / Issue #34)
- **Severity:** **CRITICAL**
- **Description:** `GenerateSeed()` ignores errors from the system RNG. If entropy is exhausted, it silently returns zeroed seeds, making the entire session trivial to decrypt.
- **Remediation:** Propagate and handle all errors from `rand.Read()`.

#### Finding A.2: Malleable X25519 Key Encapsulation (KD-SEC-2026-003 / Issue #36)
- **Severity:** **MEDIUM**
- **Description:** The `X25519Encapsulate` function uses a raw XOR stream cipher without a Message Authentication Code (MAC), allowing bit-flipping attacks on encapsulated seeds.
- **Remediation:** Replace the XOR construction with a standard AEAD (e.g., ChaCha20-Poly1305) for seed wrapping.

#### Finding A.3: Lack of Perfect Forward Secrecy (PFS) (KD-SEC-2026-005 / Issue #38)
- **Severity:** **HIGH**
- **Description:** The protocol uses static (per-process) identity keys to encapsulate re-keying seeds. If the process memory is compromised, all past and future traffic for that session can be decrypted.
- **Remediation:** Use identity keys only for authentication; perform an ephemeral Diffie-Hellman or KEM exchange for every re-keying event.

### Category B: Protocol & Logic Flaws

#### Finding B.1: Incomplete Path Traversal Protection (KD-SEC-2026-004 / Issue #37)
- **Severity:** **CRITICAL**
- **Description:** While `Mkdir` was fixed, the `ADD_FILE` and `RENAME_FILE` handlers in `service.go` still use `filepath.Join` without validation, allowing a malicious peer to write or move files anywhere on the host system.
- **Remediation:** Use the `secureJoin` helper for all path resolutions in `KeibidropServiceImpl.Notify`.

#### Finding B.2: Lack of Stream Integrity (KD-SEC-2026-002 / Issue #35)
- **Severity:** **HIGH**
- **Description:** The `SecureConn` layer encrypts messages independently without binding them to a sequence. A MITM attacker can reorder, drop, or duplicate encrypted gRPC messages without detection.
- **Remediation:** Include a monotonic sequence number in the AEAD AAD for every message.

#### Finding B.3: Fingerprint-Based Relay Privacy Leakage (KD-SEC-2026-006 / Issue #39)
- **Severity:** **MEDIUM**
- **Description:** The relay lookup token is derived directly from the user's public fingerprint. If a fingerprint is leaked, the relay (or an observer) can track the user's IP and online status.
- **Remediation:** Use a random "Room ID" or temporary token for relay discovery instead of the identity fingerprint.

---

## Formal Methods Analysis (Conceptual)

### Tamarin Prover Model
To formally verify KeibiDrop, we would define the following model in **Tamarin**:

1.  **State Rules:**
    - `Ltk(Identity, PrivateKey)`: Long-term identity key.
    - `Session(Alice, Bob, Key)`: Established session state.
2.  **Protocol Steps:**
    - `Alice_1`: Alice generates `seed1`, `seed2`, encapsulates them with `Ltk(Bob, PubKey)`.
    - `Bob_1`: Bob decapsulates seeds and derives `k = HKDF(seed1, seed2)`.
3.  **Security Properties (Lemmas):**
    - `lemma secrecy`: `all-trace "not(Ex #i #j. K(key) @ i & SessionEstablished(key) @ j)"`
    - `lemma forward_secrecy`: `all-trace "not(Ex #i #j #k key. K(key) @ i & SessionEstablished(key) @ j & LtkReveal(Identity) @ k & k > j)"`

**Predicted Result:** The `forward_secrecy` lemma would **FAIL** because the `seed` is decapsulatable directly from the long-term private key.

---

## Remediation Roadmap

1. **Phase 1: Fix Now (High Impact)**
    - Address RNG error handling (#34).
    - Fix remaining path traversal vectors in `service.go` (#37).
2. **Phase 2: Protocol Hardening (Medium Term)**
    - Implement stream-level sequence numbers to fix reordering (#35).
    - Replace XOR-wrap with AEAD-wrap for seeds (#36).
    - Redesign relay discovery to use ephemeral/random Room IDs instead of fingerprint-derived ones (#39).
3. **Phase 3: Long-Term Maintenance**
    - Implement true ephemeral re-keying for PFS (#38).
    - Formalize the protocol using Verifpal or Tamarin for ongoing verification.

---

## Appendix: Tools & Techniques Used
- **OWASP Top 10 2025** (A04: Cryptographic Failures)
- **CWE Taxonomy** (CWE-22, CWE-321, CWE-326, CWE-327, CWE-338, CWE-353)
- **Tamarin Prover Principles** (Dolev-Yao adversary model, Injective Synchronization)
- **Manual Static Analysis** (Search for hard-coded keys, weak PRNG, crypto primitive inspection)
