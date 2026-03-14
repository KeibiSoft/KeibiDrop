# kd - KeibiDrop CLI for AI Agents

`kd` is a non-interactive CLI for KeibiDrop. It runs as a background daemon and accepts one-shot commands via Unix socket. Designed for AI agents (Claude Code, etc.) to share files between systems programmatically.

## Quick Start

### Build

```bash
make build-kd
```

This produces a `./kd` binary.

### Start the daemon

```bash
# no-FUSE mode (use add/pull commands for file transfer)
KD_SAVE_PATH=./received KD_NO_FUSE=1 ./kd start

# FUSE mode (files appear in a virtual folder)
KD_SAVE_PATH=./saved KD_MOUNT_PATH=./mount ./kd start
```

The daemon runs in the foreground and prints its fingerprint as JSON on startup. All subsequent commands are run in a separate terminal/process.

### Connect to a peer

```bash
# 1. Get your fingerprint (send this to your peer via any channel)
./kd show fingerprint

# 2. Register the peer's fingerprint
./kd register <peer-fingerprint>

# 3. One side creates, the other joins
./kd create    # initiator
./kd join      # joiner (in the other peer's terminal)
```

### Share files

**no-FUSE mode:**
```bash
./kd add ./myfile.pdf              # share a local file
./kd list                          # see all files (local + remote)
./kd pull report.pdf ./report.pdf  # download a remote file
```

**FUSE mode:**
```bash
# After connecting, peer's files appear in KD_MOUNT_PATH
ls ./mount/                        # list remote files
cat ./mount/config.yaml            # read a remote file directly
cp ./myfile.pdf ./mount/           # share a file (copy into mount)
```

### Check status

```bash
./kd status
```

Returns JSON with: `running`, `connection_status`, `fingerprint`, `peer_ip`, `fuse`, `mount_path`, `save_path`, file counts.

### Disconnect and stop

```bash
./kd disconnect   # disconnect from peer, keys rotate, ready for new session
./kd stop         # shutdown the daemon
```

## Environment Variables

All set before `kd start`:

| Variable | Description | Default |
|---|---|---|
| `KD_RELAY` | Relay server URL | `https://keibidroprelay.keibisoft.com` |
| `KD_INBOUND_PORT` | TCP listen port (range 26000-27000) | `26431` |
| `KD_OUTBOUND_PORT` | TCP outbound port (range 26000-27000) | `26432` |
| `KD_SAVE_PATH` | Where to save received files | |
| `KD_MOUNT_PATH` | FUSE mount point (directory) | |
| `KD_NO_FUSE` | Set to any value to disable FUSE | |
| `KD_LOG_FILE` | Log file path | stderr |
| `KD_SOCKET` | Unix socket path | `/tmp/kd.sock` |

## Running Two Instances (same machine)

Use different ports and sockets:

```bash
# Terminal 1: Alice
KD_SAVE_PATH=./SaveAlice KD_NO_FUSE=1 \
  KD_INBOUND_PORT=26001 KD_OUTBOUND_PORT=26002 \
  KD_SOCKET=/tmp/kd-alice.sock ./kd start

# Terminal 2: Bob
KD_SAVE_PATH=./SaveBob KD_NO_FUSE=1 \
  KD_INBOUND_PORT=26003 KD_OUTBOUND_PORT=26004 \
  KD_SOCKET=/tmp/kd-bob.sock ./kd start

# Terminal 3: connect them
KD_SOCKET=/tmp/kd-alice.sock ./kd show fingerprint
KD_SOCKET=/tmp/kd-bob.sock ./kd show fingerprint

KD_SOCKET=/tmp/kd-alice.sock ./kd register <bob-fp>
KD_SOCKET=/tmp/kd-bob.sock ./kd register <alice-fp>

KD_SOCKET=/tmp/kd-alice.sock ./kd create &
KD_SOCKET=/tmp/kd-bob.sock ./kd join
```

## JSON Output Format

Every command returns a single JSON line:

```json
{"ok":true,"data":{"fingerprint":"abc123..."}}
{"ok":false,"error":"daemon not running (socket: /tmp/kd.sock)"}
```

- `ok: true` — command succeeded, result in `data`
- `ok: false` — command failed, reason in `error`
- Exit code 0 on success, 1 on failure

## Command Reference

| Command | Description | Example Output |
|---|---|---|
| `kd start` | Start daemon (foreground) | `{"ok":true,"data":{"fingerprint":"...","ip":"...","socket":"..."}}` |
| `kd stop` | Stop daemon | `{"ok":true,"data":{"status":"stopped"}}` |
| `kd show [what]` | Show info (fingerprint/ip/peer/relay/status/all) | `{"ok":true,"data":{"fingerprint":"..."}}` |
| `kd register <fp>` | Register peer fingerprint | `{"ok":true,"data":{"registered":"..."}}` |
| `kd create` | Create room (blocks until peer joins) | `{"ok":true,"data":{"status":"connected","peer_ip":"..."}}` |
| `kd join` | Join room (blocks until connected) | `{"ok":true,"data":{"status":"connected","peer_ip":"..."}}` |
| `kd add <path>` | Share a file | `{"ok":true,"data":{"added":"./file.txt"}}` |
| `kd list` | List all files | `{"ok":true,"data":{"files":[{"name":"...","size":123,"source":"remote"}]}}` |
| `kd pull <name> [path]` | Download remote file | `{"ok":true,"data":{"pulled":"file.txt","to":"./file.txt"}}` |
| `kd status` | Full status | `{"ok":true,"data":{"running":true,"connection_status":"healthy",...}}` |
| `kd disconnect` | Disconnect, rotate keys | `{"ok":true,"data":{"status":"disconnected","new_fingerprint":"..."}}` |
| `kd help` | Show help text | (plain text) |

## Security Notes

- No login or accounts. Identity is a cryptographic fingerprint (ML-KEM + X25519).
- Keys are generated fresh on startup and rotated on every disconnect.
- All traffic is encrypted end-to-end (ChaCha20-Poly1305).
- The relay only sees encrypted blobs — it cannot read your files or metadata.
- Fingerprint exchange is the trust anchor. Send it via a secure channel (Signal, etc.).

## For Agent Developers

When building an agent tool that uses `kd`:

1. Start the daemon in a background process before issuing commands.
2. Parse all output as JSON — check the `ok` field.
3. `kd create` and `kd join` are blocking — they wait for the peer. Run them in the background or with a timeout.
4. After connecting, use `kd status` to get `mount_path` and `save_path` — these are the directories your agent should read from / write to.
5. In FUSE mode, the `mount_path` is a live view of the peer's shared files. Just read/write normally.
6. In no-FUSE mode, use `kd add` to share and `kd pull` to download.
7. Each daemon instance needs unique ports and a unique `KD_SOCKET`.
