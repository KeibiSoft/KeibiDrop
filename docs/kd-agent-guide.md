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

- No login or accounts. Identity is an ephemeral cryptographic fingerprint (ML-KEM + X25519).
- Keys are generated fresh on startup and rotated on every disconnect. Identity disappears after the session.
- All traffic is encrypted end-to-end (ChaCha20-Poly1305).
- The relay only sees encrypted blobs — it cannot read your files or metadata.
- Fingerprint exchange is the trust anchor. Send it via a secure channel (Signal, etc.).

## For Agent Developers

When building an agent tool that uses `kd`:

1. Start the daemon with FUSE enabled (`KD_MOUNT_PATH=./mount`). This is the recommended mode for agents.
2. Parse all output as JSON — check the `ok` field.
3. `kd create` and `kd join` are blocking — they wait for the peer. Run them in the background or with a timeout.
4. After connecting, use `kd status` to get the `mount_path` — this is the synced folder your agent should use.
5. The `mount_path` is a live, bidirectional view of shared files. Read remote files and write local files directly to/from this folder. No need for `kd add` or `kd pull` — just use normal file I/O.
6. Each daemon instance needs unique ports and a unique `KD_SOCKET`.

## Full Example with Outputs

Below is a real session showing every command and its JSON output.

### 1. Start the daemon

```bash
$ KD_SAVE_PATH=./SaveAlice KD_MOUNT_PATH=./MountAlice \
    KD_INBOUND_PORT=26001 KD_OUTBOUND_PORT=26002 \
    KD_SOCKET=/tmp/kd-alice.sock ./kd start
```
```json
{"ok":true,"data":{"fingerprint":"6c_RJID9Twnm7o1QtyHgtSPlOjLz74D5a8doUExb-4QhzY_UFl3GWa-o-1tHGktv6U1FLfxvpL1bWixaAO8ayQ","fuse":true,"ip":"2a02:2f00:c40d:4a00:c86:145:393d:ea31","mount_path":"./MountAlice","relay":"https://keibidroprelay.keibisoft.com","save_path":"./SaveAlice","socket":"/tmp/kd-alice.sock"}}
```

### 2. Get your fingerprint

```bash
$ KD_SOCKET=/tmp/kd-alice.sock ./kd show fingerprint
```
```json
{"ok":true,"data":{"fingerprint":"6c_RJID9Twnm7o1QtyHgtSPlOjLz74D5a8doUExb-4QhzY_UFl3GWa-o-1tHGktv6U1FLfxvpL1bWixaAO8ayQ"}}
```

Send this fingerprint to your peer via Signal, Telegram, or any secure channel.

### 3. Register peer's fingerprint

```bash
$ KD_SOCKET=/tmp/kd-alice.sock ./kd register "Y9cykk9ez6blXF_3-hAQrIr8WGCWiUFd4f7-eoWeLafK87IkXmLFUwuW7M9geff3ePPelQlthF0Jy6KJtev_oQ"
```
```json
{"ok":true,"data":{"registered":"Y9cykk9ez6blXF_3-hAQrIr8WGCWiUFd4f7-eoWeLafK87IkXmLFUwuW7M9geff3ePPelQlthF0Jy6KJtev_oQ"}}
```

### 4. Create room (or join)

```bash
$ KD_SOCKET=/tmp/kd-alice.sock ./kd create
```
```json
{"ok":true,"data":{"peer_ip":"2a02:2f00:c40d:4a00:c86:145:393d:ea31","status":"connected"}}
```

This blocks until the peer joins. The peer runs `./kd join` on their side.

### 5. Check status

```bash
$ KD_SOCKET=/tmp/kd-alice.sock ./kd status
```
```json
{"ok":true,"data":{"connection_status":"healthy","fingerprint":"6c_RJID9Twnm7o1Q...","fuse":true,"ip":"2a02:2f00:c40d:4a00:...","local_files":0,"mount_path":"./MountAlice","peer_fingerprint":"Y9cykk9ez6blXF_3...","peer_ip":"2a02:2f00:c40d:4a00:...","relay":"https://keibidroprelay.keibisoft.com","remote_files":0,"running":true,"save_path":"./SaveAlice"}}
```

The `mount_path` in the response is the synced folder. Use it for all file operations.

### 6. Use the synced folder

After connecting, peer's files appear in the mount path. Read and write directly:

```bash
# List remote files from peer
$ ls ./MountAlice/
report.pdf    config.yaml    notes.txt

# Read a remote file
$ cat ./MountAlice/config.yaml

# Share a file with peer (copy into mount)
$ cp ./myfile.pdf ./MountAlice/
```

### 7. List files (alternative to ls)

```bash
$ KD_SOCKET=/tmp/kd-alice.sock ./kd list
```
```json
{"ok":true,"data":{"files":[{"name":"kd-test-hello.txt","size":69,"path":"/tmp/kd-test-hello.txt","source":"local"},{"name":"report.pdf","size":204800,"path":"","source":"remote"}]}}
```

### 8. Disconnect

```bash
$ KD_SOCKET=/tmp/kd-alice.sock ./kd disconnect
```
```json
{"ok":true,"data":{"new_fingerprint":"5j32NIvH6oRP0QKvvNNm_PwnPRBbllHrIk0fAyoiig3qcSTF4dFXbsbyA4r_6BA2v8HoEAW4f1_LAIjPqOrYlA","status":"disconnected"}}
```

Keys are rotated. A new fingerprint is generated. You can start a new session with a different peer.

### 9. Stop the daemon

```bash
$ KD_SOCKET=/tmp/kd-alice.sock ./kd stop
```
```json
{"ok":true,"data":{"status":"stopped"}}
```
