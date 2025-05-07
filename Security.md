# Secure Ephemeral File Transfer - Protocol Security Design (v0)

This document describes the current cryptographic design and security architecture implemented in the project as of version 0.

## Overview

The system establishes a **post-quantum resilient, ephemeral, peer-to-peer encrypted file transfer protocol** between two parties (e.g., Alice and Bob) using a hybrid asymmetric key exchange and symmetric encryption.

## Threat Model

- The relay server is **untrusted**.
- Attackers may control the network, but not the local devices.
- Key authenticity is verified **out-of-band via fingerprint exchange**.
- The goal is to ensure **confidentiality, integrity, and authenticity** of all transferred files, even in the presence of **active attackers**.

---

## Cryptographic Primitives

### Asymmetric Key Exchange

The protocol uses a **hybrid approach** combining:

| Algorithm | Purpose | Security Level |
|----------|--------|----------------|
| **ML-KEM-1024** | Post-quantum KEM (Kyber) | ≥ AES-256 equivalent |
| **X25519** | Classical ECDH | ~AES-128 equivalent |

Each party generates **ephemeral key pairs** for both schemes during each session.

### Symmetric Encryption

| Algorithm | Purpose |
|----------|---------|
| **ChaCha20-Poly1305** | AEAD encryption for file transfer |

The symmetric encryption key (KEK) is derived using **HKDF** over the combined shared secrets from ML-KEM and X25519.

---

## Key Derivation

```
DeriveChaCha20Key(sharedSecrets ...[]byte) ([]byte, error)
```

## Chunk Size and Limitations

- The encryption operates on **fixed-size blocks of 256 KiB**.
- Each chunk is encrypted independently using **ChaCha20-Poly1305 AEAD**, which ensures **confidentiality** and **integrity** of each chunk.
- The output format for each encrypted block is:


- Nonces are generated randomly **per chunk** using a cryptographically secure RNG (`crypto/rand`).
- Each nonce is **prepended** to the ciphertext and must be **unique for the lifetime of the KEK**.

### ⚠️ Critical Warning: Nonce Reuse Across Session

If a nonce is **ever reused with the same encryption key**, **ChaCha20-Poly1305 fails catastrophically** — leading to:

- Plaintext leakage (via keystream reuse)
- Forgery potential
- Complete loss of security for the affected chunks

This system currently derives **one KEK per session**, which is then used to encrypt **all files in that session**.

Therefore, the effective security limit is:

> **2³² total encrypted chunks per session (~1 petabyte with 256 KB blocks).**

That includes **all files, across both directions, during that session**. This limit is inherited from the size of the 96-bit nonce space and the statistical collision probability of random generation.

### ❗ Implication

If this system ever encrypts more than ~1 PB with the same session key, the probability of a **nonce collision becomes non-negligible**. Beyond that point, **all guarantees of confidentiality collapse**.


### ⚠️ Known Limitation

While each chunk is authenticated individually, the system **does not currently provide integrity guarantees across the full stream**. This means:

> If a malicious actor **removes or reorders entire encrypted blocks** along a clean boundary (i.e., 256 KB), the receiving side **will not detect it**.

This is acceptable for now, as files are generally hash-verified or user-checked post-transfer. However, a future version may implement:

- A **Merkle tree** over chunk hashes
- A **running MAC** over chunk boundaries
- Or an **end-of-stream signed digest**

### 🔧 Future TODOs

- 🔁 Switch to **deterministic nonce generation** (e.g., counter-based or chunk-index-derived nonces) to eliminate collision risk.
- 🔑 Rotate KEK per file, or per N chunks, to reset nonce space safely.
- 📈 Implement high-watermark nonce tracking to enforce safe limits.
- ❗ Add **cross-block integrity** or final digest verification
- ⏳ Optionally include block number as part of AEAD AAD for better tamper detection
- 🔐 Support authenticated stream resumption (for partial file recovery)

---

## Summary

This design currently offers:

- Ephemeral key exchange with **post-quantum and classical hybrid** security
- Per-chunk **authenticated encryption**
- Deterministic and tamper-detectable fingerprints for peer validation
- Efficient **streaming mode** suited for file transfer over any transport.

**Cross-chunk stream authentication is not yet implemented.** The system ensures chunk-level security but assumes trusted handling of the full stream at the application layer.

For most use cases, this provides strong encryption with reasonable performance and deployability. Future improvements will focus on tightening stream integrity guarantees.
