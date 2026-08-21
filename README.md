<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="logo.svg">
    <source media="(prefers-color-scheme: light)" srcset="logo-dark.svg">
    <img alt="KeibiDrop" src="logo-dark.svg" width="400">
  </picture>
</p>

<p align="center">
  <a href="https://keibidrop.com">Website</a> · <a href="https://github.com/KeibiSoft/KeibiDrop/releases">Download</a> · <a href="https://keibisoft.com/blog.html">Blog</a>
</p>

<h2 align="center">Work on shared files instantly.<br>No more upload or download waiting.</h2>

<p align="center">
  <a href="https://www.youtube.com/watch?v=9Wt0NMx2_I8">
    <img src="demo-photos/keibidrop-demo.gif" alt="KeibiDrop demo, Mac and Windows side by side. Click to watch on YouTube with sound." width="900">
  </a>
  <br><sub><b>Click to watch on YouTube</b> (with sound)</sub>
</p>

Another computer's files show up as a folder on yours. Open and edit them in your own apps, with nothing to download first.

- Open a 40 GB file and start working instantly. Your edits sync back.
- End-to-end encrypted (post-quantum). No cloud, and the relay never reads your files.
- Same network: one click. Over the internet: swap a code once, then save the contact.
- Their files become a real folder on your machine. Open, edit, `git clone`, anything.
- macOS, Linux, Windows. iOS and Android coming soon. Open source (MPL-2.0).

<p align="center">
  <img src="demo-photos/initial-screen.png" alt="KeibiDrop connection screen" width="700">
</p>

| Direct Transfer | Virtual Folder (FUSE) |
|:---:|:---:|
| <img src="demo-photos/connected-in-direct-transfer-mode.png" alt="Direct transfer mode" width="450"> | <img src="demo-photos/connected-as-virtual-filesystem.png" alt="Virtual filesystem mode" width="450"> |
| Drag files in. Your peer saves what they need. | Peer files appear as a folder. Open in any app. |

| Finder | Terminal (git) |
|:---:|:---:|
| <img src="demo-photos/as-filesystem-accessible-from-finder.png" alt="Shared files in Finder" width="450"> | <img src="demo-photos/as-filesystem-git-ops-work.png" alt="Git on FUSE mount" width="450"> |

---

## Install

### macOS

```bash
brew tap keibisoft/keibidrop
brew trust keibisoft/keibidrop
brew install keibidrop
```

### Linux (Debian/Ubuntu)

```bash
wget -O keibidrop.deb $(curl -s https://api.github.com/repos/KeibiSoft/KeibiDrop/releases/latest \
  | grep -o 'https://[^"]*_amd64\.deb')
sudo apt install ./keibidrop.deb
```

### Windows

```bash
choco install keibidrop
```

Or download from [GitHub Releases](https://github.com/KeibiSoft/KeibiDrop/releases).

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

**Same network (LAN):**
1. Both peers launch KeibiDrop
2. Devices discover each other automatically
3. One click to connect
4. Share files

**Over the internet:**
1. Both peers launch KeibiDrop
2. Copy your fingerprint and send it to your peer (Signal, Telegram, anything)
3. Paste each other's fingerprints and connect
4. Share files

Save a peer as a contact to skip the fingerprint exchange next time. On the same network, saved contacts connect with one click using pseudonyms.

It works through firewalls automatically. If direct connection fails, KeibiDrop falls back to an encrypted relay. No port forwarding, no router configuration.

---

## Two modes

| | Direct Transfer | Virtual Folder (FUSE) |
|--|--|--|
| Speed | Up to ~660 MB/s | Up to ~276 MB/s |
| How it works | Add files, peer pulls them | Peer's files appear as a local folder |
| Setup | Nothing extra | Install [macFUSE](https://macfuse.github.io/), [fuse3](https://github.com/libfuse/libfuse), or [WinFsp](https://winfsp.dev/) |
| Best for | Sending large files | Working on shared files, git repos |

---

## How it works

1. Peers exchange fingerprints (or discover each other on LAN)
2. KeibiDrop finds the fastest path: LAN, direct IPv6, or encrypted relay
3. Files transfer over an encrypted channel the relay cannot read
4. Keys rotate automatically for forward secrecy

The encryption is post-quantum (ML-KEM-1024 + X25519) with AES-256-GCM or ChaCha20-Poly1305. Full protocol details in [Security.md](./Security.md).

---

<details>
<summary><strong>Three interfaces</strong></summary>

**Desktop UI** (Rust/Slint)
```bash
./keibidrop
```

**Interactive CLI** (terminal REPL)
```bash
./keibidrop-cli
```

**Agent CLI** (for scripts and AI agents, all output is JSON)
```bash
./kd start                           # Start daemon
./kd show fingerprint                # Get your fingerprint
./kd register <peer-fingerprint>     # Register peer
./kd create                          # Create room (or: kd join)
./kd add /path/to/file.zip           # Share a file
./kd list                            # List shared files
./kd pull file.zip ~/Downloads/      # Download a file
```

See [docs/kd-agent-guide.md](./docs/kd-agent-guide.md).

</details>

<details>
<summary><strong>Configuration</strong></summary>

KeibiDrop reads `~/.config/keibidrop/config.toml`. Environment variables override the config.

| Setting | Env var | Default |
|--|--|--|
| Relay server | `KD_RELAY` | `https://keibidroprelay.keibisoft.com/` |
| Bridge relay | `KD_BRIDGE` | `bridge.keibisoft.com:26600` |
| Save folder | `KD_SAVE_PATH` | `~/KeibiDrop/Received/` |
| FUSE mount | `KD_MOUNT_PATH` | `~/KeibiDrop/Mount/` |
| Inbound port | `KD_INBOUND_PORT` | 26431 |
| Disable FUSE | `KD_NO_FUSE` | false |

</details>

<details>
<summary><strong>Security</strong></summary>

Post-quantum hybrid key exchange prevents future quantum computers from decrypting recorded traffic. Forward secrecy via periodic re-keying limits exposure if a session key is ever compromised.

By default, identity persists across sessions (so saved contacts keep working), encrypted with a per-install master key stored in the OS keychain (macOS Keychain Services, Linux Secret Service, Windows Credential Manager). Headless setups fall back to `~/.config/keibidrop/.master.key` (mode 0600). Optional passphrase protection via Argon2id for users who back up config to the cloud. Incognito mode switches to ephemeral keys: a fresh keypair each session, anonymous, unlinkable, nothing written to disk.

Full protocol description: [Security.md](./Security.md)

</details>

---

## Troubleshooting

See [TROUBLESHOOTING.md](./TROUBLESHOOTING.md).

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md).

## License

Go engine, CLI, and mobile bindings: [Mozilla Public License 2.0](./LICENSE) (per-file copyleft)

Rust UI and brand assets: Proprietary - see [DUAL-LICENSE.md](./DUAL-LICENSE.md)

Desktop UI built with [Slint](https://slint.dev)

Built by [KeibiSoft SRL](https://keibisoft.com).

---

## By trade

### Remote DFIR triage

[![Remote triage without a VPN: 20 GB mounted, 606 MB moved](https://keibidrop.com/media/dfir-remote-triage-video.jpg)](https://youtu.be/Y_4PZcqr4U4)

A recorded run from start to finish. A Mac holds 2,683 files that come to 19.9 GB, a Windows laptop mounts that folder read only, and the link is relayed 410 km away so both machines are sharing the same 50 Mbps uplink. The video walks the tree, greps every log in it, pulls out the artifact set with robocopy, and hashes the same two registry hives on both sides.

Over the whole session 606 MB crossed the link against the 19.9 GB sitting on the Mac, which works out at 34 times less data. That figure comes off the network card, so it includes the grep. The artifacts written to disk came to 382 MB, nothing on the evidence machine changed, and the access times stayed where they were.

Do not read the clock in the video as a speed you would get. The extract took just over seven minutes because there were 2,174 separate files in it and every one waited for its own round trip, and that part has been rewritten since. The evidence tree was generated for the demo.

Mount an evidence share read only across NAT and extract the artifact set you need, with no inbound port, native on Windows, and no kernel driver on the evidence machine. There is a [write-up](https://keibidrop.com/blog/triage-a-remote-machine-without-a-vpn.html) and a [DFIR page](https://keibidrop.com/for/dfir.html).

### Post-production handoff

[Mount the camera originals](https://keibidrop.com/for/post-production.html). No proxy transcode before the handoff, and no relink after it.

### Datasets on a rented GPU box

[Mount the workstation that holds the data](https://keibidrop.com/for/ml.html). No bucket, and no upload before the first batch.

## Links

- [Website](https://keibidrop.com)
- [Documentation](https://keibidrop.com/docs/)
- [Compared with sshfs](https://keibidrop.com/compare/sshfs.html) - measured on a 200 ms link
- [Technical deep dive](https://keibisoft.com/tools/keibidrop.html)
- [Blog posts](https://keibisoft.com/blog.html) (53 posts on FUSE, crypto, performance)
- [FAQ](https://keibisoft.com/tools/keibidrop-faq.html)
- [Comparison with alternatives](https://keibisoft.com/tools/keibidrop-vs-alternatives.html)
- [About and credentials](https://keibidrop.com/about.html)
