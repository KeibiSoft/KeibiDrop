# Troubleshooting

## Connection issues

### "Connection failed" or peers can't find each other

1. Both peers need IPv6. Test at [test-ipv6.com](https://test-ipv6.com/) — you need at least 9/10.
2. Check your firewall allows inbound TCP on the configured port (default: 26431).
3. Check your router doesn't block IPv6 inbound connections.
4. Both peers must use the same relay server.

### Rate limit errors

The public relay limits requests per IP. Wait a few minutes and try again. For heavy testing, run a local relay.

---

## FUSE issues

### "FUSE not detected"

**macOS:** Install [macFUSE](https://macfuse.github.io/). Restart after installation.

**Linux:**
```bash
sudo apt install fuse3         # Debian/Ubuntu
sudo dnf install fuse3         # Fedora
sudo pacman -S fuse3           # Arch
```

### "Permission denied" when opening files from FUSE mount (macOS)

Finder and Preview need `allow_other` to access FUSE mounts:

```bash
sudo sh -c 'echo "user_allow_other" > /etc/fuse.conf'
```

Then remount (disconnect and reconnect, or restart **KEIBI**DROP).

### macFUSE kernel extension blocked (macOS)

macOS may block the macFUSE kernel extension. Go to System Settings > Privacy & Security and allow it.

On macOS 15.4+, macFUSE supports the **FSKit backend** which runs entirely in user space — no kernel extension needed. Enable it in System Settings > macFUSE > Use FSKit.

### FUSE mount not cleaning up

If **KEIBI**DROP crashes and the mount point is stale:

**macOS:**
```bash
sudo /sbin/umount -f /path/to/mount
```

**Linux:**
```bash
fusermount -u /path/to/mount
```

---

## Build issues

### `pthread.h` or `stdlib.h` not found (macOS)

Install Xcode Command Line Tools:
```bash
xcode-select --install
```

### `gcc not found` or CGO errors

```bash
export CGO_ENABLED=1
xcode-select --install          # macOS
sudo apt install build-essential  # Linux
```

On macOS, Go must use `clang` (not `gcc`). Check with `go env CC`. If it shows `gcc`:
```bash
export CC=clang
```

### Rust build fails with missing `libkeibidrop.a`

Build the Go static library first:
```bash
make build-static-rust-bridge
cd rust && cargo build --release
```

### `protoc-gen-go: program not found`

You don't need protoc — the generated code is committed. Build without it:
```bash
make build-static-rust-bridge
cd rust && cargo build --release
```

If you do want to regenerate:
```bash
brew install protobuf     # macOS
make install-proto
make protoc
```

---

## Files and logs

### Where is the config file?

`~/.config/keibidrop/config.toml` — created on first run with default values.

### Where are log files?

**macOS:** `~/Library/Logs/KeibiDrop/keibidrop.log`
**Linux:** `~/.local/share/keibidrop/keibidrop.log`

Override with the `LOG_FILE` environment variable or `log_file` in config.toml.

### Where are received files saved?

Default: `~/KeibiDrop/Received/`

Override with `TO_SAVE_PATH` environment variable or `save_path` in config.toml.

---

## Platform notes

### macOS

- Requires macFUSE for FUSE mode. Works fine without it in no-FUSE mode.
- Apple Silicon and Intel both supported.
- macOS 15.4+ can use the FSKit backend (no kernel extension).

### Linux

- Requires `fuse3` and `libfuse3-dev` (for building from source).
- Tested on Ubuntu 22.04+ (x86_64).
- ARM64 Linux support is available for CLI binaries.

### Windows

Not yet supported. Planned for a future release with [WinFsp](https://winfsp.dev/).
