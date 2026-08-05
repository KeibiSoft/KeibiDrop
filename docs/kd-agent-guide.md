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

### Connect to a peer (first time)

```bash
./kd show fingerprint                # send this to your peer
./kd register <peer-fingerprint>     # paste theirs
./kd connect                         # both peers run this (auto role)
```

### Reconnect with a saved contact

```bash
./kd contacts                        # list saved contacts
./kd quick-connect <fingerprint>     # 1-click, no code exchange
```

### Save a peer as a contact (after connecting)

```bash
./kd save-contact Alice              # saves for quick-connect next time
```

### Share files

**no-FUSE mode:**
```bash
./kd add ./myfile.pdf                # share a local file
./kd add-as ./f.txt report.txt       # share with a custom remote name
./kd unshare myfile.pdf              # stop sharing (keeps on disk)
./kd list                            # see all files (local + remote)
./kd pull report.pdf ./report.pdf    # download a remote file
./kd progress report.pdf             # check download progress (0-100)
./kd cancel-download report.pdf      # pause download (resume later with pull)
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
| `KD_INCOGNITO` | Force ephemeral mode, no identity saved | |
| `KD_PASSPHRASE_PROTECT` | Prompt for passphrase to encrypt identity | |
| `KD_SOCKET` | Unix socket path | `/tmp/kd.sock` |

## Running Two Instances (same machine)

Use different ports and sockets:

```bash
# Terminal 1: Alice
KEIBIDROP_CONFIG_DIR=~/.config/keibidrop-alice \
  KD_SAVE_PATH=./SaveAlice KD_NO_FUSE=1 \
  KD_INBOUND_PORT=26001 KD_OUTBOUND_PORT=26002 \
  KD_SOCKET=/tmp/kd-alice.sock ./kd start

# Terminal 2: Bob
KEIBIDROP_CONFIG_DIR=~/.config/keibidrop-bob \
  KD_SAVE_PATH=./SaveBob KD_NO_FUSE=1 \
  KD_INBOUND_PORT=26003 KD_OUTBOUND_PORT=26004 \
  KD_SOCKET=/tmp/kd-bob.sock ./kd start

# Terminal 3: connect them
KD_SOCKET=/tmp/kd-alice.sock ./kd show fingerprint
KD_SOCKET=/tmp/kd-bob.sock ./kd register <alice-fp>

KD_SOCKET=/tmp/kd-bob.sock ./kd show fingerprint
KD_SOCKET=/tmp/kd-alice.sock ./kd register <bob-fp>

KD_SOCKET=/tmp/kd-alice.sock ./kd connect &
KD_SOCKET=/tmp/kd-bob.sock ./kd connect
```

## JSON Output Format

Every command returns a single JSON line:

```json
{"ok":true,"data":{"fingerprint":"abc123..."}}
{"ok":false,"error":"daemon not running (socket: /tmp/kd.sock)"}
```

- `ok: true` — command succeeded, result in `data`
- `ok: false` — command failed, reason in `error`
- Exit code is 0 even when a command fails; it is 1 only when the daemon
  socket is unreachable. Always check the `ok` field, not the exit code.

## Command Reference

| Command | Description | Example Output |
|---|---|---|
| **Lifecycle** | | |
| `kd start` | Start daemon (foreground) | `{"ok":true,"data":{"fingerprint":"...","ip":"...","socket":"..."}}` |
| `kd stop` | Stop daemon | `{"ok":true,"data":{"status":"stopped"}}` |
| `kd show [what]` | Show info (fingerprint/ip/peer/relay/status/config/all) | `{"ok":true,"data":{"fingerprint":"..."}}` |
| `kd status` | Full status | `{"ok":true,"data":{"running":true,"connection_status":"healthy",...}}` |
| `kd version` | Version and commit hash | `{"ok":true,"data":{"version":"...","commit":"..."}}` |
| **Connection** | | |
| `kd register <fp>` | Register peer fingerprint (or LAN address in local mode) | `{"ok":true,"data":{"registered":"..."}}` |
| `kd discover` | Discover peers on local network (10s scan) | `{"ok":true,"data":{"my_name":"...","peers":[{"name":"...","addr":"..."}]}}` |
| `kd connect` | Connect (auto role via fingerprint tiebreak) | `{"ok":true,"data":{"status":"connected","peer_ip":"...","mode":"..."}}` |
| `kd create` | Create room (blocks until peer joins) | `{"ok":true,"data":{"status":"connected","peer_ip":"..."}}` |
| `kd join` | Join room (blocks until connected) | `{"ok":true,"data":{"status":"connected","peer_ip":"..."}}` |
| `kd disconnect` | Disconnect | `{"ok":true,"data":{"status":"disconnected","new_fingerprint":"..."}}` |
| `kd peer-info` | Peer details and connection mode | `{"ok":true,"data":{"peer_fingerprint":"...","connection_mode":"lan"}}` |
| **File Operations** | | |
| `kd add <path>` | Share a file | `{"ok":true,"data":{"added":"./file.txt"}}` |
| `kd add-as <path> <name>` | Share with custom remote name | `{"ok":true,"data":{"added":"./f.txt","remote_name":"report.txt"}}` |
| `kd unshare <name>` | Stop sharing a file (keeps on disk) | `{"ok":true,"data":{"unshared":"file.txt"}}` |
| `kd list` | List all files | `{"ok":true,"data":{"files":[{"name":"...","size":123,"source":"remote"}]}}` |
| `kd pull <name> [path]` | Download remote file | `{"ok":true,"data":{"pulled":"file.txt","to":"./file.txt"}}` |
| `kd cancel-download <name>` | Pause/cancel active download | `{"ok":true,"data":{"cancelled":"file.txt"}}` |
| `kd progress <name>` | Download progress 0-100 | `{"ok":true,"data":{"name":"file.txt","progress":42}}` |
| `kd file-size <name>` | Get remote file size in bytes | `{"ok":true,"data":{"name":"file.txt","size":1048576}}` |
| `kd local-file-path <name>` | Get local file real path | `{"ok":true,"data":{"name":"file.txt","path":"/tmp/file.txt"}}` |
| `kd bench-pull <name> [path]` | Benchmark: pull file, report MB/s, cleanup | `{"ok":true,"data":{"file":"big.mp4","size":104857600,"duration":"1.5s","mb_per_s":"66.7","mode":"lan"}}` |
| **Contacts** | | |
| `kd contacts` | List saved contacts (with online status) | `{"ok":true,"data":[{"name":"Alice","fingerprint":"...","online":true}]}` |
| `kd add-contact <name> <fp>` | Save a contact | `{"ok":true,"data":{"added":"Alice"}}` |
| `kd remove-contact <fp>` | Remove a contact | `{"ok":true,"data":{"removed":"..."}}` |
| `kd quick-connect <fp>` | Connect to saved contact (1-click) | `{"ok":true,"data":{"status":"connecting"}}` |
| `kd save-contact <name>` | Save current peer | `{"ok":true,"data":{"saved":"Alice"}}` |
| **Identity** | | |
| `kd incognito [on\|off]` | Query or toggle incognito mode | `{"ok":true,"data":{"incognito":"true","fingerprint":"..."}}` |
| **Events** | | |
| `kd poll-event` | Pop next event from queue | `{"ok":true,"data":{"event":"health_changed:healthy:disconnected"}}` |
| **Diagnostics** | | |
| `kd export-logs [dest]` | Export sanitized logs | `{"ok":true,"data":{"path":"keibidrop-sanitized.log"}}` |
| `kd sanitize-logs [dest]` | Alias for export-logs | `{"ok":true,"data":{"path":"keibidrop-sanitized.log"}}` |
| `kd config-path` | Show config file path | `{"ok":true,"data":{"path":"~/.config/keibidrop/config.toml"}}` |
| `kd log-path` | Show log file path | `{"ok":true,"data":{"path":"~/Library/Logs/KeibiDrop/keibidrop.log"}}` |
| `kd help` | Show help text | (plain text) |

## LAN / Local Mode

Connect to peers on the same network without a relay (e.g., phone to desktop):

```bash
./kd discover                         # scans LAN for 10s, returns peer list
./kd register <peer-addr>             # e.g. fe80::1%en0:26431
./kd connect                          # auto role, direct LAN connection
```

After connecting, `kd status` shows `"connection_mode":"lan"`.

### Benchmark a LAN transfer

Once connected, measure real transfer speed:

```bash
./kd bench-pull big-video.mp4         # pulls, times, reports MB/s, cleans up
```

Returns: `{"ok":true,"data":{"file":"big-video.mp4","size":104857600,"duration":"1.5s","mb_per_s":"66.7","mode":"lan"}}`

## Security Notes

- No login or accounts. Identity is a cryptographic fingerprint (ML-KEM-1024 + X25519).
- By default, identity persists across sessions (saved contacts work). Set `KD_INCOGNITO=1` for ephemeral mode.
- All traffic is encrypted end-to-end (AES-256-GCM or ChaCha20-Poly1305, auto-negotiated).
- The relay only sees encrypted blobs. It cannot read your files or metadata.
- Fingerprint exchange is the trust anchor. Send it via a secure channel (Signal, etc.).

## For Agent Developers

When building an agent tool that uses `kd`:

1. **Start the daemon** with `KD_SAVE_PATH` and `KD_NO_FUSE=1` for simple file transfer, or `KD_MOUNT_PATH=./mount` for FUSE (bidirectional sync folder).
2. **Parse all output as JSON** — check the `ok` field.
3. **`kd connect` is the preferred connection method** — auto-determines creator/joiner role via fingerprint tiebreak. Both peers just run `kd connect`.
4. **`kd create` and `kd join` are blocking** — they wait for the peer. Use them if you need explicit role control.
5. **LAN mode**: use `kd discover` to find peers on the same network, then `kd register <addr>` and `kd connect`. No relay needed.
6. **Monitor events**: `kd poll-event` returns health changes, disconnects, and reconnect events. Poll it periodically to react to connection state changes.
7. **Track downloads**: `kd progress <name>` returns 0-100. `kd cancel-download <name>` pauses (partial data preserved for resume).
8. **Benchmark**: `kd bench-pull <name>` measures real transfer throughput and reports MB/s — useful for agents that need to estimate transfer time.
9. Each daemon instance needs unique ports and a unique `KD_SOCKET`.

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
