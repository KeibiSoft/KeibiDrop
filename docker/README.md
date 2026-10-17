# KeibiDrop serving peer, in a container

Turn an always-on box into the other half of a KeibiDrop share. The box
serves a folder; your computer mounts it with the KeibiDrop app and reads it
on demand, end to end encrypted. Only the bytes you read cross the wire.

The container holds the serving side only. It needs no FUSE, no `/dev/fuse`
and no extra capabilities. The machine that reads the files runs the desktop
app or `kd` with its own driver (macFUSE, fuse3 or WinFsp).

Full guide: https://keibidrop.com/docs/how-to/run-on-a-nas.html

## Get the image

- On Unraid, open the Apps tab and search for KeibiDrop in Community Applications.
- On Synology Container Manager, QNAP Container Station or any other Docker host,
  use `keibisoft/keibidrop` from Docker Hub.
- Any registry client can also pull `ghcr.io/keibisoft/keibidrop`.

Both registries carry the same image for linux/amd64 and linux/arm64.

## Set it up

1. On your computer, install KeibiDrop and the Teleport driver: macFUSE on a
   Mac, WinFsp on Windows, fuse3 on Linux. Open the app, keep Local mode off
   and press **Copy my code**. That is your computer's code.
2. On the box, save this as `docker-compose.yml`. Set the folder to share and
   paste your computer's code into `KD_PEER`:

   ```yaml
   services:
     keibidrop:
       image: ghcr.io/keibisoft/keibidrop:latest
       container_name: keibidrop
       restart: unless-stopped
       network_mode: host
       environment:
         KD_PEER: ""            # your computer's code, from Copy my code
         KD_PEER_NAME: laptop
         KD_SHARE_READ_ONLY: "false"
       volumes:
         - /path/to/your/files:/shares
         - keibidrop-config:/config
   volumes:
     keibidrop-config:
   ```

3. Start it and read the log. The log prints the box's code.

   ```
   docker compose up -d
   docker compose logs -f
   ```

4. On your computer, turn on **Teleport**, paste the box's code and press
   **Connect**.
5. When it connects, press **Save contact** and turn on **Auto** on the
   contact's row. From then on both sides reconnect by themselves after a
   drop.
6. The box's files now show in your Teleport Folder, `~/KeibiDrop/Mount`
   unless you changed it. Settings shows the path. Open the files from there in any
   app.

If the box started with `KD_PEER` empty, give it your computer's code while it
runs:

```
docker exec keibidrop kd add-contact laptop <your computer's code>
```

## Use it in your apps

Point the app at the Teleport Folder. For DaVinci Resolve: Preferences,
System, Media Storage; keep a local disk as the first entry, add the Teleport
Folder, and leave Direct I/O off. Step by step:
https://keibidrop.com/docs/how-to/edit-from-a-nas-in-davinci-resolve.html

## Check it

When your computer connects, the box's log prints `Peer connected` and the
path: `direct`, `bridge` (through the relay, which forwards encrypted bytes it
cannot read) or `lan`. `docker exec keibidrop kd status` prints the session as
JSON.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `KD_PEER` | empty | The other machine's code. Saved as a contact on first start. |
| `KD_PEER_NAME` | `laptop` | Contact name for `KD_PEER`. |
| `KD_SHARE_READ_ONLY` | `false` | `true` refuses every change the peer sends. Add `:ro` to the `/shares` mount as well. |
| `KD_INBOUND_PORT` | `26431` | TCP port peers dial. Range 26000 to 27000. |
| `KD_OUTBOUND_PORT` | `26432` | TCP port this box dials from. |
| `KD_STRICT` | `false` | `true` never falls back to the bridge. |
| `KD_RESCAN_SHARED_SECONDS` | `30` | How often the box re-walks the shared folder and announces files that landed since the last pass. `0` turns it off; new files then wait for the next connect. |
| `KD_RELAY`, `KD_BRIDGE` | production | A self-hosted relay and bridge. |
| `KD_LOG_FILE` | `/tmp/keibidrop.log` | Engine log. Capped at `KD_LOG_MAX_MB` (64). |

Every other key from the configuration reference works through
`/config/config.toml`. Environment variables win over the file.

`docker exec keibidrop kd status` prints the session as JSON. The full `kd`
surface is documented at https://keibidrop.com/docs/reference/kd-cli.html.

## Networking

Host networking gives the peer the box's real ports and its IPv6 address,
which is what a direct connection needs. In bridge networking every
connection goes through the KeibiDrop bridge instead. Direct connections
cost nothing; bridged traffic rides the free tier until you add relay credit.

## Identity

`/config` holds this box's private identity and its contacts. The key file is
mode 0600 and the container refuses to start when the folder is readable by
other users. Anyone who can read the folder can act as this box: keep it out
of shared backups, or encrypt those backups. Passphrase protection of the
identity needs a terminal at every start, so it is not offered in the
container; run the desktop app where that trade is wanted.

## Build

From the repository root:

```
docker build -f docker/Dockerfile -t keibidrop:dev .
```

The release workflow publishes `ghcr.io/keibisoft/keibidrop` and Docker Hub's
`keibisoft/keibidrop` for linux/amd64 and linux/arm64 on every tag.

## Limits

- A session is exactly two peers. One box serves one computer at a time.
- The box's uplink bounds a full copy. Opening a file is quick because only
  touched blocks move; exporting all of it is a normal upload.
- Application catalogs and databases (Lightroom, Resolve, SQLite files) stay
  on the computer that runs the application. Media streams over the mount.
