# KeibiDrop - Developer Setup Guide

Step-by-step guide for building and running KeibiDrop locally.

---

## Prerequisites

### 1. Go (1.24.3 or newer)

Install from [go.dev/dl](https://go.dev/dl/) or via Homebrew:

```bash
brew install go
```

Verify:
```bash
go version
```

**Important**: Make sure `GOPATH/bin` is in your `PATH`:
```bash
# Add to your ~/.zshrc or ~/.bashrc:
export PATH="$PATH:$(go env GOPATH)/bin"
```

### 2. Rust & Cargo

Install via [rustup](https://rustup.rs/):
```bash
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
```

### 3. Xcode Command Line Tools (macOS)

Required for CGO compilation (pthread, stdlib headers):
```bash
xcode-select --install
```

If you see errors about `pthread.h`, `stdlib.h`, or missing system headers, this is almost certainly the fix.

### 4. CGO Enabled

KeibiDrop uses CGO. Verify it's enabled:
```bash
go env CGO_ENABLED
# Should print: 1
```

If not:
```bash
export CGO_ENABLED=1
```

### 5. GOPRIVATE (for private repo access)

Since KeibiDrop is a private repo, set:
```bash
go env -w GOPRIVATE='github.com/keibiSoft'
```

This tells Go to skip the public proxy/checksum database for KeibiSoft packages.

### 6. macFUSE (optional, for FUSE mode)

Only needed if you want the virtual filesystem mount. You can run without it using `NO_FUSE=1`.

- Download from [macfuse.github.io](https://macfuse.github.io/)
- Restart your Mac after installation
- Trust "Benjamin Fleischer" as a developer in System Settings > Privacy & Security

### 7. IPv6 Connectivity

KeibiDrop requires IPv6 for peer-to-peer connections. Test at [test-ipv6.com](https://test-ipv6.com/) — you need at least 9/10.

---

## Build

### Option A: Rust UI (recommended)

The protobuf code is already generated and committed, so you do **not** need to install `protoc`. However, the Makefile `build-rust` target runs `protoc` by default.

**Skip protoc and build directly:**

```bash
# Step 1: Build the Go static library
make build-static-rust-bridge

# Step 2: Build the Rust UI binary
cd rust && cargo build --release && cd ..
```

Or if you have `protoc` installed (optional):
```bash
make install-proto   # install protoc Go plugins
make build-rust      # full build including protoc
```

### Option B: Go-only (CLI)

```bash
make build-cli
```

### Option C: Go-only (GUI, Fyne-based)

```bash
make build-gui
```

---

## Run

### With Rust UI (no FUSE)

Create your save directory and run:

```bash
mkdir -p SaveAlice

LOG_FILE="Log_Alice.txt" \
NO_FUSE=1 \
TO_SAVE_PATH="$(pwd)/SaveAlice" \
TO_MOUNT_PATH="$(pwd)/MountAlice" \
KEIBIDROP_RELAY="https://keibidroprelay.keibisoft.com/" \
INBOUND_PORT=26001 \
OUTBOUND_PORT=26002 \
./rust/target/release/keibidrop-rust
```

### With Rust UI (with FUSE)

```bash
mkdir -p SaveAlice MountAlice

LOG_FILE="Log_Alice.txt" \
TO_SAVE_PATH="$(pwd)/SaveAlice" \
TO_MOUNT_PATH="$(pwd)/MountAlice" \
KEIBIDROP_RELAY="https://keibidroprelay.keibisoft.com/" \
INBOUND_PORT=26001 \
OUTBOUND_PORT=26002 \
./rust/target/release/keibidrop-rust
```

### Environment Variables

| Variable | Description | Example |
|---|---|---|
| `LOG_FILE` | Log output file | `Log_Alice.txt` |
| `NO_FUSE` | Set to `1` to disable FUSE mount | `1` |
| `TO_SAVE_PATH` | **Absolute path** where received files are saved | `/Users/you/KeibiDrop/SaveAlice` |
| `TO_MOUNT_PATH` | **Absolute path** for FUSE mount point | `/Users/you/KeibiDrop/MountAlice` |
| `KEIBIDROP_RELAY` | Relay server URL | `https://keibidroprelay.keibisoft.com/` |
| `INBOUND_PORT` | TCP port for incoming connections (26000-27000) | `26001` |
| `OUTBOUND_PORT` | TCP port for outgoing connections (26000-27000) | `26002` |

---

## Connecting Two Peers

1. Both peers start KeibiDrop
2. Peer A clicks "Export Fingerprint" and copies the string
3. Peer A sends the fingerprint to Peer B via a secure channel (Signal, Telegram, etc.)
4. Peer B pastes it into "Add Peer Fingerprint"
5. Peer B does the same in reverse (exports their fingerprint to Peer A)
6. Connection establishes automatically

---

## Troubleshooting

### `pthread.h` / `stdlib.h` not found
Install Xcode Command Line Tools:
```bash
xcode-select --install
```

### `protoc-gen-go: program not found or is not executable`
You don't need protoc — the generated code is already committed. Build without it:
```bash
make build-static-rust-bridge
cd rust && cargo build --release
```

Or install it if you want to regenerate:
```bash
brew install protobuf
make install-proto
```

### `GOPATH/bin` not in PATH
```bash
export PATH="$PATH:$(go env GOPATH)/bin"
```
Add this line to your `~/.zshrc` or `~/.bashrc` to make it permanent.

### CGO errors / `gcc not found`
```bash
export CGO_ENABLED=1
xcode-select --install
```

### IPv6 not working
Check [test-ipv6.com](https://test-ipv6.com/). If your ISP doesn't provide IPv6, KeibiDrop cannot establish P2P connections.

---

## Running Tests

```bash
go test -v -count=1 -timeout 180s ./tests/...
```
