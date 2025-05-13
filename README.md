```text
██╗  ██╗███████╗██╗██████╗ ██╗██████╗ ██████╗  ██████╗ ██████╗ 
██║ ██╔╝██╔════╝██║██╔══██╗██║██╔══██╗██╔══██╗██╔═══██╗██╔══██╗
█████╔╝ █████╗  ██║██████╔╝██║██║  ██║██████╔╝██║   ██║██████╔╝
██╔═██╗ ██╔══╝  ██║██╔══██╗██║██║  ██║██╔══██╗██║   ██║██╔═══╝ 
██║  ██╗███████╗██║██████╔╝██║██████╔╝██║  ██║╚██████╔╝██║     
╚═╝  ╚═╝╚══════╝╚═╝╚═════╝ ╚═╝╚═════╝ ╚═╝  ╚═╝ ╚═════╝ ╚═╝     
```

# KeibiDrop

KeibiDrop is an ephemeral, zero-trust file transfer tool for direct peer-to-peer exchange over untrusted networks.
Although traffic is fully end-to-end encrypted, your IP is visible to the peer and the relay. If they’re hostile, they know where to send packets. Or drones. Or worse: lawyers.


> In simple terms: You can connect two computers. Open the mountpoint folder.  
> Drag files into the mountpoint to make them visible between the computers. Double-click a file.  
> Edit instantly, or save it to your system, or do whatever.  
> **No cloud. No sync. Just an encrypted duplex connection.**
> It also works from the terminal/command line.

It uses modern post-quantum cryptography (ML-KEM + X25519) and symmetric encryption (ChaCha20-Poly1305) to ensure that only the intended recipient can decrypt the file.

The sender and receiver perform a secure key exchange via a short-lived relay server, after which all communication is encrypted end-to-end.

⚙️ Usage example, CLI + GUI instructions coming soon.  
📷 Screenshots and GIFs will be added after the v0 transport is finalized.

---

## Disclaimer

This project was developed with prior experience in the relevant technologies and domain.
To accelerate development and ship faster, we made extensive use of **GPT-4o (in Monday mode)** - for brainstorming, scaffolding, and drafting code.
Every line was reviewed, corrected, and adapted by us, with multiple rounds of validation to ensure accuracy and quality.

This would not have been possible without the **technical knowledge** We’ve gained without relying on AI and the ability to critically evaluate and refine its output.

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

## ⚠️ Networking Requirements

KeibiDrop uses **direct P2P communication over IPv6**. This simplifies connection setup and avoids reliance on third-party STUN/TURN servers, preserving metadata privacy and minimizing external dependencies.

### In order for a session to connect successfully:

- Both peers must have **globally routable IPv6 addresses**.
- Both peers must be able to **accept inbound TCP connections** on the advertised port.
- **Firewalls must allow these inbound connections**. (Check your router and OS firewall.)
- **NAT traversal is not supported** — KeibiDrop does **not** use STUN, TURN, or UPnP.

> If your system is not reachable via IPv6, KeibiDrop will not work.  
> You can test your IPv6 connectivity at: [https://test-ipv6.com](https://test-ipv6.com)

This approach avoids leaking IP metadata to third-party STUN servers, aligning with KeibiDrop’s privacy-first design. However, this also limits compatibility in restrictive or NATed IPv4-only networks.

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
