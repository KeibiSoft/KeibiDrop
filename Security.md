# Secure Synchronous File Transfer - Protocol Security Design (v0)

This document describes the current cryptographic design and security architecture implemented in the project as of version 0.

## Overview

The system establishes a **synchronous, peer-to-peer post-quantum resilient file transfer** between two parties (e.g., Alice and Bob) using a hybrid asymmetric key exchange and symmetric encryption.

## Threat Model

* The relay server is **untrusted**.
* Attackers may control the network, but not the local devices.
* Key authenticity is verified **out-of-band via fingerprint exchange**.
* The goal is to ensure **confidentiality, integrity, and authenticity** of all transferred files, even in the presence of **active attackers**.

---

## Cryptographic Primitives

### Asymmetric Key Exchange

The protocol uses a **hybrid approach** combining:

| Algorithm       | Purpose                  | Security Level       |
| --------------- | ------------------------ | -------------------- |
| **ML-KEM-1024** | Post-quantum KEM (Kyber) | [Security Category 5](https://nvlpubs.nist.gov/nistpubs/fips/nist.fips.203.pdf) |
| **X25519**      | Classical ECDH           | No NIST Security Category but [~128-bit is Category 1](https://www.rfc-editor.org/rfc/rfc8031.html#section-4) |

Each party generates **ephemeral key pairs** for both schemes during each session.

### Symmetric Encryption

| Algorithm             | Purpose                           |
| --------------------- | --------------------------------- |
| **ChaCha20-Poly1305** | AEAD encryption for file transfer |

The symmetric encryption key (KEK) is derived using **HKDF** over the combined shared secrets from ML-KEM and X25519.

Transport-layer security between peers is protected using **ChaCha20-Poly1305 with AEAD**.

Two independent bidirectional gRPC channels are established:

* **Inbound**: Alice acts as a gRPC server; Bob connects and sends requests.
* **Outbound**: Alice acts as a gRPC client; Bob accepts connections and serves.

Each connection uses a separate symmetric session key and runs over its own secure channel.

---

## Protocol Flow (v0) — With Fingerprint Ownership & Direction

This section outlines the handshake process used to establish a secure file transfer session between two peers using ephemeral key exchange and fingerprint verification. The flow is designed to:

* Break key derivation symmetry deterministically
* Avoid interactive negotiation
* Use out-of-band verification of peer identity via fingerprints

### Roles

* **Initiator**: The peer that connects second (after receiving the fingerprint)
* **Responder**: The peer that connects first and generates the fingerprint

---

### Step-by-Step Flow

#### 1. Responder (Alice) connects first

* Alice generates ephemeral public/private key pairs for:

  * **X25519** (classical ECDH)
  * **ML-KEM-1024** (post-quantum KEM)
* Alice computes a deterministic fingerprint over her public keys using `ProtocolFingerprintV0`
* She publishes:

  * Her public keys
  * Her fingerprint
  * Any relay-specific session metadata
* Alice **shares her fingerprint out-of-band** with Bob (e.g., via Signal or QR code)

---

#### 2. Initiator (Bob) connects second

* Bob downloads Alice's public keys and fingerprint from the relay
* Bob recomputes Alice’s fingerprint locally and compares it to the out-of-band fingerprint
* If they match:

  * Bob generates two random secrets (e.g., `seed1`, `seed2`)
  * Bob encrypts the secrets:

    * `ctMLKEM = Kyber.Encapsulate(seed1, Alice's ML-KEM pub)`
    * `ctDH = X25519.Encrypt(seed2, Alice's pub key)` — or just use seed2 with DH directly
  * Bob prepares his message:

    * Bob's ephemeral public keys (X25519, ML-KEM)
    * `ctKEM` and `ctDH`
  * Bob sends this to Alice via the relay
  * Bob also sends his own **public key fingerprint** to Alice **out-of-band** for identity verification

---

#### 3. Responder (Alice) receives Bob's key material

* Alice verifies the fingerprint matches Bob's out-of-band value
* Alice computes:

  * `sharedKEM = Kyber.Decapsulate(ctKyber, Alice's private key)`
  * `sharedX = X25519(seed2, Alice's private key)`
* Alice then derives the KEK:

```md
Session_KEK = HKDF(sharedX || sharedKEM) # Where || means concatenation.
```

## Stream Encryption and Integrity

The system uses **ChaCha20-Poly1305 AEAD encryption** applied at the transport stream layer. To prevent message reordering, duplication, or replay attacks (**KD-SEC-2026-002**), the encryption layer binds each message to its position in the stream:

* **Monotonic Nonces**: Every message uses a unique, incrementing 96-bit nonce.
* **Sequence-Bound AAD**: The nonce itself is included as **Additional Authenticated Data (AAD)** in the AEAD construction.
* **Integrity Guarantee**: Decryption will fail if a message is received out of order, as the expected sequence AAD will not match the ciphertext's MAC.

File transfers also use this construction at the chunk level, ensuring that large files cannot be silently corrupted by swapping or dropping blocks.

---

## Seed Encapsulation Integrity

To prevent malleability in the key exchange (**KD-SEC-2026-003**), X25519 seed wrapping has been hardened:

* **AEAD Wrapper**: Seeds are no longer masked with raw XOR. Instead, they are wrapped using **ChaCha20-Poly1305 AEAD**.
* **Key Derivation**: A dedicated wrapping key is derived from the X25519 shared secret and a random salt using HKDF-SHA512.
* **Authentication**: The KEM payload now includes a 16-byte Poly1305 MAC, ensuring that any tampering with the encapsulated seeds is detected before key derivation.

---

### ⚠️ Critical Warning: Nonce Reuse in AEAD Stream Encryption

ChaCha20-Poly1305 requires a **unique nonce** for every message encrypted with a given key. Reusing a nonce with the same key causes **catastrophic failure** of the cipher, including:

* Plaintext recovery (via keystream reuse)
* Forgery potential
* Complete loss of confidentiality for the stream

In the new stream model, **nonces are managed at the stream connection level**, not per file or chunk. The encryption layer uses either:

* A deterministic nonce sequence (e.g., counter-based)
* Or a randomized nonce generation scheme with collision safeguards

Each stream uses a **separate symmetric key**, and nonces are not shared between the inbound and outbound connections. Specifically:

* Alice ⇐ Bob: Encrypted using session key A and AEAD instance A
* Alice ⇒ Bob: Encrypted using session key B and AEAD instance B

This ensures that even bidirectional traffic during a session does **not risk nonce reuse**, since keys and nonce domains are disjoint.

---

### 🧬 Implication

Because nonces are now associated with the transport stream (not file content), the primary limitation is:

> **The lifetime of the stream key must not exceed the safe number of AEAD invocations.**

For ChaCha20-Poly1305, this is approximately:

> **2³² encryptions per key**, regardless of total bytes transferred.

While that is sufficient for most practical sessions, key rotation is strongly advised for long-lived or high-throughput connections.

---

### 🔧 Future TODOs

* ♻️ Switch to **counter-based nonces** per stream if not already used
* 🔐 Support stream resumption via authenticated key+offset resync
* 🔄 Change the IPv6 address on session end or re-keying.

---

## Relay Privacy

The relay server is designed to be a **blind intermediary** that cannot read peer metadata. Registration data (fingerprints, public keys, connection hints) is encrypted before being sent to the relay.

### Key Derivation for Relay Privacy

When Alice shares her fingerprint with Bob out-of-band, the first 32 bytes of the decoded fingerprint serve as a **room password**. From this, two keys are derived using HKDF:

| Key             | Derivation                                          | Purpose                            |
| --------------- | --------------------------------------------------- | ---------------------------------- |
| **Lookup Key**  | `HKDF(room_password, "keibidrop-relay-lookup-v1")`  | Index for relay storage (opaque)   |
| **Encrypt Key** | `HKDF(room_password, "keibidrop-relay-encrypt-v1")` | ChaCha20-Poly1305 encryption of blob |

### Relay Storage Model

The relay stores: `lookup_key -> encrypted_blob`

The relay **cannot**:
- Decode the lookup key back to the fingerprint
- Read the contents of the encrypted blob
- Learn peer fingerprints, public keys, or IP addresses

Only peers with the shared fingerprint (exchanged out-of-band) can derive the correct keys to fetch and decrypt the registration.

---

## Session Re-keying (Forward Secrecy)

Long-lived sessions risk nonce exhaustion and lack forward secrecy. To address this, periodic key rotation is implemented.

### Thresholds

Re-keying is triggered when either direction exceeds:
- **1 GB** of data transferred, or
- **~1 million** encrypted messages

### Re-key Protocol

The re-key uses the same hybrid approach as the initial key exchange:

1. **Initiator** generates fresh random seeds and encapsulates them:
   - `enc_seed_x25519 = X25519Encapsulate(seed1, own_priv, peer_pub)`
   - `enc_seed_mlkem, seed2 = MLKEMEncapsulate(peer_mlkem_pub)`
2. **Initiator** derives new outbound key: `HKDF(seed1 || seed2)`
3. **Initiator** sends `RekeyRequest{enc_seeds, epoch}` via gRPC
4. **Responder** decapsulates seeds, derives new inbound key
5. **Responder** creates its own fresh seeds for the reverse direction
6. **Responder** sends `RekeyResponse{enc_seeds, epoch}`
7. **Initiator** processes response, updates keys
8. Both parties increment epoch and reset byte counters

### Forward Secrecy Guarantee

Each epoch uses fresh random seeds. Compromise of one epoch's key does not expose data from other epochs, provided private keys remain secure. Old session keys are discarded after rotation.

---

## Summary

This design currently offers:

* Ephemeral key exchange with **post-quantum and classical hybrid** security
* Per-connection **authenticated stream encryption**
* Deterministic and tamper-detectable fingerprints for peer validation
* Efficient **streaming mode** suited for file transfer over any transport
* **Relay privacy** - the relay cannot read peer metadata (encrypted blobs only)
* **Session re-keying** for forward secrecy during long-lived connections

**Cross-stream integrity is not yet implemented beyond message-level AEAD and gRPC guarantees.** The system assumes trusted handling of the full stream at the application layer.

For most use cases, this provides strong encryption with reasonable performance and deployability. Future improvements will focus on tightening stream integrity guarantees and connection resumption.
