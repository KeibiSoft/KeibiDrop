# KeibiDrop

KeibiDrop is an ephemeral, zero-trust file transfer tool designed for direct peer-to-peer exchange over untrusted networks.

It uses modern post-quantum cryptography (ML-KEM + X25519) and symmetric encryption (ChaCha20-Poly1305) to ensure that only the intended recipient can decrypt the file.

The sender and receiver perform a secure key exchange via a short-lived relay server, after which all communication is encrypted end-to-end.

⚙️ Usage example, CLI + GUI instructions coming soon.  
📷 Screenshots and GIFs will be added after the v1 transport is finalized.

---

## Disclaimer

This project was developed with prior experience in the relevant technologies and domain.
To accelerate development and ship faster, I made extensive use of **GPT-4o (in Monday mode)** - for brainstorming, scaffolding, and drafting code.
Every line was reviewed, corrected, and adapted by me, with multiple rounds of validation to ensure accuracy and quality.

This would not have been possible without the **technical knowledge** I’ve gained without relying on AI and the ability to critically evaluate and refine its output.

---

## Inspiration

This project was loosely inspired by:

- [croc](https://github.com/schollz/croc) – for its clean approach to secure, peer-to-peer transfers
- [rclone](https://rclone.org/) – for the general concept of mapping cloud storage to local workflows

I haven’t used these tools directly, but I liked the ideas they explored and wanted to build something in that direction, using my own design and implementation.

---

## Features

- Post-quantum hybrid key exchange using ML-KEM-1024 and X25519
- ChaCha20-Poly1305 symmetric encryption
- Encrypted streaming with chunked transfer
- Deterministic fingerprint verification
- No persistent metadata or tracking
- Designed for use over untrusted relays

---

## Repository Structure

```md
cmd/ # Main entry point
pkg/crypto/ # Cryptographic primitives
go.mod # Module definition
go.sum # Dependencies
Security.md # Protocol-level cryptographic design
```

---

## Build

```bash
go build -o keibidrop ./cmd
```

---

## Test

```bash
go test ./pkg/...
```

---

## Cryptographic Summary

- **Asymmetric Key Exchange**: ML-KEM-1024 (Kyber) + X25519
- **Symmetric Encryption**: ChaCha20-Poly1305
- **Key Derivation**: HKDF over shared secrets
- **Streaming Mode**: Encrypted chunked transfer with per-chunk AEAD

See [`Security.md`](./Security.md) for a complete protocol overview.

---

## Contributing & Legal

Want to contribute? Great—but please read the [CONTRIBUTING.md](./CONTRIBUTING.md) first. All commits must be signed (`git commit -S`) and by doing so, you agree to the terms outlined in our [Developer Certificate of Origin](./DCO.txt).

For information about how we (don’t) use your data, see the [Privacy Policy](./privacy.md).

---

## LICENSE

This project is licensed under the Mozilla Public License 2.0.

See the LICENSE file for details.

This open-source release is the community edition.

---

### Enterprise Edition Available

This project is developed and maintained as Free and Open Source Software (FOSS) under the MPL 2.0 license.

An Enterprise Edition is also available, which includes:

- Additional features not found in the open-source version
- Commercial support and onboarding assistance
- Customization services to fit specific business needs

Commercial licensing and support available at [keibisoft.com](https://keibisoft.com/tools/keibidrop.html)
