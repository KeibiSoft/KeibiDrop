```
██╗  ██╗███████╗██╗██████╗ ██╗██████╗ ██████╗  ██████╗ ██████╗
██║ ██╔╝██╔════╝██║██╔══██╗██║██╔══██╗██╔══██╗██╔═══██╗██╔══██╗
█████╔╝ █████╗  ██║██████╔╝██║██║  ██║██████╔╝██║   ██║██████╔╝
██╔═██╗ ██╔══╝  ██║██╔══██╗██║██║  ██║██╔══██╗██║   ██║██╔═══╝
██║  ██╗███████╗██║██████╔╝██║██████╔╝██║  ██║╚██████╔╝██║
╚═╝  ╚═╝╚══════╝╚═╝╚═════╝ ╚═╝╚═════╝ ╚═╝  ╚═╝ ╚═════╝ ╚═╝
```

Peer-to-peer encrypted file sharing. Post-quantum. Open source.

**Your relay can't read your files.**

Files transfer directly between machines when possible, or through an encrypted relay when firewalls block direct connections. Either way, only you and your peer can read the data. The relay sees only encrypted bytes it can't decrypt.

45 MB/s through relay. 442 MB/s on LAN. Zero configuration for end users.

| | KeibiDrop | SCP | Blip | AirDrop |
|--|--|--|--|--|
| E2E encryption | Post-quantum (ML-KEM + X25519) | SSH | TLS only (relay can read) | TLS |
| Cross-platform | macOS, Linux, Windows | All | All | Apple only |
| No accounts | Yes | N/A | Email required | Apple ID |
| Virtual filesystem | FUSE mount | No | No | No |
| Open source | MPL-2.0 | Yes | No | No |
| Works through firewalls | Encrypted relay fallback | Port forwarding | Relay (can read data) | Local only |

---

## Install

### macOS

```bash
brew tap keibisoft/keibidrop
brew install keibidrop
```

### Linux (Debian/Ubuntu)

```bash
wget https://github.com/KeibiSoft/KeibiDrop/releases/latest/download/keibidrop_amd64.deb
sudo dpkg -i keibidrop_amd64.deb
```

### Windows

```bash
choco install keibidrop
```

Or download the `.zip` from [GitHub Releases](https://github.com/KeibiSoft/KeibiDrop/releases).

### Build from source

```bash
git clone https://github.com/KeibiSoft/KeibiDrop.git
cd KeibiDrop
make build-kd       # CLI daemon
make build-cli      # Interactive CLI
make build-rust     # Desktop UI (needs Rust + Slint)
```

---

## Quick start

1. Both peers launch KeibiDrop
2. Copy your fingerprint and send it to your peer (Signal, Telegram, anything)
3. Paste each other's fingerprints
4. One peer creates a room, the other joins
5. Share files

It works through firewalls automatically. If direct IPv6 fails, KeibiDrop falls back to an encrypted relay. No port forwarding, no router configuration.

---

## Two modes

| | Direct Transfer | Virtual Folder (FUSE) |
|--|--|--|
| Speed | Up to 550 MB/s | Up to 250 MB/s |
| How it works | Add files, peer pulls them | Peer's files appear as a local folder |
| Setup | Nothing extra | Install [macFUSE](https://macfuse.github.io/) or `fuse3` |
| Best for | Sending large files | Working on shared files, git repos |

FUSE lets you `cp`, `cat`, or even `git clone` files directly from your peer's machine.

---

## Three interfaces

**Desktop UI** (Rust/Slint)
```bash
./keibidrop
```

**Interactive CLI** (terminal REPL)
```bash
./keibidrop-cli
```

**Agent CLI** (for scripts and AI agents)
```bash
./kd start                           # Start daemon
./kd show fingerprint                # Get your fingerprint
./kd register <peer-fingerprint>     # Register peer
./kd create                          # Create room (or: kd join)
./kd add /path/to/file.zip           # Share a file
./kd list                            # List shared files
./kd pull file.zip ~/Downloads/      # Download a file
```

All output is JSON for programmatic use. See [docs/kd-agent-guide.md](./docs/kd-agent-guide.md).

---

## How it works

1. Peers exchange fingerprints out-of-band (the fingerprint IS the security)
2. KeibiDrop registers encrypted connection info to a signaling relay
3. Both peers try direct IPv6 connection first
4. If direct fails (firewall, NAT), both connect outbound to an encrypted relay
5. Post-quantum handshake: ML-KEM-1024 + X25519 hybrid key exchange
6. Authenticated encryption: AES-256-GCM or ChaCha20-Poly1305
7. gRPC streams files over the encrypted channel
8. Re-keying after 1M messages or 1 GB for forward secrecy

The relay is a blind pipe. It forwards encrypted bytes between peers using `io.Copy`. It cannot decrypt, inspect, or modify the data. If the relay is compromised, the attacker gets an encrypted byte stream they can't read.

---

## Configuration

KeibiDrop reads `~/.config/keibidrop/config.toml`. Environment variables override the config.

| Setting | Env var | Default |
|--|--|--|
| Relay server | `KD_RELAY` | `https://keibidroprelay.keibisoft.com/` |
| Bridge relay | `KD_BRIDGE` | (auto-fallback when direct fails) |
| Save folder | `KD_SAVE_PATH` | `~/KeibiDrop/Received/` |
| FUSE mount | `KD_MOUNT_PATH` | `~/KeibiDrop/Mount/` |
| Inbound port | `KD_INBOUND_PORT` | 26431 |
| Disable FUSE | `KD_NO_FUSE` | false |

---

## Security

Post-quantum hybrid key exchange prevents future quantum computers from decrypting recorded traffic. Forward secrecy via periodic re-keying limits exposure if a session key is ever compromised.

Full protocol description: [Security.md](./Security.md)

## Troubleshooting

See [TROUBLESHOOTING.md](./TROUBLESHOOTING.md).

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md).

## License

Go engine, CLI, and mobile bindings: [Mozilla Public License 2.0](./LICENSE) (per-file copyleft)

Rust UI and brand assets: Proprietary - see [LICENSE-GUIDE.md](./LICENSE-GUIDE.md)

Desktop UI built with [Slint](https://slint.dev)

Built by [KeibiSoft SRL](https://keibisoft.com).
