# NAS demo

Records the always-on peer story as a GIF: the box's container log on the
left, the laptop pairing and using the folder on the right.

Needs a running box (any `docker compose up` of `docker/docker-compose.yml`),
a running `kd` daemon on the laptop with its mount, tmux, vhs, rsvg-convert,
ffmpeg and magick.

```
BOX_LOGS="docker compose logs -f" \
NAS_CODE=<the box's code from its log> \
KD=/path/to/kd KD_SOCKET=/tmp/kd.sock MOUNT=$HOME/KeibiDrop/Mount \
./make-gif.sh
```

`BOX_LOGS` can be `ssh box docker logs -f keibidrop` for a remote box. The
laptop pane skips `add-contact` when the contact exists.
