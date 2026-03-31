# Transfer Modes

**KEIBI**DROP has two modes. You pick one before connecting. Both use the same encryption and connection.

## Direct Transfer (no-FUSE)

You pick files to send. Your peer picks files to download.

```
Alice: adds file -> notification -> Bob: downloads file
```

Select files to share with drag-and-drop or the file picker. Your peer sees the list, clicks Save, and the file downloads to their save folder (`~/KeibiDrop/Received/` by default). Downloads can be paused and resumed, even after reconnecting.

Only files you explicitly download are stored. No disk usage for files you skip.

| File size | Throughput |
|-----------|-----------|
| 1 MB | ~200 MB/s |
| 10 MB | ~380 MB/s |
| 100 MB | ~590 MB/s |
| 1 GB | ~550 MB/s |

Works on all platforms. Good for sending files, videos, archives, backups. Each transfer is manual and there is no folder browsing.

---

## Virtual Folder (FUSE)

Your peer's files show up as a regular folder on your machine.

```
Alice's files -> appear in Bob's ~/KeibiDrop/Mount/ -> Bob opens them with any app
```

A virtual folder mounts at `~/KeibiDrop/Mount/`. The peer's files appear inside it. Opening a file streams it from the peer on demand, so you don't need to download the whole thing first. Writing a file in the folder sends it to your peer. Changes sync both ways.

Files are cached locally as you access them. A 10 GB file you never open uses 0 bytes on disk. Once you read it, the full file is cached for later.

| File size | Read throughput | Write throughput |
|-----------|----------------|-----------------|
| 1 MB | ~110 MB/s | ~110 MB/s |
| 10 MB | ~200 MB/s | ~200 MB/s |
| 100 MB | ~265 MB/s | ~265 MB/s |
| 1 GB | ~250 MB/s | ~250 MB/s |

The ~48% speed difference compared to Direct Transfer comes from the FUSE kernel boundary, not from encryption or the network.

Only the bytes you actually read cross the wire. Opening a 10 GB video and watching the first 30 seconds transfers just those bytes.

Good for collaborative work, sharing git repos, browsing large collections without downloading everything, and editing files in place with any app.

Requires FUSE: [macFUSE](https://macfuse.github.io/) on macOS, `fuse3` on Linux, [WinFsp](https://winfsp.dev/) on Windows. On macOS you also need `user_allow_other` in `/etc/fuse.conf` for Finder access. Git clone into the mount works but `.lock` file renames can occasionally race with sync notifications.

---

## Side by side

|  | Direct Transfer | Virtual Folder |
|--|----------------|----------------|
| Setup | Nothing extra | Install FUSE |
| Transfer speed (1 GB) | ~550 MB/s | ~250 MB/s |
| Bandwidth usage | Full file per download | Only bytes you read |
| Disk usage | Only files you pull | Cache grows as you browse |
| Sync | Manual (add/pull) | Automatic (read/write) |
| Resume | Pause/resume button in UI | Bitmap-based, resumes on reconnect |
| After reconnect | Partial downloads preserved | Partial cache preserved |
| Git repos | Pull the repo as a folder | Clone directly into mount |
| File browsing | List in the app | Finder / file manager |
| Platform | macOS, Linux, Windows | macOS, Linux (Windows planned) |

---

## Save folder

Both modes write received files to a save folder on disk. Default: `~/KeibiDrop/Received/`. Change it in `~/.config/keibidrop/config.toml` or with the `TO_SAVE_PATH` environment variable.

In Direct Transfer, files you download go straight to the save folder. Partial downloads stay on disk with a `.kdbitmap` sidecar that tracks completed chunks. Resume picks up where it stopped with no re-transfer. The `.kdbitmap` is deleted when the download finishes. Disconnecting does not delete partial files.

In FUSE mode, the mount folder (`~/KeibiDrop/Mount/`) is the virtual view and goes away when you disconnect. The save folder holds the persistent cache. Files you opened or wrote are there. On reconnect, cached files are available immediately.

After disconnecting, everything in the save folder stays. The mount folder unmounts and is empty. Next session starts fresh but previously cached files persist.

To clean up, delete the save folder contents when you no longer need them. `.kdbitmap` files are safe to delete (you lose resume tracking, not data already written to disk).

---

## File ownership and safety

**KEIBI**DROP reads your original files but never modifies them. When your peer downloads a file, they get their own independent copy. Both peers always have separate copies. If Alice edits a file and Bob edits the same file, both edits exist on their own machines. Sync notifications use last-write-wins, but no local file is overwritten without the user doing something.

In FUSE mode, the mount point is sandboxed. You cannot navigate above it (`cd ..` stops at the mount root). The mount shows the peer's files. The save folder holds your local copies. The original file on the sender's machine is read-only from the tool's perspective.

```
Alice's machine:
  /home/alice/project/report.pdf    <- original, read by KeibiDrop, never modified

Bob's machine:
  ~/KeibiDrop/Mount/report.pdf      <- virtual, reads from Alice on demand
  ~/KeibiDrop/Received/report.pdf   <- local cache, persists after disconnect
```

---

## What the tool changes on your system

`~/.config/keibidrop/config.toml` is created on first run with default values.

Two directories are created if they don't exist: the save folder and the mount folder (defaults: `~/KeibiDrop/Received/` and `~/KeibiDrop/Mount/`).

A log file is written to `~/Library/Logs/KeibiDrop/keibidrop.log` on macOS or `~/.local/share/keibidrop/keibidrop.log` on Linux.

One TCP port (default 26431) is opened for inbound peer connections over IPv6. No other ports. No background processes after you close the app.

For FUSE mode only: macOS may need `user_allow_other` added to `/etc/fuse.conf` (one-time). macFUSE or fuse3 must be installed separately. Linux may need the same line uncommented in `/etc/fuse.conf`.

No daemons, no startup items, no browser extensions, no telemetry, no accounts.

---

## Encryption and connection (both modes)

- ML-KEM-1024 + X25519 post-quantum key exchange
- AES-256-GCM or ChaCha20-Poly1305, auto-negotiated based on hardware AES support
- Session re-keying every 1 GB or ~1M messages for forward secrecy
- Peer-to-peer over IPv6, no relay for file data
- Health monitoring with automatic reconnection
- Pause and resume downloads across disconnections
- Desktop UI, interactive CLI, or agent CLI (Unix socket JSON protocol)
