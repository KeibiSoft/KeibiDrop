# KeibiDrop serving peer, in a container

Turn an always-on box into the other half of a KeibiDrop share. The box
serves a folder; your computer mounts it with the KeibiDrop app and reads it
on demand, end to end encrypted. Only the bytes you read cross the wire.

The container holds the serving side only. It needs no FUSE, no `/dev/fuse`
and no extra capabilities. The machine that reads the files runs the desktop
app or `kd` with its own driver (macFUSE, fuse3 or WinFsp).

Setup guide with the pairing steps: https://keibidrop.com/docs/how-to/run-on-a-nas.html

## Run it

```yaml
services:
  keibidrop:
    image: ghcr.io/keibisoft/keibidrop:latest
    container_name: keibidrop
    restart: unless-stopped
    network_mode: host
    environment:
      KD_PEER: ""            # your computer's 86-character code, or pair later
      KD_PEER_NAME: laptop
      KD_SHARE_READ_ONLY: "false"
    volumes:
      - /path/to/your/files:/shares
      - keibidrop-config:/config
volumes:
  keibidrop-config:
```

```
docker compose up -d
docker compose logs -f
```

The log prints this box's code and the three pairing steps. After pairing,
the box keeps the contact connected on its own: connect from your computer
whenever you like, and after a drop the box reconnects by itself.

Pair later, without restarting:

```
docker exec keibidrop kd add-contact laptop <your 86-character code>
```

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

The release workflow publishes `ghcr.io/keibisoft/keibidrop` for linux/amd64
and linux/arm64 on every tag.

## Limits

- A session is exactly two peers. One box serves one computer at a time.
- The box's uplink bounds a full copy. Opening a file is quick because only
  touched blocks move; exporting all of it is a normal upload.
- Application catalogs and databases (Lightroom, Resolve, SQLite files) stay
  on the computer that runs the application. Media streams over the mount.
