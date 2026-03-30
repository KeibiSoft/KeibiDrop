```
██╗  ██╗███████╗██╗██████╗ ██╗██████╗ ██████╗  ██████╗ ██████╗
██║ ██╔╝██╔════╝██║██╔══██╗██║██╔══██╗██╔══██╗██╔═══██╗██╔══██╗
█████╔╝ █████╗  ██║██████╔╝██║██║  ██║██████╔╝██║   ██║██████╔╝
██╔═██╗ ██╔══╝  ██║██╔══██╗██║██║  ██║██╔══██╗██║   ██║██╔═══╝
██║  ██╗███████╗██║██████╔╝██║██████╔╝██║  ██║╚██████╔╝██║
╚═╝  ╚═╝╚══════╝╚═╝╚═════╝ ╚═╝╚═════╝ ╚═╝  ╚═╝ ╚═════╝ ╚═╝
```

End-to-end encrypted, peer-to-peer file sharing between desktops.

Files transfer directly between your machines. No cloud, no accounts, no upload limits. Traffic is encrypted with post-quantum cryptography (ML-KEM-1024 + X25519) and AES-256-GCM / ChaCha20-Poly1305.

| Connect | Share files |
|---|---|
| ![Connect screen](demo-photos/HomeScreen2-Client2.png) | ![Connected](demo-photos/ConnectedScreen-NO-FUSE3-Client2.png) |

---

## Install

### macOS (Homebrew)

```bash
brew tap keibisoft/keibidrop
brew install keibidrop
```

### Linux (Debian/Ubuntu)

```bash
wget https://github.com/KeibiSoft/KeibiDrop/releases/latest/download/keibidrop_amd64.deb
sudo dpkg -i keibidrop_amd64.deb
```

### Download binary

Grab the latest release for your platform from [GitHub Releases](https://github.com/KeibiSoft/KeibiDrop/releases).

### Build from source

Requires Go 1.24+, Rust, and CGO enabled. On macOS, `go env CC` must show `clang`. See [SETUP.md](./SETUP.md) for full instructions.

```bash
make build-static-rust-bridge
cd rust && cargo build --release
```

---

## Quick start

1. Both peers launch **KEIBI**DROP.
2. Copy your fingerprint code and send it to your peer (Signal, Telegram, email, anything).
3. Paste each other's codes.
4. One peer creates a room, the other joins.
5. Share files.

In FUSE mode, the peer's files appear as a virtual folder you can browse in Finder or your file manager. In no-FUSE mode, use the UI or CLI to add/pull files.

**Which mode to use?**
- Transferring a few large files → no-FUSE (faster, simpler)
- Working on shared files in real-time (edit, save, see changes) → FUSE

---

## FUSE setup (optional)

FUSE gives you a virtual folder where the peer's files appear as regular files. Without it, you use add/pull commands instead.

**macOS:** Install [macFUSE](https://macfuse.github.io/). On macOS 15.4+, the FSKit backend works without kernel extensions.
```bash
sudo sh -c 'echo "user_allow_other" > /etc/fuse.conf'
```

**Linux:**
```bash
sudo apt install fuse3    # Debian/Ubuntu
```

---

## Three ways to run

**Desktop UI** (Rust/Slint) - the main app. Point-and-click file sharing.
```bash
./keibidrop-rust
```

**Interactive CLI** - terminal REPL with autocomplete.
```bash
./keibidrop-cli
```

**Agent CLI** (`kd`) - for AI agents and scripts. Daemon + JSON protocol over Unix socket.
```bash
KD_SAVE_PATH=./received ./kd start    # Terminal 1
./kd show fingerprint                  # Terminal 2
./kd register <peer-fingerprint>
./kd create
```

See [docs/kd-agent-guide.md](./docs/kd-agent-guide.md) for the full agent integration guide.

---

## Configuration

**KEIBI**DROP reads `~/.config/keibidrop/config.toml` on startup. A default config is created on first run. Environment variables override the config file.

| Setting | Config key | Env var | Default |
|---|---|---|---|
| Relay server | `relay` | `KEIBIDROP_RELAY` | `https://keibidroprelay.keibisoft.com/` |
| Save folder | `save_path` | `TO_SAVE_PATH` | `~/KeibiDrop/Received/` |
| FUSE mount | `mount_path` | `TO_MOUNT_PATH` | `~/KeibiDrop/Mount/` |
| Log file | `log_file` | `LOG_FILE` | platform-specific |
| Inbound port | `inbound_port` | `INBOUND_PORT` | 26431 |
| Outbound port | `outbound_port` | `OUTBOUND_PORT` | 26432 |
| Disable FUSE | `no_fuse` | `NO_FUSE` | false |

---

## Networking

**KEIBI**DROP connects peers directly over IPv6. Both machines need:
- Globally routable IPv6 addresses
- Inbound TCP allowed on the configured port
- No NAT traversal (no STUN/TURN)

Test your IPv6 at [test-ipv6.com](https://test-ipv6.com/).

---

## Troubleshooting

See [TROUBLESHOOTING.md](./TROUBLESHOOTING.md) for common issues (connection failures, FUSE problems, build errors).

## Security

Post-quantum hybrid key exchange, authenticated encryption, forward secrecy via re-keying. Full protocol description in [Security.md](./Security.md).

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md). All commits must be signed (`git commit -S`) and signed-off (`git commit --sign-off`) per the [DCO](./DCO.txt).

## License

[Mozilla Public License 2.0](./LICENSE)

Developed by [KeibiSoft SRL](https://keibisoft.com).
