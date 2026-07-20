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

Each party's handshake key pairs are its **long-term identity keys**, the same keys whose fingerprint is checked out-of-band. The handshake alone is therefore not forward-secret: later theft of the identity keys would expose a recorded session. Forward secrecy is added on top by the symmetric ratchet and the ephemeral post-quantum fold (see [Forward Secrecy](#session-key-ratchet-and-forward-secrecy)). Incognito mode generates fresh identity keys that are never written to disk, but they are still per-session, not per-message.

### Symmetric Encryption

| Algorithm             | Purpose                           | When used |
| --------------------- | --------------------------------- | --------- |
| **AES-256-GCM**       | AEAD encryption for file transfer | Default when both peers have hardware AES (AES-NI on x86, AES extension on ARM64) |
| **ChaCha20-Poly1305** | AEAD encryption for file transfer | Fallback when either peer lacks hardware AES |

Both ciphers use identical parameters: 32-byte key, 12-byte nonce, 16-byte auth tag. The cipher is negotiated during the handshake based on hardware capabilities. If both peers have hardware AES acceleration, AES-256-GCM is selected for better throughput. Otherwise, ChaCha20-Poly1305 is used.

The symmetric encryption key (SEK) is derived using **HKDF** over the combined shared secrets from ML-KEM and X25519, with domain-separated labels per cipher suite (different ciphers produce different keys from the same input secrets).

Transport-layer security between peers is protected using the negotiated AEAD cipher.

Two independent bidirectional gRPC channels are established:

* **Inbound**: Alice acts as a gRPC server; Bob connects and sends requests.
* **Outbound**: Alice acts as a gRPC client; Bob accepts connections and serves.

Each connection uses a separate symmetric session key and runs over its own secure channel.

---

## Protocol Flow (v0) - Fingerprint Ownership & Direction

This section outlines the handshake process used to establish a secure file transfer session between two peers using identity-key exchange and fingerprint verification. The flow is designed to:

* Break key derivation symmetry deterministically
* Avoid interactive negotiation
* Use out-of-band verification of peer identity via fingerprints

### Roles

* **Initiator**: The peer that connects second (after receiving the fingerprint)
* **Responder**: The peer that connects first and generates the fingerprint

---

### Step-by-Step Flow

#### 1. Responder (Alice) connects first

* Alice uses her long-term identity key pairs for:

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
    * `ctDH = X25519.Encrypt(seed2, Alice's pub key)` (or just use seed2 with DH directly)
  * Bob prepares his message:

    * Bob's identity public keys (X25519, ML-KEM)
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

## Stream Encryption and Limitations

The system uses **AEAD encryption (AES-256-GCM or ChaCha20-Poly1305, negotiated per session) applied at the transport stream layer**, not at the file or chunk level. This encryption is layered beneath the gRPC framing logic, meaning:

* File contents are passed over a **secure, AEAD-encrypted duplex stream**.
* Encryption is **connection-oriented**, rather than chunk-based.
* Each connection has its own **symmetric session key** (derived from the identity-key handshake, then advanced by the ratchet and mixed with fresh post-quantum entropy once per connection) and independent AEAD state per direction.

Encryption is handled transparently within the gRPC transport layer using authenticated stream wrappers written in Go. Data is encrypted continuously and decrypted on the fly as it flows across the connection.

This model simplifies the handling of large files, avoids intermediate buffering and chunking logic, and ensures **confidentiality and integrity of the transport stream itself**.

---

### Critical Warning: Nonce Reuse in AEAD Stream Encryption

Both AES-256-GCM and ChaCha20-Poly1305 require a **unique nonce** for every message encrypted with a given key. Reusing a nonce with the same key causes **catastrophic failure** of the cipher, including:

* Plaintext recovery (via keystream reuse)
* Forgery potential
* Complete loss of confidentiality for the stream

Nonces are managed at the stream connection level, not per file or chunk. The encryption layer uses a deterministic counter-based nonce sequence.

Each stream uses a **separate symmetric key**, and nonces are not shared between the inbound and outbound connections. Specifically:

* Alice ⇐ Bob: Encrypted using session key A and AEAD instance A
* Alice ⇒ Bob: Encrypted using session key B and AEAD instance B

This ensures that even bidirectional traffic during a session does **not risk nonce reuse**, since keys and nonce domains are disjoint.

---

### Nonce safety and re-key thresholds

The nonce space is 2³² (~4.3 billion). We re-key after 1 GB of data transferred or ~1 million encrypted messages, whichever comes first. This gives a ~4000x safety margin before nonce exhaustion. The re-key thresholds are conservative forward secrecy limits that bound the exposure window if a key is ever compromised, not hard cryptographic boundaries.

---

### Future TODOs

* Change the IPv6 address on session end or re-keying.

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

## Session Key Ratchet and Forward Secrecy

The handshake derives the session key from long-term identity keys, so on its own it is not forward-secret. Two mechanisms are layered on top.

### Symmetric ratchet (always on)

Each direction advances its key with a one-way KDF ratchet: every epoch derives the next chain key from the current one and discards the old one, so a leak of the live key does not expose earlier epochs. The bump rides an on-wire epoch marker in the nonce, with no pause in the data stream. It rotates well before nonce exhaustion, at latest every 1 GB or ~1 million messages per direction, far under the 2^32 nonce space.

Peers that predate the ratchet fall back to a full re-handshake rekey over the `Rekey` gRPC (fresh seeds, new epoch, sockets re-established). This is a compatibility fallback only; two current peers never use it.

### Ephemeral post-quantum fold

The ratchet is deterministic from the identity-key handshake, so it alone adds no fresh entropy: a later theft of the identity keys would still expose a recorded session (record-now-decrypt-later). To close this, the peers run one **ephemeral hybrid-KEM fold** right after connecting, and again after each reconnect.

The deterministic initiator (lower fingerprint) generates a fresh ephemeral ML-KEM-1024 and X25519 key pair; the responder encapsulates to them and returns its own ephemeral X25519 public. Both combine the two shared secrets into 32 bytes with `HKDF-SHA512`, salted by a value bound to the authenticated session, and mix that into the ratchet. The ephemeral private keys are dropped as soon as the secret is derived. The exchange rides the authenticated AEAD stream, so the relay cannot read, inject, or replay it.

Once a direction has folded, its keys depend on ephemeral secrets that were never stored and never sent, so they are **forward-secret against later theft of the identity keys**: an attacker who records the traffic and later steals `identity.enc` still cannot derive the post-fold keys. This rests on the ephemeral KEM's hardness ([NIST SP 800-227](https://csrc.nist.gov/pubs/sp/800/227/ipd)) plus destruction of the ephemeral keys, the construction Apple's iMessage PQ3 and Signal use, not on the transport.

### What it does not cover

- **Pre-fold residual.** A direction folds on its next frame after the secret is staged: promptly on the connection-carrying direction, and on the next carrier frame elsewhere, bounded per direction by the ~5 second heartbeat. Frames sent before a direction folds are protected only by the identity-key handshake.
- **No post-compromise healing.** The fold runs once per connection, not periodically, so a leak of the live ratchet state mid-session is not healed by a later fold. A reconnect does re-fold with fresh entropy.
- **Additive, never fatal.** A failed or dropped fold leaves the connection on the handshake-derived ratchet with no interruption; the next reconnect re-folds.

---

Cross-stream integrity is not yet implemented beyond message-level AEAD and gRPC guarantees. The system assumes trusted handling of the full stream at the application layer.

---

## At-Rest Identity Encryption

Persistent identity files (`identity.enc`, `contacts.enc`) are encrypted with ChaCha20-Poly1305 using a per-install random master key.

**Master key location** (in order of preference):
1. OS keychain (macOS Keychain, Windows Credential Manager, Linux Secret Service)
2. File on disk (`~/.config/keibidrop/.master.key`, mode 0600), automatic fallback on headless systems
3. User passphrase via Argon2id, opt-in with `KD_PASSPHRASE_PROTECT=1`

Incognito mode uses ephemeral keys and writes nothing to disk.

Per-file encryption keys are derived via HKDF-SHA512 from the master key with a random salt. The file header (magic, format version, KDF identifier, salt) is bound as AAD. Corrupted files produce a clear error, KeibiDrop does not auto-regenerate identity files because that would silently wipe saved contacts.
