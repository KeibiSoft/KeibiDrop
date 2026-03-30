# Troubleshooting

## Connection issues

### "Connection failed" or peers can't find each other

1. Both peers need IPv6. Test at [test-ipv6.com](https://test-ipv6.com/), you need at least 9/10.
2. Check your firewall allows inbound TCP on the configured port (default: 26431).
3. Check your router doesn't block IPv6 inbound connections.
4. Both peers must use the same relay server.

### Rate limit errors

The public relay limits requests per IP. Wait a few minutes and try again. For heavy testing, run a local relay.

---

## FUSE issues

### "FUSE not detected"

macOS:
```bash
# Install macFUSE from https://macfuse.github.io/
# Restart your Mac after installation
```

Linux (Debian/Ubuntu):
```bash
sudo apt install fuse3
```

Linux (Fedora):
```bash
sudo dnf install fuse3
```

### "Permission denied" when opening files from FUSE mount (macOS)

Finder and Preview need `allow_other` to access FUSE mounts:

```bash
sudo sh -c 'echo "user_allow_other" > /etc/fuse.conf'
```

Then remount (disconnect and reconnect, or restart **KEIBI**DROP).

### macFUSE kernel extension blocked (macOS)

Go to System Settings > Privacy & Security and allow the macFUSE extension.

On macOS 15.4+, macFUSE supports the FSKit backend which runs in user space, no kernel extension needed. Enable it in System Settings > macFUSE > Use FSKit.

### FUSE mount not cleaning up

If **KEIBI**DROP crashes and the mount point is stale:

macOS:
```bash
sudo /sbin/umount -f /path/to/mount
```

Linux:
```bash
fusermount -u /path/to/mount
```

---

## Build issues

### `pthread.h` or `stdlib.h` not found (macOS)

```bash
xcode-select --install
```

### CGO errors (macOS)

```bash
export CGO_ENABLED=1
xcode-select --install
```

Go must use `clang` on macOS. Check with `go env CC`. If it shows `gcc`:
```bash
export CC=clang
```

### CGO errors (Linux)

```bash
export CGO_ENABLED=1
sudo apt install build-essential
```

### Rust build fails with missing `libkeibidrop.a`

Build the Go static library first:
```bash
make build-static-rust-bridge
cd rust && cargo build --release
```

### `protoc-gen-go: program not found`

The generated code is committed, so you can build without protoc:
```bash
make build-static-rust-bridge
cd rust && cargo build --release
```

If you want to regenerate protobuf stubs:

macOS:
```bash
brew install protobuf
make install-proto
make protoc
```

Linux:
```bash
sudo apt install protobuf-compiler
make install-proto
make protoc
```

---

## Files and logs

### Where is the config file?

`~/.config/keibidrop/config.toml`, created on first run with default values.

### Where are log files?

macOS: `~/Library/Logs/KeibiDrop/keibidrop.log`

Linux: `~/.local/share/keibidrop/keibidrop.log`

Override with the `LOG_FILE` environment variable or `log_file` in config.toml.

### Where are received files saved?

Default: `~/KeibiDrop/Received/`

Override with `TO_SAVE_PATH` environment variable or `save_path` in config.toml.

---

## Platform notes

### macOS

- Requires macFUSE for FUSE mode. Works without it in no-FUSE mode.
- Apple Silicon and Intel both supported.
- macOS 15.4+ can use the FSKit backend (no kernel extension).

### Linux

- Requires `fuse3` and `libfuse3-dev` (for building from source).
- Tested on Ubuntu 22.04+ (x86_64).
- ARM64 Linux support available for CLI binaries.

### Windows

Works in no-FUSE mode (not yet tested). FUSE mode requires [WinFsp](https://winfsp.dev/) and has not been tested yet.
