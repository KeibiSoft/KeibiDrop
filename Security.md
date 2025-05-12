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

## Protocol Flow (v0) — With Fingerprint Ownership & Direction

This section outlines the handshake process used to establish a secure file transfer session between two peers using ephemeral key exchange and fingerprint verification. The flow is designed to:

- Break key derivation symmetry deterministically
- Avoid interactive negotiation
- Use out-of-band verification of peer identity via fingerprints

### Roles

- **Initiator**: The peer that connects second (after receiving the fingerprint)
- **Responder**: The peer that connects first and generates the fingerprint

---

### Step-by-Step Flow

#### 1. Responder (Alice) connects first

- Alice generates ephemeral public/private key pairs for:
  - **X25519** (classical ECDH)
  - **ML-KEM-1024** (post-quantum KEM)
- Alice computes a deterministic fingerprint over her public keys using `ProtocolFingerprintV0`
- She publishes:
  - Her public keys
  - Her fingerprint
  - Any relay-specific session metadata
- Alice **shares her fingerprint out-of-band** with Bob (e.g., via Signal or QR code)

---

#### 2. Initiator (Bob) connects second

- Bob downloads Alice's public keys and fingerprint from the relay
- Bob recomputes Alice’s fingerprint locally and compares it to the out-of-band fingerprint

- Bob downloads Alice's public keys and fingerprint from the relay
- Bob recomputes Alice’s fingerprint locally and compares it to the out-of-band fingerprint
- If they match:
  - Bob generates two random secrets (e.g., `seed1`, `seed2`)
  - Bob encrypts the secrets:
    - `ctMLKEM = Kyber.Encapsulate(seed1, Alice's ML-KEM pub)`
    - `ctDH = X25519.Encrypt(seed2, Alice's pub key)` — or just use seed2 with DH directly
  - Bob prepares his message:
    - Bob's ephemeral public keys (X25519, ML-KEM)
    - `ctKEM` and `ctDH`
  - Bob sends this to Alice via the relay
  - Bob also sends his own **public key fingerprint** to Alice **out-of-band** for identity verification

---

#### 3. Responder (Alice) receives Bob's key material

- Alice verifies the fingerprint matches Bob's out-of-band value
- Alice computes:
  - `sharedKEM = Kyber.Decapsulate(ctKyber, her private key)`
  - `sharedX = X25519(seed2, Alice's private key)`
- Alice then derives the KEK:

```md
Session_KEK = HKDF(sharedX || sharedKEM)
```

## Chunk Size and Limitations

- The encryption operates on **fixed-size blocks of 256 KiB**.
- Each chunk is encrypted independently using **ChaCha20-Poly1305 AEAD**, which ensures **confidentiality** and **integrity** of each chunk.
- The output format for each encrypted block is:

- Nonces are generated randomly **per chunk** using a cryptographically secure RNG (`crypto/rand`).
- Each nonce is **prepended** to the ciphertext and must be **unique for the lifetime of the KEK**.

### ⚠️ Critical Warning: Nonce Reuse Across Session

If a nonce is **ever reused with the same encryption key**, **ChaCha20-Poly1305 fails catastrophically** - leading to:

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
