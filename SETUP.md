# Developer Setup Guide

Build and run **KEIBI**DROP from source.

---

## Prerequisites

### 1. Go 1.24.3+

```bash
brew install go          # macOS
# or download from https://go.dev/dl/
```

### 2. Rust

```bash
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
```

### 3. C compiler (CGO)

**KEIBI**DROP uses CGO for the FUSE bindings and the Go-to-Rust static library bridge.

**macOS:**
```bash
xcode-select --install
```

On macOS, Go must use Apple's `clang` as the C compiler (not `gcc`). Verify:
```bash
go env CC    # should print "clang" or "/usr/bin/clang"
```

If it shows `gcc`, fix it:
```bash
export CC=clang
```

**Linux (Debian/Ubuntu):**
```bash
sudo apt install build-essential libfuse3-dev
```

Verify CGO is enabled:
```bash
go env CGO_ENABLED    # must print 1
```

### 4. macFUSE (optional, macOS only)

Only needed for FUSE mode. Download from [macfuse.github.io](https://macfuse.github.io/).

After installing, allow the kernel extension in System Settings > Privacy & Security, then restart. On macOS 15.4+, the FSKit backend runs in user space, no kernel extension needed.

```bash
sudo sh -c 'echo "user_allow_other" > /etc/fuse.conf'
```

### 5. IPv6

**KEIBI**DROP requires IPv6 for peer-to-peer connections. Test at [test-ipv6.com](https://test-ipv6.com/).

---

## Build

### Rust UI (recommended)

Protobuf code is already committed, no need to install `protoc`.

```bash
make build-static-rust-bridge          # Go → static library (libkeibidrop.a)
cd rust && cargo build --release       # Rust UI binary
```

Binary: `rust/target/release/keibidrop-rust`

### Interactive CLI

```bash
make build-cli
```

Binary: `keibidrop-cli`

### Agent CLI

```bash
make build-kd
```

Binary: `kd`

### Full build (with protoc regeneration)

If you modified `keibidrop.proto`:

```bash
brew install protobuf
make install-proto
make build-rust        # runs protoc → Go static lib → Rust
```

---

## Run

On first run, **KEIBI**DROP creates `~/.config/keibidrop/config.toml` with default settings. You can edit this file or override values with environment variables.

```bash
# Rust UI (no FUSE)
NO_FUSE=1 ./rust/target/release/keibidrop-rust

# Rust UI (with FUSE)
./rust/target/release/keibidrop-rust

# Interactive CLI
./keibidrop-cli

# Agent CLI
./kd start
```

### Configuration

Settings are loaded in order: defaults → config file → environment variables.

Config file location: `~/.config/keibidrop/config.toml`

```toml
relay = "https://keibidroprelay.keibisoft.com/"
save_path = "/Users/you/KeibiDrop/Received"
mount_path = "/Users/you/KeibiDrop/Mount"
log_file = "/Users/you/Library/Logs/KeibiDrop/keibidrop.log"
inbound_port = 26431
outbound_port = 26432
no_fuse = false
```

Environment variables override the config file. See [README.md](./README.md) for the full table.

---

## Tests

```bash
# Self-contained test suite (36+ tests, no external relay needed)
go test -v -count=1 -timeout 180s ./tests/...
```

---

## Building releases

```bash
make package-macos      # .dmg (macOS only)
make package-deb        # .deb (Linux only)
make package-tar        # .tar.gz (any platform)
```

See `.github/workflows/release.yml` for the automated release pipeline.

---

## Troubleshooting

### `pthread.h` / `stdlib.h` not found
```bash
xcode-select --install
```

### `protoc-gen-go: program not found`
You don't need protoc, generated code is committed. Build without it:
```bash
make build-static-rust-bridge && cd rust && cargo build --release
```

### CGO errors
```bash
export CGO_ENABLED=1
xcode-select --install    # macOS
sudo apt install build-essential    # Linux
```

### IPv6 not working
Check [test-ipv6.com](https://test-ipv6.com/). If your ISP doesn't provide IPv6, **KEIBI**DROP cannot connect peers.

See [TROUBLESHOOTING.md](./TROUBLESHOOTING.md) for more.
